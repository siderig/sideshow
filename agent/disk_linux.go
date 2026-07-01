//go:build linux

package main

import "syscall"

// readDisk reports usage of the filesystem holding path. Used = total minus all
// free blocks; Free = blocks available to an unprivileged writer (Bavail, which
// already excludes the root-reserved slack), matching what `df` shows.
func readDisk(path string) DiskStats {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return DiskStats{}
	}
	bs := uint64(st.Bsize)
	total := st.Blocks * bs
	free := st.Bavail * bs
	used := total - st.Bfree*bs
	gb := func(b uint64) float64 { return round1(float64(b) / (1 << 30)) }
	var pct float64
	if total > 0 {
		pct = round1(float64(used) / float64(total) * 100)
	}
	return DiskStats{TotalGB: gb(total), UsedGB: gb(used), FreeGB: gb(free), Percent: pct}
}
