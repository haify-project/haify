package controller

import (
	"context"
	crand "crypto/rand"
	"fmt"
	"math/big"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/wanproxy"
	"go.uber.org/zap"
)

// wanConfig carries the WAN-replication parameters for a resource. When non-nil,
// generateDrbdConfig emits the opt-in WAN variant: protocol A, DRBD-level
// pull-ahead, and loopback-routed addresses so DRBD talks to the local
// per-resource haify-proxy instead of the peer's real IP. Nil ⇒ ordinary LAN
// config (unchanged). See docs/design/wan-replication.md.
type wanConfig struct {
	DRNode string // the DR-site node

	// PrimaryNodes are the production-site replicas, in config order. With one
	// entry this is the historic two-endpoint WAN resource; with several it is
	// a synchronous primary site plus one asynchronous DR copy, and each
	// primary gets its own WAN leg (DRBD 9 is a full mesh, so the DR must peer
	// with every one of them — otherwise a failover inside the primary site
	// lands on a node with no path to the DR and replication stops).
	PrimaryNodes []string

	// BindPorts are the loopback ports each primary's DRBD binds for its WAN
	// leg, in the same order as PrimaryNodes. Empty means "use the default
	// offset", which is only safe when nothing else on the host has taken those
	// ports — see wanDRBDBindOffset.
	BindPorts []int

	// Tuning is the proxy buffer, which DRBD's congestion-fill follows.
	Tuning wanproxy.Tuning
}

// bindPort returns the loopback port primary i binds for its WAN leg.
func (w *wanConfig) bindPort(legPort, i int) int {
	if i < len(w.BindPorts) && w.BindPorts[i] > 0 {
		return w.BindPorts[i]
	}
	return legPort + wanDRBDBindOffset
}

// wanDRBDBindOffset is the starting guess for the loopback port DRBD binds on
// the PRIMARY side of a WAN leg. It has to differ from the leg port itself,
// which the local dialer already listens on; the primary's DRBD then connects to
// the leg port and lands on that dialer.
//
// It is only a starting point, because a fixed offset collides across resources:
// a resource on port 7300 would bind 7400, which is exactly the leg port of a
// second resource on 7400 — the primary's DRBD then fails to bind with
// EADDRINUSE and the leg never comes up. Callers resolve the real port against
// what the host is already using; see pickWANBindPorts.
const wanDRBDBindOffset = 100

// multiPrimary reports whether the primary site holds more than one replica.
func (w *wanConfig) multiPrimary() bool { return w != nil && len(w.PrimaryNodes) > 1 }

// wanproxyLocalBinaryPath is the controller-local path to the haify-proxy binary
// the WAN provisioner pushes to both nodes. We follow the same convention as
// the service-ip / haify-controller helpers: a well-known /usr/local/bin path.
var wanproxyLocalBinaryPath = "/usr/local/bin/haify-proxy"

// wanproxyBinaryPath returns the controller-local haify-proxy binary to push to
// the WAN nodes, or "" when it is not present locally. Returning "" makes
// wanproxy.Provision skip the binary push and assume the binary was pre-staged
// on the nodes (a warning is logged) rather than failing the create outright —
// most fleets stage haify-proxy alongside drbd-utils via their image/package.
//
// This is the architecture-blind answer, kept for callers that push to a single
// known-compatible node. Anything pushing to a set of nodes should use
// wanproxyBinaryResolver, because the two ends of a WAN leg frequently differ:
// the off-site node is whatever the cloud rents, the primary site is whatever is
// on the shelf.
//
// Nothing calls it today: every WAN path currently provisions a set of nodes and
// so goes through wanproxyBinaryResolver. It is kept rather than deleted because
// it is the single-node half of that pair, and the next single-node caller would
// otherwise have to rediscover the rule stated above — a missing binary is a
// warning, not a failed create. The unused check is silenced for exactly that
// reason, not because the function is a leftover.
//
//nolint:unused // deliberately retained; see the note above.
func (rm *ResourceManager) wanproxyBinaryPath() string {
	if _, err := os.Stat(wanproxyLocalBinaryPath); err != nil {
		rm.controller.logger.Warn("haify-proxy binary not found on controller; assuming it is pre-staged on WAN nodes",
			zap.String("path", wanproxyLocalBinaryPath))
		return ""
	}
	return wanproxyLocalBinaryPath
}

// nodeArchProbe reports a node's machine architecture in Go's naming.
const nodeArchProbe = `case "$(uname -m)" in x86_64) echo amd64;; aarch64|arm64) echo arm64;; *) uname -m;; esac`

// wanproxyBinaryResolver returns a function that picks the controller-local
// haify-proxy binary appropriate to each node.
//
// Pushing one file to every node is right only while the fleet is uniform, and
// a two-site cluster is the case least likely to be: an arm64 machine at home
// replicating to whatever architecture the off-site provider rents. The same
// push then installs an unrunnable file, and the failure surfaces nowhere near
// the cause — systemd reports 203/EXEC on the node while the operator sees a
// DRBD connection that never forms.
//
// Per-architecture binaries are looked for beside the default path, named
// "<path>-<goarch>" (e.g. /usr/local/bin/haify-proxy-arm64). A node whose
// architecture matches the controller's own falls back to the plain path, which
// keeps every existing single-architecture deployment working untouched.
func (rm *ResourceManager) wanproxyBinaryResolver(ctx context.Context, hosts []string) func(string) string {
	log := rm.resourcesLog()
	arch := make(map[string]string, len(hosts))
	for _, h := range hosts {
		start := time.Now()
		res, err := rm.deployment.Exec(ctx, []string{h}, nodeArchProbe)
		if err == nil && res != nil {
			for _, r := range res.Hosts {
				arch[h] = strings.TrimSpace(r.Output)
				break
			}
		}
		log.Debug("wanproxyBinaryResolver: probed node architecture", zap.String("host", h), zap.String("arch", arch[h]),
			zap.Duration("took", time.Since(start)), zap.Error(err))
	}

	return func(host string) string {
		a := arch[host]
		if a != "" {
			if p := wanproxyLocalBinaryPath + "-" + a; fileExists(p) {
				log.Debug("wanproxyBinaryResolver: per-architecture binary", zap.String("host", host), zap.String("path", p))
				return p
			}
		}
		// The plain path is the controller's own architecture. Offer it only when
		// the node agrees, or when the probe failed and there is nothing better
		// to go on — pushing a binary of the wrong architecture is worse than
		// pushing none, because "missing" is a failure the operator can read.
		if (a == runtime.GOARCH || a == "") && fileExists(wanproxyLocalBinaryPath) {
			log.Debug("wanproxyBinaryResolver: controller's own binary", zap.String("host", host), zap.String("arch", a),
				zap.String("path", wanproxyLocalBinaryPath))
			return wanproxyLocalBinaryPath
		}
		rm.controller.logger.Warn("No haify-proxy binary on the controller for this node's architecture; assuming it is pre-staged",
			zap.String("host", host), zap.String("arch", a),
			zap.String("looked_for", wanproxyLocalBinaryPath+"-"+a))
		return ""
	}
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// wanproxyDeployClient adapts the resource manager's deployment client to the
// wanproxy.DeploymentClient interface used by the WAN provisioning path.
func (rm *ResourceManager) wanproxyDeployClient() wanproxy.DeploymentClient {
	return NewWanproxyDeploymentClient(rm.deployment)
}

// wanEndpointAddrs resolves a stored WAN resource's primary and DR node
// addresses (used by the delete path to deprovision the proxy). The DR node is
// dbRes.DRNode; the primary is the other node in dbRes.Nodes.
// wanPrimaryAddrs returns every primary-site node address of a WAN resource
// plus the DR address. A multi-replica primary site has one WAN leg per
// primary, so tearing the resource down has to reach all of them — using only
// the first would strand the other legs' proxy units and configs on the nodes.
func (rm *ResourceManager) wanPrimaryAddrs(dbRes *database.Resource) (primaryAddrs []string, drAddr string) {
	drNode := strings.TrimSpace(dbRes.DRNode)
	drAddr = rm.controller.ResolveHost(drNode)
	for _, n := range strings.Split(dbRes.Nodes, ",") {
		n = strings.TrimSpace(n)
		if n == "" || n == drNode {
			continue
		}
		primaryAddrs = append(primaryAddrs, rm.controller.ResolveHost(n))
	}
	rm.resourcesLog().Debug("wanPrimaryAddrs: resolved", zap.String("resource", dbRes.Name), zap.Strings("primaries", primaryAddrs),
		zap.String("dr", drAddr))
	return primaryAddrs, drAddr
}

func (rm *ResourceManager) wanEndpointAddrs(dbRes *database.Resource) (primaryAddr, drAddr string) {
	drNode := strings.TrimSpace(dbRes.DRNode)
	drAddr = rm.controller.ResolveHost(drNode)
	for _, n := range strings.Split(dbRes.Nodes, ",") {
		n = strings.TrimSpace(n)
		if n == "" || n == drNode {
			continue
		}
		primaryAddr = rm.controller.ResolveHost(n)
		break
	}
	rm.resourcesLog().Debug("wanEndpointAddrs: resolved", zap.String("resource", dbRes.Name), zap.String("primary", primaryAddr),
		zap.String("dr", drAddr))
	return primaryAddr, drAddr
}

// WANStatusInfo carries a WAN resource's DR endpoints and the live
// haify-proxy@<resource> unit state on each WAN node (keyed by node name).
type WANStatusInfo struct {
	DRNode     string
	DREndpoint string
	WANPort    int
	// ProxyState maps a node name to its `systemctl is-active haify-proxy@<res>`
	// result ("active" / "inactive" / "failed" / "unknown").
	ProxyState map[string]string
	// WANReachable is true when the primary can currently reach the DR WAN
	// endpoint over TCP (firewall/security group permits the mTLS port).
	WANReachable bool
	// Metrics is the primary side's published proxy counters, or nil when they
	// could not be read. Nil means UNKNOWN and must never be rendered as zero:
	// the headline figure here is the un-replicated backlog, i.e. how much a DR
	// failover would lose, and a confident "0" would be dangerous.
	Metrics *wanproxy.Metrics
	// MetricsHost is the primary-site node the metrics were read from.
	MetricsHost string
	// Tuning is the proxy buffer and compression the record holds.
	Tuning wanproxy.Tuning
}

// WANStatus returns the WAN replication view for a resource, or (nil, nil) for a
// LAN resource (WANMode false / no record). It probes the haify-proxy unit on the
// primary and DR nodes so `resource status` can surface proxy health.
func (rm *ResourceManager) WANStatus(ctx context.Context, name string) (*WANStatusInfo, error) {
	log := rm.resourcesLog()
	if rm.controller.db == nil {
		log.Debug("WANStatus: no database", zap.String("resource", name))
		return nil, nil
	}
	dbRes, err := rm.controller.db.GetResource(ctx, name)
	if err != nil || dbRes == nil || !dbRes.WANMode {
		log.Debug("WANStatus: not a WAN resource", zap.String("resource", name), zap.Bool("found", dbRes != nil), zap.Error(err))
		return nil, nil
	}
	info := &WANStatusInfo{
		DRNode:     dbRes.DRNode,
		DREndpoint: dbRes.DREndpoint,
		WANPort:    dbRes.WANPort,
		ProxyState: map[string]string{},
		Tuning:     wanTuningOf(dbRes),
	}
	_, drAddr := rm.wanEndpointAddrs(dbRes)

	// One leg per primary-site replica, each its own systemd instance, named
	// exactly as provisioning and repair name it (the multi spec's legs).
	// Probing only "haify-proxy@<resource>" reports every leg of a
	// multi-replica resource as inactive, and so does deriving the leg name
	// from the node's address after legs were renamed to node names.
	multi := rm.wanMultiSpecFor(dbRes)
	single := len(multi.PrimaryNodeKeys) <= 1

	probe := func(label, addr, unit string) {
		state := "unknown"
		if addr != "" && rm.deployment != nil {
			// `|| true` so an inactive unit (non-zero exit) still yields its state.
			start := time.Now()
			res, err := rm.deployment.Exec(ctx, []string{addr}, "systemctl is-active "+unit+" 2>/dev/null || true")
			if err == nil && res != nil {
				if hr, ok := res.Hosts[addr]; ok {
					if s := strings.TrimSpace(hr.Output); s != "" {
						state = s
					}
				}
			}
			log.Debug("WANStatus: probed proxy unit", zap.String("resource", name), zap.String("host", addr), zap.String("unit", unit),
				zap.String("state", state), zap.Duration("took", time.Since(start)), zap.Error(err))
		}
		info.ProxyState[label] = state
	}

	// The DR terminates every leg, so it runs one unit per primary. Report them
	// per leg rather than collapsing to one line for the DR node, or a single
	// dead tunnel hides behind a healthy one.
	for i, leg := range multi.Legs() {
		n := multi.PrimaryNodeKeys[i]
		unit := wanproxy.UnitInstance(leg.Resource)
		probe(n, leg.PrimaryNodeAddr, unit)
		label := dbRes.DRNode
		if !single {
			label = dbRes.DRNode + " (leg " + n + ")"
		}
		probe(label, drAddr, unit)
	}
	// Reachability and counters come from a leg whose dialer is actually
	// running. Probing from the first node in the list instead reports "WAN
	// unreachable" whenever that particular replica happens to be down, which
	// points at the wrong end of the link.
	start := time.Now()
	st, serr := wanproxy.StatusMulti(ctx, rm.wanproxyDeployClient(), multi)
	log.Debug("WANStatus: read leg status", zap.String("resource", name), zap.Bool("found", st != nil),
		zap.Duration("took", time.Since(start)), zap.Error(serr))
	if serr == nil && st != nil {
		info.WANReachable = st.WANReachable
		// Proxy counters from a live primary (the side that holds the backlog).
		// Best effort: an older proxy publishes nothing, and that must not fail
		// status.
		if host := liveWANPrimary(st); host != "" {
			start := time.Now()
			info.Metrics = wanproxy.ReadNodeMetrics(ctx, rm.wanproxyDeployClient(), host, wanLegFor(st, host))
			info.MetricsHost = host
			log.Debug("WANStatus: read proxy metrics", zap.String("resource", name), zap.String("host", host),
				zap.Bool("found", info.Metrics != nil), zap.Duration("took", time.Since(start)))
		} else {
			log.Debug("WANStatus: no live primary dialer; no metrics", zap.String("resource", name))
		}
	}
	log.Debug("WANStatus: done", zap.String("resource", name), zap.Bool("reachable", info.WANReachable),
		zap.Int("units", len(info.ProxyState)))
	return info, nil
}

// liveWANPrimary returns the address of a primary-site node whose dialer is
// running, or "" when none is.
func liveWANPrimary(st *wanproxy.MultiStatus) string {
	for _, l := range st.Legs {
		if l.PrimaryActive {
			return l.PrimaryHost
		}
	}
	return ""
}

// wanLegFor returns the leg id served by the given primary host.
func wanLegFor(st *wanproxy.MultiStatus, host string) string {
	for _, l := range st.Legs {
		if l.PrimaryHost == host {
			return l.LegID
		}
	}
	return st.Resource
}

// randomWANPort picks a random TCP port in [3001, 65535] for a WAN proxy when
// the caller does not specify one, matching the project convention of using
// high, non-well-known ports.
func randomWANPort() uint32 {
	const lo, hi = 3001, 65535
	n, err := crand.Int(crand.Reader, big.NewInt(int64(hi-lo+1)))
	if err != nil {
		// crypto/rand should never fail; fall back to a time-derived port so we
		// still return a usable high port rather than aborting the create.
		port := uint32(lo + int(time.Now().UnixNano()%(hi-lo+1)))
		zap.L().Debug("randomWANPort: crypto/rand failed; time-derived port", zap.Uint32("port", port), zap.Error(err))
		return port
	}
	port := uint32(lo) + uint32(n.Int64())
	zap.L().Debug("randomWANPort: chose port", zap.Uint32("port", port))
	return port
}

// applyLocalSiteQuorum narrows a running WAN resource's quorum to a majority of
// its primary site.
//
// It is a separate step from config generation on purpose. A numeric quorum is
// enforced absolutely, from the moment the resource exists — before any peer has
// connected there is exactly one node visible, so the force-promote that
// establishes the first UpToDate generation is refused with "No quorum". DRBD's
// own "majority" is adaptive to the membership it has seen, so creation needs
// it. Once the peers are up, the number is both satisfiable and the thing we
// actually want: see localSiteQuorum for why the DR must not vote.
func (rm *ResourceManager) applyLocalSiteQuorum(ctx context.Context, resource string, hosts []string, localVoters int) error {
	log := rm.resourcesLog()
	log.Debug("applyLocalSiteQuorum: start", zap.String("resource", resource), zap.Strings("hosts", hosts), zap.Int("local_voters", localVoters))
	if len(hosts) == 0 {
		log.Debug("applyLocalSiteQuorum: no hosts", zap.String("resource", resource))
		return nil
	}
	resPath := fmt.Sprintf("/etc/drbd.d/%s.res", resource)
	start := time.Now()
	catRes, err := rm.deployment.Exec(ctx, []string{hosts[0]}, "cat "+resPath)
	log.Debug("applyLocalSiteQuorum: read config", zap.String("resource", resource), zap.String("host", hosts[0]),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("read resource config to set quorum: %w", err)
	}
	live, ok := "", false
	for _, r := range catRes.Hosts {
		live, ok = r.Output, r.Success
		break
	}
	if !ok || strings.TrimSpace(live) == "" {
		log.Debug("applyLocalSiteQuorum: config unreadable", zap.String("resource", resource))
		return fmt.Errorf("read resource config for %q: %s", resource, catRes.FailureDetails())
	}

	updated := setLocalSiteQuorum(live, localVoters)
	if updated == live {
		log.Debug("applyLocalSiteQuorum: quorum already set", zap.String("resource", resource))
		return nil
	}
	start = time.Now()
	_, err = rm.deployment.DistributeConfig(ctx, hosts, updated, resPath)
	log.Debug("applyLocalSiteQuorum: distributed config", zap.String("resource", resource), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("distribute quorum change: %w", err)
	}
	return rm.execAllSuccess(ctx, hosts, fmt.Sprintf("sudo drbdadm adjust %s", resource),
		"apply the primary-site quorum")
}

// localSiteVoters counts the members entitled to decide whether the primary
// site may write. allMembers is every configured participant of a WAN resource,
// exactly one of which is the off-site DR; the DR replicates and still counts as
// a member, it just does not get to decide whether home can write. See
// primarySiteQuorum for why.
//
// A named function rather than a `- 1` at the call site: the subtraction is the
// entire safety property, and an inline one is neither greppable nor testable.
func localSiteVoters(allMembers int) int {
	return allMembers - 1
}

// wanProxySpecFor rebuilds the haify-proxy spec for a stored WAN resource so its
// live status can be queried. The primary is the resource node that is not the
// DR node.
// wanMultiSpecFor rebuilds the full multi-leg haify-proxy spec for a stored WAN
// resource so its live status can be queried.
//
// It must mirror what provisioning built, because leg names and ports are
// derived from it: one leg per primary-site node, in the stored node order
// (Legs assigns WAN and DRBD ports by index), with the DR node excluded.
//
// This replaced a version that collapsed the resource to a single ProxySpec
// named after the resource with an arbitrary node as "the primary". Once a WAN
// resource has more than one primary-site replica, the real units are
// haify-proxy@<resource>_<node>, so that spec named a unit present on no node and
// reported a perfectly healthy WAN as entirely down.
func (rm *ResourceManager) wanMultiSpecFor(dbRes *database.Resource) wanproxy.MultiSpec {
	var primaries, primaryNames []string
	for _, n := range splitCSV(dbRes.Nodes) {
		if n == "" || n == dbRes.DRNode {
			continue
		}
		primaries = append(primaries, rm.controller.ResolveHost(n))
		primaryNames = append(primaryNames, n)
	}
	rm.resourcesLog().Debug("wanMultiSpecFor: rebuilt spec", zap.String("resource", dbRes.Name), zap.Strings("primaries", primaryNames),
		zap.String("dr_node", dbRes.DRNode), zap.Int("wan_port", dbRes.WANPort))
	return wanproxy.MultiSpec{
		Resource:          dbRes.Name,
		PrimaryNodeAddrs:  primaries,
		PrimaryNodeKeys:   primaryNames,
		DRNodeAddr:        rm.controller.ResolveHost(dbRes.DRNode),
		DRPublicEndpoint:  dbRes.DREndpoint,
		PrimaryEgressAddr: dbRes.WANEgressAddress,
		BaseWANPort:       int(dbRes.WANPort),
		BaseDRBDPort:      int(dbRes.Port),
		Tuning:            wanTuningOf(dbRes),
	}
}

// wanTuningOf is the proxy tuning a resource's record holds. Every path that
// re-provisions legs from the record has to carry it, or adding a replica or
// repairing a tunnel would quietly put the buffer and compression back to the
// defaults.
func wanTuningOf(dbRes *database.Resource) wanproxy.Tuning {
	return wanproxy.Tuning{BufferBytes: dbRes.WANBufferBytes, ZstdLevel: dbRes.WANZstdLevel}
}

// wanStatusMessage renders a short human description of an unhealthy WAN proxy
// pair; it returns "" when the pair is healthy.
func wanStatusMessage(st *wanproxy.MultiStatus) string {
	if st == nil {
		return "WAN status unavailable"
	}
	if len(st.Legs) == 0 {
		return "no WAN legs configured"
	}
	var problems []string
	// Name the node on each broken leg. "primary proxy inactive" on a
	// multi-replica resource does not say which replica lost its tunnel, which
	// is the only thing the operator needs in order to act.
	for _, l := range st.Unhealthy() {
		switch {
		case !l.PrimaryActive && !l.DRActive:
			problems = append(problems, fmt.Sprintf("leg %s down on both ends", l.PrimaryHost))
		case !l.PrimaryActive:
			problems = append(problems, fmt.Sprintf("proxy inactive on %s", l.PrimaryHost))
		default:
			problems = append(problems, fmt.Sprintf("DR proxy inactive for leg %s", l.PrimaryHost))
		}
	}
	if !st.WANReachable {
		problems = append(problems, "DR WAN endpoint unreachable")
	}
	return strings.Join(problems, "; ")
}
