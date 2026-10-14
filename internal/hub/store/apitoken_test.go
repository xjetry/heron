package store

import (
	"crypto/sha256"
	"errors"
	"fmt"
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
	tok, err := s.CreateAPIToken(ctx, "ci", h, clk.Now(), 100, nil)
	if err != nil || tok.ID == 0 || tok.Name != "ci" || !tok.CreatedAt.Equal(clk.Now().Truncate(time.Second)) || !tok.LastUsedAt.IsZero() {
		t.Fatalf("create: %+v %v", tok, err)
	}
	got, ok, err := s.APITokenByHash(ctx, h)
	if err != nil || !ok || got.ID != tok.ID {
		t.Fatalf("by hash: %+v %v %v", got, ok, err)
	}
	if list, err := s.ListAPITokens(ctx); err != nil || len(list) != 1 || list[0].ID != tok.ID {
		t.Fatalf("list: %+v %v", list, err)
	}
	if found, err := s.DeleteAPIToken(ctx, tok.ID); err != nil || !found {
		t.Fatalf("delete: %v %v", found, err)
	}
	if _, ok, err := s.APITokenByHash(ctx, h); err != nil || ok {
		t.Fatalf("deleted token still resolves: %v %v", ok, err)
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
	first, _ := s.CreateAPIToken(ctx, "a", sha256.Sum256([]byte("a")), clk.Now(), 100, nil)
	if _, err := s.DeleteAPIToken(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateAPIToken(ctx, "b", sha256.Sum256([]byte("b")), clk.Now(), 100, nil)
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
			_, err := s.CreateAPIToken(ctx, "n", sha256.Sum256([]byte(fmt.Sprint(i))), clk.Now(), limit, nil)
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
	tok, _ := s.CreateAPIToken(ctx, "t", h, clk.Now(), 100, nil)
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
		s.CreateAPIToken(ctx, "x", sha256.Sum256([]byte(fmt.Sprint(i))), clk.Now(), 100, nil)
	}
	if n, err := s.DeleteAllAPITokens(ctx); err != nil || n != 3 {
		t.Fatalf("deleted %d err %v", n, err)
	}
}

// grant 的形状错误是输入校验（ErrInvalidGrant），不是授权拒绝；引用不存在的节点是 ErrNotFound。三者都不落库。
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
		"node that does not exist":   {TokenGrant{NodeIDs: []int64{node + 1}}, ErrNotFound},
		"valid grant for comparison": {TokenGrant{NodeIDs: []int64{node}, Permissions: []Permission{PermissionConfigure}}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := s.CreateAPIToken(t.Context(), name, sha256.Sum256([]byte(name)), clk.Now(), 100, &tc.grant)
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
