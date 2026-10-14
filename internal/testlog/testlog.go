// Package testlog 只给测试用。
package testlog

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// WholeSeconds 按 scripts/e2e.sh 的 startup_seconds 同样的形状取值：在 msg="<msg>" 的记录里找
// " <key>=<整数>s "，返回第一条能读出的。time.Duration 的其它写法（1m0s、1.5s）脚本读成空串，
// 这里同样返回 false；两边的形状必须一起改。
func WholeSeconds(text, msg, key string) (time.Duration, bool) {
	re := regexp.MustCompile(`msg="` + regexp.QuoteMeta(msg) + `".* ` + regexp.QuoteMeta(key) + `=([0-9]+)s `)
	for _, line := range strings.Split(text, "\n") {
		m := re.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		n, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			continue
		}
		return time.Duration(n) * time.Second, true
	}
	return 0, false
}
