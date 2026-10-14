package ingest

import (
	"encoding/json"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"testing"

	"github.com/xjetry/heron/internal/releasesig/sigtest"
	"github.com/xjetry/heron/internal/update"
)

// TestRelayAcceptServe 只在隔离验收机上运行：用测试公钥与假官方源起一套带 GetRelease 的 hub，
// 监听 HERON_RELAY_LISTEN（例如 127.0.0.1:8080，前面由 Caddy 终止 TLS），把节点 token 与任务写进
// HERON_RELAY_OUT（JSON：{"token","task_id","version"}），然后阻塞到进程收到 SIGTERM 或 SIGINT。
// HERON_RELAY_TAMPER=1：假官方源给出与清单不符的归档，hub 预验签应失败。
// HERON_RELAY_SKIP_VERIFY=1：hub 不做预验签、原样转发（模拟失守的 hub），节点更新器应拒绝。
func TestRelayAcceptServe(t *testing.T) {
	if os.Getenv("HERON_RELAY_ACCEPT") != "hub" {
		t.Skip("only run on the disposable relay acceptance machines")
	}
	official := func(arch string) update.Artifacts {
		return signedAgent(arch, sigtest.Archive("agent", []byte(relayBinary)))
	}
	if os.Getenv("HERON_RELAY_TAMPER") == "1" {
		official = tampered
	}
	verify := acceptVerify
	if os.Getenv("HERON_RELAY_SKIP_VERIFY") == "1" {
		verify = func(string, string, update.Artifacts) error { return nil }
	}
	_, tok, task := relayHub(t, official, verify, os.Getenv("HERON_RELAY_LISTEN"))
	out, err := json.Marshal(map[string]string{"token": tok, "task_id": task.Id, "version": task.Version})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("HERON_RELAY_OUT"), out, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("serving relay on %s for %s/%s", os.Getenv("HERON_RELAY_LISTEN"), runtime.GOOS, runtime.GOARCH)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, os.Interrupt)
	<-stop
}
