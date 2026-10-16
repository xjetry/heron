package store

import (
	"database/sql"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/clock"
	"google.golang.org/protobuf/proto"
)

// 冻结手填出口地址引入后的结构，后续生产 DDL 变化不改变旧版夹具。
var schemaV40 = append(slices.Clone(schemaV39),
	`ALTER TABLE node ADD COLUMN ipv4_pin TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE node ADD COLUMN ipv6_pin TEXT NOT NULL DEFAULT ''`,
)

// removeV44Config 把当前版本的配置层快照撤回到 v43 的结构：回填更早的版本号之前必须撤掉，
// 否则迁移 44 的 ADD COLUMN 会撞上已有的列。
func removeV44Config(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec("ALTER TABLE node DROP COLUMN rented"); err != nil {
		t.Fatalf("drop rented: %v", err)
	}
}

// removeV47Config 把配置层快照撤回到 v46 的结构：去掉 api_token 的失效时刻列，否则回填更早的版本号后迁移 47 再加一次。
// 只有列要撤：快照按 CREATE TABLE … AS SELECT 拷数据，不带触发器。
func removeV47Config(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec("ALTER TABLE api_token DROP COLUMN expires_at"); err != nil {
		t.Fatalf("drop expires_at: %v", err)
	}
}

// removeV41Config 把配置层快照撤回到 v40 的结构：合计形态的基线行回填成 traffic 行的 last_rx、last_tx，其余（没有基线、
// 逐网卡形态）取 -1，即 v40 的"没有基线"；回填更早的版本号之前必须撤掉，否则迁移 41 会再建一次 traffic_baseline。
func removeV41Config(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, q := range []string{
		"ALTER TABLE traffic ADD COLUMN last_rx INTEGER NOT NULL DEFAULT -1",
		"ALTER TABLE traffic ADD COLUMN last_tx INTEGER NOT NULL DEFAULT -1",
		"UPDATE traffic SET last_rx = b.rx, last_tx = b.tx FROM traffic_baseline b WHERE b.node_id = traffic.node_id AND b.iface = ''",
		"DROP TABLE traffic_baseline",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// removeV40Config 把当前版本的配置层快照撤回到 v39 的结构：回填更早的版本号之前必须撤掉，否则迁移 40 的
// ADD COLUMN 会撞上已有的列。
func removeV40Config(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, q := range []string{"ALTER TABLE node DROP COLUMN ipv4_pin", "ALTER TABLE node DROP COLUMN ipv6_pin"} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// 迁移 40：已有节点没有手填地址，显示值照旧取 agent 探测。
func TestMigrationFromV39AddsAddressPins(t *testing.T) {
	t.Parallel()
	s := migrateFrom(t, 39, func(t *testing.T, db *sql.DB) {
		if _, err := db.Exec("INSERT INTO node (id, name, token_hash, created_at) VALUES (7, 'kept', x'01', 1)"); err != nil {
			t.Fatal(err)
		}
	})
	n, err := s.GetNode(t.Context(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if n.IPv4Pin != "" || n.IPv6Pin != "" {
		t.Fatalf("pins after migration = %q/%q, want empty", n.IPv4Pin, n.IPv6Pin)
	}
	if got, err := s.NetworkDetections(t.Context()); err != nil || len(got) != 0 {
		t.Fatalf("detections after migration = %v %v, want none", got, err)
	}
}

func pinEdit(v4, v6 string) NodeEdit {
	return NodeEdit{Name: "n", TrafficResetDay: 1, IPv4Pin: v4, IPv6Pin: v6}
}

// 写入口按该族公网单播地址裁决、存规范形；不合法的值整次拒绝，库里保留原值。错族、私网、CGNAT、文档段、带 zone、
// IPv4 映射写法都拒绝——与 agent 自报出口同一个谓词。
func TestUpdateNodeAddressPins(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	id, _, err := s.CreateNode(t.Context(), NewNode{Name: "n"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateNode(t.Context(), id, pinEdit("8.8.8.8", "2606:4700:0:0:0:0:0:1111")); err != nil {
		t.Fatal(err)
	}
	read := func() (string, string) {
		t.Helper()
		n, err := s.GetNode(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		return n.IPv4Pin, n.IPv6Pin
	}
	if v4, v6 := read(); v4 != "8.8.8.8" || v6 != "2606:4700::1111" {
		t.Fatalf("stored pins = %q/%q, want canonical 8.8.8.8 and 2606:4700::1111", v4, v6)
	}
	for _, tc := range []struct {
		name, v4, v6 string
		ipv4         bool
	}{
		{"v6_in_ipv4", "2606:4700::1111", "", true},
		{"v4_in_ipv6", "", "8.8.8.8", false},
		{"private_v4", "10.0.0.1", "", true},
		{"cgnat_v4", "100.64.0.1", "", true},
		{"documentation_v4", "203.0.113.7", "", true},
		{"ula_v6", "", "fd00::1", false},
		{"zone_v6", "", "2606:4700::1111%eth0", false},
		{"mapped_v6", "", "::ffff:8.8.8.8", false},
		{"not_an_address", "example.com", "", true},
		{"surrounding_space", " 8.8.8.8", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.UpdateNode(t.Context(), id, pinEdit(tc.v4, tc.v6))
			var pinErr AddressPinError
			got := tc.v6
			if tc.ipv4 {
				got = tc.v4
			}
			if !errors.As(err, &pinErr) || pinErr.IPv4 != tc.ipv4 || pinErr.Got != got {
				t.Fatalf("UpdateNode(%q, %q) = %v, want AddressPinError{IPv4: %v, Got: %q}", tc.v4, tc.v6, err, tc.ipv4, got)
			}
			if v4, v6 := read(); v4 != "8.8.8.8" || v6 != "2606:4700::1111" {
				t.Fatalf("rejected edit changed pins to %q/%q", v4, v6)
			}
		})
	}
	if _, err := s.UpdateNode(t.Context(), id, pinEdit("", "")); err != nil {
		t.Fatal(err)
	}
	if v4, v6 := read(); v4 != "" || v6 != "" {
		t.Fatalf("clearing left %q/%q", v4, v6)
	}
}

// 显示值：手填非空取手填（AVAILABLE）；否则 agent 原报状态为 AVAILABLE / UNSUPPORTED / DISABLED 的族取原报；
// 原报缺该族或为 FAILED 时，hub 看到的公网来源（last_source）是该族公网单播地址则取来源（HUB_SEEN / AVAILABLE）。
// 两族各自独立；Facts.Network 不被改写。
func TestDisplayNetwork(t *testing.T) {
	t.Parallel()
	available := heronv1.AddressDetectionState_ADDRESS_DETECTION_STATE_AVAILABLE
	failed := heronv1.AddressDetectionState_ADDRESS_DETECTION_STATE_FAILED
	disabledState := heronv1.AddressDetectionState_ADDRESS_DETECTION_STATE_DISABLED
	unsupported := heronv1.AddressDetectionState_ADDRESS_DETECTION_STATE_UNSUPPORTED
	only := func(state heronv1.AddressDetectionState) *heronv1.NetworkInfo {
		return &heronv1.NetworkInfo{Ipv4: &heronv1.AddressDetection{State: state, CheckedAt: 10}}
	}
	reported := &heronv1.NetworkInfo{
		Ipv4: &heronv1.AddressDetection{State: failed, CheckedAt: 10},
		Ipv6: &heronv1.AddressDetection{State: available, Address: "2606:4700::1111", CheckedAt: 10},
	}
	for _, tc := range []struct {
		name string
		node Node
		want [2]DisplayAddress
	}{
		{"never_reported", Node{}, [2]DisplayAddress{}},
		{"pin_without_facts", Node{IPv4Pin: "8.8.8.8"}, [2]DisplayAddress{{Address: "8.8.8.8", Source: AddressManual, State: available}, {}}},
		{"detected", Node{Facts: &heronv1.Facts{Network: reported}}, [2]DisplayAddress{
			{Source: AddressDetected, State: failed},
			{Address: "2606:4700::1111", Source: AddressDetected, State: available}}},
		{"pin_over_failed", Node{IPv4Pin: "8.8.8.8", Facts: &heronv1.Facts{Network: reported}}, [2]DisplayAddress{
			{Address: "8.8.8.8", Source: AddressManual, State: available},
			{Address: "2606:4700::1111", Source: AddressDetected, State: available}}},
		{"pin_over_available", Node{IPv6Pin: "2001:4860::8888", Facts: &heronv1.Facts{Network: reported}}, [2]DisplayAddress{
			{Source: AddressDetected, State: failed},
			{Address: "2001:4860::8888", Source: AddressManual, State: available}}},
		{"disabled_after_clear", Node{Facts: &heronv1.Facts{Network: &heronv1.NetworkInfo{Ipv4: &heronv1.AddressDetection{State: disabledState, CheckedAt: 10}}}}, [2]DisplayAddress{
			{Source: AddressDetected, State: disabledState}, {}}},
		{"unspecified_is_none", Node{Facts: &heronv1.Facts{Network: &heronv1.NetworkInfo{Ipv4: &heronv1.AddressDetection{}}}}, [2]DisplayAddress{}},
		// hub 来源（规则 3）：FAILED 或原报缺族时回退，另一族不跟着填，私网来源不回退，手填与 agent 可用结果优先。
		{"hub_seen_v4_after_failed", Node{LastSource: "8.8.8.8", Facts: &heronv1.Facts{Network: only(failed)}}, [2]DisplayAddress{
			{Address: "8.8.8.8", Source: AddressHubSeen, State: available}, {}}},
		{"hub_seen_v4_without_facts", Node{LastSource: "8.8.8.8"}, [2]DisplayAddress{
			{Address: "8.8.8.8", Source: AddressHubSeen, State: available}, {}}},
		{"hub_seen_v6_leaves_failed_v4_alone", Node{LastSource: "2606:4700::1111", Facts: &heronv1.Facts{Network: only(failed)}}, [2]DisplayAddress{
			{Source: AddressDetected, State: failed}, {Address: "2606:4700::1111", Source: AddressHubSeen, State: available}}},
		{"unsupported_kept_over_source", Node{LastSource: "8.8.8.8", Facts: &heronv1.Facts{Network: only(unsupported)}}, [2]DisplayAddress{
			{Source: AddressDetected, State: unsupported}, {}}},
		{"disabled_kept_over_source", Node{LastSource: "8.8.8.8", Facts: &heronv1.Facts{Network: only(disabledState)}}, [2]DisplayAddress{
			{Source: AddressDetected, State: disabledState}, {}}},
		{"private_v4_source_no_fallback", Node{LastSource: "10.0.0.5", Facts: &heronv1.Facts{Network: only(failed)}}, [2]DisplayAddress{
			{Source: AddressDetected, State: failed}, {}}},
		{"private_v6_source_no_fallback", Node{LastSource: "fd00::5", Facts: &heronv1.Facts{Network: &heronv1.NetworkInfo{Ipv6: &heronv1.AddressDetection{State: failed, CheckedAt: 10}}}}, [2]DisplayAddress{
			{}, {Source: AddressDetected, State: failed}}},
		{"manual_over_source", Node{IPv4Pin: "8.8.8.8", LastSource: "1.1.1.1", Facts: &heronv1.Facts{Network: only(failed)}}, [2]DisplayAddress{
			{Address: "8.8.8.8", Source: AddressManual, State: available}, {}}},
		{"detected_available_over_source", Node{LastSource: "1.1.1.1", Facts: &heronv1.Facts{Network: &heronv1.NetworkInfo{Ipv4: &heronv1.AddressDetection{State: available, Address: "9.9.9.9", CheckedAt: 10}}}}, [2]DisplayAddress{
			{Address: "9.9.9.9", Source: AddressDetected, State: available}, {}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := proto.Clone(tc.node.Facts.GetNetwork())
			if got := tc.node.DisplayNetwork(); got != tc.want {
				t.Fatalf("DisplayNetwork() = %+v, want %+v", got, tc.want)
			}
			if !proto.Equal(tc.node.Facts.GetNetwork(), before) {
				t.Fatal("DisplayNetwork rewrote the reported network")
			}
		})
	}
}

// 逐族开关只由手填是否为空得出；启动加载只列出有手填的节点。
func TestNetworkDetectionsFollowPins(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	var ids [3]int64
	for i := range ids {
		id, _, err := s.CreateNode(t.Context(), NewNode{Name: "n"}, hash(byte(i+1)))
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = id
	}
	if _, err := s.UpdateNode(t.Context(), ids[0], pinEdit("8.8.8.8", "")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateNode(t.Context(), ids[1], pinEdit("", "2606:4700::1111")); err != nil {
		t.Fatal(err)
	}
	got, err := s.NetworkDetections(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := map[int64]*heronv1.NetworkDetection{ids[0]: {SkipIpv4: true}, ids[1]: {SkipIpv6: true}}
	if len(got) != len(want) {
		t.Fatalf("detections = %v, want %v", got, want)
	}
	for id, w := range want {
		if !proto.Equal(got[id], w) {
			t.Fatalf("node %d detection = %v, want %v", id, got[id], w)
		}
	}
}

// 快照里的手填地址与写入口写出的一样受检：合法的规范形原样恢复；非公网、错族与非规范写法让恢复失败，不进库。
func TestAddressPinSnapshotRestore(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, column, value, err string
	}{
		{"valid", "ipv4_pin", "8.8.8.8", ""},
		{"private", "ipv4_pin", "10.0.0.1", "ipv4_pin"},
		{"wrong_family", "ipv6_pin", "8.8.8.8", "ipv6_pin"},
		{"not_canonical", "ipv6_pin", "2606:4700:0:0:0:0:0:1111", "canonical"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := open(t)
			id, _, err := s.CreateNode(t.Context(), NewNode{Name: "n"}, hash(1))
			if err != nil {
				t.Fatal(err)
			}
			config := filepath.Join(t.TempDir(), "config.db")
			if err := s.SnapshotConfig(t.Context(), config); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", config)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec("UPDATE node SET "+tc.column+" = ?", tc.value); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(t.TempDir(), "restored.db")
			_, err = Restore(t.Context(), target, config, "", "", time.Now(), slog.Default())
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("restore of %s=%q: %v, want an error mentioning %q", tc.column, tc.value, err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			restored, err := Open(target, clock.Real(), slog.Default(), RequireCurrentSchema)
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			n, err := restored.GetNode(t.Context(), id)
			if err != nil || n.IPv4Pin != tc.value {
				t.Fatalf("restored pin = %q (%v), want %q", n.IPv4Pin, err, tc.value)
			}
		})
	}
}
