package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"go.uber.org/zap"
)

// Fault domains. Two replicas on two VMs of the same physical host are one
// failure away from being zero replicas, and a tiebreaker on that host turns
// the same failure into lost quorum for the survivor elsewhere. Haify cannot see
// the host a VM runs on, so the operator says it with a node label — by default
// `host` (`haify node set-labels <node> host=<machine>`), configurable as
// [resource] fault_domain_label.
//
// Automatic placement prefers distinct domains but does not insist: a
// three-VM lab on one machine must still be able to create a resource. When it
// cannot spread, it says so. The explicit replicas-on-different constraint is
// the hard version of the same rule.

// defaultFaultDomainLabel is used when the controller has no configuration.
const defaultFaultDomainLabel = "host"

// faultDomainKey is the node label that names a fault domain.
func (rm *ResourceManager) faultDomainKey() string {
	if rm.controller != nil && rm.controller.config != nil {
		if k := strings.TrimSpace(rm.controller.config.Resource.FaultDomainLabel); k != "" {
			loggerOfController(rm.controller).Debug("ResourceManager.faultDomainKey: configured label", zap.String("key", k))
			return k
		}
	}
	loggerOfController(rm.controller).Debug("ResourceManager.faultDomainKey: default label", zap.String("key", defaultFaultDomainLabel))
	return defaultFaultDomainLabel
}

// faultDomain names the domain a node belongs to. A node without the label is
// a domain of its own, so an unlabelled cluster places exactly as before.
func faultDomain(node string, labels map[string]string, key string) string {
	if v := labels[key]; v != "" {
		return key + "=" + v
	}
	return "node:" + node
}

// labelledDomain reports whether the domain came from the label rather than
// standing in for a single node.
func labelledDomain(d string) bool {
	return !strings.HasPrefix(d, "node:")
}

// withDomainLabels returns the candidates with key filled in for nodes that
// lack it, each with a value of its own, so key can be used as an on-different
// constraint without excluding unlabelled nodes.
func withDomainLabels(nodes []placementNode, key string) []placementNode {
	out := make([]placementNode, len(nodes))
	for i, n := range nodes {
		labels := make(map[string]string, len(n.labels)+1)
		for k, v := range n.labels {
			labels[k] = v
		}
		labels[key] = strings.TrimPrefix(faultDomain(n.node, n.labels, key), key+"=")
		n.labels = labels
		out[i] = n
	}
	zap.L().Debug("withDomainLabels: filled in fault domain labels", zap.String("key", key), zap.Int("count", len(out)))
	return out
}

// spreadAcrossDomains places replicas under the hard constraints c, preferring
// a selection in which no two replicas share a fault domain. warning is set
// when the preference could not be met.
func spreadAcrossDomains(nodes []placementNode, replicas int, c placementConstraints, key string) ([]string, string, error) {
	log := zap.L()
	log.Debug("spreadAcrossDomains: start", zap.Int("candidates", len(nodes)), zap.Int("replicas", replicas),
		zap.String("key", key), zap.Strings("on_different", c.onDifferent), zap.Strings("on_same", c.onSame))
	spread := c
	if !containsString(c.onDifferent, key) {
		spread.onDifferent = append(append([]string(nil), c.onDifferent...), key)
	}
	picked, err := selectConstrained(withDomainLabels(nodes, key), replicas, spread)
	if err == nil {
		log.Debug("spreadAcrossDomains: replicas spread over distinct fault domains", zap.Strings("nodes", picked))
		return picked, "", nil
	}
	log.Debug("spreadAcrossDomains: cannot spread, falling back to the hard constraints only", zap.Error(err))
	picked, err = selectConstrained(nodes, replicas, c)
	if err != nil {
		log.Debug("spreadAcrossDomains: hard constraints cannot be met either", zap.Error(err))
		return nil, "", err
	}
	labels := make(map[string]map[string]string, len(nodes))
	for _, n := range nodes {
		labels[n.node] = n.labels
	}
	warning := sharedDomainWarning(picked, labels, key)
	log.Debug("spreadAcrossDomains: placed with shared fault domains", zap.Strings("nodes", picked), zap.String("warning", warning))
	return picked, warning, nil
}

// sharedDomainWarning describes replicas that ended up in one fault domain.
func sharedDomainWarning(nodes []string, labels map[string]map[string]string, key string) string {
	byDomain := map[string][]string{}
	for _, n := range nodes {
		d := faultDomain(n, labels[n], key)
		byDomain[d] = append(byDomain[d], n)
	}
	var parts []string
	for d, ns := range byDomain {
		if len(ns) > 1 {
			parts = append(parts, fmt.Sprintf("%s share %s", strings.Join(ns, " and "), d))
		}
	}
	if len(parts) == 0 {
		zap.L().Debug("sharedDomainWarning: every replica in its own fault domain", zap.Strings("nodes", nodes))
		return ""
	}
	sort.Strings(parts)
	zap.L().Debug("sharedDomainWarning: replicas share fault domains", zap.Strings("shared", parts))
	return fmt.Sprintf("replicas %s: one failure there loses every copy on it; no candidate set spans more %q values",
		strings.Join(parts, "; "), key)
}

// faultDomainRisk names the fault domain whose loss would take the resource
// down: one holding every diskful replica (the data), or a majority of the
// voters (quorum). Empty when no labelled domain does. Unlabelled nodes are
// their own domains and never trip it, so it only speaks once the operator has
// said which nodes fail together.
func faultDomainRisk(diskful, tiebreakers []string, labels map[string]map[string]string, key string) string {
	log := zap.L()
	if len(diskful) < 2 {
		log.Debug("faultDomainRisk: fewer than two diskful replicas, nothing to judge", zap.Strings("diskful", diskful))
		return ""
	}
	voters := map[string]int{}
	data := map[string]int{}
	for _, n := range diskful {
		d := faultDomain(n, labels[n], key)
		voters[d]++
		data[d]++
	}
	for _, n := range tiebreakers {
		voters[faultDomain(n, labels[n], key)]++
	}
	total := len(diskful) + len(tiebreakers)
	var risky []string
	for d, v := range voters {
		if !labelledDomain(d) {
			continue
		}
		if data[d] == len(diskful) || 2*v >= total {
			log.Debug("faultDomainRisk: domain holds every copy or half the voters", zap.String("domain", d),
				zap.Int("data", data[d]), zap.Int("voters", v), zap.Int("total", total))
			risky = append(risky, d)
		}
	}
	sort.Strings(risky)
	log.Debug("faultDomainRisk: done", zap.Strings("diskful", diskful), zap.Strings("tiebreakers", tiebreakers),
		zap.Strings("risky", risky))
	return strings.Join(risky, ",")
}

// labelsByNode maps every registered node to its labels.
func (rm *ResourceManager) labelsByNode(ctx context.Context) map[string]map[string]string {
	out := map[string]map[string]string{}
	if rm.controller == nil || rm.controller.nodes == nil {
		zap.L().Debug("ResourceManager.labelsByNode: no node manager, no labels")
		return out
	}
	nodes, err := rm.controller.nodes.ListNodes(ctx)
	if err != nil {
		loggerOfController(rm.controller).Debug("ResourceManager.labelsByNode: listing nodes failed", zap.Error(err))
		return out
	}
	for _, n := range nodes {
		if n != nil {
			out[n.Name] = n.Labels
		}
	}
	return out
}
