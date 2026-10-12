package controller

import (
	"context"
	"fmt"
	"strings"

	"github.com/haify-project/haify/pkg/wanproxy"
	"go.uber.org/zap"
)

// planCreateWAN validates the WAN part of a create request and works out what
// it changes: the member list becomes the primary-site nodes followed by the
// DR node, a single-replica primary site replicates under protocol A, and
// several primaries each get a loopback port to bind their WAN leg. A nil wan
// returns nodes and protocol unchanged and no WAN config: the LAN path.
func (rm *ResourceManager) planCreateWAN(ctx context.Context, name string, port uint32, nodes []string, protocol string, wan *WANSpec) ([]string, string, *wanConfig, error) {
	log := rm.resourcesLog()
	if wan == nil {
		log.Debug("planCreateWAN: LAN resource", zap.String("resource", name))
		return nodes, protocol, nil, nil
	}
	log.Debug("planCreateWAN: start", zap.String("resource", name), zap.Strings("nodes", nodes),
		zap.String("dr_node", wan.DRNode), zap.String("dr_endpoint", wan.DREndpoint),
		zap.String("egress", wan.EgressAddress), zap.Uint32("wan_port", wan.WANPort))

	// The primary site may hold several synchronous replicas — the
	// "两地三中心" shape: protocol C inside the production site, one
	// asynchronous copy far away. Only the legs that cross the WAN go
	// through haify-proxy.
	primaries := make([]string, 0, len(nodes))
	seen := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		p := strings.TrimSpace(n)
		if p == "" {
			continue
		}
		if seen[p] {
			log.Debug("planCreateWAN: primary listed twice", zap.String("resource", name), zap.String("node", p))
			return nil, "", nil, fmt.Errorf("WAN resource %q lists primary node %q twice", name, p)
		}
		seen[p] = true
		primaries = append(primaries, p)
	}
	if len(primaries) == 0 {
		log.Debug("planCreateWAN: no primary-site node", zap.String("resource", name))
		return nil, "", nil, fmt.Errorf("WAN resource %q requires at least one primary-site node in --nodes", name)
	}

	drNode := strings.TrimSpace(wan.DRNode)
	if drNode == "" {
		log.Debug("planCreateWAN: no DR node", zap.String("resource", name))
		return nil, "", nil, fmt.Errorf("WAN resource %q requires a DR node (--dr-node)", name)
	}
	if seen[drNode] {
		log.Debug("planCreateWAN: DR node is also a primary", zap.String("resource", name), zap.String("dr_node", drNode))
		return nil, "", nil, fmt.Errorf("WAN DR node %q must not also be a primary-site node of %q", drNode, name)
	}
	if rm.controller.nodes.GetNodeAddressByName(drNode) == "" {
		log.Debug("planCreateWAN: DR node not registered", zap.String("resource", name), zap.String("dr_node", drNode))
		return nil, "", nil, fmt.Errorf("WAN DR node %q is not a registered node", drNode)
	}
	if strings.TrimSpace(wan.DREndpoint) == "" {
		log.Debug("planCreateWAN: no DR endpoint", zap.String("resource", name))
		return nil, "", nil, fmt.Errorf("WAN resource %q requires a DR endpoint (--dr-endpoint)", name)
	}
	if err := validDREndpoint(strings.TrimSpace(wan.DREndpoint)); err != nil {
		log.Debug("planCreateWAN: invalid DR endpoint", zap.String("resource", name), zap.Error(err))
		return nil, "", nil, err
	}
	// WAN legs are systemd instances named "<resource>_<node>". A resource
	// named like one of those would be indistinguishable from another
	// resource's leg, and leg reconciliation would treat one's tunnels as
	// the other's litter. Refusing the name is cheaper than disambiguating
	// it forever.
	if rm.controller.db != nil {
		existing, lerr := rm.controller.db.ListResources(ctx)
		log.Debug("planCreateWAN: listed resources for leg-name check", zap.Int("count", len(existing)), zap.Error(lerr))
		if lerr == nil {
			names := make([]string, 0, len(existing))
			for _, e := range existing {
				names = append(names, e.Name)
			}
			if verr := wanproxy.ValidateResourceNameForWAN(name, names); verr != nil {
				log.Debug("planCreateWAN: name collides with a WAN leg", zap.String("resource", name), zap.Error(verr))
				return nil, "", nil, verr
			}
		}
	}
	if wan.WANPort == 0 {
		wan.WANPort = randomWANPort()
		rm.controller.logger.Info("auto-allocated WAN proxy port",
			zap.String("resource", name), zap.Uint32("wan_port", wan.WANPort))
	}

	// Protocol: a single-replica primary site is the historic two-endpoint
	// WAN resource and is async end to end. With several replicas the LAN
	// mesh stays synchronous (that is the point of having them) and only
	// the WAN legs are async — expressed per connection, below.
	if len(primaries) == 1 {
		log.Debug("planCreateWAN: single primary; protocol A end to end", zap.String("resource", name))
		protocol = "A"
	}
	nodes = append(append([]string{}, primaries...), drNode)
	wanCfg := &wanConfig{DRNode: drNode, PrimaryNodes: primaries}

	// Each primary's DRBD needs a loopback port to bind for its WAN leg that
	// nothing else on that host has claimed; a fixed offset collides with
	// another resource whose DRBD port happens to sit one offset away.
	if len(primaries) > 1 && rm.deployment != nil {
		primaryAddrs := make([]string, 0, len(primaries))
		for _, n := range primaries {
			primaryAddrs = append(primaryAddrs, rm.controller.ResolveHost(n))
		}
		bindPorts, perr := rm.pickWANBindPorts(ctx, primaryAddrs, int(port))
		log.Debug("planCreateWAN: picked WAN bind ports", zap.String("resource", name), zap.Strings("hosts", primaryAddrs),
			zap.Ints("bind_ports", bindPorts), zap.Error(perr))
		if perr != nil {
			return nil, "", nil, fmt.Errorf("choose WAN bind ports: %w", perr)
		}
		wanCfg.BindPorts = bindPorts
	}
	log.Debug("planCreateWAN: planned", zap.String("resource", name), zap.Strings("primaries", primaries),
		zap.String("dr_node", drNode), zap.String("protocol", protocol), zap.Uint32("wan_port", wan.WANPort))
	return nodes, protocol, wanCfg, nil
}
