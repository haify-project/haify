package controller

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/alert"
	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/metrics"
)

type backupLister interface {
	ListBackups(ctx context.Context, resource, target string) ([]*database.Backup, error)
}

type resourceLister interface {
	ListResources(ctx context.Context) ([]*ResourceInfo, error)
}

// observeHealth exports what the alerting decides on: which nodes answered,
// how full each thin pool is, what is raised, how fresh the backups are, and
// which resources one machine's loss would take down.
func (o *metricsObserver) observeHealth(obs alert.Observation) {
	log := o.debugLog()
	if obs.Nodes.Enabled && obs.Nodes.OK {
		reach := make([]metrics.NodeReach, 0, len(obs.Nodes.Items))
		for _, n := range obs.Nodes.Items {
			reach = append(reach, metrics.NodeReach{Node: n.Name, Reachable: n.Reachable})
		}
		o.metrics.SetNodeReachability(reach)
		log.Debug("metricsObserver.observeHealth: exported node reachability", zap.Int("count", len(reach)))
	} else {
		log.Debug("metricsObserver.observeHealth: node reachability not exported",
			zap.Bool("enabled", obs.Nodes.Enabled), zap.Bool("ok", obs.Nodes.OK))
	}
	if obs.Pools.Enabled && obs.Pools.OK {
		var thin []metrics.ThinUsage
		for _, p := range obs.Pools.Items {
			if p.ThinPool != "" {
				thin = append(thin, metrics.ThinUsage{Pool: p.Name, Node: p.Node, DataPercent: p.DataPercent, MetaPercent: p.MetaPercent})
			}
		}
		o.metrics.SetThinPoolUsage(thin)
		log.Debug("metricsObserver.observeHealth: exported thin pool usage", zap.Int("count", len(thin)))
	} else {
		log.Debug("metricsObserver.observeHealth: thin pool usage not exported",
			zap.Bool("enabled", obs.Pools.Enabled), zap.Bool("ok", obs.Pools.OK))
	}
	firing := make([]string, 0, len(obs.Firing))
	for _, f := range obs.Firing {
		firing = append(firing, f.Type+"/"+f.Severity)
	}
	o.metrics.SetFiringAlerts(firing)
	log.Debug("metricsObserver.observeHealth: exported firing alerts", zap.Strings("firing", firing))
	o.observeBackups()
	o.observeFaultDomains()
}

// observeBackups exports the newest completed backup per resource and target.
func (o *metricsObserver) observeBackups() {
	log := o.debugLog()
	if o.backups == nil {
		log.Debug("metricsObserver.observeBackups: no database, skipping")
		return
	}
	start := time.Now()
	all, err := o.backups.ListBackups(o.baseContext(), "", "")
	log.Debug("metricsObserver.observeBackups: listed backups", zap.Int("count", len(all)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return
	}
	newest := map[string]*database.Backup{}
	for _, b := range all {
		if b.State != database.BackupStateCompleted {
			continue
		}
		key := b.Resource + "\x00" + b.Target
		if cur, ok := newest[key]; !ok || b.StartedAt.After(cur.StartedAt) {
			newest[key] = b
		}
	}
	out := make([]metrics.BackupState, 0, len(newest))
	for _, b := range newest {
		kind, shipped := database.BackupKindFull, b.TotalBytes
		if b.Kind == database.BackupKindIncremental {
			kind, shipped = b.Kind, 0
			for _, v := range b.Volumes {
				shipped += v.ChangedBytes
			}
		}
		log.Debug("metricsObserver.observeBackups: newest backup", zap.String("resource", b.Resource),
			zap.String("target", b.Target), zap.String("kind", string(kind)), zap.Time("started", b.StartedAt))
		out = append(out, metrics.BackupState{
			Resource: b.Resource, Target: b.Target, Kind: kind,
			StartedUnix: float64(b.StartedAt.Unix()), ShippedBytes: shipped,
		})
	}
	o.metrics.SetBackups(out)
	log.Debug("metricsObserver.observeBackups: exported", zap.Int("count", len(out)))
}

// observeFaultDomains exports which resources sit in a single fault domain.
func (o *metricsObserver) observeFaultDomains() {
	log := o.debugLog()
	if o.resources == nil {
		log.Debug("metricsObserver.observeFaultDomains: no resource manager, skipping")
		return
	}
	start := time.Now()
	list, err := o.resources.ListResources(o.baseContext())
	log.Debug("metricsObserver.observeFaultDomains: listed resources", zap.Int("count", len(list)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return
	}
	risks := make(map[string]string, len(list))
	for _, r := range list {
		if r.FaultDomainRisk != "" {
			log.Debug("metricsObserver.observeFaultDomains: resource at risk", zap.String("resource", r.Name),
				zap.String("risk", r.FaultDomainRisk))
			risks[r.Name] = r.FaultDomainRisk
		}
	}
	o.metrics.SetFaultDomainRisks(risks)
	log.Debug("metricsObserver.observeFaultDomains: exported", zap.Int("at_risk", len(risks)))
}

// peerTLS is nil for the node whose status was read, which has no connection
// to describe.
func peerTLS(state alert.NodeStateInfo) *bool {
	if state.Connection == "" {
		return nil
	}
	tls := state.TLS
	return &tls
}
