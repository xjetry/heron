package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
)

type Permission string

const (
	PermissionConfigure Permission = "configure"
	PermissionCreate    Permission = "create"
	PermissionRegister  Permission = "register"
	PermissionRotate    Permission = "rotate"
	PermissionDelete    Permission = "delete"
	PermissionUpdate    Permission = "update"
)

// ErrPermission 是变更写契约的一部分：策略（ChangePolicy）拒绝时返回它，写边界据此回滚事务，api 据此回答
// PermissionDenied。store 只定义它，不在任何分支里作出拒绝。
var ErrPermission = errors.New("operation or resource is outside the API token grant")

// ErrInvalidGrant 是建或改 token 时 grant 本身不合法（未知权限位、全部节点与显式节点并存、引用不存在的节点）。
// 它是输入校验，不是授权拒绝，与授权哨兵分开。具体原因由 *GrantError 携带。
var ErrInvalidGrant = errors.New("invalid API token grant")

// GrantError 是 grant 不合法的具体原因：Field 是 TokenGrant 的字段（permissions、node_ids），api 据此写出请求里的
// 字段路径。errors.Is(err, ErrInvalidGrant) 对它成立。
type GrantError struct{ Field, Reason string }

func (e *GrantError) Error() string {
	return ErrInvalidGrant.Error() + ": " + e.Field + ": " + e.Reason
}
func (e *GrantError) Is(target error) bool { return target == ErrInvalidGrant }

type TokenGrant struct {
	Permissions []Permission
	AllNodes    bool
	NodeIDs     []int64
}

func (g TokenGrant) Allows(p Permission) bool { return slices.Contains(g.Permissions, p) }
func (g TokenGrant) AllowsNode(id int64) bool {
	return id > 0 && (g.AllNodes || slices.Contains(g.NodeIDs, id))
}
func (g TokenGrant) AllowsSelector(all bool, tags []string, ids []int64) bool {
	if g.AllNodes {
		return true
	}
	// 空显式集合没有归属，不能据此认领其他主体留下的共享对象。
	if all || len(tags) != 0 || len(ids) == 0 {
		return false
	}
	for _, id := range ids {
		if !g.AllowsNode(id) {
			return false
		}
	}
	return true
}

// RuleInScope 报告 grant 是否覆盖探测任务或告警规则 id 当前作用域里的每一个节点：全部节点与标签选择器只有
// 全部节点的 grant 覆盖，显式节点必须逐个在 grant 内，空显式集合不归任何主体（同 AllowsSelector）；告警规则
// 还要求它引用的探测任务同样被覆盖。资源不存在时没有未被覆盖的节点，结果为真——写侧在删除之前已按资源存在时
// 的作用域裁决过，删除之后的写后授权读到无行即放行；读侧只对事务里读到的现存行调用它。告警规则的 task_id 没有
// 外键，引用的探测任务不存在时同样为真。
// 变更策略与读侧可见性过滤（告警规则列表、引用错误里的隐藏规则）共用这一个判定，二者口径不会分叉。
func RuleInScope(r RuleScopeReader, g TokenGrant, kind ChangeKind, id int64) (bool, error) {
	if g.AllNodes {
		return true, nil
	}
	s, err := r.RuleScope(kind, id)
	if errors.Is(err, ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if s.AllNodes || s.TagCount != 0 || !g.AllowsSelector(false, nil, s.NodeIDs) {
		return false, nil
	}
	if s.TaskID != 0 {
		return RuleInScope(r, g, ChangeProbe, s.TaskID)
	}
	return true, nil
}

func (g TokenGrant) validate(tx *sql.Tx) error {
	for _, p := range g.Permissions {
		switch p {
		case PermissionConfigure, PermissionCreate, PermissionRegister, PermissionRotate, PermissionDelete, PermissionUpdate:
		default:
			return &GrantError{"permissions", fmt.Sprintf("unknown permission %q", p)}
		}
	}
	if g.AllNodes && len(g.NodeIDs) != 0 {
		return &GrantError{"node_ids", "must be empty when all_nodes is true"}
	}
	for _, id := range g.NodeIDs {
		ok, err := nodeExistsTx(tx, id)
		if err != nil {
			return err
		}
		if !ok {
			return &GrantError{"node_ids", fmt.Sprintf("node %d does not exist", id)}
		}
	}
	return nil
}

type principalKey struct{}

func WithPrincipal(ctx context.Context, token APIToken) context.Context {
	return context.WithValue(ctx, principalKey{}, token)
}
func Principal(ctx context.Context) (APIToken, bool) {
	t, ok := ctx.Value(principalKey{}).(APIToken)
	return t, ok
}
func OwnerID(ctx context.Context) int64 { t, _ := Principal(ctx); return t.ID }

// 范围从数据库关系表读取，建节点后同一请求的回读也能看见刚获得的授权；空列表不放宽。这里不判过期：
//   - 请求在入口（APITokenByHash）按当时的墙钟准入；服务端不给处理时长设上界（cmd/hub/serve.go 只限读请求的时长），
//     准入后才到期的只读请求读到结束。
//   - 这里只看 api_token 行与授权行在不在：吊销删行，此后的范围过滤立即为空；过期不删行，范围照旧。
//   - 写入不靠这里：ExecuteChange 在写事务里经 lookupAPIToken 重判（txReader.APIToken）。
func nodeScopeSQL(ctx context.Context, column string) string {
	t, ok := Principal(ctx)
	if !ok {
		return "1"
	}
	return fmt.Sprintf("EXISTS (SELECT 1 FROM api_token a WHERE a.id=%d AND (a.all_nodes=1 OR EXISTS (SELECT 1 FROM api_token_node g WHERE g.token_id=a.id AND g.node_id=%s)))", t.ID, column)
}

func grantNode(tx *sql.Tx, owner, node int64) error {
	if owner == 0 {
		return nil
	}
	_, err := tx.Exec("INSERT OR IGNORE INTO api_token_node (token_id, node_id) SELECT ?, ? WHERE EXISTS (SELECT 1 FROM api_token WHERE id=? AND all_nodes=0)", owner, node, owner)
	return err
}
