package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/gen/heron/v1/heronv1connect"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/testdeps"
	"github.com/xjetry/heron/internal/testwait"
)

func restoreDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestRestoreStartsHubWithRecoveredConfig(t *testing.T) {
	t.Parallel()
	config, metrics := restoreSnapshots(t)
	restoreExec(t, restoreDB(t, config), "INSERT INTO setting VALUES ('site.public_enabled','1')")
	path := filepath.Join(t.TempDir(), "restored.db")
	if out, err := hubCommand(t, "restore", "--db", path, "--config", config, "--metrics", metrics, "--yes").CombinedOutput(); err != nil {
		t.Fatalf("restore: %v %s", err, out)
	}
	cmd := hubCommand(t, "serve", "--db", path, "--listen", "127.0.0.1:0")
	output := &commandOutput{}
	cmd.Stdout, cmd.Stderr = output, output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	finished := make(chan struct{})
	var result error
	go func() { result = cmd.Wait(); close(finished) }()
	defer func() { _ = cmd.Process.Kill(); <-finished }()
	var addr string
	testwait.Until(t, 10*time.Millisecond, func() bool {
		for _, field := range strings.Fields(output.String()) {
			if strings.HasPrefix(field, "listen=") {
				addr = strings.Trim(strings.TrimPrefix(field, "listen="), `"`)
			}
		}
		select {
		case <-finished:
			t.Fatalf("restored hub exited: %v %s", result, output.String())
		default:
		}
		return addr != ""
	}, "restored hub never listened: %s", output)
	client := heronv1connect.NewPublicServiceClient(&http.Client{Transport: testdeps.OwnedTransport(t), Timeout: testwait.Bound}, "http://"+addr)
	site, err := client.GetSite(context.Background(), connect.NewRequest(&heronv1.GetSiteRequest{}))
	if err != nil || site.Msg.Title != "snapshot" {
		t.Fatalf("restored configuration not served: %v %v", site, err)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
		if result != nil {
			t.Fatal(result)
		}
	case <-time.After(testwait.Bound):
		t.Fatal("restored hub did not stop")
	}
}

func restoreExec(t *testing.T, db *sql.DB, query string) {
	t.Helper()
	if _, err := db.Exec(query); err != nil {
		t.Fatal(err)
	}
}

func restoreWant(t *testing.T, db *sql.DB, query, want string) {
	t.Helper()
	var got sql.NullString
	if err := db.QueryRow(query).Scan(&got); err != nil {
		t.Errorf("%s: %v", query, err)
		return
	}
	if !got.Valid || got.String != want {
		t.Errorf("%s = %q (valid=%t), want %q", query, got.String, got.Valid, want)
	}
}

func restoreDump(t *testing.T, path string) string {
	t.Helper()
	out, err := exec.Command("sqlite3", path, ".dump").CombinedOutput()
	if err != nil {
		t.Fatalf("dump: %v %s", err, out)
	}
	return string(out)
}

func restoreSnapshots(t *testing.T) (config, metrics string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "source.db")
	clk := clock.NewFake(time.Unix(1000, 0))
	s, err := store.Open(path, clk, slog.Default(), store.MigrateSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	db := restoreDB(t, path)
	restoreExec(t, db, `INSERT INTO node (id,name,token_hash,created_at) VALUES (1,'one',x'01',0),(2,'two',x'02',0);
		INSERT INTO api_token (id,name,token_hash,created_at,all_nodes) VALUES (1,'scoped',x'01',0,0);
		INSERT INTO tag (id,name,name_fold) VALUES (1,'shared','shared');
		INSERT INTO alert_rule (id,name,kind,created_at) VALUES (1,'rule','offline',0);
		INSERT INTO alert_event (rule_id,node_id,transition,at,summary,value) VALUES (0,0,'firing',0,'system',0),(1,99,'firing',0,'deleted',0);
		INSERT INTO setting VALUES ('site.title','snapshot');
		INSERT INTO sqlite_sequence VALUES ('retired_table',17);
		INSERT INTO probe_task (id,kind,target,interval_s,timeout_ms,created_at,config_id) VALUES (7,1,'localhost',60,1000,0,x'01010101010101010101010101010101');
		DELETE FROM probe_task;
		INSERT INTO silence (id,name,kind,created_at) VALUES (1,'quiet','once',0);`)
	for _, id := range []int{2, 3} {
		restoreExec(t, db, fmt.Sprintf(`INSERT INTO node_facts (node_id,facts_hash,hostname,os,kernel,arch,virtualization,cpu_model,cpu_cores,agent_version,icmp_available,updated_at) VALUES (%[1]d,0,'','','','','','',0,'',0,0);
			INSERT INTO api_token_node VALUES (1,%[1]d);
			INSERT INTO node_tag (node_id,tag_id) VALUES (%[1]d,1);
			INSERT INTO traffic (node_id,boot_id,total_rx,total_tx,period_rx,period_tx,period_start,updated_at) VALUES (%[1]d,'',0,0,0,0,0,0);
			INSERT INTO traffic_baseline (node_id,iface,rx,tx) VALUES (%[1]d,'',0,0);
			INSERT INTO probe_task_node VALUES (1,%[1]d);
			INSERT INTO probe_cert (node_id,task_id,not_after,observed_at,config_id) VALUES (%[1]d,7,0,0,x'01010101010101010101010101010101');
			INSERT INTO probe_cert_presented (node_id,task_id,config_id,spki_sha256,not_after,reason,observed_at) VALUES (%[1]d,7,x'01010101010101010101010101010101',x'0202020202020202020202020202020202020202020202020202020202020202',1,1,0);
			INSERT INTO alert_rule_node VALUES (1,%[1]d);
			INSERT INTO silence_node VALUES (1,%[1]d);
			INSERT INTO alert_state (rule_id,node_id,state,since_at) VALUES (1,%[1]d,'firing',0);
			INSERT INTO alert_baseline (rule_id,node_id,baseline_us,buckets,computed_at,task_fingerprint) VALUES (1,%[1]d,0,0,0,'');`, id))
	}
	config, metrics = filepath.Join(dir, "config.db"), filepath.Join(dir, "metrics.db")
	if err := s.SnapshotConfig(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	restoreExec(t, db, `DELETE FROM node WHERE id=2;
		INSERT INTO node (name,token_hash,created_at) VALUES ('three',x'03',0);
		DELETE FROM sqlite_sequence WHERE name='retired_table';
		INSERT INTO sqlite_sequence VALUES ('metrics_only',23);
		UPDATE rollup_state SET upto_ts=600;
		INSERT INTO maintenance_state VALUES ('prune',2000);`)
	restoreExec(t, db, "INSERT INTO node_coverage(node_id,start_ts) VALUES(2,600),(3,600)")
	for _, table := range []string{"metric_1m", "metric_5m", "metric_1h"} {
		restoreExec(t, db, "INSERT INTO "+table+" (node_id,ts) VALUES (2,600),(3,600)")
	}
	for _, table := range []string{"probe_1m", "probe_5m", "probe_1h"} {
		restoreExec(t, db, "INSERT INTO "+table+" (node_id,ts,task_id) VALUES (2,600,7),(3,600,7)")
	}
	clk.Advance(1000 * time.Second)
	if err := s.SnapshotMetrics(t.Context(), metrics); err != nil {
		t.Fatal(err)
	}
	return config, metrics
}

func restoreTarget(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "target.db")
	s, err := store.Open(path, clock.Real(), slog.Default(), store.MigrateSchema)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	db := restoreDB(t, path)
	restoreExec(t, db, `INSERT INTO node (id,name,token_hash,created_at) VALUES (5,'old',x'05',0);
		DELETE FROM node;
		INSERT INTO node (id,name,token_hash,created_at) VALUES (1,'target',x'01',0);
		INSERT INTO metric_1m (node_id,ts) VALUES (2,300),(5,300);
		INSERT INTO setting VALUES ('site.title','target');
		INSERT INTO sqlite_sequence VALUES ('target_only',19);`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRestoreCommandTimeline(t *testing.T) {
	t.Parallel()
	for _, configAt := range []int64{1000, 3000} {
		t.Run(fmt.Sprint(configAt), func(t *testing.T) { testRestoreTimeline(t, configAt) })
	}
}

func testRestoreTimeline(t *testing.T, configAt int64) {
	t.Helper()
	config, metrics := restoreSnapshots(t)
	restoreExec(t, restoreDB(t, config), fmt.Sprintf("UPDATE snapshot_meta SET taken_at=%d", configAt))
	path := filepath.Join(t.TempDir(), "restored.db")
	start := time.Now().Unix()
	cmd := hubCommand(t, "restore", "--db", path, "--config", config, "--metrics", metrics, "--yes")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("restore command: %v %s", err, out)
	}
	var summary store.RestoreResult
	if err := json.Unmarshal(bytes.TrimPrefix(bytes.TrimSpace(out), []byte("restored: ")), &summary); err != nil {
		t.Fatalf("restore summary: %s: %v", out, err)
	}
	if summary.ConfigTakenAt != configAt || summary.MetricsTakenAt == nil || *summary.MetricsTakenAt != 2000 || summary.RestoredAt < start || summary.RestoredAt > time.Now().Unix() {
		t.Errorf("restore summary timestamps: %+v", summary)
	}
	db := restoreDB(t, path)
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if want := `msg="database schema created" version=` + strconv.Itoa(version); !strings.Contains(stderr.String(), want) {
		t.Errorf("restore stderr = %q, want %q", stderr.String(), want)
	}
	restoreWant(t, db, "SELECT seq FROM sqlite_sequence WHERE name='metrics_only'", "23")
	for query, want := range map[string]string{
		"SELECT group_concat(id) FROM node":                                     "1,2",
		"SELECT name FROM alert_rule WHERE id=1":                                "rule",
		"SELECT value FROM setting WHERE key='site.title'":                      "snapshot",
		"SELECT seq FROM sqlite_sequence WHERE name='node'":                     "3",
		"SELECT seq FROM sqlite_sequence WHERE name='probe_task'":               "7",
		"SELECT seq FROM sqlite_sequence WHERE name='retired_table'":            "17",
		"SELECT count(*) FROM restore_record":                                   "1",
		"SELECT config_taken_at || ',' || metrics_taken_at FROM restore_record": fmt.Sprintf("%d,2000", configAt),
		"SELECT count(*) FROM rollup_state WHERE upto_ts=600":                   "4",
		"SELECT finished_at FROM maintenance_state WHERE name='prune'":          "2000",
		"SELECT count(*) FROM alert_event":                                      "3",
		"SELECT count(*) FROM alert_event WHERE transition='backup_restored'":   "1",
	} {
		restoreWant(t, db, query, want)
	}
	wantOrphans := make(map[string]int64)
	// Restore 按共享的节点从属清单输出每张表（含零计数）；TestRestoreRecordUnion 校验摘要键与清单一致，
	// TestNodeDependentTablesComplete 校验 schema 中的 node_id 表与清理、保留清单的集合关系。
	for table := range summary.Orphans {
		if table == "node_update" {
			// 更新授权是运行簿记，不进快照，恢复后即使节点存在也不能重放。
			restoreWant(t, db, "SELECT count(*) FROM node_update", "0")
			wantOrphans[table] = 0
			continue
		}
		restoreWant(t, db, "SELECT group_concat(node_id) FROM "+table, "2")
		wantOrphans[table] = 1
	}
	if !reflect.DeepEqual(summary.Orphans, wantOrphans) {
		t.Errorf("summary orphan counts = %v, want %v", summary.Orphans, wantOrphans)
	}
	var record store.RestoreResult
	var orphanJSON string
	if err := db.QueryRow("SELECT restored_at,config_taken_at,metrics_taken_at,orphans FROM restore_record").Scan(&record.RestoredAt, &record.ConfigTakenAt, &record.MetricsTakenAt, &orphanJSON); err != nil {
		t.Error(err)
	} else {
		if err := json.Unmarshal([]byte(orphanJSON), &record.Orphans); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(record, summary) {
			t.Errorf("stored restore record = %+v, want summary %+v", record, summary)
		}
	}
	restoreExec(t, db, "INSERT INTO node (name,token_hash,created_at) VALUES ('next',x'04',0)")
	restoreWant(t, db, "SELECT id FROM node WHERE name='next'", "4")
}

func TestRestoreHistoricalSnapshotVersions(t *testing.T) {
	t.Parallel()
	for _, version := range []int{17, 18, 19} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			t.Parallel()
			config, metrics := restoreSnapshots(t)
			cfg := restoreDB(t, config)
			met := restoreDB(t, metrics)
			removeV44Config(t, cfg)
			removeV41Config(t, cfg)
			removeV40Config(t, cfg)
			removeV39Config(t, cfg)
			removeV38Config(t, cfg)
			removeV36Config(t, cfg)
			removeV35Config(t, cfg)
			removeV34Config(t, cfg)
			removeV43Metrics(t, met)
			removeV34Metrics(t, met)
			removeV32Config(t, cfg)
			removeV30Config(t, cfg)
			removeV29Config(t, cfg)
			removeV26Config(t, cfg)
			removeV27Config(t, cfg)
			removeV25Config(t, cfg)
			restoreExec(t, cfg, "ALTER TABLE node_facts DROP COLUMN network")
			removeV22ThemeConfig(t, cfg)
			removeV21Columns(t, cfg, met)
			removeV31Metrics(t, met)
			removeV28Metrics(t, met)
			restoreExec(t, cfg, `DROP TABLE admin_security; DROP TABLE probe_task_tag; DROP TABLE alert_rule_tag;
				ALTER TABLE alert_rule DROP COLUMN resource_metric; ALTER TABLE alert_rule DROP COLUMN recovery_threshold;
				DROP TABLE snapshot_theme; ALTER TABLE snapshot_meta DROP COLUMN format_version`)
			if version < 19 {
				restoreExec(t, cfg, "ALTER TABLE restore_record DROP COLUMN themes")
			}
			if version < 18 {
				restoreExec(t, cfg, `ALTER TABLE alert_delivery DROP COLUMN batch_id; ALTER TABLE alert_delivery DROP COLUMN not_before; ALTER TABLE notify_channel DROP COLUMN rate_per_minute`)
			}
			restoreExec(t, cfg, fmt.Sprintf("UPDATE snapshot_meta SET schema_version=%d", version))
			for _, table := range []string{"metric_1m", "metric_5m", "metric_1h"} {
				for _, column := range []string{"memory_used_pct_sum", "memory_used_pct_n", "disk_used_pct_sum", "disk_used_pct_n"} {
					restoreExec(t, met, "ALTER TABLE "+table+" DROP COLUMN "+column)
				}
			}
			restoreExec(t, met, fmt.Sprintf("ALTER TABLE snapshot_meta DROP COLUMN format_version; UPDATE snapshot_meta SET schema_version=%d", version))
			beforeConfig, beforeMetrics := restoreDump(t, config), restoreDump(t, metrics)
			path := restoreTarget(t)
			restoreExec(t, restoreDB(t, path), "INSERT INTO admin_session (token_hash,created_at,last_used_at,expires_at) VALUES(x'01',1,1,9999999999)")
			if err := runRestoreWith([]string{"--db", path, "--config", config, "--metrics", metrics, "--yes"}, &bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}
			db := restoreDB(t, path)
			restoreWant(t, db, "SELECT data FROM admin_security WHERE id=1", "{}")
			restoreWant(t, db, "SELECT count(*) FROM admin_session", "0")
			restoreWant(t, db, "SELECT count(*) FROM node", "2")
			restoreWant(t, db, "SELECT count(*) FROM metric_1m", "1")
			restoreWant(t, db, "SELECT memory_used_pct_n+disk_used_pct_n FROM metric_1m", "0")
			if restoreDump(t, config) != beforeConfig || restoreDump(t, metrics) != beforeMetrics {
				t.Fatal("historical snapshot changed during migration")
			}
		})
	}
}

func removeV26Config(t *testing.T, config *sql.DB) {
	t.Helper()
	restoreExec(t, config, "ALTER TABLE node_facts DROP COLUMN diagnostics; ALTER TABLE traffic DROP COLUMN net_counter_epoch")
}

// 42 给 admin_session 加了签发时的来源地址列，在配置层。
// 44 给 node 加了 rented，回填旧版本号前必须撤回，否则迁移 44 的 ADD COLUMN 撞上已有的列。
func removeV44Config(t *testing.T, config *sql.DB) {
	t.Helper()
	restoreExec(t, config, "ALTER TABLE node DROP COLUMN rented")
}

func removeV42Config(t *testing.T, config *sql.DB) {
	t.Helper()
	restoreExec(t, config, `ALTER TABLE admin_session DROP COLUMN source`)
}

// 41 把流量基线从 traffic 的 last_rx、last_tx 移到 traffic_baseline，在配置层；合计形态的行回填，其余取 v40 的 -1（没有基线）。
func removeV41Config(t *testing.T, config *sql.DB) {
	t.Helper()
	restoreExec(t, config, `ALTER TABLE traffic ADD COLUMN last_rx INTEGER NOT NULL DEFAULT -1; ALTER TABLE traffic ADD COLUMN last_tx INTEGER NOT NULL DEFAULT -1;
		UPDATE traffic SET last_rx = b.rx, last_tx = b.tx FROM traffic_baseline b WHERE b.node_id = traffic.node_id AND b.iface = '';
		DROP TABLE traffic_baseline`)
}

// 同上：32 号给 node 加了 public_remark，回填旧版本号前必须撤回。
// 33 只在指标层的探测表上加了对比索引；拆库读用不到它，回退就是删除三个索引。
// 34 给 node_facts 加了 execution 与 facts_rev，给三张指标表加了 load1_per_core 的 sum/n。
// 回填更早的版本号之前必须撤掉，否则配置层的 ADD COLUMN 会撞上重复列。
// 40 给 node 加了手填出口地址两列，在配置层。
func removeV40Config(t *testing.T, config *sql.DB) {
	t.Helper()
	restoreExec(t, config, `ALTER TABLE node DROP COLUMN ipv4_pin; ALTER TABLE node DROP COLUMN ipv6_pin`)
}

// 39 给 alert_rule 加了 rtt 相对判定的八列、给 alert_state 加了 fired_at，并新建 alert_baseline，全部在配置层。
func removeV39Config(t *testing.T, config *sql.DB) {
	t.Helper()
	restoreExec(t, config, `DROP TABLE alert_baseline; ALTER TABLE alert_state DROP COLUMN fired_at;
		ALTER TABLE alert_rule DROP COLUMN rtt_mode; ALTER TABLE alert_rule DROP COLUMN baseline_mode;
		ALTER TABLE alert_rule DROP COLUMN baseline_window_s; ALTER TABLE alert_rule DROP COLUMN baseline_min_samples;
		ALTER TABLE alert_rule DROP COLUMN upper_deviation_pct; ALTER TABLE alert_rule DROP COLUMN lower_deviation_pct;
		ALTER TABLE alert_rule DROP COLUMN cooldown_s; ALTER TABLE alert_rule DROP COLUMN fixed_baseline_ms`)
}

// removeV38Config 撤掉清理作业表：38 之前的配置层没有它。
func removeV38Config(t *testing.T, config *sql.DB) {
	t.Helper()
	restoreExec(t, config, "DROP TABLE cleanup_job")
}

func removeV37Coordination(t *testing.T, db *sql.DB) {
	t.Helper()
	restoreExec(t, db, "DROP TABLE hub_coordination")
}

func removeV36Config(t *testing.T, config *sql.DB) {
	t.Helper()
	restoreExec(t, config, "ALTER TABLE node DROP COLUMN traffic_quota_bytes; ALTER TABLE node DROP COLUMN traffic_quota_mode")
}

func removeV35Config(t *testing.T, config *sql.DB) {
	t.Helper()
	restoreExec(t, config, "DROP TABLE probe_cert_presented")
	restoreExec(t, config, "ALTER TABLE probe_cert DROP COLUMN config_id")
	restoreExec(t, config, "ALTER TABLE probe_task DROP COLUMN cert_spki_sha256")
	restoreExec(t, config, "ALTER TABLE probe_task DROP COLUMN config_id")
}

func removeV34Config(t *testing.T, config *sql.DB) {
	t.Helper()
	restoreExec(t, config, "ALTER TABLE node_facts DROP COLUMN execution; ALTER TABLE node_facts DROP COLUMN facts_rev")
}

// 43 给三张指标表加了 TCP 速率四列的 sum/n（建连两列另有 max），在指标层。
func removeV43Metrics(t *testing.T, metrics *sql.DB) {
	t.Helper()
	for _, table := range []string{"metric_1m", "metric_5m", "metric_1h"} {
		for _, column := range []string{"tcp_active_opens_ps_sum", "tcp_active_opens_ps_n", "tcp_active_opens_ps_max",
			"tcp_passive_opens_ps_sum", "tcp_passive_opens_ps_n", "tcp_passive_opens_ps_max",
			"tcp_out_segs_ps_sum", "tcp_out_segs_ps_n", "tcp_retrans_segs_ps_sum", "tcp_retrans_segs_ps_n"} {
			restoreExec(t, metrics, "ALTER TABLE "+table+" DROP COLUMN "+column)
		}
	}
}

func removeV34Metrics(t *testing.T, metrics *sql.DB) {
	t.Helper()
	for _, table := range []string{"metric_1m", "metric_5m", "metric_1h"} {
		for _, column := range []string{"load1_per_core_sum", "load1_per_core_n"} {
			restoreExec(t, metrics, "ALTER TABLE "+table+" DROP COLUMN "+column)
		}
	}
}

func removeV33Metrics(t *testing.T, metrics *sql.DB) {
	t.Helper()
	for _, table := range []string{"probe_1m", "probe_5m", "probe_1h"} {
		restoreExec(t, metrics, "DROP INDEX IF EXISTS "+table+"_by_task")
	}
}

func removeV32Config(t *testing.T, config *sql.DB) {
	t.Helper()
	restoreExec(t, config, "ALTER TABLE node DROP COLUMN public_remark")
}

// 同上：30 号建了 probe_cert 表，回填旧版本号前必须撤回。
func removeV30Config(t *testing.T, config *sql.DB) {
	t.Helper()
	restoreExec(t, config, "DROP TABLE probe_cert")
}

// 同上：29 号给 probe_task 加了 dns_server，回填旧版本号前必须撤回。
func removeV29Config(t *testing.T, config *sql.DB) {
	t.Helper()
	restoreExec(t, config, "ALTER TABLE probe_task DROP COLUMN dns_server")
}

// 28 版配置快照没有 dns_server 列；恢复把它升到当前版本，快照里既有的任务行新列取空串，
// 与"非 DNS 任务不携带解析器"的既有值一致。快照表由 CREATE TABLE AS 生成、不带约束与默认值，
// 夹具的行要显式写全各列，不能靠列默认值补齐。
func TestRestoreV28ConfigSnapshotAddsDNSServerColumn(t *testing.T) {
	t.Parallel()
	config, metrics := restoreSnapshots(t)
	cfg := restoreDB(t, config)
	restoreExec(t, cfg, "INSERT INTO probe_task (id,kind,target,interval_s,timeout_ms,created_at,all_nodes,sort_order,config_id) VALUES (8,1,'legacy.example',60,1000,0,0,0,x'02020202020202020202020202020202')")
	removeV44Config(t, cfg)
	removeV41Config(t, cfg)
	removeV40Config(t, cfg)
	removeV39Config(t, cfg)
	removeV38Config(t, cfg)
	removeV36Config(t, cfg)
	removeV35Config(t, cfg)
	removeV34Config(t, cfg)
	removeV32Config(t, cfg)
	removeV30Config(t, cfg)
	removeV29Config(t, cfg)
	restoreExec(t, cfg, "UPDATE snapshot_meta SET schema_version=28")
	path := restoreTarget(t)
	if err := runRestoreWith([]string{"--db", path, "--config", config, "--metrics", metrics, "--yes"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	db := restoreDB(t, path)
	restoreWant(t, db, "SELECT count(*) FROM pragma_table_info('probe_task') WHERE name='dns_server'", "1")
	restoreWant(t, db, "SELECT dns_server FROM probe_task WHERE target='legacy.example'", "")
}

// 降级夹具必须同时撤回真实列与版本号，不能让当前列伪装成旧 schema：快照用当前版本建立再回填
// 一个更早的版本号，若不先撤列，恢复时的迁移会撞上 duplicate column。
func removeV27Config(t *testing.T, config *sql.DB) {
	t.Helper()
	restoreExec(t, config, `ALTER TABLE node DROP COLUMN maintenance;
		ALTER TABLE alert_event DROP COLUMN silenced;
		ALTER TABLE alert_state DROP COLUMN fired_silenced;
		DROP TABLE silence_node; DROP TABLE silence_tag; DROP TABLE silence`)
}

// 指标快照同理：28 号给三个指标层各加了四项的 sum/n/max，回填旧版本号前必须逐列撤回。
func removeV28Metrics(t *testing.T, metrics *sql.DB) {
	t.Helper()
	for _, table := range []string{"metric_1m", "metric_5m", "metric_1h"} {
		for _, column := range []string{
			"disk_read_bps_sum", "disk_read_bps_n", "disk_read_bps_max",
			"disk_write_bps_sum", "disk_write_bps_n", "disk_write_bps_max",
			"cpu_steal_pct_sum", "cpu_steal_pct_n", "cpu_steal_pct_max",
			"cpu_iowait_pct_sum", "cpu_iowait_pct_n", "cpu_iowait_pct_max",
		} {
			restoreExec(t, metrics, "ALTER TABLE "+table+" DROP COLUMN "+column)
		}
	}
}

// 配置快照只包含持久授权与回执，不包含注册窗口和更新队列。
func removeV25Config(t *testing.T, config *sql.DB) {
	t.Helper()
	restoreExec(t, config, `ALTER TABLE api_token DROP COLUMN permissions;
		ALTER TABLE api_token DROP COLUMN all_nodes;
		DROP TABLE api_token_node; DROP TABLE operation`)
}

// 历史快照必须真实还原主题表结构；只改版本号会跳过或重复当前版本的迁移。
func removeV22ThemeConfig(t *testing.T, config *sql.DB) {
	t.Helper()
	restoreExec(t, config, `DROP TABLE theme_selection; DROP TABLE theme_version; DROP TABLE theme;
		CREATE TABLE theme(id TEXT PRIMARY KEY,name TEXT NOT NULL,version TEXT NOT NULL,preview TEXT NOT NULL,uploaded_at INTEGER NOT NULL,enabled INTEGER NOT NULL DEFAULT 0 CHECK(enabled IN(0,1)))`)
}

// 降级夹具必须同时撤回真实列与版本号，不能让当前列伪装成旧 schema。
func removeV21Columns(t *testing.T, config, metrics *sql.DB) {
	t.Helper()
	restoreExec(t, config, "ALTER TABLE probe_task DROP COLUMN sort_order")
	for _, table := range []string{"metric_1m", "metric_5m", "metric_1h"} {
		for _, column := range []string{"net_rx_bps_sum", "net_rx_bps_n", "net_rx_bps_max", "net_tx_bps_sum", "net_tx_bps_n", "net_tx_bps_max"} {
			restoreExec(t, metrics, "ALTER TABLE "+table+" DROP COLUMN "+column)
		}
	}
}

func TestRestoreExistingHighWaterAndOptionalMetrics(t *testing.T) {
	t.Parallel()
	for _, withMetrics := range []bool{true, false} {
		t.Run(fmt.Sprint(withMetrics), func(t *testing.T) {
			config, metrics := restoreSnapshots(t)
			restoreExec(t, restoreDB(t, config), "UPDATE sqlite_sequence SET seq=3 WHERE name='node'")
			path := restoreTarget(t)
			args := []string{"--db", path, "--config", config, "--yes"}
			wantTS, wantTaken := "300", "none"
			if withMetrics {
				args = append(args, "--metrics", metrics)
				wantTS, wantTaken = "600", "2000"
			}
			if err := runRestoreWith(args, &bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}
			db := restoreDB(t, path)
			restoreWant(t, db, "SELECT seq FROM sqlite_sequence WHERE name='node'", "5")
			restoreWant(t, db, "SELECT seq FROM sqlite_sequence WHERE name='target_only'", "19")
			restoreWant(t, db, "SELECT seq FROM sqlite_sequence WHERE name='probe_task'", "7")
			restoreWant(t, db, "SELECT group_concat(node_id || ':' || ts) FROM metric_1m", "2:"+wantTS)
			restoreWant(t, db, "SELECT coalesce(metrics_taken_at,'none') FROM restore_record", wantTaken)
			restoreExec(t, db, "INSERT INTO node (name,token_hash,created_at) VALUES ('next',x'06',0)")
			restoreWant(t, db, "SELECT id FROM node WHERE name='next'", "6")
			if err := runRestoreWith(args, &bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}
			restoreWant(t, db, "SELECT count(*) FROM restore_record", "2")
		})
	}
}

func TestRestoreTargetSchemaPolicy(t *testing.T) {
	t.Parallel()
	var currentVersion int
	if err := restoreDB(t, restoreTarget(t)).QueryRow("PRAGMA user_version").Scan(&currentVersion); err != nil {
		t.Fatal(err)
	}
	for _, version := range []int{currentVersion - 1, currentVersion + 1, -1, 0} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			config, metrics := restoreSnapshots(t)
			path := restoreTarget(t)
			db := restoreDB(t, path)
			restoreExec(t, db, fmt.Sprintf("PRAGMA user_version=%d", version))
			before := restoreDump(t, path)
			err := runRestoreWith([]string{"--db", path, "--config", config, "--metrics", metrics, "--yes"}, &bytes.Buffer{})
			statsErr := runStatsWith([]string{"--db", path}, &bytes.Buffer{})
			if err == nil || statsErr == nil || err.Error() != statsErr.Error() {
				t.Errorf("restore/stats admission differs: restore=%v stats=%v", err, statsErr)
			}
			if err == nil || !strings.Contains(err.Error(), "schema version") {
				t.Errorf("target version %d must be rejected without migration: %v", version, err)
			}
			if after := restoreDump(t, path); after != before {
				t.Error("rejected target schema changed database")
			}
			restoreWant(t, db, "PRAGMA user_version", fmt.Sprint(version))
		})
	}
}

func TestRestoreRejectsSnapshotsWithoutChangingTarget(t *testing.T) {
	t.Parallel()
	for _, layer := range []string{"config", "metrics"} {
		for _, defect := range []string{"schema", "future schema", "page", "table", "layer", "metadata", "empty metadata"} {
			t.Run(layer+"/"+defect, func(t *testing.T) {
				t.Parallel()
				config, metrics := restoreSnapshots(t)
				path := restoreTarget(t)
				source := config
				if layer == "metrics" {
					source = metrics
				}
				db := restoreDB(t, source)
				query, wantErr := "", ""
				switch defect {
				case "schema":
					query, wantErr = "UPDATE snapshot_meta SET schema_version=-1", "schema_version=-1"
				case "future schema":
					query, wantErr = "UPDATE snapshot_meta SET schema_version=999", "schema_version=999"
				case "page":
					// VACUUM 会移除没有 AUTOINCREMENT 表的内部序列表；保留簿记，避免先撞缺表守卫。
					query, wantErr = `CREATE TABLE saved_sequence AS SELECT * FROM sqlite_sequence;
						PRAGMA page_size=8192; VACUUM;
						CREATE TABLE sequence_seed (id INTEGER PRIMARY KEY AUTOINCREMENT);
						DROP TABLE sequence_seed;
						INSERT INTO sqlite_sequence SELECT * FROM saved_sequence;
						DROP TABLE saved_sequence`, layer+" snapshot page_size=8192"
				case "table":
					if layer == "config" {
						query, wantErr = "DROP TABLE alert_rule", "missing table alert_rule"
					} else {
						query, wantErr = "DROP TABLE metric_1h", "missing table metric_1h"
					}
				case "layer":
					query, wantErr = "UPDATE snapshot_meta SET layer='wrong'", `layer="wrong"`
				case "metadata":
					query, wantErr = "INSERT INTO snapshot_meta SELECT * FROM snapshot_meta", "rows=2"
				case "empty metadata":
					query, wantErr = "DELETE FROM snapshot_meta", "rows=0"
				}
				restoreExec(t, db, query)
				before := restoreDump(t, path)
				err := runRestoreWith([]string{"--db", path, "--config", config, "--metrics", metrics, "--yes"}, &bytes.Buffer{})
				if err == nil || !strings.Contains(err.Error(), wantErr) {
					t.Errorf("reject %s %s: err=%v, want %q", layer, defect, err, wantErr)
				}
				if after := restoreDump(t, path); after != before {
					t.Error("invalid snapshot changed target database")
				}
			})
		}
	}
}

// 快照里 maintenance 与 rented 同真被拒绝：迁移 44 只加列、不回填，校验在恢复预检
// validateSnapshotNodeFlags 里显式拒绝，而不是让两个标志共存进库。
func TestRestoreRejectsNodeWithBothFlags(t *testing.T) {
	t.Parallel()
	config, metrics := restoreSnapshots(t)
	restoreExec(t, restoreDB(t, config), "UPDATE node SET maintenance=1, rented=1 WHERE id=1")
	path := restoreTarget(t)
	before := restoreDump(t, path)
	err := runRestoreWith([]string{"--db", path, "--config", config, "--metrics", metrics, "--yes"}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "both set") {
		t.Fatalf("restore err = %v, want %q", err, "both set")
	}
	if after := restoreDump(t, path); after != before {
		t.Error("invalid snapshot changed target database")
	}
}

func TestRestorePageSizeAgainstExistingTarget(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                                string
		targetSize, configSize, metricsSize int
	}{
		{"both mismatch", 4096, 8192, 8192},
		{"config mismatch", 4096, 8192, 4096},
		{"config without metrics", 4096, 8192, 0},
		{"nondefault target", 8192, 4096, 8192},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config, metrics := restoreSnapshots(t)
			path := restoreTarget(t)
			if tc.targetSize != 4096 {
				restoreExec(t, restoreDB(t, path), fmt.Sprintf(`PRAGMA journal_mode=DELETE;
					PRAGMA page_size=%d; VACUUM; PRAGMA journal_mode=WAL`, tc.targetSize))
			}
			for _, source := range []struct {
				path string
				size int
			}{{config, tc.configSize}, {metrics, tc.metricsSize}} {
				if source.size == 0 || source.size == 4096 {
					continue
				}
				restoreExec(t, restoreDB(t, source.path), fmt.Sprintf(`CREATE TABLE saved_sequence AS SELECT * FROM sqlite_sequence;
					PRAGMA page_size=%d; VACUUM;
					CREATE TABLE sequence_seed (id INTEGER PRIMARY KEY AUTOINCREMENT); DROP TABLE sequence_seed;
					INSERT INTO sqlite_sequence SELECT * FROM saved_sequence; DROP TABLE saved_sequence`, source.size))
			}
			if tc.metricsSize == 0 {
				metrics = ""
			}
			before := restoreDump(t, path)
			err := runRestoreWith([]string{"--db", path, "--config", config, "--metrics", metrics, "--yes"}, &bytes.Buffer{})
			wantErr := fmt.Sprintf("config snapshot page_size=%d; expected page_size=%d", tc.configSize, tc.targetSize)
			if err == nil || !strings.Contains(err.Error(), wantErr) {
				t.Errorf("page size must match target and name the mismatched source: %v", err)
			}
			if after := restoreDump(t, path); after != before {
				t.Error("page size mismatch changed existing target")
			}
		})
	}
}

func TestRestoreFailureRollsBackAllTables(t *testing.T) {
	t.Parallel()
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing_empty_target=%t", existing), func(t *testing.T) {
			config, metrics := restoreSnapshots(t)
			restoreExec(t, restoreDB(t, config), "UPDATE node SET token_hash=x'01'")
			path := filepath.Join(t.TempDir(), "new.db")
			if existing {
				if err := os.WriteFile(path, nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			cmd := hubCommand(t, "restore", "--db", path, "--config", config, "--metrics", metrics, "--yes")
			out, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(out), "UNIQUE constraint failed: node.token_hash") {
				t.Fatalf("expected mid-copy unique failure, got %v: %s", err, out)
			}
			if strings.Contains(string(out), "database schema created") {
				t.Errorf("failed restore reported schema creation: %s", out)
			}
			if existing {
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("failed restore removed pre-existing target: %v", err)
				}
				db := restoreDB(t, path)
				restoreWant(t, db, "SELECT count(*) FROM sqlite_schema", "0")
				restoreWant(t, db, "PRAGMA user_version", "0")
			} else {
				for _, suffix := range []string{"", "-wal", "-shm"} {
					if _, err := os.Stat(path + suffix); !os.IsNotExist(err) {
						t.Errorf("failed restore left newly created target%s: %v", suffix, err)
					}
				}
			}
		})
	}
	config, metrics := restoreSnapshots(t)
	path := restoreTarget(t)
	db := restoreDB(t, path)
	restoreExec(t, db, `CREATE TRIGGER reject_restore_record BEFORE INSERT ON restore_record BEGIN SELECT RAISE(ABORT,'record write rejected'); END`)
	before := restoreDump(t, path)
	err := runRestoreWith([]string{"--db", path, "--config", config, "--metrics", metrics, "--yes"}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "record write rejected") {
		t.Errorf("expected late record failure, got %v", err)
	}
	if after := restoreDump(t, path); after != before {
		t.Error("late restore failure changed target database: config, metrics, sequences and record must roll back together")
	}
}

func TestRestoreValidatesSourcesBeforeCreatingTarget(t *testing.T) {
	t.Parallel()
	for _, defect := range []string{"missing config", "missing metrics", "invalid config", "invalid metrics", "page config"} {
		t.Run(defect, func(t *testing.T) {
			config, metrics := restoreSnapshots(t)
			missing := filepath.Join(t.TempDir(), "missing.db")
			switch defect {
			case "missing config":
				config = missing
			case "missing metrics":
				metrics = missing
			case "invalid config":
				restoreExec(t, restoreDB(t, config), "DELETE FROM snapshot_meta")
			case "invalid metrics":
				restoreExec(t, restoreDB(t, metrics), "DELETE FROM snapshot_meta")
			case "page config":
				metrics = ""
				restoreExec(t, restoreDB(t, config), `CREATE TABLE saved_sequence AS SELECT * FROM sqlite_sequence;
					PRAGMA page_size=8192; VACUUM;
					CREATE TABLE sequence_seed (id INTEGER PRIMARY KEY AUTOINCREMENT); DROP TABLE sequence_seed;
					INSERT INTO sqlite_sequence SELECT * FROM saved_sequence; DROP TABLE saved_sequence`)
			}
			path := filepath.Join(t.TempDir(), "target.db")
			if err := runRestoreWith([]string{"--db", path, "--config", config, "--metrics", metrics, "--yes"}, &bytes.Buffer{}); err == nil {
				t.Error("invalid source must be rejected")
			}
			if _, err := os.Stat(missing); !os.IsNotExist(err) {
				t.Errorf("restore created missing snapshot: %v", err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("source validation failure created target: %v", err)
			}
		})
	}
}

func TestRestoreCopiesColumnsByName(t *testing.T) {
	t.Parallel()
	config, metrics := restoreSnapshots(t)
	restoreExec(t, restoreDB(t, config), `ALTER TABLE setting RENAME TO old_setting;
		CREATE TABLE setting AS SELECT value,key FROM old_setting; DROP TABLE old_setting`)
	path := restoreTarget(t)
	if err := runRestoreWith([]string{"--db", path, "--config", config, "--metrics", metrics, "--yes"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	restoreWant(t, restoreDB(t, path), "SELECT value FROM setting WHERE key='site.title'", "snapshot")
}

func TestRestoreRequiresExplicitConfirmation(t *testing.T) {
	t.Parallel()
	config, metrics := restoreSnapshots(t)
	path := restoreTarget(t)
	before := restoreDump(t, path)
	out, err := hubCommand(t, "restore", "--db", path, "--config", config, "--metrics", metrics).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "requires --yes") || !strings.Contains(string(out), "stop hub") {
		t.Errorf("unconfirmed restore: err=%v output=%s", err, out)
	}
	if after := restoreDump(t, path); after != before {
		t.Error("unconfirmed restore changed target database")
	}
}
