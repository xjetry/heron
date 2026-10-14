package testlog

import (
	"testing"
	"time"
)

func TestWholeSecondsMatchesTheScriptShape(t *testing.T) {
	const line = `time=x level=INFO msg="hub listening" ttl=12s offline_sweep=1m0s retry=1.5s interval=4s version=dev`
	for _, c := range []struct {
		name, text, msg, key string
		want                 time.Duration
		ok                   bool
	}{
		{"整秒", line, "hub listening", "ttl", 12 * time.Second, true},
		{"分钟写法读不出", line, "hub listening", "offline_sweep", 0, false},
		{"小数读不出", line, "hub listening", "retry", 0, false},
		{"别的消息不算", line, "agent starting", "ttl", 0, false},
		{"字段后须有空格", `msg="hub listening" ttl=12s`, "hub listening", "ttl", 0, false},
		{"前缀相同的键不算", `msg="hub listening" xttl=5s ttl=12s version=dev`, "hub listening", "ttl", 12 * time.Second, true},
		{"取第一条能读出的记录", "msg=\"hub listening\" version=dev\nmsg=\"hub listening\" ttl=7s version=dev\nmsg=\"hub listening\" ttl=9s version=dev", "hub listening", "ttl", 7 * time.Second, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ok := WholeSeconds(c.text, c.msg, c.key)
			if got != c.want || ok != c.ok {
				t.Fatalf("WholeSeconds(%q) = %v, %v; want %v, %v", c.key, got, ok, c.want, c.ok)
			}
		})
	}
}
