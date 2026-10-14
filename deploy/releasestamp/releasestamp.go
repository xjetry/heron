// Package releasestamp 把本版的版本号与全部 tar 包的 SHA-256 写进安装脚本。安装脚本只按写进
// 自己的这份清单校验下载的 tar 包，--base-url 指向的目录里的 SHA256SUMS 不是校验依据：换掉下载目录的人
// 能同时换掉其中的每个文件，包括 SHA256SUMS。
//
// 写入只有这一份实现：发布目标（release-full / release-hub-only）经 scripts/stampinstall 调用 WriteDir，deploy 的脚本测试直接调用它，
// 测试跑的就是发布流程产出的那种脚本。
//
// 写进去的值会成为 shell 源码，能改写脚本本身。所以版本号与文件名在写入之前按字符集逐个核对，摘要由这里
// 自己计算；三者都只含 [A-Za-z0-9_.-] 与十六进制数字，放进单引号里不会提前闭合引号，也不含换行与空白，
// 脚本按空白切分清单行时字段不会错位。
package releasestamp

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// 写入区的边界：源码脚本里这两行各出现一次、整行相同、相邻，之间什么都没有。
const (
	Begin = "# >>> release stamp >>>"
	End   = "# <<< release stamp <<<"
)

// 版本号与 Makefile 的 check_version 同一口径（镜像 tag 的字符集：只含 [A-Za-z0-9_.-]、首字符不是 . 或 -、
// 至多 128 个字符）。发布目标已经先拦过一次；这里再核对，是因为写入的安全不能依赖调用方做过检查。
var versionRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

// release 资产名的字符集：Makefile 打出的 tar 包名（heron-agent_linux_amd64.tar.gz 之类）都在其中。
var assetRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,200}\.tar\.gz$`)

// Asset 是清单里的一行：release 资产名与它的 SHA-256。
type Asset struct {
	Name   string
	SHA256 [sha256.Size]byte
}

// Assets 计算 dir 下全部 *.tar.gz（不进子目录）的 SHA-256，按文件名的字节序排列。
// 空清单是错误：写进去的脚本会拒绝每一次安装，在发布时就该失败。
func Assets(dir string) ([]Asset, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.tar.gz"))
	if err != nil {
		return nil, err
	}
	var assets []Asset
	for _, p := range paths {
		st, err := os.Lstat(p)
		if err != nil {
			return nil, err
		}
		if !st.Mode().IsRegular() {
			return nil, fmt.Errorf("%s is not a regular file", p)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		assets = append(assets, Asset{Name: filepath.Base(p), SHA256: sha256.Sum256(data)})
	}
	if len(assets) == 0 {
		return nil, fmt.Errorf("no *.tar.gz in %s", dir)
	}
	slices.SortFunc(assets, func(a, b Asset) int { return strings.Compare(a.Name, b.Name) })
	return assets, nil
}

// Script 返回在写入区写好版本号与清单的脚本。src 的写入区必须为空：只从源码写入，同一组输入的输出逐字节相同，
// 也不会在已写入的脚本上叠加第二份。清单按文件名排序后写出，与传入的顺序无关。
func Script(src []byte, version string, assets []Asset) ([]byte, error) {
	if !versionRE.MatchString(version) {
		return nil, fmt.Errorf("version %q: want only [A-Za-z0-9_.-], not starting with . or -, at most 128 characters", version)
	}
	if len(assets) == 0 {
		return nil, errors.New("no assets to embed")
	}
	sorted := slices.Clone(assets)
	slices.SortFunc(sorted, func(a, b Asset) int { return strings.Compare(a.Name, b.Name) })
	for i, a := range sorted {
		if !assetRE.MatchString(a.Name) {
			return nil, fmt.Errorf("asset name %q: want [A-Za-z0-9_.-] ending in .tar.gz, not starting with . or -", a.Name)
		}
		if i > 0 && sorted[i-1].Name == a.Name {
			return nil, fmt.Errorf("asset %q listed twice", a.Name)
		}
	}

	lines := strings.SplitAfter(string(src), "\n")
	begin, end := -1, -1
	for i, l := range lines {
		switch strings.TrimSuffix(l, "\n") {
		case Begin:
			if begin >= 0 {
				return nil, fmt.Errorf("%q appears more than once", Begin)
			}
			begin = i
		case End:
			if end >= 0 {
				return nil, fmt.Errorf("%q appears more than once", End)
			}
			end = i
		}
	}
	if begin < 0 || end < 0 {
		return nil, fmt.Errorf("script lacks the %q and %q lines", Begin, End)
	}
	if end != begin+1 {
		return nil, errors.New("the release stamp region is not empty; stamp the source script, not a stamped one")
	}

	var b bytes.Buffer
	for _, l := range lines[:end] {
		b.WriteString(l)
	}
	fmt.Fprintf(&b, "RELEASE_VERSION='%s'\n", version)
	b.WriteString("RELEASE_SHA256='")
	for i, a := range sorted {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%s  %s", hex.EncodeToString(a.SHA256[:]), a.Name)
	}
	b.WriteString("'\n")
	for _, l := range lines[end:] {
		b.WriteString(l)
	}
	return b.Bytes(), nil
}

// WriteDir 按 dir 下的全部 tar 包写入每个 scripts 里的源码脚本，输出到 dir/<脚本文件名>，权限位与源码相同。
// 清单只计算一次，同一次调用写出的各个脚本内嵌同一份清单。
func WriteDir(version, dir string, scripts ...string) error {
	if len(scripts) == 0 {
		return errors.New("no scripts to stamp")
	}
	assets, err := Assets(dir)
	if err != nil {
		return err
	}
	for _, s := range scripts {
		src, err := os.ReadFile(s)
		if err != nil {
			return err
		}
		st, err := os.Stat(s)
		if err != nil {
			return err
		}
		out, err := Script(src, version, assets)
		if err != nil {
			return fmt.Errorf("%s: %w", s, err)
		}
		dst := filepath.Join(dir, filepath.Base(s))
		if err := os.WriteFile(dst, out, st.Mode().Perm()); err != nil {
			return err
		}
		if err := os.Chmod(dst, st.Mode().Perm()); err != nil {
			return err
		}
	}
	return nil
}
