package collect

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// diskSectorSize 是 /proc/diskstats sectors 字段的固定单位。内核一律按 512 字节计，
// 与设备自己报告的逻辑扇区大小无关（Documentation/admin-guide/iostats.rst），所以不按
// 设备查询扇区大小，直接乘 512。
const diskSectorSize = 512

// parseDiskstats 读 /proc/diskstats 的累计读/写字节，只保留 wholeDisk 认的整盘设备。
// 字段（从 1 起）：1 主号、2 次号、3 设备名、4 读完成数、5 读合并、6 读扇区、7 读毫秒、
// 8 写完成数、9 写合并、10 写扇区。取读扇区与写扇区，其余不用。
func parseDiskstats(r io.Reader, wholeDisk func(name string) bool) ([]diskCounters, error) {
	sc := bufio.NewScanner(r)
	var out []diskCounters
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 10 {
			return nil, fmt.Errorf("/proc/diskstats: line %q has %d fields, need at least 10", sc.Text(), len(f))
		}
		name := f[2]
		if !isWholeDiskName(name) || !wholeDisk(name) {
			continue
		}
		readSectors, err := strconv.ParseUint(f[5], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("/proc/diskstats: %s read sectors: %w", name, err)
		}
		writeSectors, err := strconv.ParseUint(f[9], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("/proc/diskstats: %s write sectors: %w", name, err)
		}
		out = append(out, diskCounters{name: name, read: readSectors * diskSectorSize, write: writeSectors * diskSectorSize})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// diskDeviceExcluded 是整盘判定的名字前缀：loop、ram、zram 是内存或回环设备，与物理 I/O 无关；
// dm-* 与 md* 是映射与聚合设备，其 I/O 与组成它的底层盘重复计数，计了会把同一次读算两遍。
var diskDeviceExcluded = []string{"loop", "ram", "zram", "dm-", "md"}

func isWholeDiskName(name string) bool {
	for _, prefix := range diskDeviceExcluded {
		if strings.HasPrefix(name, prefix) {
			return false
		}
	}
	return true
}

// errNoDiskCounters 供平台实现表示"本平台不提供磁盘计数器"：Collector 据此只让两项缺失，不记失败日志。
var errNoDiskCounters = errors.New("disk counters are not available on this platform")
