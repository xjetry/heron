// Package collecttest 给测试一个 collect.Root：内容来自一个 fs.FS（MapFS 或 testdata 目录），
// 系统调用层的三个查询（StatID、FSType、Usage）由函数注入。只被 _test.go 导入。
//
// 与生产的根对象一样拒绝不合法的名字（fs.ErrInvalid），并记下每一次经它的名字与 Close 次数，
// 测试据此检查采集只用相对根的合法名字、根有没有被关闭。
package collecttest

import (
	"io/fs"
	"strings"
	"sync"
	"testing/fstest"
)

// Root 的函数字段为 nil 时：StatIDFunc、FSTypeFunc 报 fs.ErrNotExist（识别据此按读不出处理），
// UsageFunc 报 0/0。构造后、交给采集之前改字段；采集期间只读。
type Root struct {
	FS         fs.FS
	StatIDFunc func(name string) (dev string, ino uint64, err error)
	FSTypeFunc func(name string) (uint64, error)
	UsageFunc  func() (total, used uint64, err error)

	mu     sync.Mutex
	names  []string
	closed int
}

// New 返回内容为 fsys、其余查询取缺省的 Root。
func New(fsys fs.FS) *Root { return &Root{FS: fsys} }

// EthernetDefaults 给夹具里只写了计数的网卡补上真实 sysfs 里每张网卡都有的 type 与 uevent，取普通以太网卡的值
// （type 1、没有 DEVTYPE、没有结构属性）。已经写了的不覆盖：测结构规则的夹具自己给出属性。
func EthernetDefaults(fsys fstest.MapFS) {
	for name := range fsys {
		rest, ok := strings.CutPrefix(name, "sys/class/net/")
		if !ok {
			continue
		}
		iface, tail, ok := strings.Cut(rest, "/")
		if !ok || !strings.HasPrefix(tail, "statistics/") {
			continue
		}
		dir := "sys/class/net/" + iface
		if _, has := fsys[dir+"/type"]; !has {
			fsys[dir+"/type"] = &fstest.MapFile{Data: []byte("1\n")}
		}
		if _, has := fsys[dir+"/uevent"]; !has {
			fsys[dir+"/uevent"] = &fstest.MapFile{Data: []byte("INTERFACE=" + iface + "\n")}
		}
	}
}

func (r *Root) record(op, name string) error {
	r.mu.Lock()
	r.names = append(r.names, name)
	r.mu.Unlock()
	if !fs.ValidPath(name) {
		return &fs.PathError{Op: op, Path: name, Err: fs.ErrInvalid}
	}
	return nil
}

func (r *Root) Open(name string) (fs.File, error) {
	if err := r.record("open", name); err != nil {
		return nil, err
	}
	return r.FS.Open(name)
}

func (r *Root) Stat(name string) (fs.FileInfo, error) {
	if err := r.record("stat", name); err != nil {
		return nil, err
	}
	return fs.Stat(r.FS, name)
}

func (r *Root) StatID(name string) (string, uint64, error) {
	if err := r.record("stat", name); err != nil {
		return "", 0, err
	}
	if r.StatIDFunc == nil {
		return "", 0, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
	}
	return r.StatIDFunc(name)
}

func (r *Root) FSType(name string) (uint64, error) {
	if err := r.record("statfs", name); err != nil {
		return 0, err
	}
	if r.FSTypeFunc == nil {
		return 0, &fs.PathError{Op: "statfs", Path: name, Err: fs.ErrNotExist}
	}
	return r.FSTypeFunc(name)
}

func (r *Root) Usage() (uint64, uint64, error) {
	if r.UsageFunc == nil {
		return 0, 0, nil
	}
	return r.UsageFunc()
}

func (r *Root) Close() error {
	r.mu.Lock()
	r.closed++
	r.mu.Unlock()
	return nil
}

// Names 返回至今经 Open、Stat、StatID、FSType 用过的全部名字，按调用顺序。
func (r *Root) Names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.names...)
}

// Closed 返回 Close 被调用的次数。
func (r *Root) Closed() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}
