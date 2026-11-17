package csi

import (
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"sync"

	"go.uber.org/zap"
)

// The service addresses of shared volumes.
//
// Every shared volume is served by an NFS gateway of its own, and a gateway
// needs a floating address that moves with it. The StorageClass names the
// addresses the driver may hand out (nfsServiceIPs) and the driver gives each
// new volume one nobody holds.
//
// Which addresses are held is never remembered here. It is read from Haify
// each time: the address is written on the volume's resource as a label in
// the same call that creates the resource, so the resource itself is the
// reservation, and a controller plugin that restarts, or another replica of
// it taking over the Lease, sees exactly what the one before it gave out. The
// only thing kept in memory is the address of a volume whose resource is
// still being created, which no listing can show yet.

// ipRange is one entry of nfsServiceIPs: the addresses first..last, and the
// prefix length of the network they are in, which the gateway needs to bring
// the address up with the right netmask.
type ipRange struct {
	first, last netip.Addr
	bits        int
}

// errServiceIPsExhausted is returned when every address of the pool is held.
var errServiceIPsExhausted = errors.New("every address of nfsServiceIPs is in use")

// parseServiceIPPool reads nfsServiceIPs: a comma-separated list whose
// entries are one address ("192.168.1.200/24") or a range
// ("192.168.1.200-192.168.1.220/24"), each with the prefix length of the
// network.
//
// The prefix length is the network's, never the size of the pool: written as
// a block ("192.168.1.192/28") an entry could mean either sixteen addresses or
// one address in a /28, and guessing wrong brings every gateway up with a
// netmask its clients do not share. So an entry is always addresses first and
// the network's prefix length after.
func parseServiceIPPool(raw string) ([]ipRange, error) {
	log := zap.L()
	log.Debug("parseServiceIPPool: start", zap.String("pool", raw))
	var out []ipRange
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		addrs, bitsRaw, ok := strings.Cut(entry, "/")
		if !ok {
			log.Debug("parseServiceIPPool: entry without a prefix length", zap.String("entry", entry))
			return nil, fmt.Errorf("%s entry %q needs the network's prefix length, as in 192.168.1.200-192.168.1.220/24", paramNFSServiceIPs, entry)
		}
		firstRaw, lastRaw, isRange := strings.Cut(addrs, "-")
		first, err := netip.ParseAddr(strings.TrimSpace(firstRaw))
		if err != nil {
			log.Debug("parseServiceIPPool: invalid address", zap.String("entry", entry), zap.Error(err))
			return nil, fmt.Errorf("%s entry %q: %q is not an IP address", paramNFSServiceIPs, entry, firstRaw)
		}
		last := first
		if isRange {
			if last, err = netip.ParseAddr(strings.TrimSpace(lastRaw)); err != nil {
				log.Debug("parseServiceIPPool: invalid range end", zap.String("entry", entry), zap.Error(err))
				return nil, fmt.Errorf("%s entry %q: %q is not an IP address", paramNFSServiceIPs, entry, lastRaw)
			}
		}
		first, last = first.Unmap(), last.Unmap()
		bits, err := strconv.Atoi(strings.TrimSpace(bitsRaw))
		if err != nil || bits < 1 || bits > first.BitLen() {
			log.Debug("parseServiceIPPool: invalid prefix length", zap.String("entry", entry))
			return nil, fmt.Errorf("%s entry %q: %q is not a prefix length for that address", paramNFSServiceIPs, entry, bitsRaw)
		}
		network := netip.PrefixFrom(first, bits).Masked()
		switch {
		case first.Is4() != last.Is4():
			log.Debug("parseServiceIPPool: range mixes address families", zap.String("entry", entry))
			return nil, fmt.Errorf("%s entry %q mixes IPv4 and IPv6", paramNFSServiceIPs, entry)
		case last.Less(first):
			log.Debug("parseServiceIPPool: range runs backwards", zap.String("entry", entry))
			return nil, fmt.Errorf("%s entry %q: the range ends before it starts", paramNFSServiceIPs, entry)
		case !network.Contains(last):
			log.Debug("parseServiceIPPool: range leaves its network", zap.String("entry", entry), zap.Stringer("network", network))
			return nil, fmt.Errorf("%s entry %q: %s is outside the network %s", paramNFSServiceIPs, entry, last, network)
		}
		out = append(out, ipRange{first: first, last: last, bits: bits})
	}
	if len(out) == 0 {
		log.Debug("parseServiceIPPool: no entries")
		return nil, fmt.Errorf("%s is empty", paramNFSServiceIPs)
	}
	log.Debug("parseServiceIPPool: parsed", zap.Int("ranges", len(out)))
	return out, nil
}

// usable reports whether addr can be a host address in r's network: the
// network address and, for IPv4, the broadcast address of a network that has
// them cannot, and a range written to its edges would otherwise hand them out.
func (r ipRange) usable(addr netip.Addr) bool {
	if !addr.Is4() || r.bits >= 31 {
		return true
	}
	network := netip.PrefixFrom(addr, r.bits).Masked()
	if addr == network.Addr() {
		return false
	}
	host := addr.As4()
	for i := r.bits; i < 32; i++ {
		if host[i/8]&(1<<(7-i%8)) == 0 {
			return true
		}
	}
	return false // every host bit set: the broadcast address
}

// serviceIPAllocator hands out pool addresses to volumes being created.
type serviceIPAllocator struct {
	mu sync.Mutex
	// pending maps an address to the volume it was picked for, from the pick
	// until that volume's resource exists with the address as its label.
	pending map[netip.Addr]string
}

// allocate picks the first address of the pool that is neither held according
// to inUse nor pending for another volume, and keeps it pending for volume
// until release.
//
// inUse runs with the allocator locked, and release takes the same lock.
// That ordering is what makes two concurrent creations safe: an address
// leaves pending only after its resource was written, so a listing that
// starts once the lock is held either still finds the address pending or
// already finds it on the resource. Listing before taking the lock would
// leave a window in which it is in neither place.
func (a *serviceIPAllocator) allocate(volume string, pool []ipRange, inUse func() (map[netip.Addr]string, error)) (netip.Prefix, error) {
	log := zap.L()
	a.mu.Lock()
	defer a.mu.Unlock()
	used, err := inUse()
	if err != nil {
		log.Debug("serviceIPAllocator.allocate: cannot read the addresses in use", zap.String("resource", volume), zap.Error(err))
		return netip.Prefix{}, err
	}
	log.Debug("serviceIPAllocator.allocate: start", zap.String("resource", volume), zap.Int("ranges", len(pool)),
		zap.Int("in_use", len(used)), zap.Int("pending", len(a.pending)))
	for _, r := range pool {
		for addr := r.first; addr.IsValid() && !r.last.Less(addr); addr = addr.Next() {
			if !r.usable(addr) {
				continue
			}
			if holder, taken := used[addr]; taken {
				log.Debug("serviceIPAllocator.allocate: address in use", zap.Stringer("address", addr), zap.String("holder", holder))
				continue
			}
			if holder, taken := a.pending[addr]; taken && holder != volume {
				log.Debug("serviceIPAllocator.allocate: address pending for another volume", zap.Stringer("address", addr),
					zap.String("holder", holder))
				continue
			}
			if a.pending == nil {
				a.pending = map[netip.Addr]string{}
			}
			a.pending[addr] = volume
			log.Debug("serviceIPAllocator.allocate: picked", zap.String("resource", volume), zap.Stringer("address", addr),
				zap.Int("prefix", r.bits))
			return netip.PrefixFrom(addr, r.bits), nil
		}
	}
	log.Debug("serviceIPAllocator.allocate: pool exhausted", zap.String("resource", volume))
	return netip.Prefix{}, errServiceIPsExhausted
}

// release forgets the address pending for volume: its resource now carries
// the address, or was never created.
func (a *serviceIPAllocator) release(volume string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for addr, holder := range a.pending {
		if holder == volume {
			zap.L().Debug("serviceIPAllocator.release: no longer pending", zap.String("resource", volume), zap.Stringer("address", addr))
			delete(a.pending, addr)
		}
	}
}

// serviceAddr reads the address out of a service IP as Haify records it:
// "192.168.1.200/24", or the bare address. ok is false for anything else.
func serviceAddr(raw string) (netip.Addr, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return netip.Addr{}, false
	}
	if p, err := netip.ParsePrefix(raw); err == nil {
		return p.Addr().Unmap(), true
	}
	if a, err := netip.ParseAddr(raw); err == nil {
		return a.Unmap(), true
	}
	return netip.Addr{}, false
}
