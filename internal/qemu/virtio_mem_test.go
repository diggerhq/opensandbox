package qemu

import "testing"

func TestAlignVirtioMemBlock(t *testing.T) {
	tests := []struct {
		name string
		in   int
		want int
	}{
		{"zero stays zero", 0, 0},
		{"negative stays zero", -10, 0},
		{"exactly one block", 128, 128},
		{"under one block rounds up", 1, 128},
		{"between blocks rounds up", 200, 256},
		{"exactly two blocks", 256, 256},
		{"non-aligned mid-range", 1000, 1024},
		{"aligned 1GB", 1024, 1024},
		{"7GB-ish — wake replug case", 7168, 7168},
		{"7GB-ish minus a hair", 7167, 7168},
		{"7GB-ish plus a hair", 7169, 7296},
		// 16GB ceiling — buildQEMUArgs uses 16384-base; test the boundary.
		{"16GB", 16384, 16384},
		{"16GB minus base 1024", 16384 - 1024, 15360},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := alignVirtioMemBlock(tc.in)
			if got != tc.want {
				t.Errorf("alignVirtioMemBlock(%d) = %d, want %d", tc.in, got, tc.want)
			}
			if got > 0 && got%virtioMemBlockSizeMB != 0 {
				t.Errorf("alignVirtioMemBlock(%d) = %d is not a multiple of block size %d",
					tc.in, got, virtioMemBlockSizeMB)
			}
			if tc.in > 0 && got < tc.in {
				t.Errorf("alignVirtioMemBlock(%d) = %d, must round up (>= input)", tc.in, got)
			}
		})
	}
}
