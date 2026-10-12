package controller

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"go.uber.org/zap"
)

// resolveToIP resolves a hostname to an IP address. If the input is already
// an IP address, it returns it unchanged. If resolution fails or returns a
// loopback address, it tries to find a non-loopback IP from network interfaces.
func resolveToIP(host string) string {
	log := zap.L()
	// Check if it's already an IP address
	if ip := net.ParseIP(host); ip != nil {
		if !ip.IsLoopback() {
			return host
		}
		// If it's a loopback IP, try to find a real IP
		log.Debug("resolveToIP: loopback address; using an interface address", zap.String("host", host))
		return getFirstNonLoopbackIP()
	}

	// Try to resolve hostname to IP
	start := time.Now()
	addrs, err := net.LookupHost(host)
	log.Debug("resolveToIP: looked up host", zap.String("host", host), zap.Strings("addresses", addrs),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil || len(addrs) == 0 {
		return getFirstNonLoopbackIP()
	}

	// Prefer non-loopback IPv4 addresses
	for _, addr := range addrs {
		ip := net.ParseIP(addr)
		if ip != nil && ip.To4() != nil && !ip.IsLoopback() {
			return addr
		}
	}

	// Fall back to first non-loopback address
	for _, addr := range addrs {
		ip := net.ParseIP(addr)
		if ip != nil && !ip.IsLoopback() {
			log.Debug("resolveToIP: no IPv4 address; using another", zap.String("host", host), zap.String("address", addr))
			return addr
		}
	}

	// If all resolved addresses are loopback, get IP from interfaces
	log.Debug("resolveToIP: every address is loopback; using an interface address", zap.String("host", host))
	return getFirstNonLoopbackIP()
}

// getFirstNonLoopbackIP returns the first non-loopback IPv4 address from network interfaces
func getFirstNonLoopbackIP() string {
	log := zap.L()
	interfaces, err := net.Interfaces()
	if err != nil {
		log.Debug("getFirstNonLoopbackIP: cannot list interfaces; using loopback", zap.Error(err))
		return "127.0.0.1"
	}

	for _, iface := range interfaces {
		// Skip loopback and down interfaces
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}

			if ip != nil && ip.To4() != nil && !ip.IsLoopback() {
				log.Debug("getFirstNonLoopbackIP: chose interface address", zap.String("interface", iface.Name), zap.String("address", ip.String()))
				return ip.String()
			}
		}
	}

	log.Debug("getFirstNonLoopbackIP: no usable interface; using loopback")
	return "127.0.0.1"
}

func localHostname() string {
	h, _ := os.Hostname()
	return h
}

// getNodeHost gets the host address for a node name
// hosts format is "nodename:ip" or just "nodename"
func (rm *ResourceManager) getNodeHost(nodeName string) string {
	log := rm.resourcesLog()
	if resolved := rm.controller.ResolveHost(nodeName); resolved != nodeName {
		log.Debug("getNodeHost: resolved through the registry", zap.String("node", nodeName), zap.String("host", resolved))
		return resolved
	}

	rm.mu.RLock()
	defer rm.mu.RUnlock()

	for _, host := range rm.hosts {
		// Check if host matches nodename:ip format
		if strings.Contains(host, ":") {
			parts := strings.SplitN(host, ":", 2)
			if parts[0] == nodeName {
				log.Debug("getNodeHost: matched configured host", zap.String("node", nodeName), zap.String("host", host))
				return host
			}
		} else if host == nodeName {
			log.Debug("getNodeHost: node is a configured host", zap.String("node", nodeName))
			return host
		}
	}
	log.Debug("getNodeHost: no host for node", zap.String("node", nodeName))
	return ""
}

// CreateFilesystemOnly creates a filesystem on a DRBD device
// primaryAddress returns the address of the node currently Primary for the
// resource, from live DRBD status.
func (rm *ResourceManager) primaryAddress(ctx context.Context, resource string) (string, error) {
	log := rm.resourcesLog()
	info, err := rm.GetResource(ctx, resource)
	if err != nil {
		log.Debug("primaryAddress: resource lookup failed", zap.String("resource", resource), zap.Error(err))
		return "", err
	}
	for addr, st := range info.NodeStates {
		if st.Role == "Primary" {
			log.Debug("primaryAddress: found Primary", zap.String("resource", resource), zap.String("host", addr))
			return addr, nil
		}
	}
	log.Debug("primaryAddress: no Primary", zap.String("resource", resource), zap.Int("nodes", len(info.NodeStates)))
	return "", fmt.Errorf("no Primary node for resource %q; specify a node explicitly", resource)
}

// resolveNodeOrPrimary resolves a node selection to a host address. An empty
// selection ("Auto (Primary)") resolves to the current Primary; if there is no
// Primary that is a hard error rather than a silent no-op on an empty host.
func (rm *ResourceManager) resolveNodeOrPrimary(ctx context.Context, resource, node string) (string, error) {
	log := rm.resourcesLog()
	if strings.TrimSpace(node) == "" {
		log.Debug("resolveNodeOrPrimary: no node given; using the Primary", zap.String("resource", resource))
		return rm.primaryAddress(ctx, resource)
	}
	addr := rm.controller.ResolveHost(node)
	if strings.TrimSpace(addr) == "" {
		log.Debug("resolveNodeOrPrimary: unknown node", zap.String("resource", resource), zap.String("node", node))
		return "", fmt.Errorf("unknown node %q", node)
	}
	log.Debug("resolveNodeOrPrimary: resolved", zap.String("resource", resource), zap.String("node", node), zap.String("host", addr))
	return addr, nil
}
