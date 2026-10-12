package controller

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/deployment"
	"github.com/haify-project/haify/pkg/event"
	"github.com/haify-project/haify/pkg/inspect"
)

// alertWindow is how far back the inspection looks for alerts that reached
// no channel: one daily schedule's worth.
const alertWindow = 24 * time.Hour

// gather builds the inspection input: the database's view of the cluster plus
// one probe round to every node. Nothing here fails the run; what could not
// be read is left empty and the checks report it.
func (im *InspectionManager) gather(ctx context.Context) *inspect.Input {
	c := im.controller
	log := c.traceLogger()
	log.Debug("InspectionManager.gather: starting")
	in := &inspect.Input{
		Now:         im.now(),
		Probes:      map[string]*inspect.NodeProbe{},
		ProbeErrors: map[string]string{},
	}
	if c.config != nil {
		in.PoolNearFull = c.config.Alert.PoolNearFullPercent
		in.PoolFull = c.config.Alert.PoolFullPercent
	}
	start := time.Now()
	nodes, err := c.nodes.ListNodes(ctx)
	log.Debug("InspectionManager.gather: listed nodes", zap.Int("count", len(nodes)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err == nil {
		for _, n := range nodes {
			log.Debug("InspectionManager.gather: node", zap.String("node", n.Name), zap.String("address", n.Address))
			in.Nodes = append(in.Nodes, inspect.Node{Name: n.Name, Address: n.Address,
				ReplicationAddress: n.ReplicationAddress, Hostname: n.Hostname})
		}
	}
	im.probe(ctx, in)
	im.gatherResources(ctx, in)
	im.gatherAlerts(ctx, in)
	im.gatherBackups(ctx, in)
	im.gatherTLS(in)
	im.gatherDisks(ctx, in)
	reports, err := im.List(ctx, 0)
	if err == nil {
		for _, r := range reports {
			if len(r.Pools) > 0 {
				log.Debug("InspectionManager.gather: previous report with pool samples", zap.String("id", r.ID))
				in.Previous = r
				break
			}
		}
	}
	log.Debug("InspectionManager.gather: done", zap.Int("nodes", len(in.Nodes)), zap.Int("probes", len(in.Probes)),
		zap.Int("probe_errors", len(in.ProbeErrors)), zap.Int("resources", len(in.Resources)),
		zap.Bool("has_previous", in.Previous != nil))
	return in
}

// probe runs inspect.ProbeScript on every node in one SSH round.
func (im *InspectionManager) probe(ctx context.Context, in *inspect.Input) {
	c := im.controller
	log := c.traceLogger()
	if len(in.Nodes) == 0 {
		log.Debug("InspectionManager.probe: no nodes, skipping probe")
		return
	}
	addrs := make([]string, 0, len(in.Nodes))
	for _, n := range in.Nodes {
		addrs = append(addrs, n.Address)
	}
	cmd := "echo " + base64Std(inspect.ProbeScript) + " | base64 -d | sudo /bin/bash"
	in.ProbeStart = im.now()
	start := time.Now()
	res, err := c.deployment.Exec(ctx, addrs, cmd, deployment.WithExecTimeout(inspectionProbeTimeout))
	in.ProbeEnd = im.now()
	if ce := log.Check(zap.DebugLevel, "InspectionManager.probe: ran probe script"); ce != nil {
		var failed []string
		if res != nil {
			failed = res.FailedHosts()
		}
		ce.Write(zap.Strings("hosts", addrs), zap.Strings("failed_hosts", failed),
			zap.Duration("took", time.Since(start)), zap.Error(err))
	}
	for _, n := range in.Nodes {
		if err != nil {
			log.Debug("InspectionManager.probe: probe round failed for node", zap.String("node", n.Name), zap.Error(err))
			in.ProbeErrors[n.Name] = err.Error()
			continue
		}
		var hr *deployment.HostResult
		if res != nil {
			hr = res.Hosts[n.Address]
		}
		if hr == nil {
			log.Debug("InspectionManager.probe: no result from node", zap.String("node", n.Name), zap.String("host", n.Address))
			in.ProbeErrors[n.Name] = "no result from " + n.Address
			continue
		}
		// A probe that ran but exited non-zero still printed what it found;
		// only output that is not the probe's at all is a failure.
		p, perr := inspect.ParseProbe(hr.Output)
		if perr != nil {
			msg := perr.Error()
			if hr.Error != nil {
				msg = hr.Error.Error() + ": " + msg
			}
			log.Debug("InspectionManager.probe: probe output unparsable", zap.String("node", n.Name), zap.String("error", msg))
			in.ProbeErrors[n.Name] = msg
			continue
		}
		log.Debug("InspectionManager.probe: probe parsed", zap.String("node", n.Name), zap.Bool("exit_error", hr.Error != nil))
		in.Probes[n.Name] = p
	}
}

// nodeRef maps a stored node reference (name, hostname or address) to the
// registered node name.
func (im *InspectionManager) nodeRef(ref string) string {
	if name := im.controller.nodes.GetNodeNameByAddress(ref); name != "" {
		return name
	}
	return ref
}

func (im *InspectionManager) nodeRefs(refs []string) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		if r = strings.TrimSpace(r); r != "" {
			out = append(out, im.nodeRef(r))
		}
	}
	return out
}

func (im *InspectionManager) gatherResources(ctx context.Context, in *inspect.Input) {
	c := im.controller
	log := c.traceLogger()
	log.Debug("InspectionManager.gatherResources: starting")
	served := map[string]string{}
	start := time.Now()
	has, err := c.db.ListHaConfigs(ctx)
	log.Debug("InspectionManager.gatherResources: listed HA configs", zap.Int("count", len(has)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err == nil {
		for _, h := range has {
			served[h.Resource] = "ha"
		}
	}
	start = time.Now()
	_, err = c.db.GetHaConfig(ctx, SelfHaResource)
	log.Debug("InspectionManager.gatherResources: read self-HA config", zap.String("resource", SelfHaResource),
		zap.Bool("found", err == nil), zap.Duration("took", time.Since(start)))
	if err == nil {
		served[SelfHaResource] = "self-ha"
	}
	start = time.Now()
	gws, err := c.db.ListGateways(ctx)
	log.Debug("InspectionManager.gatherResources: listed gateways", zap.Int("count", len(gws)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err == nil {
		for _, g := range gws {
			in.Gateways = append(in.Gateways, inspect.Gateway{Resource: g.Resource, Type: string(g.Type), Status: g.Status})
		}
	}
	start = time.Now()
	infos, err := c.resources.ListResources(ctx)
	log.Debug("InspectionManager.gatherResources: listed resources", zap.Int("count", len(infos)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		c.logger.Warn("Inspection: list resources", zap.Error(err))
		return
	}
	for _, r := range infos {
		res := inspect.Resource{
			Name:            r.Name,
			Diskful:         im.nodeRefs(r.Nodes),
			Tiebreakers:     im.nodeRefs(r.DisklessNodes),
			Clients:         im.nodeRefs(r.DisklessClients),
			ServedBy:        served[r.Name],
			QuorumRisk:      r.QuorumRisk,
			FaultDomainRisk: r.FaultDomainRisk,
		}
		if r.WANMode && r.DRNode != "" {
			res.DR = im.nodeRef(r.DRNode)
		}
		log.Debug("InspectionManager.gatherResources: resource", zap.String("resource", r.Name),
			zap.Strings("diskful", res.Diskful), zap.String("served_by", res.ServedBy), zap.String("dr", res.DR))
		in.Resources = append(in.Resources, res)
		if res.ServedBy == "self-ha" {
			in.SelfHA = &inspect.SelfHAInput{Resource: r.Name, Nodes: res.Diskful}
		}
	}
	if served[SelfHaResource] == "self-ha" && in.SelfHA == nil {
		log.Debug("InspectionManager.gatherResources: self-HA configured but its resource is not listed",
			zap.String("resource", SelfHaResource))
		in.SelfHA = &inspect.SelfHAInput{Resource: SelfHaResource}
	}
}

// gatherAlerts collects the channels, their delivery records and the alerts
// raised in the window.
func (im *InspectionManager) gatherAlerts(ctx context.Context, in *inspect.Input) {
	c := im.controller
	log := c.traceLogger()
	if c.config == nil || !c.config.Alert.Enabled {
		log.Debug("InspectionManager.gatherAlerts: alerting off, skipping", zap.Bool("has_config", c.config != nil))
		return
	}
	in.Alerts.Enabled = true
	in.Alerts.Since = in.Now.Add(-alertWindow)
	start := time.Now()
	records, rerr := c.db.ListNotifyDeliveries(ctx)
	log.Debug("InspectionManager.gatherAlerts: read delivery records", zap.Int("count", len(records)),
		zap.Duration("took", time.Since(start)), zap.Error(rerr))
	target := func(name string, enabled bool, filter event.Filter) inspect.DeliveryTarget {
		t := inspect.DeliveryTarget{Name: name, Enabled: enabled, Accepts: func(e inspect.AlertEvent) bool {
			return filter.Match(event.Event{Type: event.Type(e.Type), Severity: event.Severity(e.Severity), Resource: e.Resource})
		}}
		if d := records[name]; d != nil {
			t.Known = true
			t.LastSuccess, t.LastFailure, t.LastError = d.LastSuccess, d.LastFailure, d.LastError
			t.ConsecutiveFailures = d.ConsecutiveFailures
			t.Failed = map[uint64]bool{}
			for _, id := range d.FailedEvents {
				t.Failed[id] = true
			}
		}
		return t
	}
	start = time.Now()
	channels, err := c.db.ListNotifyChannels(ctx)
	log.Debug("InspectionManager.gatherAlerts: listed notification channels", zap.Int("count", len(channels)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err == nil {
		for _, ch := range channels {
			cfg, err := webhookConfigFor(ch)
			if err != nil {
				// The error is not logged: it describes the channel's
				// configuration, which can hold a webhook URL or password.
				log.Debug("InspectionManager.gatherAlerts: channel configuration unusable", zap.String("channel", ch.Name),
					zap.Bool("enabled", ch.Enabled))
				in.Alerts.Targets = append(in.Alerts.Targets, inspect.DeliveryTarget{Name: ch.Name, Enabled: ch.Enabled,
					Known: true, ConsecutiveFailures: 1, LastFailure: in.Now,
					LastError: "the channel's configuration is unusable: " + err.Error(),
					Accepts:   func(inspect.AlertEvent) bool { return false }})
				continue
			}
			t := target(ch.Name, ch.Enabled, cfg.Filter)
			t.Since = ch.CreatedAt
			log.Debug("InspectionManager.gatherAlerts: channel", zap.String("channel", ch.Name), zap.Bool("enabled", ch.Enabled),
				zap.Bool("known", t.Known), zap.Int("consecutive_failures", t.ConsecutiveFailures))
			in.Alerts.Targets = append(in.Alerts.Targets, t)
		}
	}
	for _, wh := range c.config.Alert.Receivers() {
		name := receiverName(wh.URL)
		log.Debug("InspectionManager.gatherAlerts: config receiver", zap.String("channel", name),
			zap.String("min_severity", wh.MinSeverity))
		in.Alerts.Targets = append(in.Alerts.Targets,
			target(name, true, event.Filter{MinSeverity: event.ParseSeverity(wh.MinSeverity)}))
	}
	if c.events == nil {
		log.Debug("InspectionManager.gatherAlerts: no event bus", zap.Int("targets", len(in.Alerts.Targets)))
		return
	}
	for _, e := range c.events.Recent(event.Filter{MinSeverity: event.SeverityWarning}, 0, 0) {
		if e.Status != event.StatusFiring || e.Timestamp.Before(in.Alerts.Since) {
			continue
		}
		in.Alerts.Events = append(in.Alerts.Events, inspect.AlertEvent{ID: e.ID, Type: string(e.Type),
			Severity: string(e.Severity), Resource: e.Resource, Node: e.Node, Message: e.Message, At: e.Timestamp})
	}
	log.Debug("InspectionManager.gatherAlerts: done", zap.Int("targets", len(in.Alerts.Targets)),
		zap.Int("events", len(in.Alerts.Events)))
}

func (im *InspectionManager) gatherBackups(ctx context.Context, in *inspect.Input) {
	c := im.controller
	log := c.traceLogger()
	b := &in.Backups
	b.SchedulerEnabled = c.config != nil && c.config.Schedule.Enabled
	log.Debug("InspectionManager.gatherBackups: starting", zap.Bool("scheduler_enabled", b.SchedulerEnabled))
	b.Targets = map[string]bool{}
	b.BaseSnapshots = map[string]bool{}
	start := time.Now()
	targets, err := c.db.ListBackupTargets(ctx)
	log.Debug("InspectionManager.gatherBackups: listed backup targets", zap.Int("count", len(targets)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err == nil {
		for _, t := range targets {
			b.Targets[t.Name] = true
		}
	}
	lastSuccess := map[string]time.Time{}
	start = time.Now()
	backups, err := c.db.ListBackups(ctx, "", "")
	log.Debug("InspectionManager.gatherBackups: listed backups", zap.Int("count", len(backups)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err == nil {
		for _, rec := range backups {
			if rec.State == database.BackupStateRunning {
				b.Running = true
			}
			if rec.State == database.BackupStateCompleted && rec.Schedule != "" && rec.FinishedAt.After(lastSuccess[rec.Schedule]) {
				lastSuccess[rec.Schedule] = rec.FinishedAt
			}
			for _, v := range rec.Volumes {
				if v.Snapshot != "" {
					b.BaseSnapshots[v.Pool+"/"+v.Snapshot] = true
				}
			}
		}
	}
	start = time.Now()
	scheds, err := c.db.ListBackupSchedules(ctx)
	log.Debug("InspectionManager.gatherBackups: listed backup schedules", zap.Int("count", len(scheds)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err == nil {
		for _, s := range scheds {
			log.Debug("InspectionManager.gatherBackups: backup schedule", zap.String("schedule", s.Name),
				zap.String("resource", s.Resource), zap.Bool("enabled", s.Enabled), zap.Time("last_success", lastSuccess[s.Name]))
			b.Schedules = append(b.Schedules, inspect.BackupSchedule{Name: s.Name, Resource: s.Resource, Target: s.Target,
				Cron: s.Cron, Enabled: s.Enabled, CreatedAt: s.CreatedAt, LastRun: s.LastRun, LastError: s.LastError,
				LastSuccess: lastSuccess[s.Name]})
		}
	}
	start = time.Now()
	snaps, err := c.db.ListSnapshotSchedules(ctx)
	log.Debug("InspectionManager.gatherBackups: listed snapshot schedules", zap.Int("count", len(snaps)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err == nil {
		for _, s := range snaps {
			b.SnapSchedules = append(b.SnapSchedules, inspect.SnapSchedule{Name: s.Name, Resource: s.Resource,
				Cron: s.Cron, Enabled: s.Enabled, CreatedAt: s.CreatedAt, LastRun: s.LastRun})
		}
	}
	log.Debug("InspectionManager.gatherBackups: done", zap.Bool("running", b.Running),
		zap.Int("base_snapshots", len(b.BaseSnapshots)))
}

// gatherTLS reads the certificates kept on the controller itself.
func (im *InspectionManager) gatherTLS(in *inspect.Input) {
	c := im.controller
	log := c.traceLogger()
	if c.config == nil {
		log.Debug("InspectionManager.gatherTLS: no config, skipping")
		return
	}
	if c.config.TLS.Enabled && c.config.TLS.CertFile != "" {
		in.TLS.APICertPath = c.config.TLS.CertFile
		raw, err := os.ReadFile(c.config.TLS.CertFile)
		log.Debug("InspectionManager.gatherTLS: read API certificate", zap.String("path", c.config.TLS.CertFile),
			zap.Int("bytes", len(raw)), zap.Error(err))
		if err != nil {
			in.TLS.APICertErr = err.Error()
		} else if cert, err := inspect.ParseCertFile(raw); err != nil {
			log.Debug("InspectionManager.gatherTLS: API certificate unparsable", zap.Error(err))
			in.TLS.APICertErr = err.Error()
		} else {
			in.TLS.APICert = cert
		}
	} else {
		log.Debug("InspectionManager.gatherTLS: API TLS off", zap.Bool("enabled", c.config.TLS.Enabled))
	}
	dbPath := c.config.Database.Path
	if dbPath == "" {
		dbPath = database.DefaultDBPath
	}
	caPath := filepath.Join(filepath.Dir(dbPath), "drbd-tls", "ca.pem")
	raw, err := os.ReadFile(caPath)
	log.Debug("InspectionManager.gatherTLS: read replication CA", zap.String("path", caPath),
		zap.Int("bytes", len(raw)), zap.Error(err))
	if err != nil {
		// No CA: replication TLS was never set up.
		return
	}
	cert, err := inspect.ParseCertFile(raw)
	if err == nil {
		in.TLS.ReplicationCA, in.TLS.ReplicationCAPath = cert, caPath
	}
	log.Debug("InspectionManager.gatherTLS: replication CA parsed", zap.Bool("ok", err == nil), zap.Error(err))
}

// receiverName names a controller.toml webhook in delivery records, which
// have no channel name to use.
func receiverName(url string) string {
	host := url
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	if i := strings.IndexAny(host, "/?"); i >= 0 {
		host = host[:i]
	}
	return "config:" + host
}

// recordDelivery returns the hook that stores a channel's delivery outcomes.
// The delivery error is not traced: an HTTP client error names the webhook
// URL, token included.
func (c *Controller) recordDelivery(channel string) func(event.Event, error) {
	return func(e event.Event, err error) {
		log := c.traceLogger()
		if c.db == nil || e.Type == event.TypeChannelTest {
			log.Debug("Controller.recordDelivery: not recording", zap.String("channel", channel),
				zap.Bool("has_db", c.db != nil), zap.String("type", string(e.Type)))
			return
		}
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		start := time.Now()
		werr := c.db.RecordNotifyDelivery(ctx, channel, e.ID, time.Now(), msg)
		log.Debug("Controller.recordDelivery: recorded delivery", zap.String("channel", channel), zap.Uint64("event_id", e.ID),
			zap.Bool("delivered", err == nil), zap.Duration("took", time.Since(start)), zap.Error(werr))
		if werr != nil {
			c.logger.Warn("Could not record a delivery result", zap.String("channel", channel), zap.Error(werr))
		}
	}
}
