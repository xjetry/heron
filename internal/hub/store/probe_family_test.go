package store

import (
	"bytes"
	"database/sql"
	"path/filepath"
	"slices"
	"testing"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"google.golang.org/protobuf/proto"
)

// schemaV51 冻结 v51 的完整 DDL：v50 加上 probe_task 的探测地址族与 DNS 记录类型两列。
var schemaV51 = append(slices.Clone(schemaV50),
	`ALTER TABLE probe_task ADD COLUMN ip_family INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE probe_task ADD COLUMN dns_record_type INTEGER NOT NULL DEFAULT 0`,
)

// removeV51Config 把当前配置库或快照退回 v50 的 probe_task 结构，供从旧版本起步的迁移与恢复测试使用。
func removeV51Config(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec("ALTER TABLE probe_task DROP COLUMN ip_family; ALTER TABLE probe_task DROP COLUMN dns_record_type"); err != nil {
		t.Fatal(err)
	}
}

// 地址族与记录类型是任务内容：随任务落库、原样读回，读法在全部读取入口一致；变了就换配置身份，没变就保留。
func TestProbeTaskFamilyAndRecordTypeAreTaskContent(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	icmp := &heronv1.ProbeTask{Kind: heronv1.ProbeKind_PROBE_KIND_ICMP, Target: "example.com", IntervalS: 60, TimeoutMs: 1000, IpFamily: heronv1.ProbeIpFamily_PROBE_IP_FAMILY_IPV6}
	dns := &heronv1.ProbeTask{Kind: heronv1.ProbeKind_PROBE_KIND_DNS, Target: "example.com", IntervalS: 60, TimeoutMs: 1000, DnsServer: "1.1.1.1:53", DnsRecordType: heronv1.DnsRecordType_DNS_RECORD_TYPE_AAAA}
	savedICMP, _, err := s.SaveProbeTask(ctx, icmp, NodeSelector{AllNodes: true})
	if err != nil {
		t.Fatal(err)
	}
	savedDNS, _, err := s.SaveProbeTask(ctx, dns, NodeSelector{AllNodes: true})
	if err != nil {
		t.Fatal(err)
	}
	if savedICMP.Task.GetIpFamily() != heronv1.ProbeIpFamily_PROBE_IP_FAMILY_IPV6 || savedDNS.Task.GetDnsRecordType() != heronv1.DnsRecordType_DNS_RECORD_TYPE_AAAA {
		t.Fatalf("save returned %v / %v", savedICMP.Task, savedDNS.Task)
	}
	_, records, err := s.LoadProbeTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || !proto.Equal(records[0].Task, savedICMP.Task) || !proto.Equal(records[1].Task, savedDNS.Task) {
		t.Fatalf("loaded %v, want %v and %v", records, savedICMP.Task, savedDNS.Task)
	}
	rec, ok, _, err := s.ListComparisonNodes(ctx, savedICMP.Task.Id, ComparisonPublic)
	if err != nil || !ok || rec.Task.GetIpFamily() != heronv1.ProbeIpFamily_PROBE_IP_FAMILY_IPV6 {
		t.Fatalf("comparison read = %v ok=%v err=%v", rec.Task, ok, err)
	}

	// 内容不变（只换分配）：身份保留。
	kept, _, err := s.SaveProbeTask(ctx, savedICMP.Task, NodeSelector{})
	if err != nil || !bytes.Equal(kept.Task.ConfigId, savedICMP.Task.ConfigId) {
		t.Fatalf("selector-only save changed identity: %x -> %x err=%v", savedICMP.Task.ConfigId, kept.Task.ConfigId, err)
	}
	// 只改地址族：新身份。
	v4 := proto.Clone(kept.Task).(*heronv1.ProbeTask)
	v4.IpFamily = heronv1.ProbeIpFamily_PROBE_IP_FAMILY_IPV4
	changed, _, err := s.SaveProbeTask(ctx, v4, NodeSelector{})
	if err != nil || bytes.Equal(changed.Task.ConfigId, kept.Task.ConfigId) || changed.Task.GetIpFamily() != heronv1.ProbeIpFamily_PROBE_IP_FAMILY_IPV4 {
		t.Fatalf("family change = %v (identity %x -> %x) err=%v", changed.Task, kept.Task.ConfigId, changed.Task.ConfigId, err)
	}
	// 只改记录类型：新身份。
	a := proto.Clone(savedDNS.Task).(*heronv1.ProbeTask)
	a.DnsRecordType = heronv1.DnsRecordType_DNS_RECORD_TYPE_UNSPECIFIED
	changedDNS, _, err := s.SaveProbeTask(ctx, a, NodeSelector{AllNodes: true})
	if err != nil || bytes.Equal(changedDNS.Task.ConfigId, savedDNS.Task.ConfigId) {
		t.Fatalf("record type change kept identity: %x err=%v", changedDNS.Task.ConfigId, err)
	}
}

// 只做指纹动作的保存沿用库里的任务字段，地址族也在其中：不能因请求里留了零值就退回自动。
func TestPinOnlySaveKeepsStoredFamily(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	task := httpsProbe()
	task.IpFamily = heronv1.ProbeIpFamily_PROBE_IP_FAMILY_IPV6
	saved, _, err := s.SaveProbeTask(ctx, task, NodeSelector{AllNodes: true})
	if err != nil {
		t.Fatal(err)
	}
	pinned, _, err := s.SaveProbeTask(ctx, &heronv1.ProbeTask{Id: saved.Task.Id}, NodeSelector{}, WithCertPin(bytes.Repeat([]byte{7}, 32)), WithExpectedConfigID(saved.Task.ConfigId))
	if err != nil {
		t.Fatal(err)
	}
	if pinned.Task.GetIpFamily() != heronv1.ProbeIpFamily_PROBE_IP_FAMILY_IPV6 || pinned.Task.GetTarget() != task.GetTarget() || !pinned.AllNodes {
		t.Fatalf("pin-only save = %v all_nodes=%v, want stored family, target and selector", pinned.Task, pinned.AllNodes)
	}
}

// 两列在配置快照里随任务表搬运，恢复后原样读回；旧快照经迁移补列，旧任务取缺省（自动、A）。
func TestProbeTaskFamilySurvivesConfigSnapshot(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	ctx := t.Context()
	task := &heronv1.ProbeTask{Kind: heronv1.ProbeKind_PROBE_KIND_TCP, Target: "example.com:443", IntervalS: 60, TimeoutMs: 1000, IpFamily: heronv1.ProbeIpFamily_PROBE_IP_FAMILY_IPV6}
	saved, _, err := s.SaveProbeTask(ctx, task, NodeSelector{AllNodes: true})
	if err != nil {
		t.Fatal(err)
	}
	dns := &heronv1.ProbeTask{Kind: heronv1.ProbeKind_PROBE_KIND_DNS, Target: "example.com", IntervalS: 60, TimeoutMs: 1000, DnsServer: "1.1.1.1:53", DnsRecordType: heronv1.DnsRecordType_DNS_RECORD_TYPE_AAAA}
	savedDNS, _, err := s.SaveProbeTask(ctx, dns, NodeSelector{AllNodes: true})
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "config.db")
	if err := s.SnapshotConfig(ctx, config); err != nil {
		t.Fatal(err)
	}
	restore := func(t *testing.T, config string) []ProbeTaskRecord {
		t.Helper()
		target := filepath.Join(t.TempDir(), "restored.db")
		if _, err := Restore(ctx, target, config, "", "", clk.Now(), s.log); err != nil {
			t.Fatal(err)
		}
		restored, err := Open(target, clk, s.log, RequireCurrentSchema)
		if err != nil {
			t.Fatal(err)
		}
		defer restored.Close()
		_, tasks, err := restored.LoadProbeTasks(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return tasks
	}
	tasks := restore(t, config)
	if len(tasks) != 2 || !proto.Equal(tasks[0].Task, saved.Task) || !proto.Equal(tasks[1].Task, savedDNS.Task) {
		t.Fatalf("restored %v, want %v and %v", tasks, saved.Task, savedDNS.Task)
	}

	// 同一份快照退回 v50 的结构：恢复时迁移补列，两项都取缺省。
	old := filepath.Join(t.TempDir(), "config-v50.db")
	if err := s.SnapshotConfig(ctx, old); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", old)
	if err != nil {
		t.Fatal(err)
	}
	removeV59Config(t, db)
	removeV58Config(t, db)
	removeV57Config(t, db)
	removeV56Config(t, db)
	removeV52Config(t, db)
	removeV51Config(t, db)
	if _, err := db.Exec("UPDATE snapshot_meta SET schema_version = 50"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	tasks = restore(t, old)
	if len(tasks) != 2 || tasks[0].Task.GetIpFamily() != heronv1.ProbeIpFamily_PROBE_IP_FAMILY_UNSPECIFIED || tasks[1].Task.GetDnsRecordType() != heronv1.DnsRecordType_DNS_RECORD_TYPE_UNSPECIFIED || tasks[0].Task.GetTarget() != "example.com:443" {
		t.Fatalf("restored v50 snapshot %v, want the default family and record type", tasks)
	}
}

// 迁移 51 只补两列，旧任务一律取缺省：升级前的任务按自动地址族、A 记录执行，迁出别的值会悄悄改变它们测的东西。
func TestMigrationFromV50AddsProbeFamilyColumns(t *testing.T) {
	t.Parallel()
	migrated := migrateFrom(t, 50, func(t *testing.T, db *sql.DB) {
		if _, err := db.Exec("INSERT INTO probe_task (kind, target, interval_s, timeout_ms, created_at, dns_server, config_id) VALUES (4, 'legacy.example', 60, 1000, 0, '1.1.1.1:53', x'01010101010101010101010101010101')"); err != nil {
			t.Fatal(err)
		}
	})
	var family, record int
	if err := migrated.r.QueryRow("SELECT ip_family, dns_record_type FROM probe_task WHERE target = 'legacy.example'").Scan(&family, &record); err != nil {
		t.Fatal(err)
	}
	if family != 0 || record != 0 {
		t.Fatalf("legacy task ip_family=%d dns_record_type=%d, want 0 and 0", family, record)
	}
}
