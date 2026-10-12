//go:build linux

package ip

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"syscall"
	"time"

	"github.com/vishvananda/netlink"
)

// NetlinkProvider defines the interface for interacting with the kernel.
// This allows us to mock netlink calls for unit testing.
type NetlinkProvider interface {
	LinkByName(name string) (netlink.Link, error)
	LinkList() ([]netlink.Link, error)
	AddrList(link netlink.Link, family int) ([]netlink.Addr, error)
	AddrAdd(link netlink.Link, addr *netlink.Addr) error
	AddrDel(link netlink.Link, addr *netlink.Addr) error
}

// RealNetlink is the actual implementation using the netlink library.
type RealNetlink struct{}

func (r *RealNetlink) LinkByName(name string) (netlink.Link, error) { return netlink.LinkByName(name) }
func (r *RealNetlink) LinkList() ([]netlink.Link, error)            { return netlink.LinkList() }
func (r *RealNetlink) AddrList(link netlink.Link, family int) ([]netlink.Addr, error) {
	return netlink.AddrList(link, family)
}
func (r *RealNetlink) AddrAdd(link netlink.Link, addr *netlink.Addr) error {
	return netlink.AddrAdd(link, addr)
}
func (r *RealNetlink) AddrDel(link netlink.Link, addr *netlink.Addr) error {
	return netlink.AddrDel(link, addr)
}

// provider holds the current implementation. Defaults to RealNetlink.
var provider NetlinkProvider = &RealNetlink{}

var errIPNotFound = errors.New("ip not found")

// SetProvider allows tests to inject a mock provider.
func SetProvider(p NetlinkProvider) {
	slog.Debug("SetProvider: replacing netlink provider")
	provider = p
}

// EnsureIP ensures that the given IP/CIDR is present on the interface.
func EnsureIP(ifaceName, ipCIDR string) (string, error) {
	slog.Debug("EnsureIP: start", "dev", ifaceName, "ip", ipCIDR)
	addr, err := netlink.ParseAddr(ipCIDR)
	if err != nil {
		slog.Debug("EnsureIP: invalid CIDR", "ip", ipCIDR, "error", err)
		return "", fmt.Errorf("invalid CIDR %q: %w", ipCIDR, err)
	}

	if ifaceName == "" {
		slog.Debug("EnsureIP: no device given, finding one on the subnet", "ip", ipCIDR)
		ifaceName, err = FindInterfaceForCIDR(addr.IPNet)
		if err != nil {
			slog.Debug("EnsureIP: no interface for subnet", "ip", ipCIDR, "error", err)
			return "", err
		}
		slog.Debug("EnsureIP: chose interface", "ip", ipCIDR, "dev", ifaceName)
	}

	start := time.Now()
	link, err := provider.LinkByName(ifaceName)
	slog.Debug("EnsureIP: LinkByName returned", "dev", ifaceName, "error", err, "took", time.Since(start))
	if err != nil {
		return "", fmt.Errorf("interface %q not found: %w", ifaceName, err)
	}

	// Ensure interface is UP
	if link.Attrs().Flags&net.FlagUp == 0 {
		slog.Debug("EnsureIP: interface is down", "dev", ifaceName)
		return "", fmt.Errorf("interface %q is DOWN, cannot add IP", ifaceName)
	}

	exists, err := hasIP(link, addr)
	if err != nil {
		slog.Debug("EnsureIP: checking existing addresses failed", "dev", ifaceName, "error", err)
		return "", err
	}

	if exists {
		slog.Debug("IP already present, skipping add", "ip", addr.IP.String(), "dev", ifaceName)
		return ifaceName, nil
	}

	start = time.Now()
	err = provider.AddrAdd(link, addr)
	slog.Debug("EnsureIP: AddrAdd returned", "ip", ipCIDR, "dev", ifaceName, "error", err, "took", time.Since(start))
	if err != nil {
		return "", fmt.Errorf("failed to add address %s to %s: %w", ipCIDR, ifaceName, err)
	}

	slog.Debug("EnsureIP: address added", "ip", ipCIDR, "dev", ifaceName)
	return ifaceName, nil
}

// RemoveIP ensures the IP is gone.
func RemoveIP(ifaceName, ipCIDR string) error {
	slog.Debug("RemoveIP: start", "dev", ifaceName, "ip", ipCIDR)
	addr, err := netlink.ParseAddr(ipCIDR)
	if err != nil {
		slog.Debug("RemoveIP: invalid CIDR", "ip", ipCIDR, "error", err)
		return fmt.Errorf("invalid CIDR %q: %w", ipCIDR, err)
	}

	if ifaceName == "" {
		slog.Debug("RemoveIP: no device given, finding the one holding the IP", "ip", ipCIDR)
		ifaceName, err = FindInterfaceHoldingIP(addr)
		if err != nil {
			if errors.Is(err, errIPNotFound) {
				slog.Debug("RemoveIP: IP on no interface, nothing to remove", "ip", ipCIDR)
				return nil
			}
			slog.Debug("RemoveIP: finding interface failed", "ip", ipCIDR, "error", err)
			return err
		}
		slog.Debug("RemoveIP: IP found on interface", "ip", ipCIDR, "dev", ifaceName)
	}

	start := time.Now()
	link, err := provider.LinkByName(ifaceName)
	slog.Debug("RemoveIP: LinkByName returned", "dev", ifaceName, "error", err, "took", time.Since(start))
	if err != nil {
		return fmt.Errorf("interface %q not found: %w", ifaceName, err)
	}

	exists, err := hasIP(link, addr)
	if err != nil {
		slog.Debug("RemoveIP: checking existing addresses failed", "dev", ifaceName, "error", err)
		return err
	}
	if !exists {
		slog.Debug("RemoveIP: IP not on interface, nothing to remove", "ip", ipCIDR, "dev", ifaceName)
		return nil
	}

	start = time.Now()
	err = provider.AddrDel(link, addr)
	slog.Debug("RemoveIP: AddrDel returned", "ip", ipCIDR, "dev", ifaceName, "error", err, "took", time.Since(start))
	if err != nil {
		if isAddrNotAvailable(err) {
			slog.Debug("RemoveIP: address already gone", "ip", ipCIDR, "dev", ifaceName)
			return nil
		}
		return fmt.Errorf("failed to delete address %s from %s: %w", ipCIDR, ifaceName, err)
	}

	slog.Debug("RemoveIP: address removed", "ip", ipCIDR, "dev", ifaceName)
	return nil
}

// CheckIP for monitor.
func CheckIP(ifaceName, ipCIDR string) (bool, error) {
	slog.Debug("CheckIP: start", "dev", ifaceName, "ip", ipCIDR)
	addr, err := netlink.ParseAddr(ipCIDR)
	if err != nil {
		slog.Debug("CheckIP: invalid CIDR", "ip", ipCIDR, "error", err)
		return false, err
	}

	if ifaceName != "" {
		start := time.Now()
		link, err := provider.LinkByName(ifaceName)
		slog.Debug("CheckIP: LinkByName returned", "dev", ifaceName, "error", err, "took", time.Since(start))
		if err != nil {
			return false, err
		}
		exists, err := hasIP(link, addr)
		slog.Debug("CheckIP: checked named interface", "ip", ipCIDR, "dev", ifaceName, "exists", exists, "error", err)
		return exists, err
	}

	start := time.Now()
	links, err := provider.LinkList()
	slog.Debug("CheckIP: LinkList returned", "count", len(links), "error", err, "took", time.Since(start))
	if err != nil {
		return false, err
	}
	var firstErr error
	for _, link := range links {
		exists, err := hasIP(link, addr)
		slog.Debug("CheckIP: checked interface", "ip", ipCIDR, "dev", link.Attrs().Name, "exists", exists, "error", err)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("failed to inspect interface %q: %w", link.Attrs().Name, err)
			}
			continue
		}
		if exists {
			return true, nil
		}
	}
	if firstErr != nil {
		slog.Debug("CheckIP: not found and an interface could not be inspected", "ip", ipCIDR, "error", firstErr)
		return false, firstErr
	}
	slog.Debug("CheckIP: IP on no interface", "ip", ipCIDR)
	return false, nil
}

func hasIP(link netlink.Link, target *netlink.Addr) (bool, error) {
	start := time.Now()
	addrs, err := provider.AddrList(link, familyForIP(target.IP))
	slog.Debug("hasIP: AddrList returned", "dev", link.Attrs().Name, "count", len(addrs), "error", err, "took", time.Since(start))
	if err != nil {
		return false, err
	}

	for _, addr := range addrs {
		if addr.IP.Equal(target.IP) && addr.Mask.String() == target.Mask.String() {
			slog.Debug("hasIP: address matches", "dev", link.Attrs().Name, "ip", target)
			return true, nil
		}
	}
	slog.Debug("hasIP: no matching address", "dev", link.Attrs().Name, "ip", target)
	return false, nil
}

// FindInterfaceForCIDR looks for an interface in the same subnet.
func FindInterfaceForCIDR(targetNet *net.IPNet) (string, error) {
	slog.Debug("FindInterfaceForCIDR: start", "subnet", targetNet)
	start := time.Now()
	links, err := provider.LinkList()
	slog.Debug("FindInterfaceForCIDR: LinkList returned", "count", len(links), "error", err, "took", time.Since(start))
	if err != nil {
		return "", err
	}

	var firstErr error
	for _, link := range links {
		if link.Attrs().Flags&net.FlagLoopback != 0 || link.Attrs().Flags&net.FlagUp == 0 {
			slog.Debug("FindInterfaceForCIDR: skipping loopback or down interface", "dev", link.Attrs().Name)
			continue
		}

		addrs, err := provider.AddrList(link, familyForIP(targetNet.IP))
		slog.Debug("FindInterfaceForCIDR: AddrList returned", "dev", link.Attrs().Name, "count", len(addrs), "error", err)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("failed to inspect interface %q: %w", link.Attrs().Name, err)
			}
			continue
		}

		for _, addr := range addrs {
			if !sameIPFamily(targetNet.IP, addr.IP) || addr.IPNet == nil {
				continue
			}
			if addr.Contains(targetNet.IP) {
				slog.Debug("FindInterfaceForCIDR: interface holds the subnet", "dev", link.Attrs().Name, "addr", addr.IPNet)
				return link.Attrs().Name, nil
			}
		}
	}
	if firstErr != nil {
		slog.Debug("FindInterfaceForCIDR: none found and an interface could not be inspected", "subnet", targetNet, "error", firstErr)
		return "", firstErr
	}
	slog.Debug("FindInterfaceForCIDR: no interface on the subnet", "subnet", targetNet)
	return "", fmt.Errorf("no interface found for subnet %s", targetNet.String())
}

// FindInterfaceHoldingIP finds which interface has this exact IP.
func FindInterfaceHoldingIP(target *netlink.Addr) (string, error) {
	slog.Debug("FindInterfaceHoldingIP: start", "ip", target)
	start := time.Now()
	links, err := provider.LinkList()
	slog.Debug("FindInterfaceHoldingIP: LinkList returned", "count", len(links), "error", err, "took", time.Since(start))
	if err != nil {
		return "", err
	}
	var firstErr error
	for _, link := range links {
		exists, err := hasIP(link, target)
		slog.Debug("FindInterfaceHoldingIP: checked interface", "dev", link.Attrs().Name, "exists", exists, "error", err)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("failed to inspect interface %q: %w", link.Attrs().Name, err)
			}
			continue
		}
		if exists {
			slog.Debug("FindInterfaceHoldingIP: found", "ip", target, "dev", link.Attrs().Name)
			return link.Attrs().Name, nil
		}
	}
	if firstErr != nil {
		slog.Debug("FindInterfaceHoldingIP: not found and an interface could not be inspected", "ip", target, "error", firstErr)
		return "", firstErr
	}
	slog.Debug("FindInterfaceHoldingIP: IP on no interface", "ip", target)
	return "", errIPNotFound
}

func familyForIP(ip net.IP) int {
	if ip.To4() != nil {
		return netlink.FAMILY_V4
	}
	if ip.To16() != nil {
		return netlink.FAMILY_V6
	}
	return netlink.FAMILY_ALL
}

func sameIPFamily(a, b net.IP) bool {
	if a == nil || b == nil {
		return false
	}
	return (a.To4() != nil) == (b.To4() != nil)
}

func isAddrNotAvailable(err error) bool {
	return errors.Is(err, syscall.EADDRNOTAVAIL)
}
