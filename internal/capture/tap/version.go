package tap

import (
	"strconv"
	"strings"
)

// minMajor and minMinor are the first macOS with process taps (14.2).
const (
	minMajor = 14
	minMinor = 2
)

// osVersion is a parsed macOS product version.
type osVersion struct{ major, minor, patch int }

// parseOSVersion parses "15.8.2" (missing parts are zero). It reports
// false for anything that is not dotted integers.
func parseOSVersion(s string) (osVersion, bool) {
	parts := strings.Split(strings.TrimSpace(s), ".")
	if len(parts) == 0 || len(parts) > 3 {
		return osVersion{}, false
	}
	var nums [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return osVersion{}, false
		}
		nums[i] = n
	}
	return osVersion{nums[0], nums[1], nums[2]}, true
}

// supportsTaps reports whether v is macOS 14.2 or later.
func (v osVersion) supportsTaps() bool {
	return v.major > minMajor || (v.major == minMajor && v.minor >= minMinor)
}

func (v osVersion) String() string {
	return strconv.Itoa(v.major) + "." + strconv.Itoa(v.minor) + "." + strconv.Itoa(v.patch)
}
