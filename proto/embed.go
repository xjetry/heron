// Package protosrc 把 proto 源文件与 agent 技能文件（SKILL.md）嵌入 hub，由 GetApiReference 下发：
// 不在仓库里的 agent 由此取得与 hub 同版本的 schema，注释即接口文档。
// 嵌入指令只能取包目录之下的文件，所以这个包放在 proto/ 根；buf 只看 .proto，不受影响。
package protosrc

import "embed"

// Files 用通配而不是逐个列出：新增的 proto 文件自动随 hub 下发，不会漏。
//
//go:embed heron/v1/*.proto
var Files embed.FS

//go:embed SKILL.md
var Guide string
