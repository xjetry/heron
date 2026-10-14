//go:build linux || darwin

package collect

import "golang.org/x/sys/unix"

// statfsUsage：total = 全部块，used = 全部块 − 空闲块（含 root 保留）。
// Linux 上这就是 GNU df 的 Used。APFS 上各卷共享容器的空间，statfs 给出容器的块数与空闲块，
// used 是整个容器的占用，对应 diskutil apfs list 的 "Capacity In Use By Volumes"；df 对 / 给出的
// Used 只是系统卷自身，比这里小得多。按容器计，是因为 statfs 的空闲块本来就是容器的空闲块（各卷共享），
// used 与它配对才满足 used + free = total，面板上的占用率才与剩余空间一致。
func statfsUsage(st *unix.Statfs_t) (total, used uint64) {
	bs := uint64(st.Bsize)
	return st.Blocks * bs, (st.Blocks - st.Bfree) * bs
}
