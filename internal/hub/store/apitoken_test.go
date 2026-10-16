package store

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAPITokenLifecycle(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	ctx := t.Context()
	h := sha256.Sum256([]byte("heron_at_x"))
	tok, err := s.CreateAPIToken(ctx, "ci", h, clk.Now(), 100, nil, time.Time{})
	if err != nil || tok.ID == 0 || tok.Name != "ci" || !tok.CreatedAt.Equal(clk.Now().Truncate(time.Second)) || !tok.LastUsedAt.IsZero() {
		t.Fatalf("create: %+v %v", tok, err)
	}
	got, state, err := s.APITokenByHash(ctx, h)
	if err != nil || state != TokenLive || got.ID != tok.ID {
		t.Fatalf("by hash: %+v %v %v", got, state, err)
	}
	if list, err := s.ListAPITokens(ctx); err != nil || len(list) != 1 || list[0].ID != tok.ID {
		t.Fatalf("list: %+v %v", list, err)
	}
	if found, err := s.DeleteAPIToken(ctx, tok.ID); err != nil || !found {
		t.Fatalf("delete: %v %v", found, err)
	}
	if _, state, err := s.APITokenByHash(ctx, h); err != nil || state != TokenMissing {
		t.Fatalf("deleted token still resolves: %v %v", state, err)
	}
	if found, err := s.DeleteAPIToken(ctx, tok.ID); err != nil || found {
		t.Fatalf("second delete reported found=%v err=%v", found, err)
	}
}

// 吊销按 id 进行：id 若被复用，针对旧 token 写下的吊销命令会落到新 token 上。
func TestAPITokenIDsAreNotReused(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	ctx := t.Context()
	first, _ := s.CreateAPIToken(ctx, "a", sha256.Sum256([]byte("a")), clk.Now(), 100, nil, time.Time{})
	if _, err := s.DeleteAPIToken(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateAPIToken(ctx, "b", sha256.Sum256([]byte("b")), clk.Now(), 100, nil, time.Time{})
	if err != nil || second.ID <= first.ID {
		t.Fatalf("id reused or went backwards: first %d second %d err %v", first.ID, second.ID, err)
	}
}

func TestAPITokenLimitIsDecidedInsideTheWriteTransaction(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	ctx := t.Context()
	const limit = 5
	var wg sync.WaitGroup
	var created, refused atomic.Int32
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.CreateAPIToken(ctx, "n", sha256.Sum256([]byte(fmt.Sprint(i))), clk.Now(), limit, nil, time.Time{})
			switch {
			case err == nil:
				created.Add(1)
			case errors.Is(err, ErrAPITokenLimit):
				refused.Add(1)
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if created.Load() != limit || refused.Load() != 12-limit {
		t.Fatalf("created %d refused %d, want %d and %d", created.Load(), refused.Load(), limit, 12-limit)
	}
}

func TestTouchRecordsUseAndNeverResurrects(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	ctx := t.Context()
	h := sha256.Sum256([]byte("t"))
	tok, _ := s.CreateAPIToken(ctx, "t", h, clk.Now(), 100, nil, time.Time{})
	clk.Advance(time.Minute)
	done := make(chan error, 1)
	s.TouchAPITokenAsync(tok.ID, clk.Now(), func(err error) { done <- err })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.APITokenByHash(ctx, h)
	if !got.LastUsedAt.Equal(clk.Now().Truncate(time.Second)) {
		t.Fatalf("last used %v, want %v", got.LastUsedAt, clk.Now())
	}
	if _, err := s.DeleteAPIToken(ctx, tok.ID); err != nil {
		t.Fatal(err)
	}
	s.TouchAPITokenAsync(tok.ID, clk.Now(), func(err error) { done <- err })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if list, err := s.ListAPITokens(ctx); err != nil || len(list) != 0 {
		t.Fatalf("touch after delete resurrected a token: %+v %v", list, err)
	}
}

func TestDeleteAllAPITokens(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	ctx := t.Context()
	for i := 0; i < 3; i++ {
		s.CreateAPIToken(ctx, "x", sha256.Sum256([]byte(fmt.Sprint(i))), clk.Now(), 100, nil, time.Time{})
	}
	if n, err := s.DeleteAllAPITokens(ctx); err != nil || n != 3 {
		t.Fatalf("deleted %d err %v", n, err)
	}
}

// grant 不合法——形状错误与引用不存在的节点——都是输入校验（ErrInvalidGrant），不是授权拒绝，也不落库。
func TestCreateAPITokenRejectsInvalidGrant(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	node, _, err := s.CreateNode(t.Context(), NewNode{Name: "node"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		grant TokenGrant
		want  error
	}{
		"unknown permission":         {TokenGrant{Permissions: []Permission{"root"}}, ErrInvalidGrant},
		"all nodes with node list":   {TokenGrant{AllNodes: true, NodeIDs: []int64{node}}, ErrInvalidGrant},
		"node that does not exist":   {TokenGrant{NodeIDs: []int64{node + 1}}, ErrInvalidGrant},
		"valid grant for comparison": {TokenGrant{NodeIDs: []int64{node}, Permissions: []Permission{PermissionConfigure}}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := s.CreateAPIToken(t.Context(), name, sha256.Sum256([]byte(name)), clk.Now(), 100, &tc.grant, time.Time{})
			if !errors.Is(err, tc.want) || errors.Is(err, ErrPermission) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
	list, err := s.ListAPITokens(t.Context())
	if err != nil || len(list) != 1 {
		t.Fatalf("rejected grants were stored: %+v %v", list, err)
	}
}

// 编辑只换可编辑字段：明文哈希派生的身份、创建与最近使用时刻不变，持有者不用换配置。
func TestUpdateAPITokenKeepsCredentialAndReplacesGrant(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	a, _, err := s.CreateNode(t.Context(), NewNode{Name: "a"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := s.CreateNode(t.Context(), NewNode{Name: "b"}, hash(2))
	if err != nil {
		t.Fatal(err)
	}
	tok, err := s.CreateAPIToken(t.Context(), "agent", sha256.Sum256([]byte("agent")), clk.Now(), 100, &TokenGrant{NodeIDs: []int64{a}, Permissions: []Permission{PermissionConfigure}}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, "UPDATE api_token SET last_used_at = 5 WHERE id = ?", tok.ID)
	expires := clk.Now().Add(time.Hour)
	got, err := s.UpdateAPIToken(t.Context(), tok.ID, "renamed", &TokenGrant{NodeIDs: []int64{b}, Permissions: []Permission{PermissionDelete}}, expires, versionOf(t, s, tok.ID))
	if err != nil {
		t.Fatal(err)
	}
	if got.Identity != tok.Identity || !got.CreatedAt.Equal(tok.CreatedAt) || got.LastUsedAt.Unix() != 5 {
		t.Fatalf("identity or timestamps changed: %+v -> %+v", tok, got)
	}
	if got.Name != "renamed" || got.AllNodes || !slices.Equal(got.NodeIDs, []int64{b}) ||
		!slices.Equal(got.Permissions, []Permission{PermissionDelete}) || got.ExpiresAt.Unix() != expires.Unix() {
		t.Fatalf("update not applied: %+v", got)
	}
	list, err := s.ListAPITokens(t.Context())
	if err != nil || len(list) != 1 || !reflect.DeepEqual(list[0], got) {
		t.Fatalf("stored row differs from returned row: %+v %v", list, err)
	}
	// 缺失的 grant 与新建相同，即全站只读；零值时刻即不过期。
	got, err = s.UpdateAPIToken(t.Context(), tok.ID, "renamed", nil, time.Time{}, versionOf(t, s, tok.ID))
	if err != nil || !got.AllNodes || len(got.NodeIDs) != 0 || len(got.Permissions) != 0 || !got.ExpiresAt.IsZero() {
		t.Fatalf("nil grant / zero expiry: %+v %v", got, err)
	}
}

// 编辑不要求有效期在未来：原样带回过去的时刻（只改名）与设到过去（立即过期）都照存，判定在读侧。
func TestUpdateAPITokenStoresPastExpiry(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	tok, err := s.CreateAPIToken(t.Context(), "agent", sha256.Sum256([]byte("agent")), clk.Now(), 100, nil, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	past := clk.Now().Add(-time.Minute)
	got, err := s.UpdateAPIToken(t.Context(), tok.ID, "agent", nil, past, versionOf(t, s, tok.ID))
	if err != nil || got.ExpiresAt.Unix() != past.Unix() {
		t.Fatalf("past expiry: %+v %v", got, err)
	}
}

func TestUpdateAPITokenScopeTransitions(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	a, _, err := s.CreateNode(t.Context(), NewNode{Name: "a"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateNode(t.Context(), NewNode{Name: "b"}, hash(2)); err != nil {
		t.Fatal(err)
	}
	tok, err := s.CreateAPIToken(t.Context(), "agent", sha256.Sum256([]byte("agent")), clk.Now(), 100, &TokenGrant{NodeIDs: []int64{a}}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithPrincipal(t.Context(), tok)
	visible := func() int {
		t.Helper()
		nodes, err := s.ListNodes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return len(nodes)
	}
	if visible() != 1 {
		t.Fatal("explicit scope before edit")
	}
	if _, err := s.UpdateAPIToken(t.Context(), tok.ID, "agent", &TokenGrant{AllNodes: true}, time.Time{}, versionOf(t, s, tok.ID)); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := s.r.QueryRow("SELECT COUNT(*) FROM api_token_node WHERE token_id = ?", tok.ID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("all-nodes grant kept explicit rows: %d %v", rows, err)
	}
	if visible() != 2 {
		t.Fatal("all-nodes scope after edit")
	}
	if _, err := s.UpdateAPIToken(t.Context(), tok.ID, "agent", &TokenGrant{}, time.Time{}, versionOf(t, s, tok.ID)); err != nil {
		t.Fatal(err)
	}
	if visible() != 0 {
		t.Fatal("empty explicit scope after edit")
	}
}

// 去掉注册权限即关闭它的窗口，与吊销一致；保留注册权限的编辑不动窗口。
func TestUpdateAPITokenWithoutRegisterClosesItsWindow(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	register := &TokenGrant{AllNodes: true, Permissions: []Permission{PermissionRegister}}
	tok, err := s.CreateAPIToken(t.Context(), "registrar", sha256.Sum256([]byte("registrar")), clk.Now(), 100, register, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetRegisterWindow(WithPrincipal(t.Context(), tok), hash(2), clk.Now().Add(time.Hour), 5); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateAPIToken(t.Context(), tok.ID, "renamed", register, time.Time{}, versionOf(t, s, tok.ID)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RegisterNode(t.Context(), hash(2), "first", hash(3)); err != nil {
		t.Fatalf("edit that kept register closed the window: %v", err)
	}
	if _, err := s.UpdateAPIToken(t.Context(), tok.ID, "renamed", &TokenGrant{AllNodes: true}, time.Time{}, versionOf(t, s, tok.ID)); err != nil {
		t.Fatal(err)
	}
	var windows int
	if err := s.r.QueryRow("SELECT COUNT(*) FROM register_window WHERE owner_id = ?", tok.ID).Scan(&windows); err != nil || windows != 0 {
		t.Fatalf("window kept after register was removed: %d %v", windows, err)
	}
	if _, _, err := s.RegisterNode(t.Context(), hash(2), "late", hash(4)); !errors.Is(err, ErrNoWindow) {
		t.Fatalf("registration after register was removed: %v", err)
	}
}

// 拒绝的编辑不落库：不存在的 token 是 ErrNotFound，grant 不合法（含引用不存在的节点）是 ErrInvalidGrant。
func TestUpdateAPITokenRejects(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	node, _, err := s.CreateNode(t.Context(), NewNode{Name: "node"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	tok, err := s.CreateAPIToken(t.Context(), "agent", sha256.Sum256([]byte("agent")), clk.Now(), 100, &TokenGrant{NodeIDs: []int64{node}}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.ListAPITokens(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		id    int64
		grant TokenGrant
		want  error
	}{
		"token that does not exist": {tok.ID + 1, TokenGrant{AllNodes: true}, ErrNotFound},
		"unknown permission":        {tok.ID, TokenGrant{Permissions: []Permission{"root"}}, ErrInvalidGrant},
		"all nodes with node list":  {tok.ID, TokenGrant{AllNodes: true, NodeIDs: []int64{node}}, ErrInvalidGrant},
		"node that does not exist":  {tok.ID, TokenGrant{NodeIDs: []int64{node + 1}}, ErrInvalidGrant},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := s.UpdateAPIToken(t.Context(), tc.id, "changed", &tc.grant, time.Time{}, versionOf(t, s, tc.id)); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
	after, err := s.ListAPITokens(t.Context())
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("rejected edits changed tokens: %+v -> %+v %v", before, after, err)
	}
}

// 编辑按读到时的版本提交：打开编辑后 agent 经自己的窗口纳入的节点会改变版本，拿旧版本整体替换会静默拿走它，
// 所以旧版本被拒、节点保留；刷新最近使用时刻不改版本，否则每次调用都会让正在进行的编辑失效。
func TestUpdateAPITokenRejectsStaleVersion(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	grant := &TokenGrant{Permissions: []Permission{PermissionRegister}}
	tok, err := s.CreateAPIToken(t.Context(), "agent", sha256.Sum256([]byte("agent")), clk.Now(), 100, grant, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	read := tok.Version()
	mustExec(t, s, "UPDATE api_token SET last_used_at = 99 WHERE id = ?", tok.ID)
	if list, err := s.ListAPITokens(t.Context()); err != nil || list[0].Version() != read {
		t.Fatalf("touching last_used_at changed the version: %v", err)
	}
	if err := s.SetRegisterWindow(WithPrincipal(t.Context(), tok), hash(2), clk.Now().Add(time.Hour), 5); err != nil {
		t.Fatal(err)
	}
	node, _, err := s.RegisterNode(t.Context(), hash(2), "registered", hash(3))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateAPIToken(t.Context(), tok.ID, "agent", grant, clk.Now().Add(time.Hour), read); !errors.Is(err, ErrAPITokenChanged) {
		t.Fatalf("stale version accepted: %v", err)
	}
	list, err := s.ListAPITokens(t.Context())
	if err != nil || !slices.Equal(list[0].NodeIDs, []int64{node}) || !list[0].ExpiresAt.IsZero() {
		t.Fatalf("rejected edit changed the token: %+v %v", list, err)
	}
	got, err := s.UpdateAPIToken(t.Context(), tok.ID, "agent", &TokenGrant{Permissions: grant.Permissions, NodeIDs: list[0].NodeIDs}, clk.Now().Add(time.Hour), list[0].Version())
	if err != nil || !slices.Equal(got.NodeIDs, []int64{node}) || got.Version() == list[0].Version() {
		t.Fatalf("edit with the current version: %+v %v", got, err)
	}
	if _, err := s.UpdateAPIToken(t.Context(), tok.ID+1, "x", nil, time.Time{}, read); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing token: %v", err)
	}
}

// versionOf 是 id 当前的 Version()，编辑时带回；token 不存在时为空串。
func versionOf(t *testing.T, s *Store, id int64) string {
	t.Helper()
	list, err := s.ListAPITokens(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range list {
		if tok.ID == id {
			return tok.Version()
		}
	}
	return ""
}

// grant 不合法时点名 TokenGrant 的字段与原因，api 据此写出请求里的字段路径；它仍是 ErrInvalidGrant。
func TestInvalidGrantNamesTheField(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	node, _, err := s.CreateNode(t.Context(), NewNode{Name: "node"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		grant         TokenGrant
		field, reason string
	}{
		"unknown permission":       {TokenGrant{Permissions: []Permission{"root"}}, "permissions", `unknown permission "root"`},
		"all nodes with node list": {TokenGrant{AllNodes: true, NodeIDs: []int64{node}}, "node_ids", "must be empty when all_nodes is true"},
		"node that does not exist": {TokenGrant{NodeIDs: []int64{node + 1}}, "node_ids", fmt.Sprintf("node %d does not exist", node+1)},
	} {
		_, err := s.CreateAPIToken(t.Context(), name, sha256.Sum256([]byte(name)), clk.Now(), 100, &tc.grant, time.Time{})
		var ge *GrantError
		if !errors.As(err, &ge) || ge.Field != tc.field || ge.Reason != tc.reason || !errors.Is(err, ErrInvalidGrant) {
			t.Errorf("%s: %v, want field %q reason %q", name, err, tc.field, tc.reason)
		}
	}
}

// 建返回的值就是库里的那一行：节点按 id 排序、去重，Version() 与此后的读取相同——面板拿它的列表版本编辑。
func TestCreateAPITokenReturnsTheStoredRow(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	a, _, err := s.CreateNode(t.Context(), NewNode{Name: "a"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := s.CreateNode(t.Context(), NewNode{Name: "b"}, hash(2))
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.CreateAPIToken(t.Context(), "agent", sha256.Sum256([]byte("agent")), clk.Now(), 100, &TokenGrant{NodeIDs: []int64{b, a, b}}, clk.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.ListAPITokens(t.Context())
	if err != nil || len(list) != 1 || !reflect.DeepEqual(list[0], got) || list[0].Version() != got.Version() {
		t.Fatalf("returned %+v, stored %+v %v", got, list, err)
	}
}
