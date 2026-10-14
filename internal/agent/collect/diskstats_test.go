package collect

import (
	"io/fs"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/xjetry/heron/internal/clock"
)

// 真机 /proc/diskstats 采样：整盘 sda 与分区 sda1 同列，另有回环 loop0、映射 dm-0 与聚合 md0。
func TestParseDiskstatsCountsWholeDisksOnly(t *testing.T) {
	data, err := os.ReadFile("testdata/diskstats/sata.txt")
	if err != nil {
		t.Fatal(err)
	}
	// wholeDisk 代表 /sys/block 的成员判定：分区 sda1 没有这一项，整盘 sda 有。
	devs, err := parseDiskstats(strings.NewReader(string(data)), func(name string) bool { return name == "sda" })
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 1 || devs[0].name != "sda" {
		t.Fatalf("whole disks = %+v, want only sda", devs)
	}
	if devs[0].read != 4000000*512 || devs[0].write != 2000000*512 {
		t.Fatalf("sda bytes read/write = %d/%d, want %d/%d (sectors are always 512 bytes)",
			devs[0].read, devs[0].write, 4000000*512, 2000000*512)
	}
}

// ProcFS 层的整盘判定：nvme0n1p1 分区没有 /sys/block/nvme0n1p1，dm-0 由前缀排除，只有整盘 nvme0n1 入计。
func TestProcFSDiskCountersDropsPartitions(t *testing.T) {
	devs, err := plainProc(os.DirFS("testdata/docker-debian")).diskCounters()
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 1 || devs[0].name != "nvme0n1" {
		t.Fatalf("whole disks = %+v, want only nvme0n1 (nvme0n1p1 and dm-0 must be excluded)", devs)
	}
}

func TestParseDiskstatsNVMeOnlyWholeDisk(t *testing.T) {
	data, err := os.ReadFile("testdata/diskstats/nvme.txt")
	if err != nil {
		t.Fatal(err)
	}
	devs, err := parseDiskstats(strings.NewReader(string(data)), func(name string) bool { return name == "nvme0n1" })
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 1 || devs[0].name != "nvme0n1" || devs[0].read != 8000000*512 || devs[0].write != 4000000*512 {
		t.Fatalf("nvme whole disk = %+v", devs)
	}
}

// 前缀排除与 /sys/block 无关：即使调用方对每个名字都点头，loop/ram/zram/dm/md 都不进合计。
func TestParseDiskstatsExcludesMemoryAndMappedDevices(t *testing.T) {
	in := "   7 0 loop0 1 0 8 0 1 0 8 0 0 0 0\n" +
		"   1 0 ram0 1 0 8 0 1 0 8 0 0 0 0\n" +
		"   1 1 zram0 1 0 8 0 1 0 8 0 0 0 0\n" +
		" 253 0 dm-0 1 0 8 0 1 0 8 0 0 0 0\n" +
		"   9 0 md0 1 0 8 0 1 0 8 0 0 0 0\n" +
		"   8 0 sda 1 0 8 0 1 0 8 0 0 0 0\n"
	devs, err := parseDiskstats(strings.NewReader(in), func(string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 1 || devs[0].name != "sda" {
		t.Fatalf("devices = %+v, want only sda", devs)
	}
}

func diskMapFS(line string) fstest.MapFS {
	return fstest.MapFS{
		"proc/diskstats": {Data: []byte(line)},
		"sys/block/sda":  {Mode: fs.ModeDir},
	}
}

// 与 net_*_bps 同一差分规则：首样本无速率，两点之间才算出速率，扇区按 512 字节换算。
func TestDiskRateNeedsTwoSamples(t *testing.T) {
	clk := clock.NewFake(time.Unix(0, 0))
	fsys := diskMapFS("   8       0 sda 1000 20 40000 500 800 10 20000 300 0 200 400\n")
	c := &Collector{Host: plainProc(fsys), Clock: clk}
	if m, _ := c.Metrics(c.Identify()); m.DiskReadBps != nil || m.DiskWriteBps != nil {
		t.Fatalf("first sample must not carry a rate: read=%v write=%v", m.DiskReadBps, m.DiskWriteBps)
	}
	// 读 +2000 扇区、写 +1000 扇区 = 1024000 / 512000 字节，2 秒 → 512000 / 256000 B/s。
	fsys["proc/diskstats"] = &fstest.MapFile{Data: []byte("   8       0 sda 1010 20 42000 500 805 10 21000 300 0 200 400\n")}
	clk.Advance(2 * time.Second)
	m, _ := c.Metrics(c.Identify())
	if m.GetDiskReadBps() != 512000 || m.GetDiskWriteBps() != 256000 {
		t.Fatalf("read/write bps = %d/%d, want 512000/256000", m.GetDiskReadBps(), m.GetDiskWriteBps())
	}
}

// 计数回退时整次放弃：两项一起缺失，不截到 0——0 会伪装成"这一秒没有 I/O"。
func TestDiskRateDroppedOnCounterRegression(t *testing.T) {
	clk := clock.NewFake(time.Unix(0, 0))
	fsys := diskMapFS("   8       0 sda 1000 20 40000 500 800 10 20000 300 0 200 400\n")
	c := &Collector{Host: plainProc(fsys), Clock: clk}
	c.Metrics(c.Identify())
	// 读计数回退（40000 → 39000），写还在增长：两项都不能设置。
	fsys["proc/diskstats"] = &fstest.MapFile{Data: []byte("   8       0 sda 1010 20 39000 500 805 10 21000 300 0 200 400\n")}
	clk.Advance(2 * time.Second)
	m, _ := c.Metrics(c.Identify())
	if m.DiskReadBps != nil || m.DiskWriteBps != nil {
		t.Fatalf("counter regression fabricated a rate: read=%v write=%v", m.DiskReadBps, m.DiskWriteBps)
	}
}

// 设备集合变化（加了整盘 sdb）时只换基线，本次两项都不设置，不把新盘的既有计数当增量。
func TestDiskRateDroppedOnDeviceSetChange(t *testing.T) {
	clk := clock.NewFake(time.Unix(0, 0))
	fsys := diskMapFS("   8       0 sda 1000 20 40000 500 800 10 20000 300 0 200 400\n")
	c := &Collector{Host: plainProc(fsys), Clock: clk}
	c.Metrics(c.Identify())
	fsys["proc/diskstats"] = &fstest.MapFile{Data: []byte(
		"   8       0 sda 1010 20 42000 500 805 10 21000 300 0 200 400\n" +
			"   8      16 sdb 500 5 9000 100 400 4 8000 80 0 50 60\n")}
	fsys["sys/block/sdb"] = &fstest.MapFile{Mode: fs.ModeDir}
	clk.Advance(2 * time.Second)
	m, _ := c.Metrics(c.Identify())
	if m.DiskReadBps != nil || m.DiskWriteBps != nil {
		t.Fatalf("device set change fabricated a rate: read=%v write=%v", m.DiskReadBps, m.DiskWriteBps)
	}
}
