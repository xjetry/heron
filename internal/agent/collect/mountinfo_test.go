package collect

import (
	"slices"
	"strings"
	"testing"
)

func TestParseMountinfoDiskFields(t *testing.T) {
	text := "22 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw,errors=remount-ro\n" +
		"30 22 8:17 / /mnt/disk\\0401 rw,relatime shared:9 master:2 - xfs /dev/sdb1 rw\n" +
		"31 22 0:40 / /srv/back\\134slash rw - nfs4 host:/export rw,vers=4.2\n"
	got, err := parseMountinfo(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	want := []mountinfoEntry{
		{id: 22, parent: 1, dev: "8:1", mountPoint: "/", fsType: "ext4", source: "/dev/sda1", superRO: false},
		{id: 30, parent: 22, dev: "8:17", mountPoint: "/mnt/disk 1", fsType: "xfs", source: "/dev/sdb1"},
		{id: 31, parent: 22, dev: "0:40", mountPoint: `/srv/back\slash`, fsType: "nfs4", source: "host:/export"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	// 超级块只读看末列，挂载项自己的 ro（ProtectSystem=strict、容器的 :ro 绑定）不算。
	ro, err := parseMountinfo(strings.NewReader("40 22 8:33 / /recovery ro - ext4 /dev/sdc1 ro\n41 22 8:1 / /etc rw - ext4 /dev/sda1 rw\n42 22 8:1 / /usr ro - ext4 /dev/sda1 rw\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !ro[0].superRO || ro[1].superRO || ro[2].superRO {
		t.Fatalf("superRO: %+v", ro)
	}
	for _, bad := range []string{
		"22 1 8:1 / / rw shared:1 ext4 /dev/sda1 rw\n", // 缺分隔符
		"x 1 8:1 / / rw - ext4 /dev/sda1 rw\n",
		"22 1 8:1 / / rw -\n",
	} {
		if _, err := parseMountinfo(strings.NewReader(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func paths(es []mountEntry) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.path)
	}
	return out
}

func eligiblePaths(es []mountEntry) []string {
	var out []string
	for _, e := range es {
		if e.eligible {
			out = append(out, e.path)
		}
	}
	return out
}

// systemd 服务的私有挂载命名空间里：ProtectHome=yes 把空目录盖在 /home 上（独立分区的 /home 与其下的挂载一起看不见），
// ProtectSystem=strict 把各挂载重挂为只读（挂载项的 ro，超级块仍可写）。被盖住的挂载点按路径打开会落到盖在上面的文件系统，
// 不能拿那份 statfs 冒充被盖住的盘。
func TestLinuxMountEntriesVisibility(t *testing.T) {
	text := strings.Join([]string{
		"22 1 8:1 / / ro,relatime shared:1 - ext4 /dev/sda1 rw",
		"23 22 0:5 / /dev rw - devtmpfs udev rw",
		"24 22 0:22 / /proc rw - proc proc rw",
		"25 22 8:2 / /boot/efi ro - vfat /dev/sda2 rw",
		"26 22 8:3 / /home ro - ext4 /dev/sda3 rw",
		"27 26 8:4 / /home/alice/archive ro - ext4 /dev/sda4 rw",
		"28 22 8:17 / /data ro - xfs /dev/sdb1 rw",
		"29 22 0:50 / /mnt/nas rw - nfs4 nas:/vol rw",
		"30 29 8:33 / /mnt/nas/cache ro - ext4 /dev/sdc1 rw",
		"31 22 0:60 / /mnt/sshfs rw - fuse.sshfs me@h:/ rw",
		"32 22 8:49 / /media/iso ro - iso9660 /dev/sr0 ro",
		"33 22 8:65 / /recovery ro - ext4 /dev/sde1 ro",
		"34 22 0:30 / /tmp rw - tmpfs tmpfs rw",
		"35 34 8:1 /tmp/systemd-private-abc/tmp /tmp rw - ext4 /dev/sda1 rw",
		"36 26 0:31 /systemd/inaccessible/dir /home ro - tmpfs tmpfs rw",
		"37 22 0:32 / /srv/pool rw - zfs tank/pool rw",
		"38 22 7:0 / /snap/core/1 ro - squashfs /dev/loop0 ro",
	}, "\n") + "\n"
	infos, err := parseMountinfo(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	got := linuxMountEntries(infos, "/")
	// /tmp 被私有 /tmp 的绑定盖住，/home 与 /home/alice/archive 被不可访问目录盖住：都不出现。
	if want := []string{"/", "/dev", "/proc", "/boot/efi", "/data", "/mnt/nas", "/mnt/nas/cache", "/mnt/sshfs", "/media/iso", "/recovery", "/tmp", "/home", "/srv/pool", "/snap/core/1"}; !slices.Equal(paths(got), want) {
		t.Fatalf("visible %v\nwant %v", paths(got), want)
	}
	// vfat、iso9660、tmpfs、squashfs 不在白名单；nfs 与 fuse 不在；挂在 nfs 下的 ext4 要穿过远端目录才到得了；/recovery 超级块只读。
	// 私有 /tmp 是根文件系统的绑定，在这一层仍然合格，由选盘时按设备号与根去重。
	if want := []string{"/", "/data", "/tmp", "/srv/pool"}; !slices.Equal(eligiblePaths(got), want) {
		t.Fatalf("eligible %v, want %v", eligiblePaths(got), want)
	}
	byPath := map[string]mountEntry{}
	for _, e := range got {
		byPath[e.path] = e
	}
	if e := byPath["/"]; e.devKey != "dev:8:1" || e.poolKey != "src:/dev/sda1" {
		t.Errorf("root keys %+v", e)
	}
	if e := byPath["/srv/pool"]; e.poolKey != "zfs:tank" {
		t.Errorf("zfs pool key %+v", e)
	}
}

// --host-root：读者命名空间里宿主根挂在 /hostfs，宿主的挂载都在它下面；路径去掉前缀后是宿主机上的路径，
// 读者自己的根与 /proc 等不属于被观测主机。
func TestLinuxMountEntriesUnderHostRoot(t *testing.T) {
	text := strings.Join([]string{
		"500 400 0:90 / / rw - overlay overlay rw,lowerdir=/l",
		"501 500 0:91 / /proc rw - proc proc rw",
		"502 500 8:1 / /hostfs ro,relatime - ext4 /dev/sda1 rw",
		"503 502 8:17 / /hostfs/data ro,relatime - xfs /dev/sdb1 rw",
		"504 502 0:5 / /hostfs/dev ro - devtmpfs udev rw",
		"505 500 8:1 /var/lib/docker/containers/x/hosts /etc/hosts rw - ext4 /dev/sda1 rw",
	}, "\n") + "\n"
	infos, err := parseMountinfo(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	got := linuxMountEntries(infos, "/hostfs")
	if want := []string{"/", "/data", "/dev"}; !slices.Equal(paths(got), want) {
		t.Fatalf("mapped %v, want %v", paths(got), want)
	}
	if want := []string{"/", "/data"}; !slices.Equal(eligiblePaths(got), want) {
		t.Fatalf("eligible %v, want %v", eligiblePaths(got), want)
	}
	if got[0].devKey != "dev:8:1" {
		t.Fatalf("host root keys %+v", got[0])
	}
}

// 根挂载点总是纳入：容器的 overlay 根、超级块只读的根都不按白名单与只读规则排除。
func TestLinuxRootAlwaysEligible(t *testing.T) {
	infos, err := parseMountinfo(strings.NewReader("1 0 0:90 / / ro - overlay overlay ro\n"))
	if err != nil {
		t.Fatal(err)
	}
	got := linuxMountEntries(infos, "/")
	if len(got) != 1 || !got[0].eligible || got[0].path != "/" {
		t.Fatalf("%+v", got)
	}
}
