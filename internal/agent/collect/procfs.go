package collect

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"strings"
)

// linuxNetExclude 是 Linux 的名字规则：回环、容器与虚拟机的路由型端点（不挂网桥时结构上与真实上联口
// 分不出来）、ZeroTier 端口与 ifb 镜像口。网桥、bond、VLAN、隧道等由结构规则按 sysfs 判定，不靠名字。
var linuxNetExclude = []string{"lo", "docker*", "veth*", "br-*", "virbr*", "tap*", "cali*", "lxc*", "cilium_*", "zt*", "ifb*"}

// ProcFS 从 Linux 的 /proc 与 /sys 取原始读数。
//
// 两棵树、两种口径：Root 是被观测主机的根，除部署标识外的每一次读取都经它（容器里以 --host-root 给出
// 宿主根，否则是 "/"）；Reader 是本进程自己的根 "/"，只读描述读者所在环境的东西——部署标识
// （deployenv）与启动自检的网络命名空间身份。两者都是 Root：名字相对根，没有绝对路径。
// ProcFS 拥有两个根，close 关闭它们。
type ProcFS struct {
	Root   Root
	Reader Root
}

func (p *ProcFS) close() error { return errors.Join(p.Root.Close(), p.Reader.Close()) }

func (p *ProcFS) bootID() (string, error) { return readTrim(p.Root, "proc/sys/kernel/random/boot_id") }

func (p *ProcFS) cpuTimes() (cpuTimes, error) {
	f, err := p.Root.Open("proc/stat")
	if err != nil {
		return cpuTimes{}, err
	}
	defer f.Close()
	return parseStat(f)
}

func (p *ProcFS) meminfo() (memInfo, error) {
	f, err := p.Root.Open("proc/meminfo")
	if err != nil {
		return memInfo{}, err
	}
	defer f.Close()
	return parseMeminfo(f)
}

// memory 按 total − MemAvailable 计已用：页缓存与可回收的 slab 不算占用。
// MemAvailable 大于 MemTotal 时差值回绕，由 Collector 的 checkUsage 拒收。
func (p *ProcFS) memory() (usage, error) {
	mi, err := p.meminfo()
	if err != nil {
		return usage{}, err
	}
	return usage{total: mi.total, used: mi.total - mi.available}, nil
}

func (p *ProcFS) swap() (usage, error) {
	mi, err := p.meminfo()
	if err != nil {
		return usage{}, err
	}
	return usage{total: mi.swapTotal, used: mi.swapTotal - mi.swapFree}, nil
}

func (p *ProcFS) disk() (usage, error) {
	total, used, err := p.Root.Usage()
	return usage{total: total, used: used}, err
}

func (p *ProcFS) load() (loadAvg, error) {
	f, err := p.Root.Open("proc/loadavg")
	if err != nil {
		return loadAvg{}, err
	}
	defer f.Close()
	return parseLoadavg(f)
}

// procs 数 /proc 下名字全是数字的目录。/proc 的目录列表只列线程组（进程），线程只在
// /proc/<pid>/task 下列出，所以这是进程数，与 darwin 的 proc_listallpids 同口径。
// /proc/loadavg 第 4 字段的分母是含线程的调度实体数，而且在容器里是整个内核的数，不用。
//
// 列表里必须有 PID 1 的目录，否则按读不到处理。PID 1 在任何 pid 命名空间里都存在，列表里没有它就说明
// 列表不全：/proc 以 hidepid=2（invisible）挂载时，非 root 进程只列得出自己同 uid 的进程目录，数出来
// 是个位数而不报错（debian 容器里 remount 后以 nobody 实测：ls 只剩自己的两个 pid，stat /proc/1 报
// ENOENT）；读的不是 procfs 时通常也没有它。判据取自同一次 ReadDir 的条目，所以返回时 n ≥ 1。
// 报错经 Collector 进日志，点名 hidepid 供排查。
func (p *ProcFS) procs() (uint32, error) {
	entries, err := fs.ReadDir(p.Root, "proc")
	if err != nil {
		return 0, err
	}
	var n uint32
	hasPID1 := false
	for _, e := range entries {
		if e.IsDir() && isPID(e.Name()) {
			n++
			hasPID1 = hasPID1 || e.Name() == "1"
		}
	}
	if !hasPID1 {
		return 0, errors.New("proc/1 is not visible as a process directory, /proc is likely mounted with hidepid so other users' processes are hidden")
	}
	return n, nil
}

func isPID(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func (p *ProcFS) uptime() (uint64, error) {
	f, err := p.Root.Open("proc/uptime")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return parseUptime(f)
}

// conns 合计 IPv4 与 IPv6 两张表，按 Host 对合计型读数的约定：表不存在（如以 ipv6.disable=1
// 启动时没有 sockstat6）即不计；表存在却打不开、读不出或认不出 inuse 行时，整个读数返回 error。
func (p *ProcFS) conns() (uint32, uint32, error) {
	var tcp, udp uint32
	got := false
	for _, name := range []string{"proc/net/sockstat", "proc/net/sockstat6"} {
		f, err := p.Root.Open(name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, 0, err
		}
		t, u, err := parseSockstat(f)
		f.Close()
		if err != nil {
			return 0, 0, fmt.Errorf("%s: %w", name, err)
		}
		tcp, udp, got = tcp+t, udp+u, true
	}
	if !got {
		return 0, 0, fs.ErrNotExist
	}
	return tcp, udp, nil
}

// tcpStats 读 /proc/net/snmp。计数属于读者所在的网络命名空间，与 conns 的 sockstat 同一口径；
// IPv4 与 IPv6 计入同一组（内核 7.0.14-orbstack 的容器里，回环上各建一条 IPv6 与 IPv4 连接，Tcp: 的 ActiveOpens 与 PassiveOpens 各加 1 两次；/proc/net/snmp6 没有 TCP 段）。
func (p *ProcFS) tcpStats() (tcpCounters, error) {
	f, err := p.Root.Open("proc/net/snmp")
	if err != nil {
		return tcpCounters{}, err
	}
	defer f.Close()
	return parseSNMPTCP(f)
}

// ifaces 从 /sys/class/net/<if>/statistics 读计数器；每个网卡一对文件，比 /proc/net/dev 少一次整表解析。
// 按 Host 对合计型读数的约定：没有 statistics 目录的条目（bonding_masters 这类文件）不是网卡，不计；
// 是网卡而计数读不出时，整个读数返回 error。
func (p *ProcFS) ifaces() ([]ifaceCounters, error) {
	entries, err := fs.ReadDir(p.Root, "sys/class/net")
	if err != nil {
		return nil, err
	}
	var out []ifaceCounters
	for _, e := range entries {
		dir := "sys/class/net/" + e.Name()
		base := dir + "/statistics"
		// 真实 /sys 里网卡条目是指向设备目录的符号链接，DirEntry 的类型看不出是不是目录，按路径 Stat。
		// 普通文件下的子路径，os.DirFS 报 ENOTDIR 而不是 ENOENT，所以先看条目本身是不是目录。
		if st, err := fs.Stat(p.Root, dir); err == nil && !st.IsDir() {
			continue
		}
		st, err := fs.Stat(p.Root, base)
		switch {
		case errors.Is(err, fs.ErrNotExist), err == nil && !st.IsDir():
			continue
		case err != nil:
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		rx, err := readUint(p.Root, base+"/rx_bytes")
		var tx uint64
		if err == nil {
			tx, err = readUint(p.Root, base+"/tx_bytes")
		}
		if err != nil {
			// 列出之后网卡可能被删除（veth、tun 随容器与 VPN 来去），sysfs 目录随之消失。错误码认不全：
			// alpine 容器里删 dummy 网卡实测，删除后打开计数文件报 ENOENT，删除前已打开的文件读时报 ENODEV。
			// 所以按网卡目录还在不在认：不在了按不存在的成员不计；还在却读不出（EACCES 之类）则整个读数缺失。
			if _, serr := fs.Stat(p.Root, dir); errors.Is(serr, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		out = append(out, ifaceCounters{name: e.Name(), rx: rx, tx: tx})
	}
	return out, nil
}

// diskCounters 读 /proc/diskstats，用 /sys/block/<name> 是否存在判定整盘设备：
// 分区只有 /sys/block/<disk>/<part>，没有 /sys/block/<part>。名字前缀的排除见 isWholeDiskName。
func (p *ProcFS) diskCounters() ([]diskCounters, error) {
	f, err := p.Root.Open("proc/diskstats")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseDiskstats(f, func(name string) bool {
		_, err := fs.Stat(p.Root, "sys/block/"+name)
		return err == nil
	})
}

func (p *ProcFS) defaultNetExclude() []string { return linuxNetExclude }

func (p *ProcFS) hasNetStructure() bool { return true }

// netStructure 读 sys/class/net/<name> 的目录项（lower_*、brport、device）与 type、uevent 两个文件。网卡目录不在了
// 即已删除（errIfaceGone）；type、uevent 每张网卡都有，读不出时先看目录还在不在：不在按已删除，还在就是读不出。
func (p *ProcFS) netStructure(name string) (netStructure, error) {
	dir := "sys/class/net/" + name
	entries, err := fs.ReadDir(p.Root, dir)
	if errors.Is(err, fs.ErrNotExist) {
		return netStructure{}, errIfaceGone
	}
	if err != nil {
		return netStructure{}, err
	}
	var s netStructure
	for _, e := range entries {
		switch n := e.Name(); {
		case strings.HasPrefix(n, "lower_"):
			s.stacked = true
		case n == "brport":
			s.bridgePort = true
		case n == "device":
			s.hasDevice = true
		}
	}
	gone := func(err error) error {
		if _, serr := fs.Stat(p.Root, dir); errors.Is(serr, fs.ErrNotExist) {
			return errIfaceGone
		}
		return err
	}
	t, err := readUint(p.Root, dir+"/type")
	if err != nil {
		return netStructure{}, gone(err)
	}
	if t > math.MaxUint16 {
		return netStructure{}, fmt.Errorf("type %d is not an ARPHRD value", t)
	}
	s.arpType = uint16(t)
	b, err := fs.ReadFile(p.Root, dir+"/uevent")
	if err != nil {
		return netStructure{}, gone(err)
	}
	s.devType = parseUeventDevType(b)
	return s, nil
}

func (p *ProcFS) facts() hostFacts {
	var f hostFacts
	f.hostname, _ = readTrim(p.Root, "proc/sys/kernel/hostname")
	f.kernel, _ = readTrim(p.Root, "proc/sys/kernel/osrelease")
	if r, err := p.Root.Open("etc/os-release"); err == nil {
		f.os = parseOSRelease(r)
		r.Close()
	}
	if r, err := p.Root.Open("proc/cpuinfo"); err == nil {
		ci := parseCPUInfo(r)
		r.Close()
		f.cpuModel = ci.model
	}
	f.virtualization = detectVirtualization(p.Root)
	return f
}

func readUint(fsys fs.FS, name string) (uint64, error) {
	s, err := readTrim(fsys, name)
	if err != nil {
		return 0, err
	}
	var v uint64
	_, err = fmt.Sscan(s, &v)
	return v, err
}
