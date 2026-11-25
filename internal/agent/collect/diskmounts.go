package collect

import (
	"cmp"
	"errors"
	"fmt"
	"slices"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/agentwire"
)

// minDiskMountBytes 是纳入监测的本地数据盘的容量下限：更小的分区（/boot、恢复分区、各种保留卷）写满不是容量问题，
// 列进来只会挤掉真正的数据盘。根 "/" 不受此限：根盘用量本来就按根报。
const minDiskMountBytes = 256 << 20

// mountEntry 是平台给出的一个挂载点，路径已换成被观测主机上的路径。平台只给出按路径打开时真正落在该挂载上的项
// （被盖住的不给），并按自己的规则判定 eligible；去重、排除规则、容量下限与排序在 Collector.diskMounts 里一次实现。
type mountEntry struct {
	path string
	// devKey 标识同一个文件系统实例（Linux 的 major:minor、darwin 的 fsid），poolKey 标识共享容量的存储池或块设备
	// （ZFS 池、APFS 容器、/dev 下的设备路径），空表示没有。任一相同的两项共享容量，只报一项。
	devKey, poolKey string
	// eligible：根总是合格；其余要求本地持久化文件系统、文件系统本身可写、路径可上报（agentwire.ValidateDiskMountPath），
	// 且从根到它的路径不穿过远端挂载（打开路径本身就可能等远端应答）。
	eligible bool
}

// errMountGone 是挂载点在列出之后已经不在（路径不存在）；errMountNotDir 是挂载点不是目录（容器里逐文件绑定的
// /etc/hosts 之类），不是一块盘。两者都让 Collector 换同组的下一个路径，不算采集失败。
var (
	errMountGone   = errors.New("mount point no longer exists")
	errMountNotDir = errors.New("mount point is not a directory")
)

// diskMounts 给出本周期的本地数据盘逐盘容量：按 agentwire.CompareDiskUsage 排序后的前 MaxDiskMounts 项与截断前的总数。
//
// 根 "/" 总是一项，读数就是本周期的根盘用量（root），与 Metrics.disk_total / disk_used 同一次 statfs；根读不出时
// 整个逐盘读数缺失——少了根的列表会被当成"根以外的盘都正常"。其余合格项按 devKey 或 poolKey 归组（任一相同即同组），
// 每组只报一项：根所在的组报根，其余组按路径由短到长（同长按字节序）取第一个打得开的目录。
//
// 排除规则作用在"这一组将以哪个路径上报"上：轮到的路径命中排除规则即整组不报，不再试同组更长的路径——面板上看到
// /backup、排除 /backup 之后它不会换个名字回来；被排除的路径也不会被 statfs。成员读不出（权限、I/O 错误）时
// 整个逐盘读数缺失并点名路径，按 Host 对合计型读数的约定处理：漏掉的那块可能正是最满的；修正办法是把它加进排除规则。
//
// 列出与打开之间被卸载、而挂载点目录仍在时，这一次打开落在下层文件系统上，读数是下层的；下一周期它就不在表里了。
func (c *Collector) diskMounts(snap *execSnapshot, root usage, rootErr error) ([]*heronv1.DiskMountUsage, uint32, error) {
	if rootErr != nil {
		return nil, 0, fmt.Errorf("root: %w", rootErr)
	}
	entries, err := c.Host.diskMounts(snap)
	if err != nil {
		return nil, 0, err
	}
	var out []*heronv1.DiskMountUsage
	for _, group := range groupMounts(entries) {
		d, err := c.pickMount(group, root)
		if err != nil {
			return nil, 0, err
		}
		if d != nil {
			out = append(out, d)
		}
	}
	slices.SortFunc(out, agentwire.CompareDiskUsage)
	return out[:min(len(out), agentwire.MaxDiskMounts)], uint32(len(out)), nil
}

// pickMount 取一组的上报项，规则见 diskMounts；整组不报时返回 nil。
func (c *Collector) pickMount(paths []string, root usage) (*heronv1.DiskMountUsage, error) {
	for _, p := range paths {
		if p == "/" {
			if root.total == 0 {
				return nil, nil
			}
			return &heronv1.DiskMountUsage{Path: p, Total: root.total, Used: root.used}, nil
		}
		if matchAny(c.DiskExclude, p) {
			return nil, nil
		}
		u, err := checkUsage(c.Host.mountUsage(p))
		switch {
		case errors.Is(err, errMountGone), errors.Is(err, errMountNotDir):
			continue
		case err != nil:
			return nil, fmt.Errorf("%s: %w", p, err)
		case u.total < minDiskMountBytes:
			return nil, nil
		}
		return &heronv1.DiskMountUsage{Path: p, Total: u.total, Used: u.used}, nil
	}
	return nil, nil
}

// groupMounts 把根与合格项按共享容量归组，每组的路径按上报优先级排好："/" 最先，其次路径短的，同长按字节序。
// 平台没给出根的挂载项时（挂载表里找不到宿主根）仍补一个不带键的根：根盘用量与挂载表无关，总要报。
func groupMounts(entries []mountEntry) [][]string {
	var cands []mountEntry
	hasRoot := false
	for _, e := range entries {
		if e.eligible {
			cands = append(cands, e)
			hasRoot = hasRoot || e.path == "/"
		}
	}
	if !hasRoot {
		cands = append(cands, mountEntry{path: "/", eligible: true})
	}
	parent := make([]int, len(cands))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		if parent[i] != i {
			parent[i] = find(parent[i])
		}
		return parent[i]
	}
	owner := map[string]int{}
	for i, e := range cands {
		for _, k := range []string{e.devKey, e.poolKey} {
			if k == "" {
				continue
			}
			if j, ok := owner[k]; ok {
				parent[find(i)] = find(j)
			} else {
				owner[k] = i
			}
		}
	}
	byRoot := map[int][]string{}
	var order []int
	for i, e := range cands {
		r := find(i)
		if _, seen := byRoot[r]; !seen {
			order = append(order, r)
		}
		if !slices.Contains(byRoot[r], e.path) {
			byRoot[r] = append(byRoot[r], e.path)
		}
	}
	groups := make([][]string, 0, len(order))
	for _, r := range order {
		g := byRoot[r]
		// "/" 是最短的绝对路径，按长度排就在最前。
		slices.SortFunc(g, func(a, b string) int { return cmp.Or(cmp.Compare(len(a), len(b)), cmp.Compare(a, b)) })
		groups = append(groups, g)
	}
	return groups
}

// DiskMounts 返回最近一次 Metrics 的逐盘容量与截断前的总数，供 ReportRequest.disk_mounts / disk_mounts_total；
// 逐盘采集失败时为 nil 与 0。与 Metrics 同一次采样，必须在同一周期的 Metrics 之后取。
func (c *Collector) DiskMounts() ([]*heronv1.DiskMountUsage, uint32) { return c.diskList, c.diskTotal }
