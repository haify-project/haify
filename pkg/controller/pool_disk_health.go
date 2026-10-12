package controller

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/deployment"
	"github.com/haify-project/haify/pkg/event"
	"github.com/haify-project/haify/pkg/inspect"
)

// The disks under the pools and their health (inspect.DiskProbeScript), for
// `haify pool disks` and for the daily inspection.

const diskProbeTimeout = 2 * time.Minute

// DiskHealth lists the disks of pool on node; empty pool or node means all.
func (sm *StorageManager) DiskHealth(ctx context.Context, pool, node string) ([]inspect.Disk, error) {
	c := sm.controller
	log := loggerOfController(c)
	log.Debug("DiskHealth: start", zap.String("pool", pool), zap.String("node", node))
	nodes, err := c.nodes.ListNodes(ctx)
	if err != nil {
		log.Debug("DiskHealth: listing nodes failed", zap.Error(err))
		return nil, err
	}
	var hosts []string
	nameOf := map[string]string{}
	for _, n := range nodes {
		if (node != "" && n.Name != node && n.Address != node) || (node == "" && n.State != NodeStateOnline) {
			log.Debug("DiskHealth: node skipped", zap.String("node", n.Name), zap.String("state", string(n.State)))
			continue
		}
		log.Debug("DiskHealth: node selected", zap.String("node", n.Name), zap.String("host", n.Address))
		hosts = append(hosts, n.Address)
		nameOf[n.Address] = n.Name
	}
	if len(hosts) == 0 {
		if node != "" {
			log.Debug("DiskHealth: node not registered", zap.String("node", node))
			return nil, fmt.Errorf("node %q is not registered", node)
		}
		log.Debug("DiskHealth: no online nodes")
		return nil, nil
	}
	cmd := "echo " + base64Std(inspect.DiskProbeScript) + " | base64 -d | sudo /bin/bash"
	start := time.Now()
	res, err := c.deployment.Exec(ctx, hosts, cmd, deployment.WithExecTimeout(diskProbeTimeout))
	if err != nil {
		log.Debug("DiskHealth: disk probe failed", zap.Strings("hosts", hosts),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		return nil, err
	}
	if ce := log.Check(zap.DebugLevel, "DiskHealth: disk probe ran"); ce != nil {
		ce.Write(zap.Strings("hosts", hosts), zap.Bool("success", res.AllSuccess()),
			zap.Strings("failed_hosts", res.FailedHosts()), zap.Duration("took", time.Since(start)))
	}
	want := normalizeManagedName(pool)
	var out []inspect.Disk
	for _, h := range hosts {
		hr := res.Hosts[h]
		if hr == nil {
			log.Debug("DiskHealth: no result from host", zap.String("host", h))
			continue
		}
		n := 0
		for _, d := range inspect.ParseDiskProbe(nameOf[h], hr.Output) {
			if pool == "" || d.Pool == want || d.Pool == pool {
				out = append(out, d)
				n++
			}
		}
		log.Debug("DiskHealth: host disks parsed", zap.String("host", h), zap.String("node", nameOf[h]),
			zap.Int("count", n))
	}
	log.Debug("DiskHealth: done", zap.String("pool", pool), zap.String("node", node), zap.Int("count", len(out)))
	return out, nil
}

// gatherDisks adds disk health to an inspection, and raises disk.health for
// each disk in trouble so it reaches the notification channels by itself
// rather than inside the inspection summary.
func (im *InspectionManager) gatherDisks(ctx context.Context, in *inspect.Input) {
	c := im.controller
	log := loggerOfController(c)
	log.Debug("InspectionManager.gatherDisks: start")
	disks, err := c.storage.DiskHealth(ctx, "", "")
	if err != nil {
		log.Debug("InspectionManager.gatherDisks: disk health failed, not probed", zap.Error(err))
		return
	}
	in.Disks, in.DisksProbed = disks, true
	if c.events == nil {
		log.Debug("InspectionManager.gatherDisks: no event bus", zap.Int("count", len(disks)))
		return
	}
	for _, d := range disks {
		sev := event.SeverityWarning
		switch d.Status {
		case inspect.DiskFail:
			sev = event.SeverityCritical
		case inspect.DiskWarn:
		default:
			continue
		}
		log.Debug("InspectionManager.gatherDisks: disk in trouble, publishing", zap.String("node", d.Node),
			zap.String("device", d.Device), zap.String("pool", d.Pool), zap.String("status", string(d.Status)))
		c.events.Publish(event.Event{Type: event.TypeDiskHealth, Severity: sev, Status: event.StatusInfo, Node: d.Node,
			Message: fmt.Sprintf("disk %s under pool %s on %s: %s; replace it with `haify pool replace-disk --pool %s --node %s --disk %s --new-disk <device>`",
				d.Device, d.Pool, d.Node, d.Detail, d.Pool, d.Node, d.PV),
			Details: map[string]string{"device": d.Device, "pool": d.Pool, "model": d.Model, "serial": d.Serial}})
	}
	log.Debug("InspectionManager.gatherDisks: done", zap.Int("count", len(disks)))
}
