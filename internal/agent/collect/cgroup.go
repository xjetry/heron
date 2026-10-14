package collect

import (
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"time"
)

// 这里只留 cgroup2 文件的解析与环境路径的每周期读数。识别（挂载根、来源、范围决策）
// 在 execscope.go；旧版"每周期自己看一遍 cpu.max 定口径"的 cgroupCPU 已并入识别。

// parseCPUMax 折算 cpu.max 的配额核数："max 100000" 不限（ok=false、err=nil）；"400000
// 100000" 得 4；周期为 0 是格式错误（内核不给，挡住除零）。
func parseCPUMax(s string) (float64, bool, error) {
	f := strings.Fields(s)
	if len(f) != 2 {
		return 0, false, fmt.Errorf("cpu.max: %q", s)
	}
	period, err := strconv.ParseUint(f[1], 10, 64)
	if err != nil {
		return 0, false, err
	}
	if period == 0 {
		return 0, false, fmt.Errorf("cpu.max: period 0 in %q", s)
	}
	if f[0] == "max" {
		return 0, false, nil
	}
	quota, err := strconv.ParseUint(f[0], 10, 64)
	if err != nil {
		return 0, false, err
	}
	if quota == 0 {
		return 0, false, fmt.Errorf("cpu.max: quota 0 in %q", s)
	}
	return float64(quota) / float64(period), true, nil
}

// parseCPUSetCount 数 cpuset.cpus.effective 的核数："0-3,8" 得 5；空串与其他认不出的
// 写法一样按格式错误返回——空列表是内核不会写出的值，当它有效等于把坏限额当不限。
func parseCPUSetCount(s string) (int, error) {
	n := 0
	for _, part := range strings.Split(s, ",") {
		if part == "" {
			return 0, fmt.Errorf("cpuset: %q", s)
		}
		lo, hi, ranged := strings.Cut(part, "-")
		l, err := strconv.Atoi(strings.TrimSpace(lo))
		if err != nil {
			return 0, err
		}
		h := l
		if ranged {
			if h, err = strconv.Atoi(strings.TrimSpace(hi)); err != nil {
				return 0, err
			}
		}
		if h < l {
			return 0, fmt.Errorf("cpuset: %q", s)
		}
		n += h - l + 1
	}
	return n, nil
}

// parseCPUStatUsage 取 cpu.stat 的 usage_usec，环境路径的 CPU 差分读数。
func parseCPUStatUsage(s string) (uint64, error) {
	for _, line := range strings.Split(s, "\n") {
		k, v, ok := strings.Cut(line, " ")
		if ok && k == "usage_usec" {
			return strconv.ParseUint(strings.TrimSpace(v), 10, 64)
		}
	}
	return 0, errors.New("cpu.stat: no usage_usec")
}

// cgroupCPUPercent 由两次 cpu.stat 的 usage_usec 差分算环境 CPU 占比：
// Δusage / (有效核数 × Δt)，按核归一后天然不超过 100%；计数器回退或 Δt 非正即放弃。
func cgroupCPUPercent(prev, cur uint64, cores float64, dt time.Duration) (float64, bool) {
	if cores <= 0 || dt.Microseconds() <= 0 || cur < prev {
		return 0, false
	}
	// usage_usec 是微秒；按核归一：Δusage / (cores × Δt)。
	pct := 100 * float64(cur-prev) / (cores * float64(dt.Microseconds()))
	return min(pct, 100), true
}

// readCpusetCount 读 cpuset.cpus.effective：文件不存在是 fileMissing（cpuset 控制器
// 未下放，不算错误，有效核数回退 cpuinfo）；读错误或认不出是 fileBad（CPU 未知）。
func (p *ProcFS) readCpusetCount() (int, fileState) {
	s, err := readTrim(p.Root, "sys/fs/cgroup/cpuset.cpus.effective")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, fileMissing
		}
		return 0, fileBad
	}
	n, err := parseCPUSetCount(s)
	if err != nil {
		return 0, fileBad
	}
	return n, fileOK
}

// readCgroupCPUUsage 读挂载根 cpu.stat 的 usage_usec。识别已确认文件存在；这里读失败
// （控制器被摘、读到一半文件没了）按错误交回，本周期 CPU 缺读数。
func (p *ProcFS) readCgroupCPUUsage() (uint64, error) {
	s, err := readTrim(p.Root, "sys/fs/cgroup/cpu.stat")
	if err != nil {
		return 0, err
	}
	return parseCPUStatUsage(s)
}

// readCgroupMemory 取环境内存占用：
//
//	used = memory.current − (file − shmem) − slab_reclaimable
//
// 页缓存与可回收 slab 不算占用，tmpfs（shmem）算。GitHub Actions ubuntu-24.04、
// 内核 6.17.0-1022-azure 上，systemd-run scope（MemoryMax=512M）四步注入
// （匿名 200MiB、页缓存 0、tmpfs 100MiB、约 385MiB 的 dentry）对应 used 增量
// +204/−0.4/+99.5/+1.3 MiB。Docker 容器 --memory 512m 与 Incus 6.0.4 guest
// （Debian 12，lxcfs 6.0.4，limits.memory=512MiB，内核 7.0.14-orbstack）的同形快照
// 各自对照注入量，不共用这一组增量。
// current 与 memory.stat 是两次读，竞争让中间量回退（file < shmem、current < 页缓存、
// 余量 < slab）时不硬算：无符号回绕出一个像样的数比缺一次读数危险得多。
func (p *ProcFS) readCgroupMemory() (uint64, error) {
	cur, err := readUint(p.Root, "sys/fs/cgroup/memory.current")
	if err != nil {
		return 0, err
	}
	f, err := p.Root.Open("sys/fs/cgroup/memory.stat")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	st, err := parseMemstat(f)
	if err != nil {
		return 0, err
	}
	if st.file < st.shmem {
		return 0, errors.New("memory.stat: file < shmem")
	}
	cache := st.file - st.shmem
	if cur < cache || cur-cache < st.slabReclaimable {
		return 0, errors.New("memory.current < page cache + reclaimable slab")
	}
	return cur - cache - st.slabReclaimable, nil
}
