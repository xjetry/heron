package agentlog

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func newTest() (*slog.Logger, *bytes.Buffer, *fakeClock) {
	var buf bytes.Buffer
	clk := &fakeClock{t: time.Unix(1000, 0)}
	return slog.New(New(slog.NewTextHandler(&buf, nil), clk.now)), &buf, clk
}

func lines(b *bytes.Buffer) []string {
	return strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
}

// 持续输出至多每 RefillEvery 一行，突发至多 Burst 行；被压掉的行数在下一次放行时汇总出现。
func TestRateBound(t *testing.T) {
	log, buf, clk := newTest()
	for range 1000 {
		log.Warn("report failed")
	}
	if n := len(lines(buf)); n != Burst {
		t.Fatalf("burst emitted %d lines, want %d", n, Burst)
	}
	buf.Reset()
	clk.t = clk.t.Add(RefillEvery)
	log.Warn("report failed")
	got := lines(buf)
	if len(got) != 1 || !strings.Contains(got[0], "report failed") || !strings.Contains(got[0], "suppressed_before=980") {
		t.Fatalf("after refill: %q", got)
	}
	buf.Reset()
	// 跨 steps 个 RefillEvery/100 的持续洪水（每步 100 次调用）：行数由经过的时间决定，与调用次数无关。
	// 经过 steps/100 个 RefillEvery，至多补充这么多令牌（再加取整的一个），每个令牌恰好一行。
	const steps = 2880
	for range steps {
		clk.t = clk.t.Add(RefillEvery / 100)
		for range 100 {
			log.Warn("x")
		}
	}
	if n, most := len(lines(buf)), steps/100+1; n > most {
		t.Fatalf("%d lines for %d refill periods of flood, want at most %d", n, steps/100, most)
	}
}

// 派生的 logger 共用同一个桶。
func TestDerivedLoggersShareTheBound(t *testing.T) {
	log, buf, _ := newTest()
	a, b := log.With("k", "a"), log.WithGroup("g")
	for range Burst {
		a.Info("a")
		b.Info("b")
	}
	if n := len(lines(buf)); n != Burst {
		t.Fatalf("%d lines from two derived loggers, want %d in total", n, Burst)
	}
}

// 消息与每个字符串值（含 error 与分组里的值、With 带入的值）都限长，截断处仍是合法 UTF-8。
func TestValuesAreTruncated(t *testing.T) {
	log, buf, _ := newTest()
	long := strings.Repeat("é", 1000)
	log.With("hub", long).WithGroup("g").Warn(long, "s", long, "err", errors.New(long), slog.Group("inner", "v", long), "n", 42)
	out := buf.String()
	if len(out) > 8*MaxValueLen {
		t.Fatalf("line is %d bytes:\n%s", len(out), out)
	}
	if strings.Count(out, "…(truncated)") != 5 || !strings.Contains(out, "g.n=42") || !utf8.ValidString(out) {
		t.Fatalf("unexpected line:\n%s", out)
	}
}

// 每个值（消息、字符串、error、分组里的值）截断后至多 MaxValueLen 字节，标记计在内；按 JSON 解码逐个量。
func TestEachValueFitsTheBudget(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(New(slog.NewJSONHandler(&buf, nil), nil))
	long := strings.Repeat("é", 1000) + strings.Repeat("a", 1000)
	log.Warn(long, "s", long, "err", errors.New(long), slog.Group("g", "v", long))
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	vals := []any{rec["msg"], rec["s"], rec["err"], rec["g"].(map[string]any)["v"]}
	for i, v := range vals {
		if n := len(v.(string)); n > MaxValueLen || !strings.HasSuffix(v.(string), "…(truncated)") {
			t.Errorf("value %d is %d bytes: %.40q", i, n, v)
		}
	}
}
