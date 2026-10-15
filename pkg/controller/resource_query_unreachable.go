package controller

import (
	"time"

	"go.uber.org/zap"
)

// unreachableFor is how long a host that failed a status query is asked last.
// A powered-off node does not refuse the SSH connection, it lets it time out,
// and every status read that asked it first waited that long before trying a
// live replica. The HA failover controller reads each volume in use every few
// seconds, so a dead node at the head of the list slowed every check by the
// connection timeout per volume, exactly when speed mattered. Asked last, the
// dead node costs nothing while another replica answers; once the window
// passes it is tried in its normal place again, so a node that came back is
// noticed within a minute even if nothing else asked it.
const unreachableFor = time.Minute

// statusAskTimeout bounds a status query to one host while other hosts are
// left to ask. `drbdadm status` over SSH answers in well under a second on a
// live node.
const statusAskTimeout = 5 * time.Second

// markHostUnreachable records that host failed a status query just now.
func (rm *ResourceManager) markHostUnreachable(host string) {
	rm.unreachableHosts.Store(host, time.Now())
	rm.resourcesLog().Debug("markHostUnreachable: asked last for a while", zap.String("host", host),
		zap.Duration("for", unreachableFor))
}

// markHostReachable clears host's mark after it answered.
func (rm *ResourceManager) markHostReachable(host string) {
	if _, was := rm.unreachableHosts.LoadAndDelete(host); was {
		rm.resourcesLog().Debug("markHostReachable: answering again", zap.String("host", host))
	}
}

// recentlyUnreachable reports whether host failed a status query within
// unreachableFor.
func (rm *ResourceManager) recentlyUnreachable(host string) bool {
	v, ok := rm.unreachableHosts.Load(host)
	if !ok {
		return false
	}
	if time.Since(v.(time.Time)) >= unreachableFor {
		rm.unreachableHosts.Delete(host)
		return false
	}
	return true
}

// unreachableLast moves the indexes of recently unreachable hosts to the end
// of order, keeping the relative order within each group.
func (rm *ResourceManager) unreachableLast(hosts []string, order []int) []int {
	out := make([]int, 0, len(order))
	var late []int
	for _, i := range order {
		if rm.recentlyUnreachable(hosts[i]) {
			late = append(late, i)
			continue
		}
		out = append(out, i)
	}
	if len(late) > 0 {
		rm.resourcesLog().Debug("unreachableLast: asking recently unreachable hosts last", zap.Int("late", len(late)))
	}
	return append(out, late...)
}
