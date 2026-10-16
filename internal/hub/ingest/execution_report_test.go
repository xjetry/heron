package ingest

import (
	"database/sql"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/agent/client"
	"github.com/xjetry/heron/internal/agentwire"
	"github.com/xjetry/heron/internal/hub/store"
	"google.golang.org/protobuf/proto"
)

func scopeForTest() *heronv1.ExecutionScope {
	return &heronv1.ExecutionScope{
		Kind: heronv1.ScopeKind_SCOPE_KIND_HOST,
		Cpu:  heronv1.ResourceScope_RESOURCE_SCOPE_HOST, Memory: heronv1.ResourceScope_RESOURCE_SCOPE_HOST,
		Swap: heronv1.ResourceScope_RESOURCE_SCOPE_HOST, Load: heronv1.ResourceScope_RESOURCE_SCOPE_HOST,
		CpuEffectiveCores: proto.Float64(4), LoadCores: proto.Uint32(4),
	}
}

func loads(perCore *float64) *heronv1.Metrics {
	m := &heronv1.Metrics{Load1: proto.Float64(2), Load5: proto.Float64(1), Load15: proto.Float64(1)}
	m.Load1PerCore = perCore
	return m
}

// 上报快于间隔的一半会被限速，和准入失败不是一回事。
func pace(h *hub) { h.clk.Advance(10 * time.Second) }

func TestLoad1PerCoreAndExecutionAdmission(t *testing.T) {
	h := newHub(t)
	id, tok := h.node(t)
	reject := func(name string, req *connect.Request[heronv1.ReportRequest], snippet string) {
		t.Helper()
		pace(h)
		_, err := h.client.Report(t.Context(), req)
		if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), snippet) {
			t.Fatalf("%s: error=%v, want InvalidArgument containing %q", name, err, snippet)
		}
		if _, ok := h.live.Get(id); ok {
			t.Fatalf("%s changed live state", name)
		}
	}
	nan := math.NaN()
	reject("非有限", report(tok, loads(&nan)), "load1_per_core")
	reject("负数", report(tok, loads(proto.Float64(-0.1))), "load1_per_core")
	alone := report(tok, &heronv1.Metrics{Load1PerCore: proto.Float64(1)})
	reject("没有 load1", alone, "load1_per_core: requires load1")
	badScope := report(tok, loads(proto.Float64(0)))
	badScope.Msg.Facts = &heronv1.Facts{Execution: &heronv1.ExecutionScope{Kind: heronv1.ScopeKind_SCOPE_KIND_UNSPECIFIED}}
	reject("非法执行环境", badScope, "execution.kind")
	tooMany := report(tok, loads(nil))
	tooMany.Msg.Facts = &heronv1.Facts{CpuCores: agentwire.MaxScopeCores + 1}
	reject("核数越界", tooMany, "facts.cpu_cores")

	pace(h)
	ok := report(tok, loads(proto.Float64(0)))
	ok.Msg.Facts = &heronv1.Facts{CpuCores: agentwire.MaxScopeCores, Execution: scopeForTest()}
	if _, err := h.client.Report(t.Context(), ok); err != nil {
		t.Fatal(err)
	}
	if m, okLive := h.live.Get(id); !okLive || m.Metrics.Load1PerCore == nil || *m.Metrics.Load1PerCore != 0 {
		t.Fatalf("合法的 0 必须留下: %+v", m.Metrics)
	}
}

// facts_rev 为 0 的行是按更少的字段集合确认过的摘要。字段集合变了之后对不上，agent 不重启、Facts 内容也不变，
// 仍要重新索取并按当前字段集合落库。没有 execution 的 Facts 落库后仍是未上报，但摘要已经确认，不再反复要。
func TestFactsRevRequestsAgainAfterThePersistedSetChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	h := newHubAt(t, path)
	id, tok := h.node(t)
	facts := &heronv1.Facts{Hostname: "box", CpuCores: 2, Execution: scopeForTest()}
	hash := client.FactsHash(facts)
	pace(h)
	first := report(tok, loads(proto.Float64(1)))
	first.Msg.Facts, first.Msg.FactsHash = facts, hash
	if resp, err := h.client.Report(t.Context(), first); err != nil || resp.Msg.WantFacts {
		t.Fatalf("first report err=%v wantFacts=%v", err, resp.Msg.GetWantFacts())
	}
	waitFactsPublished(t, h, id, hash)

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("UPDATE node_facts SET facts_rev = 0 WHERE node_id = ?", id); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	pace(h)
	again := report(tok, loads(proto.Float64(1)))
	again.Msg.FactsHash = hash
	resp, err := h.client.Report(t.Context(), again)
	if err != nil || !resp.Msg.WantFacts {
		t.Fatalf("stale confirmation err=%v wantFacts=%v, want a re-request", err, resp.Msg.GetWantFacts())
	}
	pace(h)
	again.Msg.Facts = facts
	if _, err := h.client.Report(t.Context(), again); err != nil {
		t.Fatal(err)
	}
	waitFactsPublished(t, h, id, hash)
	got, err := h.store.GetNode(t.Context(), id)
	if err != nil || !proto.Equal(got.Facts.GetExecution(), facts.Execution) {
		t.Fatalf("restored execution=%v err=%v", got.Facts.GetExecution(), err)
	}
	var rev int
	if err := rawRev(t, path, id, &rev); err != nil || rev != 1 {
		t.Fatalf("facts_rev=%d err=%v, want 1", rev, err)
	}
	pace(h)
	settled := report(tok, loads(proto.Float64(1)))
	settled.Msg.FactsHash = hash
	if resp, err := h.client.Report(t.Context(), settled); err != nil || resp.Msg.WantFacts {
		t.Fatalf("confirmed digest err=%v wantFacts=%v", err, resp.Msg.GetWantFacts())
	}

	old := &heronv1.Facts{Hostname: "old-agent", CpuCores: 4}
	oldHash := client.FactsHash(old)
	pace(h)
	oldReq := report(tok, loads(nil))
	oldReq.Msg.Facts, oldReq.Msg.FactsHash = old, oldHash
	if _, err := h.client.Report(t.Context(), oldReq); err != nil {
		t.Fatal(err)
	}
	waitFactsPublished(t, h, id, oldHash)
	got, err = h.store.GetNode(t.Context(), id)
	if err != nil || got.Facts.GetExecution() != nil || got.Facts.GetHostname() != "old-agent" {
		t.Fatalf("facts without execution=%v err=%v, want no execution", got.Facts, err)
	}
	pace(h)
	follow := report(tok, loads(nil))
	follow.Msg.FactsHash = oldHash
	if resp, err := h.client.Report(t.Context(), follow); err != nil || resp.Msg.WantFacts {
		t.Fatalf("confirmed absence err=%v wantFacts=%v", err, resp.Msg.GetWantFacts())
	}
}

func rawRev(t *testing.T, path string, id int64, rev *int) error {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer raw.Close()
	return raw.QueryRow("SELECT facts_rev FROM node_facts WHERE node_id = ?", id).Scan(rev)
}

// 旧快照没有 facts_rev。恢复后缺省是 0，启动加载不把那份摘要当成已确认，agent 仍被索取。
func TestRestoredOldFactsAreRequestedAgain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	h := newHubAt(t, path)
	id, tok := h.node(t)
	facts := &heronv1.Facts{Hostname: "kept", CpuCores: 2, Execution: scopeForTest()}
	hash := client.FactsHash(facts)
	pace(h)
	req := report(tok, loads(proto.Float64(1)))
	req.Msg.Facts, req.Msg.FactsHash = facts, hash
	if _, err := h.client.Report(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	waitFactsPublished(t, h, id, hash)
	config := filepath.Join(t.TempDir(), "config.db")
	if err := h.store.SnapshotConfig(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", config)
	if err != nil {
		t.Fatal(err)
	}
	// 迁移 47 的 api_token 失效时刻列同理，否则回填 33 后迁移 47 撞上已有的列。
	if _, err := raw.Exec("ALTER TABLE api_token DROP COLUMN expires_at"); err != nil {
		t.Fatal(err)
	}
	// 迁移 44 的 rented 列同理，否则回填 33 后迁移 44 撞上已有的列。
	if _, err := raw.Exec("ALTER TABLE node DROP COLUMN rented"); err != nil {
		t.Fatal(err)
	}
	// 迁移 41 的流量基线表同理：合计基线写回 last_rx、last_tx，删掉 traffic_baseline，否则回填 33 后迁移 41 撞上已有的表。
	if _, err := raw.Exec(`ALTER TABLE traffic ADD COLUMN last_rx INTEGER NOT NULL DEFAULT -1; ALTER TABLE traffic ADD COLUMN last_tx INTEGER NOT NULL DEFAULT -1;
		UPDATE traffic SET last_rx = b.rx, last_tx = b.tx FROM traffic_baseline b WHERE b.node_id = traffic.node_id AND b.iface = '';
		DROP TABLE traffic_baseline`); err != nil {
		t.Fatal(err)
	}
	// 迁移 40 的手填出口地址两列同理，否则回填 33 后迁移 40 撞上已有的列。
	if _, err := raw.Exec("ALTER TABLE node DROP COLUMN ipv4_pin; ALTER TABLE node DROP COLUMN ipv6_pin"); err != nil {
		t.Fatal(err)
	}
	// 迁移 39 的 rtt 相对判定列、fired_at 与 alert_baseline 也要撤回，否则回填 33 后迁移 39 撞上已有的列。
	if _, err := raw.Exec("DROP TABLE alert_baseline; ALTER TABLE alert_state DROP COLUMN fired_at; ALTER TABLE alert_rule DROP COLUMN rtt_mode; ALTER TABLE alert_rule DROP COLUMN baseline_mode; ALTER TABLE alert_rule DROP COLUMN baseline_window_s; ALTER TABLE alert_rule DROP COLUMN baseline_min_samples; ALTER TABLE alert_rule DROP COLUMN upper_deviation_pct; ALTER TABLE alert_rule DROP COLUMN lower_deviation_pct; ALTER TABLE alert_rule DROP COLUMN cooldown_s; ALTER TABLE alert_rule DROP COLUMN fixed_baseline_ms"); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("DROP TABLE cleanup_job; ALTER TABLE node DROP COLUMN traffic_quota_bytes; ALTER TABLE node DROP COLUMN traffic_quota_mode; DROP TABLE probe_cert_presented; ALTER TABLE probe_cert DROP COLUMN config_id; ALTER TABLE probe_task DROP COLUMN cert_spki_sha256; ALTER TABLE probe_task DROP COLUMN config_id; ALTER TABLE node_facts DROP COLUMN execution; ALTER TABLE node_facts DROP COLUMN facts_rev; UPDATE snapshot_meta SET schema_version = 33"); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(t.TempDir(), "restored.db")
	if _, err := store.Restore(t.Context(), restored, config, "", "", h.clk.Now(), h.svc.log); err != nil {
		t.Fatal(err)
	}
	next := newHubAt(t, restored)
	pace(next)
	onlyHash := report(tok, loads(nil))
	onlyHash.Msg.FactsHash = hash
	resp, err := next.client.Report(t.Context(), onlyHash)
	if err != nil || !resp.Msg.WantFacts {
		t.Fatalf("restored old snapshot err=%v wantFacts=%v, want a re-request", err, resp.Msg.GetWantFacts())
	}
}
