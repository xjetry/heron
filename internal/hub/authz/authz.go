// Package authz 是类型化变更（ExecuteChange）的授权策略，实现 store.ChangePolicy。
//
// store 持有写边界与调用次序（主体 → 重放 → 作用域 → 写 → 资源），这里只回答"允许还是拒绝"。全部读取经
// store.ChangeReader 在 store 开的事务里进行，裁决与写看到同一份快照；拒绝一律返回 store.ErrPermission，
// 写边界据此回滚事务、api 据此回答 PermissionDenied。数据模型上的覆盖判定（TokenGrant 的 Allows*、
// store.RuleInScope）留在 store，读侧可见性过滤也调用它们；这里决定每个操作要满足哪些判定。
package authz

import (
	"context"
	"errors"

	"github.com/xjetry/heron/internal/hub/store"
)

// Policy 无状态，零值即可用。
type Policy struct{}

var _ store.ChangePolicy = Policy{}

// Principal 在事务里重读 bearer token：请求入口缓存的主体可能已被吊销，或数字 id 已被另一枚 token 复用
// （Identity 是凭据哈希派生的身份，复用的 id 身份不同）。会话主体没有 token，返回 nil。
func (Policy) Principal(ctx context.Context, r store.ChangeReader, c *store.Change) (*store.APIToken, error) {
	p, bearer := store.Principal(ctx)
	if !bearer {
		return nil, nil
	}
	t, err := r.APIToken(p.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, store.ErrPermission
	}
	if err != nil {
		return nil, err
	}
	if t.Identity != p.Identity || !t.Allows(c.Action.Permission()) {
		return nil, store.ErrPermission
	}
	return &t, nil
}

// Scope 按当前库状态裁决作用域；写前与 ChangeVersion 都调用它。
func (Policy) Scope(_ context.Context, r store.ChangeReader, c *store.Change, t *store.APIToken) error {
	return scope(r, c, t)
}

// Resource 在写之后重做作用域裁决：此时 ResourceID 已知，新建的资源已有身份，保存后的选择器已落库，
// 写前看不到的越权（新建一个覆盖范围外节点的规则）在这里被拒绝并随事务回滚。
func (Policy) Resource(_ context.Context, r store.ChangeReader, c *store.Change, t *store.APIToken) error {
	return scope(r, c, t)
}

func scope(r store.ChangeReader, c *store.Change, t *store.APIToken) error {
	if t == nil {
		return nil
	}
	if c.Selector != nil && !t.AllowsSelector(c.Selector.AllNodes, c.Selector.Tags, c.Selector.NodeIDs) {
		return store.ErrPermission
	}
	if c.ReferenceTask != 0 {
		if err := rule(r, t.TokenGrant, store.ChangeProbe, c.ReferenceTask); err != nil {
			return err
		}
	}
	switch kind := c.Action.Kind(); kind {
	case store.ChangeNode:
		// 建节点时还没有节点身份可比；新节点由写事务授予创建者（store.grantNode）。
		if c.Action.Permission() == store.PermissionCreate {
			return nil
		}
		if !t.AllowsNode(c.ResourceID) {
			return store.ErrPermission
		}
	case store.ChangeUpdate:
		if !t.AllowsNode(c.ResourceID) {
			return store.ErrPermission
		}
	case store.ChangeWindow:
		if c.ResourceID != t.ID {
			return store.ErrPermission
		}
	case store.ChangeTag:
		if !t.AllNodes {
			return store.ErrPermission
		}
	case store.ChangeProbe, store.ChangeAlert:
		if c.ResourceID == 0 {
			return nil
		}
		return rule(r, t.TokenGrant, kind, c.ResourceID)
	default:
		return store.ErrPermission
	}
	return nil
}

func rule(r store.ChangeReader, g store.TokenGrant, kind store.ChangeKind, id int64) error {
	ok, err := store.RuleInScope(r, g, kind, id)
	if err != nil {
		return err
	}
	if !ok {
		return store.ErrPermission
	}
	return nil
}
