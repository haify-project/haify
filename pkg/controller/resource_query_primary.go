package controller

import "go.uber.org/zap"

// The node that answers a status query reports what it sent to and received
// from each peer. Only the Primary's view carries the traffic on every
// replication link — a Secondary sees its own link to the Primary and nothing
// of the Primary's links to the other replicas — so the Primary is asked
// first whenever it is known.

// statusAskOrder returns the indexes of hosts in the order to ask them: the
// resource's last-seen Primary first, the rest in their configured order.
func (rm *ResourceManager) statusAskOrder(resource string, hosts []string) []int {
	order := make([]int, 0, len(hosts))
	first := -1
	if v, ok := rm.primaryHosts.Load(resource); ok {
		for i, h := range hosts {
			if h == v.(string) {
				first = i
				order = append(order, i)
				break
			}
		}
	}
	for i := range hosts {
		if i != first {
			order = append(order, i)
		}
	}
	order = rm.unreachableLast(hosts, order)
	rm.resourcesLog().Debug("statusAskOrder: order", zap.String("resource", resource), zap.Strings("hosts", hosts),
		zap.Bool("known_primary_first", first >= 0), zap.Ints("order", order))
	return order
}

// rememberPrimaryHost records which of hosts is the resource's Primary, as
// the status just read says. answeredAt indexes both hosts and nodes. Peers
// are keyed by their DRBD host name, which is either the registered node name
// or resolves to the node's address.
func (rm *ResourceManager) rememberPrimaryHost(resource string, hosts, nodes []string, answeredAt int, localRole string, states map[string]*ResourceNodeState) {
	log := rm.resourcesLog()
	if answeredAt < 0 || answeredAt >= len(hosts) {
		log.Debug("rememberPrimaryHost: no answering host", zap.String("resource", resource), zap.Int("answered_at", answeredAt))
		return
	}
	if localRole == "Primary" {
		log.Debug("rememberPrimaryHost: answering host is Primary", zap.String("resource", resource), zap.String("host", hosts[answeredAt]))
		rm.primaryHosts.Store(resource, hosts[answeredAt])
		return
	}
	for name, st := range states {
		if st == nil || st.Role != "Primary" {
			continue
		}
		for i, h := range hosts {
			if (i < len(nodes) && nodes[i] == name) || h == name || h == rm.controller.ResolveHost(name) {
				log.Debug("rememberPrimaryHost: peer is Primary", zap.String("resource", resource), zap.String("node", name), zap.String("host", h))
				rm.primaryHosts.Store(resource, h)
				return
			}
		}
		log.Debug("rememberPrimaryHost: Primary peer not among hosts", zap.String("resource", resource), zap.String("node", name))
	}
}
