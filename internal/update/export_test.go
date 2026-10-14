package update

import (
	"net/http"
	"time"

	"github.com/xjetry/heron/internal/githubtransport"
)

// NewTestOfficialSource 让包外的测试（update_test）把官方源接到受控传输上并缩短停滞期限，读路径与正式构造相同。
// 它只在测试构建里存在，正式代码拿不到一个停滞期限不是 stallTimeout 的官方源。
func NewTestOfficialSource(rt http.RoundTripper, stall time.Duration) *OfficialSource {
	return &OfficialSource{http: githubtransport.WithTransport(rt, 0), stall: stall}
}
