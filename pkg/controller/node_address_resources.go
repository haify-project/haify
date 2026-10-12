package controller

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Bringing the DRBD configs of resources onto renumbered nodes' addresses.

// RenumberInResources brings every resource a renumbered node takes part in
// onto the registry's addresses: each participant's config gets them and is
// adjusted. Resources are done one at a time and a failure does not stop the
// rest; each one's outcome is in the change.
func (rm *ResourceManager) RenumberInResources(ctx context.Context, change *NodeAddressChange) {
	log := loggerOfController(rm.controller)
	log.Debug("ResourceManager.RenumberInResources: start", zap.Int("moves", len(change.Moves)))
	if rm.controller.db == nil {
		log.Debug("ResourceManager.RenumberInResources: no database, no resources to rewrite")
		return
	}
	start := time.Now()
	all, err := rm.controller.db.ListResources(ctx)
	if err != nil {
		log.Debug("ResourceManager.RenumberInResources: listing resources failed", zap.Duration("took", time.Since(start)), zap.Error(err))
		change.Failed = append(change.Failed, "list resources: "+err.Error())
		return
	}
	log.Debug("ResourceManager.RenumberInResources: listed resources", zap.Int("count", len(all)), zap.Duration("took", time.Since(start)))
	byHost := rm.controller.nodes.drbdAddressesByHost()
	for _, r := range all {
		var involved []NodeMove
		for _, mv := range change.Moves {
			if nodeTakesPart(mv.Node, r.Nodes, r.DisklessNodes, r.DisklessClients, r.DRNode) {
				involved = append(involved, mv)
			}
		}
		if len(involved) == 0 {
			log.Debug("ResourceManager.RenumberInResources: no renumbered node takes part, skipped", zap.String("resource", r.Name))
			continue
		}
		replicationMoved := false
		for _, mv := range involved {
			replicationMoved = replicationMoved || mv.OldReplication != mv.ReplicationAddress
		}
		log.Debug("ResourceManager.RenumberInResources: resource involved", zap.String("resource", r.Name),
			zap.Int("involved", len(involved)), zap.Bool("replication_moved", replicationMoved), zap.Bool("wan", r.WANMode))
		if r.WANMode {
			change.WANResources = append(change.WANResources, r.Name)
			// The endpoint the primary dials and the source it dials from are
			// operator-given addresses, often a node's own.
			moved := false
			for _, mv := range involved {
				if r.DRNode == mv.Node && (r.DREndpoint == mv.OldAddress || r.DREndpoint == mv.OldReplication) {
					r.DREndpoint, moved = mv.ReplicationAddress, true
					log.Debug("ResourceManager.RenumberInResources: DR endpoint follows the node", zap.String("resource", r.Name),
						zap.String("node", mv.Node), zap.String("endpoint", r.DREndpoint))
				}
				if r.WANEgressAddress != "" && (r.WANEgressAddress == mv.OldAddress || r.WANEgressAddress == mv.OldReplication) {
					r.WANEgressAddress, moved = mv.ReplicationAddress, true
					log.Debug("ResourceManager.RenumberInResources: WAN egress address follows the node", zap.String("resource", r.Name),
						zap.String("node", mv.Node), zap.String("egress", r.WANEgressAddress))
				}
			}
			if moved {
				start := time.Now()
				err := rm.controller.db.SaveResource(ctx, r)
				log.Debug("ResourceManager.RenumberInResources: saved WAN endpoints", zap.String("resource", r.Name),
					zap.Duration("took", time.Since(start)), zap.Error(err))
				if err != nil {
					change.Failed = append(change.Failed, fmt.Sprintf("%s: save WAN endpoint: %v", r.Name, err))
				}
			}
		}
		if !replicationMoved {
			log.Debug("ResourceManager.RenumberInResources: only management addresses moved, config kept", zap.String("resource", r.Name))
			continue // DRBD's addresses did not move; only management ones did
		}
		if err := rm.rewriteResourceConfig(ctx, r.Name, func(current string) (string, error) {
			return reconcileDrbdAddresses(current, byHost), nil
		}); err != nil {
			log.Debug("ResourceManager.RenumberInResources: config rewrite failed", zap.String("resource", r.Name), zap.Error(err))
			change.Failed = append(change.Failed, fmt.Sprintf("%s: %v", r.Name, err))
			continue
		}
		log.Debug("ResourceManager.RenumberInResources: config rewritten", zap.String("resource", r.Name))
		change.Resources = append(change.Resources, r.Name)
	}
	sort.Strings(change.Resources)
	log.Debug("ResourceManager.RenumberInResources: done", zap.Strings("resources", change.Resources),
		zap.Int("failed", len(change.Failed)), zap.Strings("wan_resources", change.WANResources))
}

func nodeTakesPart(node string, lists ...string) bool {
	for _, l := range lists {
		for _, n := range splitCSV(l) {
			if n == node {
				return true
			}
		}
	}
	return false
}

// drbdAddressStmt is an IPv4 `address` statement, wherever it sits on a line.
var drbdAddressStmt = regexp.MustCompile(`(\baddress\s+(?:ipv4\s+)?)(\d+\.\d+\.\d+\.\d+):(\d+)`)

// reconcileDrbdAddresses sets the address of every `on <host>` stanza whose
// host is in addrByHost to that address, keeping its port. It does not look
// at what the address was, so a config left half-rewritten — some stanzas
// renumbered, some not, two peers briefly on one address — comes out right
// either way. Loopback addresses belong to WAN proxy legs and are left alone,
// as is every host the map does not know.
func reconcileDrbdAddresses(config string, addrByHost map[string]string) string {
	log := zap.L()
	log.Debug("reconcileDrbdAddresses: start", zap.Int("config_bytes", len(config)), zap.Int("hosts", len(addrByHost)))
	lines := strings.Split(config, "\n")
	depth := 0
	host := ""
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if host == "" && depth == 1 && strings.HasPrefix(trimmed, "on ") && strings.Contains(trimmed, "{") {
			if f := strings.Fields(trimmed); len(f) >= 2 {
				host = f[1]
				_, known := addrByHost[host]
				log.Debug("reconcileDrbdAddresses: entering host stanza", zap.String("host", host), zap.Bool("known", known))
			}
		}
		if want, ok := addrByHost[host]; host != "" && ok && want != "" {
			lines[i] = drbdAddressStmt.ReplaceAllStringFunc(line, func(stmt string) string {
				m := drbdAddressStmt.FindStringSubmatch(stmt)
				if strings.HasPrefix(m[2], "127.") {
					log.Debug("reconcileDrbdAddresses: loopback address left alone", zap.String("host", host), zap.String("address", m[2]))
					return stmt
				}
				log.Debug("reconcileDrbdAddresses: address set", zap.String("host", host),
					zap.String("from", m[2]), zap.String("to", want), zap.String("port", m[3]))
				return m[1] + want + ":" + m[3]
			})
		}
		depth += strings.Count(line, "{") - strings.Count(line, "}")
		if host != "" && depth <= 1 {
			host = ""
		}
	}
	return strings.Join(lines, "\n")
}

// drbdAddressesByHost maps every registered node's DRBD host name to the
// address DRBD should use for it.
func (nm *NodeManager) drbdAddressesByHost() map[string]string {
	nm.mu.RLock()
	defer nm.mu.RUnlock()
	out := make(map[string]string, len(nm.nodes))
	for _, n := range nm.nodes {
		name := n.Hostname
		if name == "" {
			name = n.Name
		}
		out[name] = replicationOf(n)
	}
	loggerOfController(nm.controller).Debug("NodeManager.drbdAddressesByHost: built", zap.Any("addresses", out))
	return out
}
