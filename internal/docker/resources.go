package docker

import (
	"errors"
	"math"
	"strconv"
	"strings"

	units "github.com/docker/go-units"
)

// ParseMemoryBytes parses a human memory string ("256m", "1g") into bytes
// (binary units). An empty string returns (0, nil), meaning "no limit".
func ParseMemoryBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	b, err := units.RAMInBytes(s)
	if err != nil {
		return 0, err
	}
	if b <= 0 {
		return 0, errors.New("memory must be positive")
	}
	return b, nil
}

// ParseNanoCPUs parses a CPU count ("0.5", "2") into nano-CPUs. An empty
// string returns (0, nil), meaning "no limit".
func ParseNanoCPUs(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, err
	}
	if f <= 0 {
		return 0, errors.New("cpu must be positive")
	}
	return int64(math.Round(f * 1e9)), nil
}
