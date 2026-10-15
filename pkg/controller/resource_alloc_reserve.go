package controller

import (
	"context"
	"sync"

	"go.uber.org/zap"
)

// allocReservations closes the window between choosing a DRBD port or device
// minor and the .res file that claims it reaching the nodes. Both are chosen
// by scanning the existing .res files, so two creates running at once — a
// StatefulSet's PVCs provisioned together, say — used to read the same files
// and pick the same port, and the second failed at create-md with "is also
// used". A reservation lasts until the create that made it returns, by which
// time its .res file is on the nodes (or rolled back).
type allocReservations struct {
	mu     sync.Mutex
	ports  map[uint32]string // port -> resource holding it
	minors map[int]string    // device minor -> resource holding it
}

// reservePort returns the lowest DRBD port at or above 7000 that neither a
// .res file on host nor a create in progress holds, and holds it for name.
func (rm *ResourceManager) reservePort(ctx context.Context, host, name string) (uint32, error) {
	log := rm.resourcesLog()
	rm.alloc.mu.Lock()
	defer rm.alloc.mu.Unlock()
	used, err := rm.usedPorts(ctx, host)
	if err != nil {
		log.Debug("reservePort: scan failed", zap.String("resource", name), zap.String("host", host), zap.Error(err))
		return 0, err
	}
	for p := range rm.alloc.ports {
		used = append(used, p)
	}
	port := lowestFreePort(used, 7000)
	if rm.alloc.ports == nil {
		rm.alloc.ports = map[uint32]string{}
	}
	rm.alloc.ports[port] = name
	log.Debug("reservePort: reserved", zap.String("resource", name), zap.Uint32("port", port),
		zap.Int("in_flight", len(rm.alloc.ports)))
	return port, nil
}

// reserveMinors returns the first of count consecutive device minors free on
// every host and not held by a create in progress, and holds them for name.
func (rm *ResourceManager) reserveMinors(ctx context.Context, hosts []string, count int, name string) (int, error) {
	log := rm.resourcesLog()
	rm.alloc.mu.Lock()
	defer rm.alloc.mu.Unlock()
	base, err := rm.nextGlobalMinor(ctx, hosts)
	if err != nil {
		log.Debug("reserveMinors: scan failed", zap.String("resource", name), zap.Error(err))
		return 0, err
	}
	for m := range rm.alloc.minors {
		if m >= base {
			base = m + 1
		}
	}
	if rm.alloc.minors == nil {
		rm.alloc.minors = map[int]string{}
	}
	for i := 0; i < count; i++ {
		rm.alloc.minors[base+i] = name
	}
	log.Debug("reserveMinors: reserved", zap.String("resource", name), zap.Int("base", base), zap.Int("count", count),
		zap.Int("in_flight", len(rm.alloc.minors)))
	return base, nil
}

// releaseAlloc drops every port and minor reserved for name.
func (rm *ResourceManager) releaseAlloc(name string) {
	rm.alloc.mu.Lock()
	defer rm.alloc.mu.Unlock()
	released := 0
	for p, owner := range rm.alloc.ports {
		if owner == name {
			delete(rm.alloc.ports, p)
			released++
		}
	}
	for m, owner := range rm.alloc.minors {
		if owner == name {
			delete(rm.alloc.minors, m)
			released++
		}
	}
	rm.resourcesLog().Debug("releaseAlloc: released", zap.String("resource", name), zap.Int("released", released))
}
