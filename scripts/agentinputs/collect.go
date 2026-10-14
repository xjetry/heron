package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// inputs 是一棵树上 agent 组的全部构建输入：文件（相对树根的路径）、第三方模块与根 go.mod 里
// 决定构建行为的指令行。比较两侧的 inputs 即门禁的判定；比较的内容逐类对应 spec 列出的三类输入。
type inputs struct {
	files   map[string]bool // 相对树根的路径
	modules map[string]bool // path@version，有替换时追加 =>replPath@replVersion
	goLines []string        // 根 go.mod 的 go、toolchain 与 godebug 指令行
}

// target 是 agent-go-targets 的一行：包路径加上它参与的平台（GOARM 有才非空）。
type target struct{ pkg, goos, goarch, goarm string }

// errNoBundleDefinition：这棵树的 Makefile 没有 agent-bundle-inputs / agent-go-targets 目标（make 报
// No rule to make target）。基点一侧出现它意味着基点早于 agent 组的定义，工作树一侧出现它意味着装配错误。
var errNoBundleDefinition = errors.New("no agent bundle definition")

// listedPackage / listedModule 只解码 go list -json 输出中门禁要用的字段；Module 与 Replace 是指针，缺省为
// nil。文件按 go list 的分类全部计入：汇编 #include 的头文件在 HFiles 里，不开 cgo 它也参与构建。
type listedPackage struct {
	ImportPath   string
	Name         string
	Dir          string
	Standard     bool
	Module       *listedModule
	GoFiles      []string
	CgoFiles     []string
	CFiles       []string
	CXXFiles     []string
	MFiles       []string
	HFiles       []string
	FFiles       []string
	SFiles       []string
	SwigFiles    []string
	SwigCXXFiles []string
	SysoFiles    []string
	EmbedFiles   []string
}

type listedModule struct {
	Path    string
	Version string
	Main    bool
	GoMod   string
	Replace *listedModule
}

const goListFields = "ImportPath,Name,Dir,Standard,Module,GoFiles,CgoFiles,CFiles,CXXFiles,MFiles,HFiles,FFiles,SFiles,SwigFiles,SwigCXXFiles,SysoFiles,EmbedFiles"

func (p listedPackage) inputFiles() []string {
	return slices.Concat(p.GoFiles, p.CgoFiles, p.CFiles, p.CXXFiles, p.MFiles, p.HFiles, p.FFiles,
		p.SFiles, p.SwigFiles, p.SwigCXXFiles, p.SysoFiles, p.EmbedFiles)
}

// pkgFiles 是一个包跨全部目标的文件并集。同一包在不同平台列出不同文件（build tag、按文件名的约束），
// 并集才是这个包的全部输入；只在 darwin 或只在 arm 编译的文件因此在输入里。
type pkgFiles struct {
	name  string
	files map[string]bool // 只收 .go：生成文件的判定与引用分析只对 Go 源码有意义
}

// collect 展开一棵树的 agent 组构建输入：make 的两份清单（打包输入与（包、平台）表）、go list 的文件与
// 模块闭包、根 go.mod 的指令行。生成文件随后按 proto 粒度筛选（keepReferencedGenerated）。
func collect(tree string) (inputs, error) {
	in := inputs{files: map[string]bool{}, modules: map[string]bool{}}
	bundleLines, err := makeLines(tree, "agent-bundle-inputs")
	if err != nil {
		return in, err
	}
	for _, f := range bundleLines {
		in.files[f] = true
	}
	targetLines, err := makeLines(tree, "agent-go-targets")
	if err != nil {
		return in, err
	}
	targets, err := parseTargets(targetLines)
	if err != nil {
		return in, err
	}
	pkgs := map[string]*pkgFiles{}
	addPkgFiles := func(p listedPackage) error {
		info := pkgs[p.ImportPath]
		if info == nil {
			info = &pkgFiles{name: p.Name, files: map[string]bool{}}
			pkgs[p.ImportPath] = info
		}
		for _, name := range p.inputFiles() {
			rel, err := relFile(tree, filepath.Join(p.Dir, name))
			if err != nil {
				return err
			}
			in.files[rel] = true
			if strings.HasSuffix(rel, ".go") {
				info.files[rel] = true
			}
		}
		return nil
	}
	for _, t := range targets {
		listed, err := goList(tree, t)
		if err != nil {
			return in, err
		}
		for _, p := range listed {
			if p.Standard {
				// 标准库由 Go 工具链版本承载（根 go.mod 的 go 指令，见 goModLines），不逐文件比较。
				continue
			}
			if p.Module == nil {
				return in, fmt.Errorf("package %s has no module; go list ran outside module mode", p.ImportPath)
			}
			if p.Module.Main {
				if err := addPkgFiles(p); err != nil {
					return in, err
				}
				continue
			}
			in.modules[moduleString(p.Module)] = true
			// 替换目标是本地目录（Replace.Version 为空）的模块没有不可变的版本：路径相同不等于内容相同，
			// 它的包按本模块同样取文件，它自己的 go.mod 也计入——其中的 go 指令决定那个模块按哪个语言版本
			// 编译。版本化的模块不需要：path@version 不可变，go.mod 随版本固定。
			if p.Module.Replace != nil && p.Module.Replace.Version == "" {
				if err := addPkgFiles(p); err != nil {
					return in, err
				}
				rel, err := relFile(tree, p.Module.GoMod)
				if err != nil {
					return in, err
				}
				in.files[rel] = true
			}
		}
	}
	if err := keepReferencedGenerated(tree, &in, pkgs); err != nil {
		return in, err
	}
	in.goLines, err = goModLines(tree)
	if err != nil {
		return in, err
	}
	return in, nil
}

// moduleString：path@version，有替换时追加 =>replPath@replVersion。本地目录替换的 replVersion 为空，只
// 记替换路径：同一模块换到另一个本地目录（内容不同）时模块串不同，门禁因而同时从模块与文件两侧看见它。
func moduleString(m *listedModule) string {
	s := m.Path + "@" + m.Version
	if m.Replace != nil {
		s += "=>" + m.Replace.Path
		if m.Replace.Version != "" {
			s += "@" + m.Replace.Version
		}
	}
	return s
}

// relFile 把绝对路径换算成相对树根的路径；逃出树根是错误：本地目录替换指到仓库外的模块既没有可绑定的
// 版本，也无法在基点的临时 worktree 里取到同样内容，只能停下让人处理。
func relFile(tree, abs string) (string, error) {
	rel, err := filepath.Rel(tree, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("%s is outside the repository %s: a module replaced from outside has no version to bind and its files cannot be read at the base ref", abs, tree)
	}
	return rel, nil
}

// makeLines 跑 make -s -C tree goal 并按行返回输出；目标不存在时返回 errNoBundleDefinition 包装。
func makeLines(tree, goal string) ([]string, error) {
	cmd := exec.Command("make", "-s", "-C", tree, goal)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if strings.Contains(stderr.String(), "No rule to make target") {
			return nil, fmt.Errorf("%w: make target %s", errNoBundleDefinition, goal)
		}
		return nil, fmt.Errorf("make -s -C %s %s: %v: %s", tree, goal, err, strings.TrimSpace(stderr.String()))
	}
	return makeLinesOrEmpty(stdout.String()), nil
}

func makeLinesOrEmpty(out string) []string {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimRight(l, "\r"); l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// parseTargets 解析 agent-go-targets 的输出：每行 <包路径> <GOOS> <GOARCH> [<GOARM>]。
func parseTargets(lines []string) ([]target, error) {
	var ts []target
	for i, line := range lines {
		f := strings.Fields(line)
		if len(f) != 3 && len(f) != 4 {
			return nil, fmt.Errorf("agent-go-targets line %d (%q): want <package> <GOOS> <GOARCH> [GOARM]", i+1, line)
		}
		t := target{pkg: f[0], goos: f[1], goarch: f[2]}
		if len(f) == 4 {
			t.goarm = f[3]
		}
		ts = append(ts, t)
	}
	return ts, nil
}

// goList 在 tree 下按目标的平台列出包闭包。CGO_ENABLED=0 与 GOWORK=off 和发布构建同一口径：cgo 文件不参与
// （agent 产物全是静态构建），也不受外层 go.work 影响。输出是 JSON 对象流，逐个解码。
func goList(tree string, t target) ([]listedPackage, error) {
	cmd := exec.Command("go", "list", "-deps", "-json="+goListFields, t.pkg)
	cmd.Dir = tree
	env := append(os.Environ(), "GOOS="+t.goos, "GOARCH="+t.goarch, "CGO_ENABLED=0", "GOWORK=off")
	if t.goarm != "" {
		env = append(env, "GOARM="+t.goarm)
	}
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("go list %s (%s/%s) in %s: %v: %s", t.pkg, t.goos, t.goarch, tree, err, strings.TrimSpace(stderr.String()))
	}
	var pkgs []listedPackage
	dec := json.NewDecoder(&stdout)
	for {
		var p listedPackage
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("go list %s in %s: %v", t.pkg, tree, err)
		}
		pkgs = append(pkgs, p)
	}
	return pkgs, nil
}

// keepReferencedGenerated 把生成文件按 proto 粒度筛进输入。生成包把 AdminService 与 AgentService 放在同一个
// Go 包里，按包算会把每个只动 admin.proto 的 hub 功能都判成 agent 改动，拆分就失去意义；而按"agent 代码引用
// 了哪些生成标识符"取根、再沿 proto import 展开，覆盖了 agent 经消息嵌套间接依赖的文件（agent.proto 引用
// types.proto 里的消息）。
func keepReferencedGenerated(tree string, in *inputs, pkgs map[string]*pkgFiles) error {
	protoByFile := map[string]string{} // 生成文件 → 它的 proto 源（buf 模块内的相对路径）
	for _, info := range pkgs {
		for rel := range info.files {
			if _, done := protoByFile[rel]; done {
				continue
			}
			proto, ok, err := generatedSource(filepath.Join(tree, rel))
			if err != nil {
				return err
			}
			if ok {
				protoByFile[rel] = proto
			}
		}
	}
	if len(protoByFile) == 0 {
		return nil
	}
	// generatedPkgs：含生成文件的包，导入路径 → 包名（无别名的 import 按包名引用它）。
	generatedPkgs := map[string]string{}
	for path, info := range pkgs {
		for rel := range info.files {
			if _, ok := protoByFile[rel]; ok {
				generatedPkgs[path] = info.name
				break
			}
		}
	}
	var nonGen []string
	for rel := range in.files {
		if strings.HasSuffix(rel, ".go") {
			if _, ok := protoByFile[rel]; !ok {
				nonGen = append(nonGen, rel)
			}
		}
	}
	slices.Sort(nonGen)
	refs, err := referencedNames(tree, nonGen, generatedPkgs)
	if err != nil {
		return err
	}
	// （导入路径，名字）→ 声明它的生成文件。名字声明在同包非生成文件里的不进表：那个文件本身已是输入。
	table := map[reference][]string{}
	for rel := range protoByFile {
		names, err := declaredNames(filepath.Join(tree, rel))
		if err != nil {
			return fmt.Errorf("parsing generated %s: %w", rel, err)
		}
		pkg := ""
		for path, info := range pkgs {
			if info.files[rel] {
				pkg = path
				break
			}
		}
		for _, n := range names {
			table[reference{pkg: pkg, name: n}] = append(table[reference{pkg: pkg, name: n}], rel)
		}
	}
	roots := map[string]bool{}
	for ref := range refs {
		for _, rel := range table[ref] {
			roots[protoByFile[rel]] = true
		}
	}
	rootList := make([]string, 0, len(roots))
	for p := range roots {
		rootList = append(rootList, p)
	}
	slices.Sort(rootList)
	closure, err := protoClosure(tree, rootList)
	if err != nil {
		return err
	}
	for rel, proto := range protoByFile {
		if !closure[proto] {
			delete(in.files, rel)
		}
	}
	return nil
}

// reference 是非生成代码对生成标识符的一次引用：生成包的导入路径加被引用的顶层名字。
type reference struct{ pkg, name string }

// referencedNames 解析非生成的 Go 文件，收集它们对生成包标识符的引用：import 了含生成文件的包的文件里，
// X.Sel 形式的选择子中 X 是该包本地名的那些 Sel。本地名取 import 的别名，没有别名取该包的 Name；别名为 _
// 的只跑 init，没有可解析的引用；别名为 . 的无法从选择子知道引用了什么，报错停下而不是猜。
func referencedNames(tree string, files []string, generatedPkgs map[string]string) (map[reference]bool, error) {
	refs := map[reference]bool{}
	for _, rel := range files {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, filepath.Join(tree, rel), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", rel, err)
		}
		local := map[string]string{} // import 的本地名 → 导入路径
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return nil, fmt.Errorf("parsing %s: bad import path %s: %w", rel, imp.Path.Value, err)
			}
			pkgName, generated := generatedPkgs[path]
			if !generated {
				continue
			}
			name := ""
			if imp.Name != nil {
				name = imp.Name.Name
			}
			switch name {
			case "":
				local[pkgName] = path
			case "_":
			case ".":
				return nil, fmt.Errorf("%s: dot import of generated package %s: the referenced identifiers cannot be resolved; import it with a name", rel, path)
			default:
				local[name] = path
			}
		}
		if len(local) == 0 {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok {
					if path, ok := local[id.Name]; ok {
						refs[reference{pkg: path, name: sel.Sel.Name}] = true
					}
				}
			}
			return true
		})
	}
	return refs, nil
}

// declaredNames 列出生成文件里的顶层声明名：无接收者的函数与 type、var、const 的名字。方法不列：包名选择子
// X.Sel 引用不到方法。
func declaredNames(path string) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil {
				names = append(names, d.Name.Name)
			}
		case *ast.GenDecl:
			for _, s := range d.Specs {
				switch s := s.(type) {
				case *ast.TypeSpec:
					names = append(names, s.Name.Name)
				case *ast.ValueSpec:
					for _, n := range s.Names {
						names = append(names, n.Name)
					}
				}
			}
		}
	}
	return names, nil
}

// generatedSource 判定一个 Go 文件是不是 protoc 生成物并取它的 proto 源。判定看文件本身而不是路径或包：
// 开头的 protoc-gen-go / protoc-gen-connect-go 生成标记，加上对应格式的 source 行（两种生成器的注释大小写
// 不同）。只读开头 4 KiB：生成标记与 source 行都在文件头部，不值得为判定读整个（可能很大的）文件。
func generatedSource(path string) (proto string, ok bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	b := make([]byte, 4096)
	n, err := io.ReadFull(f, b)
	if err != nil && err != io.ErrUnexpectedEOF {
		return "", false, err
	}
	genGo, genConnect := false, false
	for _, line := range strings.Split(string(b[:n]), "\n") {
		t := strings.TrimSpace(line)
		switch t {
		case "// Code generated by protoc-gen-go. DO NOT EDIT.":
			genGo = true
		case "// Code generated by protoc-gen-connect-go. DO NOT EDIT.":
			genConnect = true
		}
		if p, left := strings.CutPrefix(t, "// source: "); left && genGo {
			proto = strings.TrimSpace(p)
		}
		if p, left := strings.CutPrefix(t, "// Source: "); left && genConnect {
			proto = strings.TrimSpace(p)
		}
	}
	if !genGo && !genConnect {
		return "", false, nil
	}
	if proto == "" {
		return "", false, fmt.Errorf("%s carries a protoc generator header but no source comment", path)
	}
	return proto, true, nil
}

var protoImportRe = regexp.MustCompile(`^\s*import\s+(?:public\s+|weak\s+)?"([^"]+)"\s*;`)

// protoModules 逐行读 buf.yaml 里 modules 的每个 `- path: <目录>`。不引入 YAML 依赖：这里只需要这一个
// 字段，逐行前缀匹配足够，buf.yaml 也不允许同名目录写法有第二种拼法。
func protoModules(tree string) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(tree, "buf.yaml"))
	if err != nil {
		return nil, fmt.Errorf("reading buf.yaml in %s: %w", tree, err)
	}
	var dirs []string
	for _, line := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(line)
		if d, ok := strings.CutPrefix(t, "- path:"); ok {
			d = strings.Trim(strings.TrimSpace(d), `"'`)
			if d != "" {
				dirs = append(dirs, d)
			}
		}
	}
	if len(dirs) == 0 {
		return nil, fmt.Errorf("buf.yaml in %s declares no modules", tree)
	}
	return dirs, nil
}

// protoClosure 从根 proto 出发沿 import 递归展开闭包。import 的目标逐个在 buf 模块目录里找；找不到的
// （google/protobuf/… 的 well-known 类型）不展开——它们的内容由 google.golang.org/protobuf 模块版本承载，
// 已在模块输入里。public 与 weak import 同样是依赖，照常展开。
func protoClosure(tree string, roots []string) (map[string]bool, error) {
	closure := map[string]bool{}
	if len(roots) == 0 {
		return closure, nil
	}
	dirs, err := protoModules(tree)
	if err != nil {
		return nil, err
	}
	resolve := func(proto string) (string, bool) {
		for _, d := range dirs {
			p := filepath.Join(tree, d, filepath.FromSlash(proto))
			if _, err := os.Stat(p); err == nil {
				return p, true
			}
		}
		return "", false
	}
	var walk func(proto string) error
	walk = func(proto string) error {
		if closure[proto] {
			return nil
		}
		closure[proto] = true
		file, ok := resolve(proto)
		if !ok {
			return fmt.Errorf("proto %s (referenced from generated code) not found under any buf module in %s", proto, tree)
		}
		b, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(b), "\n") {
			if m := protoImportRe.FindStringSubmatch(line); m != nil {
				if _, ok := resolve(m[1]); ok {
					if err := walk(m[1]); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	for _, r := range roots {
		if err := walk(r); err != nil {
			return nil, err
		}
	}
	return closure, nil
}

// goModLines 取根 go.mod 里决定构建行为的指令行：go（编译器版本与主模块的语言版本，CI 用 go-version-file
// 取工具链）、toolchain 与 godebug（改变运行时默认行为）。godebug 块里的每一行都算。require、replace、
// exclude 不逐行比较：它们的效果由模块集合承载，否则任何只给 hub 加依赖的改动都会被判成 agent 改动。
func goModLines(tree string) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(tree, "go.mod"))
	if err != nil {
		return nil, fmt.Errorf("reading go.mod in %s: %w", tree, err)
	}
	var lines []string
	inGodebug := false
	for _, line := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(line)
		if inGodebug {
			switch {
			case t == ")":
				inGodebug = false
			case t != "" && !strings.HasPrefix(t, "//"):
				lines = append(lines, t)
			}
			continue
		}
		switch {
		case t == "godebug (":
			inGodebug = true
		case strings.HasPrefix(t, "godebug "), strings.HasPrefix(t, "go "), strings.HasPrefix(t, "toolchain "):
			if !strings.HasPrefix(t, "//") {
				lines = append(lines, t)
			}
		}
	}
	return lines, nil
}
