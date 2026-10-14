package auth

import (
	"net/netip"
	"testing"
)

// 来源地址的规范文本：IPv4 映射还原成点分 IPv4，IPv6 取压缩形式、去掉区域标识；取不到对端为空串。
func TestSourceText(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.7":          "203.0.113.7",
		"::ffff:203.0.113.7":   "203.0.113.7",
		"2001:DB8:0:0:0:0:0:7": "2001:db8::7",
		"fe80::1%eth0":         "fe80::1",
	} {
		if got := SourceText(netip.MustParseAddr(in)); got != want {
			t.Errorf("SourceText(%s) = %q, want %q", in, got, want)
		}
	}
	if got := SourceText(netip.Addr{}); got != "" {
		t.Errorf("SourceText(invalid) = %q, want empty", got)
	}
}
