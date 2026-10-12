//go:build linux

package main

import (
	"log/slog"
	"strings"
)

// ParseInstanceID parses the systemd instance name (e.g. "1.2.3.4-24_eth0")
// returns (ipCIDR, dev)
// It handles the conversion of '-' to '/' for the CIDR part.
func ParseInstanceID(instance string) (string, string) {
	// Format: <IP-DASHED>[_<DEV>]
	parts := strings.SplitN(instance, "_", 2)
	ipDashed := parts[0]
	dev := ""
	if len(parts) > 1 {
		dev = parts[1]
	}

	// Convert 192.168.1.1-24 -> 192.168.1.1/24
	ipCIDR := strings.ReplaceAll(ipDashed, "-", "/")
	slog.Debug("ParseInstanceID: parsed", "instance", instance, "ip", ipCIDR, "dev", dev, "has_dev", len(parts) > 1)

	return ipCIDR, dev
}
