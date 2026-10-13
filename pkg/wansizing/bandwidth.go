package wansizing

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var bandwidthPattern = regexp.MustCompile(`^(\d+(?:\.\d+)?)\s*([a-z]*)$`)

// ParseBandwidth reads a link bandwidth into bytes per second. Bits are how
// links are sold ("100Mbit", "1Gbps", "500mbit/s"), bytes how disks are
// measured ("12MiB/s", "40MB/s"); both are accepted, and the unit is required,
// because a bare number is as likely to mean one as the other.
func ParseBandwidth(s string) (float64, error) {
	v := strings.ToLower(strings.TrimSpace(s))
	v = strings.TrimSuffix(v, "/s")
	v = strings.TrimSuffix(v, "ps")
	m := bandwidthPattern.FindStringSubmatch(v)
	if m == nil {
		return 0, fmt.Errorf("bandwidth %q: give a rate such as 100Mbit, 1Gbit or 12MiB/s", s)
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("bandwidth %q: give a rate above zero", s)
	}
	unit := m[2]
	bits := map[string]float64{"kbit": 1e3, "kb": 1e3, "mbit": 1e6, "mb": 1e6, "gbit": 1e9, "gb": 1e9, "tbit": 1e12}
	bytes := map[string]float64{"kib": 1 << 10, "mib": 1 << 20, "gib": 1 << 30, "kbyte": 1e3, "mbyte": 1e6, "gbyte": 1e9}
	// "MB/s" is bytes, but "Mbps" (with the ps trimmed: "mb") is bits. Only
	// the original spelling tells them apart, so look at it for an upper B.
	if strings.Contains(s, "B/s") || strings.Contains(s, "B/S") {
		bytes["kb"], bytes["mb"], bytes["gb"] = 1e3, 1e6, 1e9
		delete(bits, "kb")
		delete(bits, "mb")
		delete(bits, "gb")
	}
	if f, ok := bits[unit]; ok {
		return n * f / 8, nil
	}
	if f, ok := bytes[unit]; ok {
		return n * f, nil
	}
	return 0, fmt.Errorf("bandwidth %q: unknown unit %q; use Kbit, Mbit, Gbit, KiB/s, MiB/s, GiB/s or MB/s", s, m[2])
}
