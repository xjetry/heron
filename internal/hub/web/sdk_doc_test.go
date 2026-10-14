package web

import (
	"os"
	"regexp"
	"slices"
	"testing"
)

// 面板随包下发的主题技能文件（web/src/assets/heron-theme-skill.md）把 SDK 的导出函数列成一张表；它与 themeSDKJS
// 是两份物理分离的说明与实现。加、删、改名导出函数时须同改那张表，否则主题作者照表调用到的函数不存在，或不知道有新函数。
// call 是底层的通用入口，不进表：主题应使用具名函数。
func TestThemeSkillSDKTableMatchesExports(t *testing.T) {
	exported := regexp.MustCompile(`export (?:const|async function|function) ([A-Za-z]+)`)
	var exports []string
	for _, m := range exported.FindAllStringSubmatch(themeSDKJS, -1) {
		if m[1] != "call" {
			exports = append(exports, m[1])
		}
	}
	raw, err := os.ReadFile("../../../web/src/assets/heron-theme-skill.md")
	if err != nil {
		t.Fatal(err)
	}
	row := regexp.MustCompile("(?m)^\\| `([A-Za-z]+)\\(")
	var documented []string
	for _, m := range row.FindAllStringSubmatch(string(raw), -1) {
		documented = append(documented, m[1])
	}
	if len(exports) == 0 || !slices.Equal(documented, exports) {
		t.Errorf("技能文件的 SDK 表 = %v，themeSDKJS 导出 = %v", documented, exports)
	}
	if !regexp.MustCompile(`(?m)^import \{[^}]*\} from '/_heron/theme-sdk\.js';$`).Match(raw) {
		t.Error("技能文件里没有从 /_heron/theme-sdk.js 导入 SDK 的示例")
	}
}
