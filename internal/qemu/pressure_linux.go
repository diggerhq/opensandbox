//go:build linux

package qemu

import "golang.org/x/sys/unix"

func sampleRAMPercent() float64 {
	var info unix.Sysinfo_t
	if err := unix.Sysinfo(&info); err != nil {
		return 100.0
	}
	total := info.Totalram * uint64(info.Unit)
	avail := info.Freeram * uint64(info.Unit)
	if total == 0 {
		return 100.0
	}
	return float64(avail) / float64(total) * 100.0
}

func sampleDiskAvail(path string) uint64 {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 1 << 40
	}
	return stat.Bavail * uint64(stat.Bsize)
}
