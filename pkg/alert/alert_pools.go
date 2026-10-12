package alert

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/event"
)

// checkPools raises capacity conditions for every thin pool in the cluster.
//
// This is the one health signal that no other check can stand in for. A thin
// pool that runs out of data space stops accepting writes; the kernel then
// detaches the backing device, and DRBD reports Diskless on a node configured
// diskful — which surfaces as a resource.degraded alert naming a *replica*
// problem for what is really a *capacity* problem, and only once the damage is
// done. Watching the pool is what makes it preventable.
func (m *Monitor) checkPools(ctx context.Context, sc *pollScope, obs *Observation) {
	if m.pools == nil {
		m.log.Debug("Monitor.checkPools: no pool lister, pool checks skipped")
		return
	}
	obs.Pools.Enabled = true

	start := time.Now()
	pools, err := m.pools.GetPoolStatusList(ctx)
	obs.Pools.Duration = time.Since(start)
	m.log.Debug("Monitor.checkPools: listed pools",
		zap.Int("count", len(pools)), zap.Duration("took", obs.Pools.Duration), zap.Error(err))
	if err != nil {
		obs.Pools.Err = err
		m.log.Warn("alert monitor: list pools failed", zap.Error(err))
		return
	}
	obs.Pools.OK = true
	obs.Pools.Items = pools
	sc.poolsOK = true

	for _, p := range pools {
		// Recorded before the thin check, not after: a pool converted back to
		// thick still exists, and saying it "no longer exists" when clearing its
		// old capacity alert would send an operator looking for a deletion that
		// never happened.
		sc.livePools[poolKey(p.Name, p.Node)] = true
		m.log.Debug("Monitor.checkPools: checking pool",
			zap.String("pool", p.Name), zap.String("node", p.Node), zap.String("thin_pool", p.ThinPool),
			zap.Float64("data_percent", p.DataPercent), zap.Float64("metadata_percent", p.MetaPercent),
			zap.Bool("out_of_space", p.OutOfSpace), zap.Bool("vdo", p.VDO),
			zap.Bool("vdo_physical_known", p.VDOPhysicalKnown), zap.Float64("vdo_physical_percent", p.VDOPhysicalPercent))

		// A group with no thin pool has no utilisation to judge. Its vg_free is
		// a real number, but it is also the number Haify drives to zero on
		// purpose, so there is nothing here to alert on.
		if p.ThinPool == "" {
			m.log.Debug("Monitor.checkPools: no thin pool in group, capacity not judged",
				zap.String("pool", p.Name), zap.String("node", p.Node))
			continue
		}
		m.checkPoolDimension(p, sc, "data", p.DataPercent,
			event.TypePoolDataNearFull, event.TypePoolDataFull)
		m.checkPoolDimension(p, sc, "metadata", p.MetaPercent,
			event.TypePoolMetadataNearFull, event.TypePoolMetadataFull)
		// A VDO pool that runs out of physical space fails writes while the
		// thin pool above it still reports room. Evaluated only while the
		// figure was read: a pool whose VDO usage is unknown this poll has its
		// conditions held as they are, neither raised nor cleared.
		switch {
		case p.VDO && p.VDOPhysicalKnown:
			m.checkPoolDimension(p, sc, "VDO physical", p.VDOPhysicalPercent,
				event.TypePoolVDOPhysicalNearFull, event.TypePoolVDOPhysicalFull)
		case p.VDO:
			m.log.Debug("Monitor.checkPools: VDO physical usage unknown this poll, holding its conditions",
				zap.String("pool", p.Name), zap.String("node", p.Node))
			for _, t := range []event.Type{event.TypePoolVDOPhysicalNearFull, event.TypePoolVDOPhysicalFull} {
				sc.mark(event.Event{Type: t, Resource: p.Name, Node: p.Node}.Key())
			}
		}

		m.level(event.Event{
			Type:     event.TypePoolOutOfSpace,
			Severity: event.SeverityCritical,
			Resource: p.Name,
			Node:     p.Node,
			Details: map[string]string{
				"thin_pool":        p.ThinPool,
				"data_percent":     formatPercent(p.DataPercent),
				"metadata_percent": formatPercent(p.MetaPercent),
			},
		}, sc, sourcePools, p.OutOfSpace,
			fmt.Sprintf("pool %s on %s is out of data space: writes are failing and any DRBD replica on it will drop to Diskless", p.Name, p.Node),
			fmt.Sprintf("pool %s on %s is no longer out of data space", p.Name, p.Node))
	}
}

// checkPoolDimension raises the near-full and full conditions for one
// utilisation dimension of one pool.
//
// The two are mutually exclusive by construction: near-full is only active
// below the critical threshold, so crossing it resolves the warning in the same
// poll that raises the critical. Reporting both at once would double every
// notification at the moment it matters most.
func (m *Monitor) checkPoolDimension(p PoolStatusInfo, sc *pollScope, dimension string, percent float64, nearType, fullType event.Type) {
	m.log.Debug("Monitor.checkPoolDimension: judging pool utilisation",
		zap.String("pool", p.Name), zap.String("node", p.Node), zap.String("dimension", dimension),
		zap.Float64("percent", percent), zap.Float64("near_full_percent", m.nearFull), zap.Float64("full_percent", m.full),
		zap.Bool("full", percent >= m.full), zap.Bool("near_full", percent >= m.nearFull && percent < m.full))
	details := func() map[string]string {
		return map[string]string{
			"thin_pool": p.ThinPool,
			"dimension": dimension,
			"percent":   formatPercent(percent),
			"threshold": formatPercent(m.nearFull) + "/" + formatPercent(m.full),
		}
	}

	m.level(event.Event{
		Type:     fullType,
		Severity: event.SeverityCritical,
		Resource: p.Name,
		Node:     p.Node,
		Details:  details(),
	}, sc, sourcePools, percent >= m.full,
		fmt.Sprintf("pool %s on %s is %s%% %s used: extend it or free space now — a full DRBD resync of the volumes it holds reallocates every block and may not fit",
			p.Name, p.Node, formatPercent(percent), dimension),
		fmt.Sprintf("pool %s on %s %s usage is back under %s%%", p.Name, p.Node, dimension, formatPercent(m.full)))

	m.level(event.Event{
		Type:     nearType,
		Severity: event.SeverityWarning,
		Resource: p.Name,
		Node:     p.Node,
		Details:  details(),
	}, sc, sourcePools, percent >= m.nearFull && percent < m.full,
		fmt.Sprintf("pool %s on %s is %s%% %s used: plan an extension", p.Name, p.Node, formatPercent(percent), dimension),
		fmt.Sprintf("pool %s on %s %s usage is back under %s%%", p.Name, p.Node, dimension, formatPercent(m.nearFull)))
}

// formatPercent renders a utilisation figure the way LVM reports it, to two
// decimal places, so an event repeats what an operator will see in `lvs`.
func formatPercent(v float64) string {
	return strconv.FormatFloat(v, 'f', 2, 64)
}

// poolKey identifies a pool by name and node. A pool name is only unique within
// a node — every node usually carries a pool of the same name — so the node is
// part of the identity, not a label on it.
func poolKey(name, node string) string { return name + "@" + node }
