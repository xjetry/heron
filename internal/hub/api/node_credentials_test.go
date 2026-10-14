package api

import (
	"strings"
	"testing"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
)

func (h *harness) claimNode(t *testing.T, key string) string {
	t.Helper()
	r, err := h.agent.Register(t.Context(), connect.NewRequest(&heronv1.RegisterRequest{Key: key}))
	if err != nil {
		t.Fatal(err)
	}
	return r.Msg.Token
}

func TestNodeCredentialPurposes(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	created, err := h.admin.CreateNode(t.Context(), connect.NewRequest(&heronv1.CreateNodeRequest{Name: "precreated"}))
	if err != nil {
		t.Fatal(err)
	}
	id, install := created.Msg.Node.Id, created.Msg.Token
	if !created.Msg.Node.Public {
		t.Error("管理端创建的节点应默认公开")
	}
	if err := h.report(t, install, &heronv1.Metrics{}); codeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("安装凭据不能直接上报：%v", err)
	}
	if err := h.auth.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	runtime := h.claimNode(t, install)
	for _, key := range []string{install, runtime, "heron_install_" + runtime} {
		if _, err := h.agent.Register(t.Context(), connect.NewRequest(&heronv1.RegisterRequest{Key: key})); codeOf(err) != connect.CodeUnauthenticated {
			t.Errorf("已用安装凭据、运行凭据及伪造用途均应拒绝：%v", err)
		}
	}
	if err := h.auth.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := h.report(t, runtime, &heronv1.Metrics{}); err != nil {
		t.Errorf("拒绝认领不应撤销运行凭据，重载后仍应可上报：%v", err)
	}
	rotated, err := h.admin.RotateNodeToken(t.Context(), connect.NewRequest(&heronv1.RotateNodeTokenRequest{Id: id}))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.report(t, runtime, &heronv1.Metrics{}); codeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("管理员换发应撤销原运行凭据：%v", err)
	}
	if err := h.report(t, rotated.Msg.Token, &heronv1.Metrics{}); codeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("换发的安装凭据不能直接上报：%v", err)
	}
	if !strings.HasPrefix(rotated.Msg.Token, "heron_install_") {
		t.Error("换发凭据必须具有安装用途")
	}
	fresh := h.claimNode(t, rotated.Msg.Token)
	if err := h.report(t, fresh, &heronv1.Metrics{}); err != nil {
		t.Fatalf("重新认领后应恢复上报：%v", err)
	}
}
