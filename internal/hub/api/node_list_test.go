package api

import (
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
)

func (h *harness) listNodes(t *testing.T, known string) *heronv1.ListNodesResponse {
	t.Helper()
	resp, err := h.admin.ListNodes(t.Context(), connect.NewRequest(&heronv1.ListNodesRequest{KnownRevision: known}))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg
}

func TestListNodesConditionalRead(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, _ := h.createNode(t, "a")
	first := h.listNodes(t, "")
	if first.Unchanged || len(first.Nodes) != 1 || first.Revision == "" {
		t.Fatalf("无条件读取 = %v", first)
	}
	again := h.listNodes(t, first.Revision)
	if !again.Unchanged || len(again.Nodes) != 0 || again.Revision != first.Revision {
		t.Fatalf("未变时应只回版本：%v", again)
	}
	if other := h.listNodes(t, "not-a-revision"); other.Unchanged || len(other.Nodes) != 1 {
		t.Fatalf("对不上的 known_revision 应整份返回：%v", other)
	}
	h.update(t, &heronv1.UpdateNodeRequest{Id: id, Name: "b", TrafficResetDay: 1, OfflineGraceS: proto.Uint32(0)})
	changed := h.listNodes(t, first.Revision)
	if changed.Unchanged || changed.Revision == first.Revision || len(changed.Nodes) != 1 || changed.Nodes[0].Name != "b" {
		t.Fatalf("改名后应整份返回新版本：%v", changed)
	}
}

// days_left 按 hub 时区的今天算：跨过零点，资料没写过也必须换版本，否则开着的页面停在昨天的剩余天数。
func TestListNodesRevisionFollowsHubDate(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	h.createNode(t, "a")
	before := h.listNodes(t, "")
	h.clk.Advance(24 * time.Hour)
	// 会话撑不过一整天，跨日后重新登录；登录只写会话，不碰节点资料。
	h.login(t)
	after := h.listNodes(t, before.Revision)
	if after.Unchanged || after.Revision == before.Revision {
		t.Fatalf("跨日后版本应变化：%q → %v", before.Revision, after)
	}
}

// 换了 hub 进程（重启、恢复备份后启动）版本必然不同：库里的计数可能回到某个旧值，纪元保证旧标签页整份重读。
func TestListNodesRevisionChangesWithServiceEpoch(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	h.createNode(t, "a")
	first := h.listNodes(t, "")
	restarted := New(h.svc.cfg, h.deps())
	resp, err := restarted.ListNodes(t.Context(), connect.NewRequest(&heronv1.ListNodesRequest{KnownRevision: first.Revision}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.Unchanged || resp.Msg.Revision == first.Revision || len(resp.Msg.Nodes) != 1 {
		t.Fatalf("新进程对旧版本应整份返回：%v", resp.Msg)
	}
}
