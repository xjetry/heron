package authz

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/store"
)

// 以下用例把策略装进真实的 store 写边界：裁决发生在写事务里，与写看到同一份快照。

func openStore(t *testing.T) (*store.Store, string, *clock.Fake) {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	path := filepath.Join(t.TempDir(), "t.db")
	s, err := store.Open(path, clk, slog.Default(), store.MigrateSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path, clk
}

func hash(b byte) []byte { h := make([]byte, 32); h[0] = b; return h }

func newChange(action store.ChangeAction, op store.Operation) *store.Change {
	return &store.Change{Operation: op, Action: action, Policy: Policy{}}
}

func readDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=query_only(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestChangeRechecksRevocationInsideTransaction(t *testing.T) {
	s, path, clk := openStore(t)
	node, _, err := s.CreateNode(t.Context(), store.NewNode{Name: "node"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.CreateAPIToken(t.Context(), "writer", sha256.Sum256([]byte("writer")), clk.Now(), 100, &store.TokenGrant{NodeIDs: []int64{node}, Permissions: []store.Permission{store.PermissionRotate}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := store.WithPrincipal(t.Context(), p)
	c := newChange(store.ActionRotateNodeToken, store.Operation{OwnerID: p.ID, RequestID: "rotate", RequestHash: "x", ResourceID: node})
	c.ExpectedVersion, err = s.ChangeVersion(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteAPIToken(t.Context(), p.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTokenHash(store.WithChange(ctx, c), node, hash(1), hash(2)); !errors.Is(err, store.ErrPermission) {
		t.Fatalf("revoked grant committed: %v", err)
	}
	var got []byte
	if err := readDB(t, path).QueryRow("SELECT token_hash FROM node WHERE id=?", node).Scan(&got); err != nil || !slices.Equal(got, hash(1)) {
		t.Fatalf("revoked write changed credential: %x %v", got, err)
	}
}

func TestAgenticReplayAfterScopeRemoval(t *testing.T) {
	s, _, clk := openStore(t)
	node, _, err := s.CreateNode(t.Context(), store.NewNode{Name: "node"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.CreateAPIToken(t.Context(), "writer", sha256.Sum256([]byte("writer")), clk.Now(), 100, &store.TokenGrant{NodeIDs: []int64{node}, Permissions: []store.Permission{store.PermissionDelete}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := store.WithPrincipal(t.Context(), p)
	c := newChange(store.ActionDeleteNode, store.Operation{OwnerID: p.ID, RequestID: "delete", RequestHash: "delete", ResourceID: node})
	c.ExpectedVersion, err = s.ChangeVersion(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	retry := *c
	if err := s.DeleteNode(store.WithChange(ctx, c), node); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteNode(store.WithChange(ctx, &retry), node); !errors.Is(err, store.ErrReplay) {
		t.Fatalf("committed retry rejected after scope removal: %v", err)
	}
	if retry.ResourceID != node || retry.CommittedAt == 0 {
		t.Fatalf("retry lost original receipt: %+v", retry)
	}
}

func TestAgenticRestoreSeparatesReusedTokenIDs(t *testing.T) {
	source, _, clk := openStore(t)
	target, targetPath, _ := openStore(t)
	create := func(s *store.Store, name string, hashByte byte) store.APIToken {
		t.Helper()
		p, err := s.CreateAPIToken(t.Context(), name, sha256.Sum256([]byte(name)), clk.Now(), 100, &store.TokenGrant{Permissions: []store.Permission{store.PermissionCreate}})
		if err != nil {
			t.Fatal(err)
		}
		ctx := store.WithPrincipal(t.Context(), p)
		c := newChange(store.ActionCreateNode, store.Operation{OwnerID: p.ID, RequestID: "same-key", RequestHash: name})
		c.ExpectedVersion, err = s.ChangeVersion(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.CreateNode(store.WithChange(ctx, c), store.NewNode{Name: name}, hash(hashByte)); err != nil {
			t.Fatal(err)
		}
		return p
	}
	p, q := create(source, "source", 1), create(target, "target", 2)
	if p.ID != q.ID || p.Identity == q.Identity {
		t.Fatal("fixture must reuse numeric IDs without reusing credentials")
	}
	snapshot := filepath.Join(t.TempDir(), "snapshot.db")
	if err := source.SnapshotConfig(t.Context(), snapshot); err != nil {
		t.Fatal(err)
	}
	if err := target.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Restore(t.Context(), targetPath, snapshot, "", "", clk.Now(), slog.Default()); err != nil {
		t.Fatal(err)
	}
	restored, err := store.Open(targetPath, clk, slog.Default(), store.RequireCurrentSchema)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	actual, state, err := restored.APITokenByHash(t.Context(), sha256.Sum256([]byte("source")))
	if err != nil || state != store.TokenLive || actual.Identity != p.Identity {
		t.Fatalf("restored credential identity changed: %+v %v", actual, err)
	}
	ctx := store.WithPrincipal(t.Context(), actual)
	o, err := restored.FindOperation(ctx, actual.ID, "same-key")
	if err != nil || o.RequestHash != "source" || !strings.Contains(o.AfterJSON, "source") || strings.Contains(o.AfterJSON, "target") {
		t.Fatalf("receipt crossed credential identities: %+v %v", o, err)
	}
	list, err := restored.ListOperations(ctx, actual.ID, "", 100)
	if err != nil || len(list) != 1 || list[0].RequestHash != "source" {
		t.Fatalf("restored token saw another identity's audit: %+v %v", list, err)
	}
	all, err := restored.ListOperations(t.Context(), actual.ID, "", 100)
	if err != nil || len(all) != 2 {
		t.Fatalf("restore lost an identity's history: %+v %v", all, err)
	}
	if o.ID == "" || list[0].ID != o.ID || all[0].ID == all[1].ID {
		t.Fatalf("restored receipts lack distinct stable IDs: own=%+v all=%+v", o, all)
	}
	original, err := source.FindOperation(store.WithPrincipal(t.Context(), p), p.ID, "same-key")
	if err != nil || original.ID != o.ID {
		t.Fatalf("restore changed receipt ID: before=%+v after=%+v err=%v", original, o, err)
	}
	// 复用的数字 id 不能借原持有者的授权：以复用者的身份执行时，策略在事务里读到的行身份不同。
	impostor := q
	c := newChange(store.ActionCreateNode, store.Operation{OwnerID: impostor.ID, RequestID: "impostor", RequestHash: "impostor"})
	if _, err := restored.ChangeVersion(store.WithPrincipal(t.Context(), impostor), c); !errors.Is(err, store.ErrPermission) {
		t.Fatalf("reused token id passed principal check after restore: %v", err)
	}
}

// 创建授予：建节点时 ResourceID 为 0，作用域放行；写事务把新节点授予创建者，同一 token 随后能改它、不能改别人的。
func TestCreateGrantsNewNodeToCreator(t *testing.T) {
	s, _, clk := openStore(t)
	p, err := s.CreateAPIToken(t.Context(), "creator", sha256.Sum256([]byte("creator")), clk.Now(), 100, &store.TokenGrant{Permissions: []store.Permission{store.PermissionCreate, store.PermissionRotate}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := store.WithPrincipal(t.Context(), p)
	c := newChange(store.ActionCreateNode, store.Operation{OwnerID: p.ID, RequestID: "create", RequestHash: "create"})
	if c.ExpectedVersion, err = s.ChangeVersion(ctx, c); err != nil {
		t.Fatal(err)
	}
	created, _, err := s.CreateNode(store.WithChange(ctx, c), store.NewNode{Name: "created"}, hash(2))
	if err != nil || c.ResourceID != created || c.CommittedAt == 0 {
		t.Fatalf("create: id=%d receipt=%+v err=%v", created, c.Operation, err)
	}
	foreign, _, err := s.CreateNode(t.Context(), store.NewNode{Name: "foreign"}, hash(3))
	if err != nil {
		t.Fatal(err)
	}
	p, _, err = s.APITokenByHash(t.Context(), sha256.Sum256([]byte("creator")))
	if err != nil {
		t.Fatal(err)
	}
	ctx = store.WithPrincipal(t.Context(), p)
	rotate := newChange(store.ActionRotateNodeToken, store.Operation{OwnerID: p.ID, RequestID: "rotate", RequestHash: "rotate", ResourceID: created})
	if _, err := s.ChangeVersion(ctx, rotate); err != nil {
		t.Fatalf("creator cannot touch the node it created: %v", err)
	}
	rotate.ResourceID = foreign
	if _, err := s.ChangeVersion(ctx, rotate); !errors.Is(err, store.ErrPermission) {
		t.Fatalf("creator reached a node outside its grant: %v", err)
	}
}

// 过期的 token 在写事务里被拒绝，复活后放行：ExecuteChange 的裁决经 ChangeReader 重读，不信入口缓存的主体。
func TestChangeRejectsExpiredTokenAndAcceptsRevived(t *testing.T) {
	s, _, clk := openStore(t)
	node, _, err := s.CreateNode(t.Context(), store.NewNode{Name: "node"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	grant := &store.TokenGrant{NodeIDs: []int64{node}, Permissions: []store.Permission{store.PermissionRotate}}
	p, err := s.CreateAPIToken(t.Context(), "writer", sha256.Sum256([]byte("writer")), clk.Now(), 100, grant)
	if err != nil {
		t.Fatal(err)
	}
	ctx := store.WithPrincipal(t.Context(), p)
	if _, err := s.UpdateAPIToken(t.Context(), p.ID, p.Name, grant, clk.Now()); err != nil {
		t.Fatal(err)
	}
	c := newChange(store.ActionRotateNodeToken, store.Operation{OwnerID: p.ID, RequestID: "rotate", RequestHash: "x", ResourceID: node})
	if _, err := s.ChangeVersion(ctx, c); !errors.Is(err, store.ErrPermission) {
		t.Fatalf("expired token previewed a change: %v", err)
	}
	if _, err := s.UpdateAPIToken(t.Context(), p.ID, p.Name, grant, clk.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if c.ExpectedVersion, err = s.ChangeVersion(ctx, c); err != nil {
		t.Fatalf("revived token rejected: %v", err)
	}
	if err := s.SetTokenHash(store.WithChange(ctx, c), node, hash(1), hash(2)); err != nil {
		t.Fatalf("revived token could not commit: %v", err)
	}
}
