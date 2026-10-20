package controller

import (
	"sync"
	"time"

	"go.uber.org/zap"
)

// resourceLocks serialises the operations that rewrite one resource's
// configuration on its nodes. Without it a CSI DeleteVolume and the node
// plugin's diskless detach for the same volume, which Kubernetes can send at
// the same moment, interleave: the delete removes the .res file everywhere,
// then the detach writes its edited copy back to the nodes it read before the
// delete, and a node keeps a resource the controller no longer knows, holding
// a port the next resource is given.
type resourceLocks struct {
	mu    sync.Mutex
	locks map[string]*resourceLock
}

type resourceLock struct {
	mu    sync.Mutex
	users int
}

// lock waits for the resource's lock and returns its release. Entries are
// dropped when no one holds or waits for them, so the map stays as small as
// the set of resources being changed.
func (l *resourceLocks) lock(name string) (unlock func()) {
	l.mu.Lock()
	if l.locks == nil {
		l.locks = map[string]*resourceLock{}
	}
	e := l.locks[name]
	if e == nil {
		e = &resourceLock{}
		l.locks[name] = e
	}
	e.users++
	l.mu.Unlock()

	start := time.Now()
	e.mu.Lock()
	zap.L().Debug("resourceLocks.lock: acquired", zap.String("resource", name), zap.Duration("waited", time.Since(start)))
	return func() {
		e.mu.Unlock()
		l.mu.Lock()
		e.users--
		if e.users == 0 {
			delete(l.locks, name)
		}
		l.mu.Unlock()
	}
}
