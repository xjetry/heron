package authz

import (
	"context"
	"errors"
	"testing"

	"github.com/xjetry/heron/internal/hub/store"
)

// fakeReader 是事务内读取的替身：tokens 与 rules 之外的 id 一律不存在。
type fakeReader struct {
	tokens map[int64]store.APIToken
	rules  map[store.ChangeKind]map[int64]store.RuleScope
}

func (f fakeReader) APIToken(id int64) (store.APIToken, error) {
	t, ok := f.tokens[id]
	if !ok {
		return store.APIToken{}, store.ErrNotFound
	}
	return t, nil
}

func (f fakeReader) RuleScope(kind store.ChangeKind, id int64) (store.RuleScope, error) {
	s, ok := f.rules[kind][id]
	if !ok {
		return store.RuleScope{}, store.ErrNotFound
	}
	return s, nil
}

func token(id int64, identity string, nodes []int64, perms ...store.Permission) store.APIToken {
	return store.APIToken{ID: id, Identity: identity, TokenGrant: store.TokenGrant{Permissions: perms, NodeIDs: nodes}}
}

func TestPrincipal(t *testing.T) {
	live := token(1, "live", []int64{10}, store.PermissionConfigure)
	r := fakeReader{tokens: map[int64]store.APIToken{1: live}}
	c := &store.Change{Action: store.ActionUpdateNode, Operation: store.Operation{ResourceID: 10}}
	if got, err := (Policy{}).Principal(context.Background(), r, c); got != nil || err != nil {
		t.Fatalf("session principal: token=%v err=%v, want nil, nil", got, err)
	}
	cases := []struct {
		name   string
		caller store.APIToken
		action store.ChangeAction
		want   error
	}{
		{"granted", live, store.ActionUpdateNode, nil},
		{"revoked", token(2, "gone", []int64{10}, store.PermissionConfigure), store.ActionUpdateNode, store.ErrPermission},
		// 数字 id 被另一枚凭据复用：库里同 id 的行身份不同，入口缓存的主体不能借它的授权。
		{"reused id", token(1, "previous-holder", []int64{10}, store.PermissionConfigure), store.ActionUpdateNode, store.ErrPermission},
		{"missing permission", live, store.ActionDeleteNode, store.ErrPermission},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &store.Change{Action: tc.action, Operation: store.Operation{ResourceID: 10}}
			got, err := (Policy{}).Principal(store.WithPrincipal(context.Background(), tc.caller), r, c)
			if !errors.Is(err, tc.want) || (err == nil) != (got != nil) {
				t.Fatalf("token=%v err=%v, want err %v", got, err, tc.want)
			}
			if got != nil && got.Identity != "live" {
				t.Fatalf("principal returned the cached caller instead of the stored row: %+v", got)
			}
		})
	}
}

func TestScopeAndResource(t *testing.T) {
	scoped := token(1, "scoped", []int64{10}, store.PermissionConfigure, store.PermissionCreate, store.PermissionRegister, store.PermissionUpdate)
	r := fakeReader{rules: map[store.ChangeKind]map[int64]store.RuleScope{
		store.ChangeProbe: {
			1: {NodeIDs: []int64{10}},
			2: {NodeIDs: []int64{10, 11}},
			3: {AllNodes: true},
			4: {TagCount: 1},
			5: {},
		},
		store.ChangeAlert: {
			1: {NodeIDs: []int64{10}},
			2: {NodeIDs: []int64{10}, TaskID: 1},
			3: {NodeIDs: []int64{10}, TaskID: 2},
		},
	}}
	cases := []struct {
		name   string
		change store.Change
		want   error
	}{
		{"session", store.Change{Action: store.ActionDeleteTag}, nil},
		{"create node grants before identity exists", store.Change{Action: store.ActionCreateNode}, nil},
		{"node in grant", store.Change{Action: store.ActionUpdateNode, Operation: store.Operation{ResourceID: 10}}, nil},
		{"node outside grant", store.Change{Action: store.ActionUpdateNode, Operation: store.Operation{ResourceID: 11}}, store.ErrPermission},
		{"update outside grant", store.Change{Action: store.ActionStartUpdate, Operation: store.Operation{ResourceID: 11}}, store.ErrPermission},
		{"own window", store.Change{Action: store.ActionOpenRegisterWindow, Operation: store.Operation{ResourceID: 1}}, nil},
		{"shared window", store.Change{Action: store.ActionCloseRegisterWindow, Operation: store.Operation{ResourceID: 0}}, store.ErrPermission},
		{"tag needs all nodes", store.Change{Action: store.ActionDeleteTag, Operation: store.Operation{ResourceID: 7}}, store.ErrPermission},
		{"new probe", store.Change{Action: store.ActionSaveProbeTask}, nil},
		{"probe in grant", store.Change{Action: store.ActionSaveProbeTask, Operation: store.Operation{ResourceID: 1}}, nil},
		{"probe partly outside", store.Change{Action: store.ActionSaveProbeTask, Operation: store.Operation{ResourceID: 2}}, store.ErrPermission},
		{"probe on all nodes", store.Change{Action: store.ActionDeleteProbeTask, Operation: store.Operation{ResourceID: 3}}, store.ErrPermission},
		{"probe by tag", store.Change{Action: store.ActionDeleteProbeTask, Operation: store.Operation{ResourceID: 4}}, store.ErrPermission},
		// 空显式集合不归任何主体，不能据此认领。
		{"probe with empty explicit set", store.Change{Action: store.ActionDeleteProbeTask, Operation: store.Operation{ResourceID: 5}}, store.ErrPermission},
		// 删除之后的写后授权读不到行：删除前的作用域裁决已经完成。
		{"deleted probe", store.Change{Action: store.ActionDeleteProbeTask, Operation: store.Operation{ResourceID: 99}}, nil},
		{"alert in grant", store.Change{Action: store.ActionSaveAlertRule, Operation: store.Operation{ResourceID: 1}}, nil},
		{"alert referencing granted probe", store.Change{Action: store.ActionSaveAlertRule, Operation: store.Operation{ResourceID: 2}}, nil},
		{"alert referencing foreign probe", store.Change{Action: store.ActionDeleteAlertRule, Operation: store.Operation{ResourceID: 3}}, store.ErrPermission},
		{"requested reference outside", store.Change{Action: store.ActionSaveAlertRule, ReferenceTask: 2}, store.ErrPermission},
		{"requested selector outside", store.Change{Action: store.ActionSaveProbeTask, Selector: &store.NodeSelector{NodeIDs: []int64{11}}}, store.ErrPermission},
		{"unregistered action", store.Change{}, store.ErrPermission},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tok := &scoped
			if tc.name == "session" {
				tok = nil
			}
			for name, decide := range map[string]func(context.Context, store.ChangeReader, *store.Change, *store.APIToken) error{"scope": Policy{}.Scope, "resource": Policy{}.Resource} {
				if err := decide(context.Background(), r, &tc.change, tok); !errors.Is(err, tc.want) {
					t.Errorf("%s: %v, want %v", name, err, tc.want)
				}
			}
		})
	}
	all := store.APIToken{ID: 2, TokenGrant: store.TokenGrant{AllNodes: true}}
	for _, c := range []store.Change{{Action: store.ActionDeleteTag}, {Action: store.ActionDeleteProbeTask, Operation: store.Operation{ResourceID: 3}}, {Action: store.ActionDeleteAlertRule, Operation: store.Operation{ResourceID: 3}}} {
		if err := (Policy{}).Scope(context.Background(), r, &c, &all); err != nil {
			t.Errorf("all-nodes grant denied %s: %v", c.Action, err)
		}
	}
}

// readerError 的读取失败必须原样上抛，不能被当成"不存在即放行"或"拒绝"。
type readerError struct{ fakeReader }

var errRead = errors.New("read failed")

func (readerError) RuleScope(store.ChangeKind, int64) (store.RuleScope, error) {
	return store.RuleScope{}, errRead
}

func TestReadFailuresPropagate(t *testing.T) {
	tok := token(1, "scoped", []int64{10}, store.PermissionConfigure)
	c := &store.Change{Action: store.ActionSaveAlertRule, Operation: store.Operation{ResourceID: 1}}
	if err := (Policy{}).Scope(context.Background(), readerError{}, c, &tok); !errors.Is(err, errRead) {
		t.Fatalf("read failure became %v", err)
	}
}
