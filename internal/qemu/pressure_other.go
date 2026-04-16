//go:build !linux

package qemu

func sampleRAMPercent() float64 {
	return 100.0
}

func sampleDiskAvail(_ string) uint64 {
	return 1 << 40
}
