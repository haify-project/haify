package announce

import (
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/j-keck/arping"
)

// Announcer defines the interface for sending network announcements.
type Announcer interface {
	GratuitousArp(ip net.IP, ifaceName string) error
}

// RealAnnouncer implements Announcer using the arping library.
type RealAnnouncer struct{}

func (r *RealAnnouncer) GratuitousArp(ip net.IP, ifaceName string) error {
	start := time.Now()
	err := arping.GratuitousArpOverIfaceByName(ip, ifaceName)
	slog.Debug("RealAnnouncer.GratuitousArp: sent", "ip", ip, "dev", ifaceName, "error", err, "took", time.Since(start))
	return err
}

var announcer Announcer = &RealAnnouncer{}

// SetAnnouncer allows tests to inject a mock announcer.
func SetAnnouncer(a Announcer) {
	slog.Debug("SetAnnouncer: replacing announcer")
	announcer = a
}

// Announce handles GARP for IPv4. IPv6 currently returns an explicit error
// until Neighbor Advertisement support is implemented.
func Announce(ifaceName, ipStr string, count int) error {
	slog.Debug("Announce: start", "dev", ifaceName, "ip", ipStr, "count", count)
	ip, _, err := net.ParseCIDR(ipStr)
	if err != nil {
		slog.Debug("Announce: not a CIDR, trying a bare IP", "ip", ipStr, "error", err)
		ip = net.ParseIP(ipStr)
		if ip == nil {
			slog.Debug("Announce: invalid IP", "ip", ipStr)
			return fmt.Errorf("invalid IP %q", ipStr)
		}
	}

	if ip.To4() != nil {
		slog.Debug("Announce: IPv4, sending gratuitous ARP", "ip", ip, "dev", ifaceName)
		return announceIPv4(ifaceName, ip, count)
	}

	slog.Debug("Announce: IPv6 not supported", "ip", ip, "dev", ifaceName)
	return fmt.Errorf("IPv6 announcement for %s on %s is not implemented", ip.String(), ifaceName)
}

func announceIPv4(ifaceName string, ip net.IP, count int) error {
	slog.Info("Sending Gratuitous ARP", "ip", ip.String(), "dev", ifaceName, "count", count)
	for i := 0; i < count; i++ {
		if err := announcer.GratuitousArp(ip, ifaceName); err != nil {
			slog.Debug("announceIPv4: GARP failed", "ip", ip, "dev", ifaceName, "attempt", i+1, "error", err)
			return fmt.Errorf("GARP failed: %w", err)
		}
		slog.Debug("announceIPv4: GARP sent", "ip", ip, "dev", ifaceName, "attempt", i+1, "count", count)
		if i < count-1 {
			time.Sleep(10 * time.Millisecond) // Reduced sleep for tests
		}
	}
	slog.Debug("announceIPv4: done", "ip", ip, "dev", ifaceName, "count", count)
	return nil
}
