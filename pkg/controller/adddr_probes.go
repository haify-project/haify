package controller

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/deployment"
)

// maxPeersProbe reads the bitmap-slot capacity DRBD baked into a resource's
// metadata, and how many of those slots are spoken for.
//
// The count is only in the on-disk metadata, so it has to be read with
// drbdmeta. `--force` is needed because the device is attached; the read is
// harmless (max-peers is written once, at create-md time, and never changes)
// and it is the only way to learn this without detaching a live replica.
const maxPeersProbe = `devs=$(drbdadm sh-dev %[1]s 2>/dev/null); lls=$(drbdadm sh-ll-dev %[1]s 2>/dev/null); set -- $lls; ` +
	`for d in $devs; do m=${d#/dev/drbd}; sudo drbdmeta --force "$m" v09 "$1" internal dump-md 2>/dev/null ` +
	`| sed -n 's/^max-peers \([0-9]*\);$/\1/p'; shift; done`

var maxPeersRe = regexp.MustCompile(`^\s*(\d+)\s*$`)

// tracedFailedHosts is res.FailedHosts for a trace: nil-safe, because an Exec
// that failed (or a test double) can hand back no result at all.
func tracedFailedHosts(res *deployment.ExecResult) []string {
	if res == nil {
		return nil
	}
	return res.FailedHosts()
}

// assertBitmapSlotFree fails unless every existing diskful replica has a spare
// bitmap slot for one more diskful peer.
//
// Diskless nodes are deliberately not counted: they hold no data, so DRBD never
// assigns them a slot. That is why a resource with a tiebreaker can look like it
// has three nodes and still be out of room at the second diskful one.
func (rm *ResourceManager) assertBitmapSlotFree(ctx context.Context, hosts, nodes []string, resource string) error {
	log := rm.controller.traceLogger()
	needed := len(hosts) // existing diskful peers-per-node, +1 for the newcomer, -1 for self
	log.Debug("assertBitmapSlotFree: checking bitmap slots", zap.String("resource", resource),
		zap.Strings("hosts", hosts), zap.Strings("nodes", nodes), zap.Int("needed", needed))
	for i, host := range hosts {
		log.Debug("assertBitmapSlotFree: probing max-peers", zap.String("resource", resource),
			zap.String("host", host), zap.String("node", nodes[i]))
		start := time.Now()
		res, err := rm.deployment.Exec(ctx, []string{host}, fmt.Sprintf(maxPeersProbe, resource))
		log.Debug("assertBitmapSlotFree: drbdmeta dump-md probe done", zap.String("host", host),
			zap.Strings("failed_hosts", tracedFailedHosts(res)), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			log.Debug("assertBitmapSlotFree: probe failed", zap.String("node", nodes[i]), zap.Error(err))
			return fmt.Errorf("probe bitmap slots on %q: %w", nodes[i], err)
		}
		out := ""
		for _, r := range res.Hosts {
			out = r.Output
			break
		}

		lowest := -1
		for _, line := range strings.Split(out, "\n") {
			m := maxPeersRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			n, cerr := strconv.Atoi(m[1])
			if cerr != nil {
				continue
			}
			if lowest < 0 || n < lowest {
				lowest = n
			}
		}
		if lowest < 0 {
			// Unreadable metadata is not proof of a problem, and refusing on it
			// would block the operation on any node whose drbdmeta behaves
			// differently. Let `adjust` be the judge.
			rm.controller.logger.Warn("Could not read bitmap-slot capacity; proceeding",
				zap.String("resource", resource), zap.String("node", nodes[i]))
			continue
		}
		log.Debug("assertBitmapSlotFree: node capacity read", zap.String("node", nodes[i]),
			zap.Int("max_peers", lowest), zap.Int("needed", needed), zap.Bool("enough", lowest >= needed))
		if lowest < needed {
			log.Debug("assertBitmapSlotFree: not enough bitmap slots", zap.String("node", nodes[i]),
				zap.Int("max_peers", lowest), zap.Int("needed", needed))
			return fmt.Errorf(
				"node %q has metadata for %d peer(s) but %d are needed to add a DR replica: "+
					"DRBD allocates bitmap slots once, at create-md time, and they cannot be grown online. "+
					"Recreate metadata one node at a time — on each Secondary run "+
					"`drbdadm down %s && drbdadm create-md --max-peers=%d --force %s && drbdadm up %s` "+
					"(it resyncs from the Primary), fail the service over, then do the last node",
				nodes[i], lowest, needed, resource, deployment.DefaultMaxPeers, resource, resource)
		}
	}
	log.Debug("assertBitmapSlotFree: every diskful replica has a free slot", zap.String("resource", resource))
	return nil
}

// freeLoopbackPortProbe walks up from a starting port and prints the first one
// the host is neither listening on nor has written into a DRBD or haify-proxy
// config. Running listeners alone are not enough: a resource that is configured
// but currently down would be skipped, and the collision would only appear the
// next time it started.
const freeLoopbackPortProbe = `for p in $(seq %[1]d %[2]d); do ` +
	`ss -lnt "sport = :$p" 2>/dev/null | grep -q LISTEN && continue; ` +
	`grep -qs "127\.0\.0\.1:$p" /etc/drbd.d/*.res /etc/haify-proxy/*.toml && continue; ` +
	`echo $p; break; done`

// pickWANBindPorts chooses, for each primary, a loopback port its DRBD can bind
// for the WAN leg.
//
// A fixed offset is not good enough. Ports are per-resource but the offset is
// not, so two resources whose DRBD ports differ by exactly the offset end up
// fighting: the first resource's primary tries to bind the port the second
// resource's dialer is already listening on, DRBD reports EADDRINUSE as a bare
// "Failed to initiate connection, err=-98", and the leg silently never comes up.
// Asking the host what is free costs one command and removes the whole class.
func (rm *ResourceManager) pickWANBindPorts(ctx context.Context, hosts []string, basePort int) ([]int, error) {
	log := rm.controller.traceLogger()
	log.Debug("pickWANBindPorts: choosing loopback bind ports", zap.Strings("hosts", hosts), zap.Int("base_port", basePort))
	ports := make([]int, len(hosts))
	for i, host := range hosts {
		start := basePort + wanDRBDBindOffset + i
		t0 := time.Now()
		res, err := rm.deployment.Exec(ctx, []string{host},
			fmt.Sprintf(freeLoopbackPortProbe, start, start+200))
		log.Debug("pickWANBindPorts: free-port probe done", zap.String("host", host), zap.Int("from_port", start),
			zap.Strings("failed_hosts", tracedFailedHosts(res)), zap.Duration("took", time.Since(t0)), zap.Error(err))
		if err != nil {
			log.Debug("pickWANBindPorts: probe failed", zap.String("host", host), zap.Error(err))
			return nil, fmt.Errorf("probe a free loopback port on %s: %w", host, err)
		}
		out := ""
		for _, r := range res.Hosts {
			out = strings.TrimSpace(r.Output)
			break
		}
		p, cerr := strconv.Atoi(strings.TrimSpace(out))
		if cerr != nil || p <= 0 {
			// Fall back to the plain offset rather than refusing: it is what the
			// code did before the probe existed, and it is right whenever the
			// host has nothing else on that port.
			rm.controller.logger.Warn("Could not probe a free loopback port; using the default offset",
				zap.String("host", host), zap.Int("port", start))
			p = start
		}
		log.Debug("pickWANBindPorts: port chosen", zap.String("host", host), zap.Int("port", p))
		ports[i] = p
	}
	log.Debug("pickWANBindPorts: done", zap.Ints("ports", ports))
	return ports, nil
}

// backingSizeProbe prints the byte size of each of a resource's backing devices.
const backingSizeProbe = `for l in $(drbdadm sh-ll-dev %s 2>/dev/null); do sudo blockdev --getsize64 "$l"; done`

// primaryBackingSizes reports the largest backing-device size seen across the
// primary site, per volume.
//
// The DR's volumes are sized from this rather than from the size recorded at
// creation, because the two drift: metadata is carved out of the same device, so
// a replica whose volume was extended (to make room for more bitmap slots, say)
// exports a larger device than the recorded size implies. Build the DR from the
// record and DRBD refuses the connection outright — "The peer's disk size is too
// small" — after everything else is already in place.
func (rm *ResourceManager) primaryBackingSizes(ctx context.Context, hosts []string, resource string, volumes int) ([]uint64, error) {
	log := rm.controller.traceLogger()
	log.Debug("primaryBackingSizes: reading backing sizes", zap.String("resource", resource),
		zap.Strings("hosts", hosts), zap.Int("volumes", volumes))
	sizes := make([]uint64, volumes)
	seen := false
	for _, host := range hosts {
		start := time.Now()
		res, err := rm.deployment.Exec(ctx, []string{host}, fmt.Sprintf(backingSizeProbe, resource))
		log.Debug("primaryBackingSizes: blockdev probe done", zap.String("host", host),
			zap.Strings("failed_hosts", tracedFailedHosts(res)), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			log.Debug("primaryBackingSizes: skipping host, probe failed", zap.String("host", host), zap.Error(err))
			continue
		}
		out := ""
		for _, r := range res.Hosts {
			out = r.Output
			break
		}
		idx := 0
		for _, line := range strings.Split(out, "\n") {
			n, cerr := strconv.ParseUint(strings.TrimSpace(line), 10, 64)
			if cerr != nil || n == 0 {
				continue
			}
			if idx < volumes && n > sizes[idx] {
				sizes[idx] = n
				seen = true
			}
			idx++
		}
	}
	if !seen {
		log.Debug("primaryBackingSizes: no size read from any host", zap.String("resource", resource))
		return nil, fmt.Errorf("could not read backing device sizes for %q from any replica", resource)
	}
	for i, s := range sizes {
		if s == 0 {
			log.Debug("primaryBackingSizes: volume size missing", zap.String("resource", resource), zap.Int("volume", i))
			return nil, fmt.Errorf("no backing device size found for volume %d of %q", i, resource)
		}
	}
	log.Debug("primaryBackingSizes: done", zap.String("resource", resource), zap.Uint64s("size_bytes", sizes))
	return sizes, nil
}
