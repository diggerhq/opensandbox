package worker

import "testing"

func TestScaleEventMemoryMB(t *testing.T) {
	tests := []struct {
		name     string
		memoryMB int
		want     int
	}{
		{name: "uses sandbox memory", memoryMB: 4096, want: 4096},
		{name: "defaults missing memory", memoryMB: 0, want: 1024},
		{name: "defaults invalid negative memory", memoryMB: -1, want: 1024},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := scaleEventMemoryMB(tt.memoryMB); got != tt.want {
				t.Fatalf("scaleEventMemoryMB(%d) = %d, want %d", tt.memoryMB, got, tt.want)
			}
		})
	}
}
