package collect

import (
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/agentwire"
	"github.com/xjetry/heron/internal/clock"
	"google.golang.org/protobuf/proto"
)

const gib = uint64(1) << 30

// diskHost 构造一台宿主根即读者根的主机：挂载表与每个挂载路径的 statfs 由参数给出，根盘用量 100 GiB 用了 50 GiB。
// MountUsage 收到的名字记在返回的切片里，测试据此确认哪些路径根本没被打开。
func diskHost(t *testing.T, mountinfo []string, usages map[string]usage, errs map[string]error) (*Collector, *[]string) {
	t.Helper()
	fsys := fstest.MapFS{"proc/self/mountinfo": {Data: []byte(strings.Join(mountinfo, "\n") + "\n")}}
	p := hostProc(fsys, func() (uint64, uint64, error) { return 100 * gib, 50 * gib, nil })
	var opened []string
	fixtureOf(p).MountUsageFunc = func(name string) (uint64, uint64, error) {
		opened = append(opened, name)
		if err, ok := errs[name]; ok {
			return 0, 0, err
		}
		u, ok := usages[name]
		if !ok {
			return 0, 0, &fs.PathError{Op: "statfs", Path: name, Err: fs.ErrNotExist}
		}
		return u.total, u.used, nil
	}
	return &Collector{Host: p, Clock: clock.NewFake(time.Unix(0, 0))}, &opened
}

func mounts(c *Collector) ([]*heronv1.DiskMountUsage, uint32) {
	return c.DiskMounts()
}

func wantDisks(t *testing.T, c *Collector, wantTotal uint32, want ...*heronv1.DiskMountUsage) {
	t.Helper()
	list, total := mounts(c)
	if total != wantTotal || len(list) != len(want) {
		t.Fatalf("disk mounts = %v (total %d), want %v (total %d)", list, total, want, wantTotal)
	}
	for i := range want {
		if !proto.Equal(list[i], want[i]) {
			t.Fatalf("disk mounts[%d] = %v, want %v\nfull: %v", i, list[i], want[i], list)
		}
	}
	if err := agentwire.ValidateDiskMounts(list, total); err != nil {
		t.Fatalf("hub would reject the list: %v", err)
	}
}

func du(path string, total, used uint64) *heronv1.DiskMountUsage {
	return &heronv1.DiskMountUsage{Path: path, Total: total, Used: used}
}

// 一台典型主机：白名单、去重（设备号、/dev 来源、ZFS 池）、容量下限、排除规则、非目录挂载点、
// 列出后消失的挂载点，以及按使用率降序、同使用率按路径升序。
func TestDiskMountsSelection(t *testing.T) {
	c, opened := diskHost(t, []string{
		"22 1 8:1 / / rw - ext4 /dev/sda1 rw",
		"23 22 8:17 / /data rw - xfs /dev/sdb1 rw",
		"24 22 8:17 /sub /data2 rw - xfs /dev/sdb1 rw",
		"25 22 0:50 /@a /srv/a rw - btrfs /dev/sdc1 rw,subvol=/@a",
		"26 22 0:51 /@b /srv/b rw - btrfs /dev/sdc1 rw,subvol=/@b",
		"27 22 8:2 / /boot rw - ext4 /dev/sda2 rw",
		"28 22 0:60 / /mnt/nas rw - nfs4 nas:/v rw",
		"29 22 8:49 / /backup rw - ext4 /dev/sdd1 rw",
		"30 22 8:1 /tmp/x /tmp rw - ext4 /dev/sda1 rw",
		"31 22 0:70 / /pool/x rw - zfs tank/x rw",
		"32 22 0:71 / /pool rw - zfs tank rw",
		"33 22 8:65 /f /etc/hostname rw - ext4 /dev/sde1 rw",
		"34 22 8:81 / /gone rw - ext4 /dev/sdf1 rw",
		"35 22 8:97 / /media/usb rw - vfat /dev/sdg1 rw",
	}, map[string]usage{
		"data":   {1000 * gib, 900 * gib},
		"srv/a":  {10 * gib, 1 * gib},
		"boot":   {200 << 20, 100 << 20},
		"pool":   {50 * gib, 25 * gib},
		"pool/x": {40 * gib, 30 * gib},
	}, map[string]error{
		"etc/hostname": &fs.PathError{Op: "statfs", Path: "etc/hostname", Err: syscall.ENOTDIR},
	})
	c.DiskExclude = []string{"/back*"}
	// 夹具只有挂载表：其余来源的失败与这里无关，逐盘部分看诊断。
	c.Metrics(c.Identify())
	// 根与 /pool 都是 50%，按路径 "/" 在前；/tmp 是根文件系统的绑定，并进根；/data2 并进 /data；/srv/b 与 /srv/a 同一分区。
	wantDisks(t, c, 4, du("/data", 1000*gib, 900*gib), du("/", 100*gib, 50*gib), du("/pool", 50*gib, 25*gib), du("/srv/a", 10*gib, 1*gib))
	for _, never := range []string{"backup", "mnt/nas", "data2", "srv/b", "tmp", "pool/x", "media/usb"} {
		if slices.Contains(*opened, never) {
			t.Errorf("%s was opened; excluded, remote, ineligible and duplicate mounts must not be touched (opened %v)", never, *opened)
		}
	}
	d := c.Facts(c.Identify()).GetDiagnostics()
	if d.GetDiskMountsTotal() != 4 || !slices.Equal(d.GetDiskExclude(), []string{"/back*"}) || slices.Contains(d.GetFailedCollectors(), heronv1.CollectionComponent_COLLECTION_COMPONENT_DISK_MOUNTS) {
		t.Fatalf("diagnostics = %v", d)
	}
}

// 排除作用在"这一组以哪个路径上报"上：最短路径被排除时整组不报，不换同组更长的路径回来。
func TestDiskExcludeDropsTheWholeGroup(t *testing.T) {
	c, opened := diskHost(t, []string{
		"22 1 8:1 / / rw - ext4 /dev/sda1 rw",
		"23 22 8:17 / /bk rw - xfs /dev/sdb1 rw",
		"24 22 8:17 / /srv/backup rw - xfs /dev/sdb1 rw",
	}, map[string]usage{"bk": {10 * gib, 9 * gib}, "srv/backup": {10 * gib, 9 * gib}}, nil)
	c.DiskExclude = []string{"/bk"}
	c.Metrics(c.Identify())
	wantDisks(t, c, 1, du("/", 100*gib, 50*gib))
	if len(*opened) != 0 {
		t.Fatalf("opened %v", *opened)
	}
}

// 超过 16 项时只发最满的 16 项，总数是截断前的。
func TestDiskMountsTruncatedToFullest(t *testing.T) {
	lines := []string{"22 1 8:1 / / rw - ext4 /dev/sda1 rw"}
	usages := map[string]usage{}
	for i := range 20 {
		lines = append(lines, fmt.Sprintf("%d 22 9:%d / /d%02d rw - xfs /dev/md%d rw", 100+i, i, i, i))
		usages[fmt.Sprintf("d%02d", i)] = usage{100 * gib, uint64(i+1) * gib} // d19 最满（20%），根 50% 更满
	}
	c, _ := diskHost(t, lines, usages, nil)
	c.Metrics(c.Identify())
	list, total := mounts(c)
	if total != 21 || len(list) != agentwire.MaxDiskMounts {
		t.Fatalf("total %d len %d", total, len(list))
	}
	if list[0].GetPath() != "/" || list[1].GetPath() != "/d19" || list[15].GetPath() != "/d05" {
		t.Fatalf("not the fullest first: %v", list)
	}
	if err := agentwire.ValidateDiskMounts(list, total); err != nil {
		t.Fatal(err)
	}
}

// 合格挂载点读不出（权限、I/O 错误）时整个逐盘读数缺失并点名路径，不静默漏掉那块盘；排除它之后恢复。
func TestDiskMountUnreadableFailsTheReading(t *testing.T) {
	c, _ := diskHost(t, []string{
		"22 1 8:1 / / rw - ext4 /dev/sda1 rw",
		"23 22 8:17 / /data rw - xfs /dev/sdb1 rw",
	}, nil, map[string]error{"data": &fs.PathError{Op: "statfs", Path: "data", Err: syscall.EACCES}})
	_, err := c.Metrics(c.Identify())
	if err == nil || !strings.Contains(err.Error(), "disk_mounts: /data: ") {
		t.Fatalf("err = %v, want the failing mount named", err)
	}
	if list, total := mounts(c); list != nil || total != 0 {
		t.Fatalf("a failed reading must not report a partial list: %v %d", list, total)
	}
	d := c.Facts(c.Identify()).GetDiagnostics()
	if !slices.Contains(d.GetFailedCollectors(), heronv1.CollectionComponent_COLLECTION_COMPONENT_DISK_MOUNTS) || d.GetDiskMountsTotal() != 0 {
		t.Fatalf("diagnostics = %v", d)
	}
	if err := agentwire.ValidateDiagnostics(d); err != nil {
		t.Fatalf("hub would reject the diagnostics: %v", err)
	}
	c.DiskExclude = []string{"/data"}
	if _, err := c.Metrics(c.Identify()); err != nil && strings.Contains(err.Error(), "disk_mounts") {
		t.Fatal(err)
	}
	wantDisks(t, c, 1, du("/", 100*gib, 50*gib))
}

// 读者在容器的 cgroup 命名空间里、又没有显式宿主根：挂载表是容器的（卷、逐文件绑定），只报根，不打开任何挂载路径。
func TestDiskMountsRootOnlyInContainer(t *testing.T) {
	fsys := fstest.MapFS{"proc/self/mountinfo": {Data: []byte("1 0 0:90 / / rw - overlay overlay rw\n2 1 8:17 /vol /data rw - xfs /dev/sdb1 rw\n")}}
	p := envProc(fsys, func() (uint64, uint64, error) { return 100 * gib, 50 * gib, nil })
	called := false
	fixtureOf(p).MountUsageFunc = func(string) (uint64, uint64, error) { called = true; return 10 * gib, 9 * gib, nil }
	c := &Collector{Host: p, Clock: clock.NewFake(time.Unix(0, 0))}
	snap := c.Identify()
	if snap.exec.GetKind() != heronv1.ScopeKind_SCOPE_KIND_CGROUP_NAMESPACE {
		t.Fatalf("fixture is not a cgroup namespace: %v", snap.exec.GetKind())
	}
	c.Metrics(snap)
	wantDisks(t, c, 1, du("/", 100*gib, 50*gib))
	if called {
		t.Fatal("a container mount was opened")
	}
}

// 显式宿主根：即使读者在容器里也按挂载表选盘，只取宿主根之下的挂载，路径去掉宿主根前缀，在宿主根内打开。
func TestDiskMountsUnderExplicitHostRoot(t *testing.T) {
	fsys := fstest.MapFS{
		".dockerenv": {},
		"proc/self/mountinfo": {Data: []byte(strings.Join([]string{
			"500 400 0:90 / / rw - overlay overlay rw",
			"502 500 8:1 / /hostfs ro - ext4 /dev/sda1 rw",
			"503 502 8:17 / /hostfs/data ro - xfs /dev/sdb1 rw",
			"505 500 8:33 / /scratch rw - ext4 /dev/sdc1 rw",
		}, "\n") + "\n")},
	}
	p := hostProc(fsys, func() (uint64, uint64, error) { return 100 * gib, 50 * gib, nil })
	p.HostRoot = "/hostfs"
	var opened []string
	fixtureOf(p).MountUsageFunc = func(name string) (uint64, uint64, error) {
		opened = append(opened, name)
		return 10 * gib, 9 * gib, nil
	}
	c := &Collector{Host: p, Clock: clock.NewFake(time.Unix(0, 0))}
	snap := c.Identify()
	if !snap.readerInContainer {
		t.Fatal("fixture reader is not in a container; the explicit host root would not be what keeps the enumeration")
	}
	c.Metrics(snap)
	wantDisks(t, c, 2, du("/data", 10*gib, 9*gib), du("/", 100*gib, 50*gib))
	if !slices.Equal(opened, []string{"data"}) {
		t.Fatalf("opened %v, want only the host's data relative to the host root", opened)
	}
}

// 这台 macOS 的挂载表（26.3，Apple Silicon）：系统卷都带 nobrowse 且与根同在 disk3 容器；外接 APFS 盘的两个卷
// 共享容器 disk5，只报路径短的；网络共享与其下的本地卷都不纳入。
func TestDarwinDiskMounts(t *testing.T) {
	const nobrowse, local, rdonly = darwinMntDontBrowse, darwinMntLocal, darwinMntRdonly
	f := newFakeDarwin()
	f.fsstat = []fsstatRecord{
		{mntOn: "/", mntFrom: "/dev/disk3s1s1", fsType: "apfs", flags: local | rdonly, fsid: [2]int32{1, 26}},
		{mntOn: "/dev", mntFrom: "devfs", fsType: "devfs", flags: local, fsid: [2]int32{2, 27}},
		{mntOn: "/System/Volumes/VM", mntFrom: "/dev/disk3s6", fsType: "apfs", flags: local | nobrowse, fsid: [2]int32{3, 26}},
		{mntOn: "/System/Volumes/xarts", mntFrom: "/dev/disk1s2", fsType: "apfs", flags: local | nobrowse, fsid: [2]int32{4, 26}},
		{mntOn: "/System/Volumes/Data", mntFrom: "/dev/disk3s5", fsType: "apfs", flags: local | nobrowse, fsid: [2]int32{5, 26}},
		{mntOn: "/Users/me/OrbStack", mntFrom: "OrbStack:/OrbStack", fsType: "nfs", fsid: [2]int32{6, 30}},
		{mntOn: "/Volumes/Ext", mntFrom: "/dev/disk5s1", fsType: "apfs", flags: local, fsid: [2]int32{7, 26}},
		{mntOn: "/Volumes/Ext Photos", mntFrom: "/dev/disk5s2", fsType: "apfs", flags: local, fsid: [2]int32{8, 26}},
		{mntOn: "/Volumes/Share", mntFrom: "//me@nas/share", fsType: "smbfs", fsid: [2]int32{9, 31}},
		{mntOn: "/Volumes/Share/disk", mntFrom: "/dev/disk6s1", fsType: "apfs", flags: local, fsid: [2]int32{10, 26}},
		{mntOn: "/Volumes/Stick", mntFrom: "/dev/disk7s1", fsType: "msdos", flags: local, fsid: [2]int32{11, 28}},
		{mntOn: "/Volumes/Old", mntFrom: "/dev/disk8s2", fsType: "hfs", flags: local, fsid: [2]int32{12, 17}},
	}
	f.volumes = map[string]usage{
		"/Volumes/Ext":        {2000 * gib, 1500 * gib},
		"/Volumes/Ext Photos": {2000 * gib, 1500 * gib},
		"/Volumes/Old":        {500 * gib, 100 * gib},
		"/Volumes/Share/disk": {1 * gib, 1 * gib},
		// 系统卷读得出用量，不报是因为 nobrowse，不是因为读不出。
		"/System/Volumes/xarts": {500 << 20, 6 << 20},
	}
	c := &Collector{Host: &darwinHost{src: f}, Clock: clock.NewFake(time.Unix(0, 0))}
	c.Metrics(c.Identify())
	root := f.disk
	wantDisks(t, c, 3, du("/", root.total, root.used), du("/Volumes/Ext", 2000*gib, 1500*gib), du("/Volumes/Old", 500*gib, 100*gib))
}

func TestApfsContainer(t *testing.T) {
	for from, want := range map[string]string{
		"/dev/disk3s1s1": "/dev/disk3", "/dev/disk3s5": "/dev/disk3", "/dev/disk12s2": "/dev/disk12",
		"/dev/disk3": "", "/dev/rdisk3s1": "", "map auto_home": "", "/dev/diskXs1": "",
	} {
		if got := apfsContainer(from); got != want {
			t.Errorf("apfsContainer(%q) = %q, want %q", from, got, want)
		}
	}
}
