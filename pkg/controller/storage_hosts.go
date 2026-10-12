package controller

import (
	"context"

	"go.uber.org/zap"
)

// poolHosts are the hosts a live pool listing asks: every registered host
// except those the node registry holds offline. A node that stopped answering
// does not refuse SSH, it lets the connection hang, so asking it cost every
// listing a TCP timeout per query — several per listing, one listing per
// placement decision — and `haify pool list` failed outright with one node down.
// What is known of its pools is still in the database.
func (sm *StorageManager) poolHosts(ctx context.Context) []string {
	log := sm.controller.traceLogger()
	hosts := sm.controller.GetHosts()
	if sm.controller.nodes == nil {
		log.Debug("poolHosts: no node registry, asking every host", zap.Strings("hosts", hosts))
		return hosts
	}
	nodes, err := sm.controller.nodes.ListNodes(ctx)
	if err != nil {
		log.Debug("poolHosts: node list failed, asking every host", zap.Strings("hosts", hosts), zap.Error(err))
		return hosts
	}
	offline := map[string]bool{}
	for _, n := range nodes {
		if n.State == NodeStateOffline {
			log.Debug("poolHosts: skipping offline node", zap.String("node", n.Name), zap.String("address", n.Address))
			offline[n.Address] = true
			offline[sm.controller.ResolveHost(n.Address)] = true
		}
	}
	if len(offline) == 0 {
		log.Debug("poolHosts: every node online", zap.Strings("hosts", hosts))
		return hosts
	}
	live := make([]string, 0, len(hosts))
	for _, h := range hosts {
		if !offline[h] && !offline[sm.controller.ResolveHost(h)] {
			live = append(live, h)
		}
	}
	log.Debug("poolHosts: live hosts", zap.Strings("hosts", live), zap.Int("all", len(hosts)))
	return live
}
