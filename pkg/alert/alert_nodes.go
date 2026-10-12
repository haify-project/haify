package alert

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/event"
)

func (m *Monitor) checkNodes(ctx context.Context, sc *pollScope, obs *Observation) {
	if m.nodes == nil {
		m.log.Debug("Monitor.checkNodes: no node lister, node checks skipped")
		return
	}
	obs.Nodes.Enabled = true

	start := time.Now()
	nodes, err := m.nodes.GetNodeStatusList(ctx)
	obs.Nodes.Duration = time.Since(start)
	m.log.Debug("Monitor.checkNodes: listed nodes",
		zap.Int("count", len(nodes)), zap.Duration("took", obs.Nodes.Duration), zap.Error(err))
	if err != nil {
		obs.Nodes.Err = err
		m.log.Warn("alert monitor: list nodes failed", zap.Error(err))
		return
	}
	obs.Nodes.OK = true
	obs.Nodes.Items = nodes
	sc.nodesOK = true

	for _, n := range nodes {
		msg := n.Message
		if msg == "" {
			msg = "no response"
		}
		m.log.Debug("Monitor.checkNodes: checking node",
			zap.String("node", n.Name), zap.Bool("reachable", n.Reachable), zap.String("message", n.Message))
		m.level(event.Event{
			Type:     event.TypeNodeUnreachable,
			Severity: event.SeverityCritical,
			Node:     n.Name,
		}, sc, sourceNodes, !n.Reachable,
			fmt.Sprintf("node %s is unreachable: %s", n.Name, msg),
			fmt.Sprintf("node %s is reachable again", n.Name))
	}
}
