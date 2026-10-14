package agentwire

import (
	"testing"
	"time"
)

// hub 在准入范围内的任何 TTL 下发的编码，agent 都原样接受；否则合法 hub 会让 agent 每次上报都告警并改用边界值。
func TestClampAcceptsEveryAdmissibleTTL(t *testing.T) {
	for ttl := MinTTL; ttl <= MaxTTL; ttl += 7 * time.Millisecond {
		ms := ReportIntervalMs(ttl)
		d, ok := ClampReportInterval(ms)
		if !ok || d != time.Duration(ms)*time.Millisecond {
			t.Fatalf("ttl %v: ClampReportInterval(%d) = %v, %v; want %v, true", ttl, ms, d, ok, time.Duration(ms)*time.Millisecond)
		}
	}
	for _, ttl := range []time.Duration{MinTTL, MaxTTL} {
		if _, ok := ClampReportInterval(ReportIntervalMs(ttl)); !ok {
			t.Fatalf("boundary ttl %v rejected", ttl)
		}
	}
}

func TestClampOutOfRange(t *testing.T) {
	lo := time.Duration(ReportIntervalMs(MinTTL)) * time.Millisecond
	hi := time.Duration(ReportIntervalMs(MaxTTL)) * time.Millisecond
	for _, tc := range []struct {
		ms   uint32
		want time.Duration
	}{
		{0, lo},
		{1, lo},
		{ReportIntervalMs(MinTTL) - 1, lo},
		{ReportIntervalMs(MaxTTL) + 1, hi},
		{^uint32(0), hi},
	} {
		if d, ok := ClampReportInterval(tc.ms); ok || d != tc.want {
			t.Fatalf("ClampReportInterval(%d) = %v, %v; want %v, false", tc.ms, d, ok, tc.want)
		}
	}
}
