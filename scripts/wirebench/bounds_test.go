package main

import (
	"bytes"
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/agentwire"
)

// TestMaxScalarsCoversEveryMetricsField：上界只有在 Metrics 的每个字段都被置为最长编码时才是上界。字段全是
// 字符串与数值标量，字符串由 runBounds 显式给出；新增一种标量类型而 maxScalars 不认识时这里红。
func TestMaxScalarsCoversEveryMetricsField(t *testing.T) {
	m := &heronv1.Metrics{BootId: strings.Repeat("0", 36), NetCounterEpoch: strings.Repeat("0", 64)}
	maxScalars(m.ProtoReflect())
	fs := m.ProtoReflect().Descriptor().Fields()
	for i := range fs.Len() {
		f := fs.Get(i)
		if !m.ProtoReflect().Has(f) {
			t.Errorf("field %s (%s) is not set to its longest encoding", f.Name(), f.Kind())
		}
		if f.Kind() == protoreflect.MessageKind {
			t.Errorf("field %s is a message; maxScalars only fills scalars", f.Name())
		}
	}
}

// TestBoundsReportsNetInterfacesWorstCase：bounds 打出的逐网卡列表上界是合法列表的满值编码（hub 准入通过、字节数与
// internal/hub/ingest 的 TestMaxNetInterfacesWire 相同）。
func TestBoundsReportsNetInterfacesWorstCase(t *testing.T) {
	if err := agentwire.ValidateNetInterfaces(maxNetInterfaces()); err != nil {
		t.Fatalf("worst-case list is not admissible: %v", err)
	}
	var buf bytes.Buffer
	if err := runBounds(&buf); err != nil {
		t.Fatal(err)
	}
	if want := "net_interfaces (field 10): 64 entries, 64-byte names, maximal counters: 5760 bytes; one entry eth0 with counters below 2^35 (5-byte varints): 20 bytes"; !strings.Contains(buf.String(), want) {
		t.Fatalf("bounds output lacks %q:\n%s", want, buf.String())
	}
}

// TestBoundsReportsOmittableNetTotals：省掉的三项按满值计（两个 10 字节 varint 各带 1 字节标签，摘要 64 字节带 2 字节
// 标签与 1 字节长度）。
func TestBoundsReportsOmittableNetTotals(t *testing.T) {
	var buf bytes.Buffer
	if err := runBounds(&buf); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"metrics net_rx_total, net_tx_total, net_counter_epoch at their longest: 89 bytes,",
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("bounds output lacks %q:\n%s", want, buf.String())
		}
	}
}
