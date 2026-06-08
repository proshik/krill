package docker

import "testing"

func TestCPUPercent(t *testing.T) {
	cases := []struct {
		name                  string
		cpuDelta, systemDelta uint64
		onlineCPUs            uint32
		want                  float64
	}{
		{"half of one core on 2-cpu host", 50, 100, 2, 100},
		{"quarter on 4-cpu", 25, 100, 4, 100},
		{"idle (zero cpu delta)", 0, 100, 2, 0},
		{"zero system delta", 50, 0, 2, 0},
		{"onlineCPUs zero defaults to 1", 50, 100, 0, 50},
	}
	for _, tc := range cases {
		if got := cpuPercent(tc.cpuDelta, tc.systemDelta, tc.onlineCPUs); got != tc.want {
			t.Errorf("%s: cpuPercent(%d,%d,%d)=%v want %v", tc.name, tc.cpuDelta, tc.systemDelta, tc.onlineCPUs, got, tc.want)
		}
	}
}
