package release

import (
	"strconv"
	"strings"
)

// ParseDiskUsed reads the percentage in use from `df -P <one path>` output.
// The agent's disk alert and `pilot doctor` both read the Pilot volume through
// it, so they agree on what counts as a reading.
//
// Anything but a header and exactly one filesystem line is rejected: a second
// line means the output describes more than one volume, and the figure could
// belong to the wrong one.
func ParseDiskUsed(out string) (int, bool) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		return 0, false
	}
	for _, f := range strings.Fields(lines[1]) {
		pct, ok := strings.CutSuffix(f, "%")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(pct)
		if err != nil {
			continue
		}
		if n < 0 || n > 100 {
			return 0, false
		}
		return n, true
	}
	return 0, false
}
