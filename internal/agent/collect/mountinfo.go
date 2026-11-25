package collect

import (
	"bufio"
	"fmt"
	"io"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/xjetry/heron/internal/agentwire"
)

// mountinfoEntry 是 /proc/self/mountinfo 的一行（proc_pid_mountinfo(5)）里识别与选盘要用的字段。
// 识别每周期读一遍挂载表（ProcFS.identify），快照交给选盘，同一周期两者看到的是同一张表。
type mountinfoEntry struct {
	id, parent int
	dev        string // 第 3 列 major:minor：超级块的 s_dev，同一文件系统实例的所有挂载（含绑定、btrfs 子卷）相同
	mountPoint string // 读者挂载命名空间里的路径，已还原八进制转义
	fsType     string
	source     string
	// superRO 取分隔符之后的超级块选项：文件系统本身只读。挂载项自己的 ro 不算——systemd 的 ProtectSystem=strict 与
	// 容器的 -v …:ro 把可写的盘逐个重挂成只读挂载项，按挂载项判定会把它们全部排除。
	superRO bool
}

// parseMountinfo 解析 mountinfo 全文。行格式：
//
//	mountID parentID major:minor root mountpoint options [可选字段…] - fstype source superoptions
//
// 可选字段个数不定，以单独的 "-" 结束；路径与来源里的空格、制表、换行与反斜杠写成 \ooo 八进制。任一行认不出或
// 整表为空即整体报错：识别据此把来源全部按未知（宁可缺读数，不把某个文件猜成 procfs），选盘据此让逐盘读数缺失
// （漏一行可能正好漏掉盖在别的挂载上的那一层，可见性的判定就错了）。
func parseMountinfo(r io.Reader) ([]mountinfoEntry, error) {
	var out []mountinfoEntry
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		f := strings.Fields(line)
		sep := slices.Index(f, "-")
		if sep < 6 || len(f) < sep+4 {
			return nil, fmt.Errorf("mountinfo: malformed line %q", line)
		}
		id, err1 := strconv.Atoi(f[0])
		parent, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil || !strings.Contains(f[2], ":") {
			return nil, fmt.Errorf("mountinfo: malformed line %q", line)
		}
		out = append(out, mountinfoEntry{
			id: id, parent: parent, dev: f[2],
			mountPoint: unescapeMountinfo(f[4]),
			fsType:     f[sep+1],
			source:     unescapeMountinfo(f[sep+2]),
			superRO:    slices.Contains(strings.Split(f[sep+3], ","), "ro"),
		})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("mountinfo: empty")
	}
	return out, nil
}

// unescapeMountinfo 还原内核 mangle 的 \ooo；认不出的反斜杠原样保留。
func unescapeMountinfo(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// linuxLocalFS 是 Linux 上本地持久化文件系统的白名单。白名单而不是黑名单：内核与 FUSE 的文件系统类型不断增加，
// 新出现的类型默认不纳入；vfat/msdos（EFI 分区）、iso9660、squashfs、tmpfs、overlay、一切网络文件系统与 FUSE
// 因不在表里而排除。
var linuxLocalFS = map[string]bool{
	"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true, "zfs": true, "f2fs": true,
	"bcachefs": true, "jfs": true, "reiserfs": true, "nilfs2": true, "ntfs3": true,
}

// linuxRemoteFS 是路径解析穿过它的挂载根时可能等远端应答的类型：网络文件系统、集群文件系统、FUSE（"fuse" 前缀，
// 见 remoteFS）与 autofs（穿过即触发自动挂载）。挂在这些挂载之下的本地盘不纳入：打开它的路径本身就可能卡住采集。
var linuxRemoteFS = map[string]bool{
	"nfs": true, "nfs4": true, "cifs": true, "smb3": true, "smbfs": true, "ceph": true, "glusterfs": true,
	"9p": true, "afs": true, "lustre": true, "gpfs": true, "ocfs2": true, "gfs2": true, "autofs": true, "virtiofs": true,
}

func remoteFS(table map[string]bool, fsType string) bool {
	return table[fsType] || strings.HasPrefix(fsType, "fuse")
}

// linuxMountEntries 把读者命名空间的挂载表变成被观测主机上的候选挂载点，按 mountinfo 的顺序。
//
// rootDir 是被观测主机的根在读者命名空间里的路径（默认 "/"，容器里是 --host-root）：只取它与它之下的挂载，
// 路径去掉这个前缀。打开这些路径时由根对象在宿主根内解析（openat2 RESOLVE_IN_ROOT），所以挂载表必须是读者自己的——
// 读者命名空间里的路径才是它真正能打开的；宿主 PID 1 的挂载表描述的是另一个命名空间，其中的挂载在读者这边未必存在。
//
// 只返回按路径打开时真正落在该挂载上的项（visibleMounts）。eligible 由平台规则判定：根总是合格；其余要求
// 本地持久化文件系统、超级块可写、路径可上报、从根到它不穿过远端挂载。
func linuxMountEntries(infos []mountinfoEntry, rootDir string) []mountEntry {
	rootDir = path.Clean(rootDir)
	visible := visibleMounts(infos)
	var mapped []mountEntry
	var remote []string // 已映射的远端挂载路径，供穿越判定
	for i, m := range infos {
		if !visible[i] {
			continue
		}
		p, ok := underRoot(m.mountPoint, rootDir)
		if !ok {
			continue
		}
		e := mountEntry{path: p, devKey: "dev:" + m.dev, poolKey: linuxPoolKey(m)}
		e.eligible = p == "/" || linuxLocalFS[m.fsType] && !m.superRO
		if remoteFS(linuxRemoteFS, m.fsType) {
			remote = append(remote, p)
		}
		mapped = append(mapped, e)
	}
	for i := range mapped {
		if mapped[i].path != "/" && (underAny(mapped[i].path, remote) || agentwire.ValidateDiskMountPath(mapped[i].path) != nil) {
			mapped[i].eligible = false
		}
	}
	return mapped
}

// underRoot 把读者命名空间里的挂载路径换成被观测主机上的路径；不在 rootDir 之下时 ok 为假。
func underRoot(mp, rootDir string) (string, bool) {
	if rootDir == "/" {
		return mp, true
	}
	if mp == rootDir {
		return "/", true
	}
	rest, ok := strings.CutPrefix(mp, rootDir+"/")
	if !ok {
		return "", false
	}
	return "/" + rest, true
}

// underAny 判断 p 是否严格位于 ancestors 中某个路径之下。
func underAny(p string, ancestors []string) bool {
	return slices.ContainsFunc(ancestors, func(a string) bool { return strictlyUnder(p, a) })
}

// linuxPoolKey 是共享容量池的标识：ZFS 数据集共享所在池的空间，按池名（来源里第一个 "/" 之前）归并；/dev 下的块设备
// 按设备路径归并（btrfs 多个子卷挂载的来源是同一个分区）；其余没有可比的来源，只按设备号归并。
func linuxPoolKey(m mountinfoEntry) string {
	switch {
	case m.fsType == "zfs":
		pool, _, _ := strings.Cut(m.source, "/")
		return "zfs:" + pool
	case strings.HasPrefix(m.source, "/dev/"):
		return "src:" + m.source
	}
	return ""
}

// visibleMounts 给出按路径解析时真正到达的挂载：从命名空间的根（父挂载不在表里）沿父子关系往下走，
//   - 同一挂载点上后挂的一层（父是它、挂载点与它相同）盖住它：它自己与它下面的挂载都到不了，盖上去的那层接替它；
//   - 同一父挂载下，后挂在祖先路径上的兄弟（例如 systemd 在没有独立分区的 /home 上盖的空目录）挡住更深处先挂的兄弟。
//
// 被盖住的挂载按路径打开会落到盖在上面的文件系统，它的 statfs 不是这块盘的；所以选盘只看这里给出的项。
// 父子成环或父不可达（读不全的表）按到不了处理，不猜。
func visibleMounts(infos []mountinfoEntry) []bool {
	byID := make(map[int]int, len(infos))
	for i, m := range infos {
		byID[m.id] = i
	}
	children := map[int][]int{}
	for i, m := range infos {
		if p, ok := byID[m.parent]; ok && m.parent != m.id {
			children[p] = append(children[p], i)
		}
	}
	covered := func(i int) bool {
		for _, c := range children[i] {
			if infos[c].mountPoint == infos[i].mountPoint {
				return true
			}
		}
		return false
	}
	shadowedBySibling := func(i, parent int) bool {
		for _, s := range children[parent] {
			if s != i && strictlyUnder(infos[i].mountPoint, infos[s].mountPoint) {
				return true
			}
		}
		return false
	}
	const (
		unknown = iota
		visiting
		yes
		no
	)
	state := make([]int, len(infos))
	var reached func(i int) bool
	reached = func(i int) bool {
		switch state[i] {
		case yes:
			return true
		case no, visiting:
			return false
		}
		state[i] = visiting
		ok := true
		if p, has := byID[infos[i].parent]; has && infos[i].parent != infos[i].id {
			ok = reached(p) && (infos[i].mountPoint == infos[p].mountPoint || !covered(p)) && !shadowedBySibling(i, p)
		}
		state[i] = no
		if ok {
			state[i] = yes
		}
		return ok
	}
	out := make([]bool, len(infos))
	for i := range infos {
		out[i] = reached(i) && !covered(i)
	}
	return out
}

// strictlyUnder 判断 p 是否严格位于 ancestor 之下。
func strictlyUnder(p, ancestor string) bool {
	if ancestor == "/" {
		return p != "/"
	}
	return strings.HasPrefix(p, ancestor+"/")
}
