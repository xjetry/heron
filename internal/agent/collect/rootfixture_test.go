package collect

import (
	"io/fs"

	"github.com/xjetry/heron/internal/agent/collect/collecttest"
)

// plainProc 是只读内容、不做识别的 ProcFS：读者根与宿主根是同一棵树，与默认根 "/" 的形状相同。
func plainProc(fsys fs.FS) *ProcFS {
	r := collecttest.New(fsys)
	return &ProcFS{Root: r, Reader: r}
}

// fixtureOf 取夹具 ProcFS 的宿主根，供测试改写注入。
func fixtureOf(p *ProcFS) *collecttest.Root { return p.Root.(*collecttest.Root) }
