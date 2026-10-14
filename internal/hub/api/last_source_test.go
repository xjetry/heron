package api

import (
	"bytes"
	"context"
	"testing"

	"connectrpc.com/connect"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/gen/heron/v1/heronv1connect"
)

// ListNodes 回显来源地址，API token 可读；同一个节点公开之后，公开快照的原文里没有这个地址。
func TestListNodesEchoesLastSourceButPublicSnapshotDoesNot(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	_, tok := createToken(t, h, "reader")
	id, nodeTok := h.createNode(t, "n")
	h.setPublic(t, id, "n", true)
	if err := h.report(t, nodeTok, &heronv1.Metrics{}); err != nil {
		t.Fatal(err)
	}
	h.ingest.Flush(context.Background(), true)

	client := heronv1connect.NewAdminServiceClient(h.srv.Client(), h.srv.URL)
	req := connect.NewRequest(&heronv1.ListNodesRequest{})
	req.Header().Set("Authorization", "Bearer "+tok)
	resp, err := client.ListNodes(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if nodes := resp.Msg.GetNodes(); len(nodes) != 1 || nodes[0].GetLastSource() != "127.0.0.1" {
		t.Fatalf("ListNodes = %v, want last_source 127.0.0.1", nodes)
	}
	snap := pubGet(t, h, "GetSnapshot", jsonQuery("{}"), nil)
	if !bytes.Contains(snap.body, []byte(`"name":"n"`)) || bytes.Contains(snap.body, []byte("127.0.0.1")) || bytes.Contains(snap.body, []byte("lastSource")) {
		t.Fatalf("public snapshot must list the node without its source: %s", snap.body)
	}
}
