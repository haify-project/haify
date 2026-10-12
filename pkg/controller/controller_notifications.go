package controller

import (
	"context"
	"encoding/json"
	"time"

	"github.com/haify-project/haify/pkg/alert"
	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/event"
)

// alertOptions assembles what the health detector is asked to watch.
//
// Split out of startNotifications so the wiring can be asserted directly. The
// observer in particular is the kind of connection that goes missing without
// anything failing: the detector keeps raising events, the metrics endpoint
// keeps answering, and only the numbers on it are quietly empty.
func (c *Controller) alertOptions() alert.Options {
	opts := alert.Options{
		Interval:     time.Duration(c.config.Alert.CheckIntervalSec) * time.Second,
		IdleInterval: time.Duration(c.config.Alert.IdleIntervalSec) * time.Second,
		Resources:    c.resources,
		Logger:       c.logger,
		WarningHold:  time.Duration(c.config.Alert.WarningHoldSec) * time.Second,
	}
	// Node reachability costs an SSH round trip per node per poll, so it is a
	// separate switch from the resource checks, which are served from state the
	// controller already gathers.
	if c.config.Alert.CheckNodes {
		opts.Nodes = c.nodes
	}
	if c.config.Alert.CheckPools {
		opts.Pools = c.storage
		opts.NearFullPercent = c.config.Alert.PoolNearFullPercent
		opts.FullPercent = c.config.Alert.PoolFullPercent
	}
	// One poll, two consumers. The state the detector gathers to raise events is
	// the same state a dashboard needs, and collecting it twice would double the
	// SSH round trips to every node while letting the alert and the panel
	// disagree about the same instant — the disagreement an operator notices
	// first and trusts least.
	var obs observers
	if c.metrics != nil {
		obs = append(obs, newMetricsObserver(c))
	}
	// The write-anomaly detector reads the same poll for the DRBD write
	// counters (write_anomaly.go).
	if c.config.Alert.WriteAnomaly.Enabled {
		obs = append(obs, newWriteAnomalyDetector(c))
	}
	// Thin pools grow before they fill (pool_autoextend.go).
	if c.config.Storage.Thin.AutoextendThreshold > 0 {
		obs = append(obs, newThinAutoextender(c))
	}
	if len(obs) > 0 {
		opts.Observer = obs
	}
	c.traceLogger().Debug("Controller.alertOptions: built", zap.Duration("interval", opts.Interval),
		zap.Duration("idle_interval", opts.IdleInterval), zap.Bool("check_nodes", c.config.Alert.CheckNodes),
		zap.Bool("check_pools", c.config.Alert.CheckPools), zap.Bool("metrics_observer", c.metrics != nil),
		zap.Bool("write_anomaly", c.config.Alert.WriteAnomaly.Enabled),
		zap.Int("thin_autoextend_threshold", c.config.Storage.Thin.AutoextendThreshold), zap.Int("observers", len(obs)))
	return opts
}

// startNotifications brings up the event bus, the health detector that feeds
// it, and any configured Webhook receivers.
//
// A Webhook is no longer required to enable this: the bus also backs the watch
// stream and the SSE endpoint, so an operator who wants to tail events without
// standing up an HTTP receiver just sets enabled = true.
func (c *Controller) startNotifications() {
	log := c.traceLogger()
	if !c.config.Alert.Enabled {
		log.Debug("Controller.startNotifications: alerts disabled, no event bus",
			zap.Bool("metrics_enabled", c.config.Metrics.Enabled))
		// The health poll is the only thing that reads cluster state on a
		// schedule, so it is also the only source the storage and DRBD gauges
		// have. Disabling alerts silently empties half of /metrics, which is
		// precisely the "a flat zero looks like a healthy cluster" failure the
		// gauges were wired up to end — so say it once, out loud, at startup.
		if c.config.Metrics.Enabled {
			c.logger.Warn("Metrics are enabled but alerts are not; the pool, gateway and DRBD replication gauges stay empty because they are fed by the health poll. Set [alert] enabled = true to populate them.")
		}
		if c.config.Storage.Thin.AutoextendThreshold > 0 {
			c.logger.Warn("[storage.thin] autoextend is set but alerts are not enabled; thin pools will NOT be grown before they fill, because autoextend reads the health poll. Set [alert] enabled = true.")
		}
		return
	}

	log.Debug("Controller.startNotifications: starting event bus", zap.Int("history_size", c.config.Alert.HistorySize),
		zap.Bool("watch_drbd_events", c.config.Alert.WatchDRBDEvents))
	c.events = event.NewBus(c.config.Alert.HistorySize)
	c.persistEvents()

	opts := c.alertOptions()
	c.alertMonitor = alert.NewMonitor(c.events, opts)
	c.alertMonitor.Start(c.ctx)
	if c.config.Alert.WatchDRBDEvents {
		c.watchDRBDEvents(c.ctx, c.alertMonitor)
	}

	// Database-backed channels come up before the file-based ones so a UI-added
	// channel is delivering by the time the first poll finishes.
	c.notify = NewNotifyManager(c)
	if err := c.notify.Reload(c.ctx); err != nil {
		c.logger.Warn("Failed to load notification channels", zap.Error(err))
	}

	for _, wh := range c.config.Alert.Receivers() {
		// The URL and headers can carry a token: presence only.
		log.Debug("Controller.startNotifications: registering webhook receiver", zap.Bool("has_url", wh.URL != ""),
			zap.Int("headers", len(wh.Headers)), zap.String("min_severity", wh.MinSeverity))
		event.NewWebhook(event.WebhookConfig{
			URL:      wh.URL,
			Headers:  wh.Headers,
			Filter:   event.Filter{MinSeverity: event.ParseSeverity(wh.MinSeverity)},
			OnResult: c.recordDelivery(receiverName(wh.URL)),
		}, c.logger).Start(c.ctx, c.events)
		c.logger.Info("Alert webhook registered",
			zap.String("url", wh.URL),
			zap.String("min_severity", wh.MinSeverity))
	}

	c.logger.Info("Notifications started",
		zap.Duration("interval", opts.Interval),
		zap.Bool("node_checks", opts.Nodes != nil),
		zap.Bool("feeding_metrics", opts.Observer != nil),
		zap.Int("webhooks", len(c.config.Alert.Receivers())),
		zap.Int("channels", c.notify.Active()))
}

// persistEvents keeps the event history in the database, so it survives the
// controller restarting — which under Self-HA is every failover, the moment an
// operator most wants to read what just happened.
func (c *Controller) persistEvents() {
	log := c.traceLogger()
	if c.db == nil {
		log.Debug("Controller.persistEvents: no database, history stays in memory")
		return
	}
	retention := c.config.Alert.HistorySize
	if retention <= 0 {
		retention = event.DefaultHistory
	}
	start := time.Now()
	records, err := c.db.RecentEventRecords(c.ctx, retention)
	log.Debug("Controller.persistEvents: db read event history", zap.Int("retention", retention),
		zap.Int("count", len(records)), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		c.logger.Warn("Could not load the event history", zap.Error(err))
	}
	restored := make([]event.Event, 0, len(records))
	for _, r := range records {
		var e event.Event
		if json.Unmarshal(r, &e) == nil {
			restored = append(restored, e)
		}
	}
	log.Debug("Controller.persistEvents: decoded history", zap.Int("restored", len(restored)),
		zap.Int("undecodable", len(records)-len(restored)))
	c.events.Restore(restored)
	c.events.SetPersister(func(e event.Event) {
		data, err := json.Marshal(e)
		if err != nil {
			log.Debug("Controller.persistEvents: event not encodable, not persisted", zap.String("type", string(e.Type)), zap.Error(err))
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		start := time.Now()
		if err := c.db.AppendEventRecord(ctx, data, retention); err != nil {
			c.logger.Warn("Could not record an event", zap.Error(err))
			return
		}
		log.Debug("Controller.persistEvents: event persisted", zap.String("type", string(e.Type)),
			zap.String("resource", e.Resource), zap.Int("bytes", len(data)), zap.Duration("took", time.Since(start)))
	})
	if len(restored) > 0 {
		c.logger.Info("Event history restored", zap.Int("events", len(restored)))
	}
}
