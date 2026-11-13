package checkflow

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// peakRSSBytes reads the process high-water resident set size (VmHWM) from
// /proc. This is the whole-process RSS the 128 MiB service budget refers to:
// it covers the HTTP layer, service, renderer, and check harness together.
func peakRSSBytes() (int64, error) {
	payload, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, fmt.Errorf("cannot read process status: %w", err)
	}
	for _, line := range strings.Split(string(payload), "\n") {
		if !strings.HasPrefix(line, "VmHWM:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[2] != "kB" {
			return 0, fmt.Errorf("unexpected VmHWM format %q", line)
		}
		value, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("cannot parse VmHWM value %q: %w", line, err)
		}
		return value * 1024, nil
	}
	return 0, fmt.Errorf("VmHWM not found in process status")
}
