package controller

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/alert"
	"github.com/haify-project/haify/pkg/deployment"
	"github.com/haify-project/haify/pkg/event"
)

// Growing a thin pool before it is full ([storage.thin] autoextend_*).
//
// A full thin pool is not a slow failure: writes to it error out, DRBD takes
// the disk away, and the resource carries on degraded or not at all. A pool
// created by Haify leaves reserve_percent of its volume group free, and this
// observer grows the pool into that room once data or metadata use reaches
// the threshold — the job LVM's own autoextend does, but done here so it is
// visible as an event, and does not depend on dmeventd running on the node.
// When the group has no room left it says so once, which is the cue to add
// a disk.

const (
	autoextendCooldown  = 3 * time.Minute
	thinMetadataCeiling = 16 << 30 // LVM's maximum thin pool metadata size
	minExtendBytes      = 64 << 20
)

type thinAutoextender struct {
	c *Controller

	mu       sync.Mutex
	busy     map[string]bool
	last     map[string]time.Time
	warnedVG map[string]bool
}

func newThinAutoextender(c *Controller) *thinAutoextender {
	c.traceLogger().Debug("newThinAutoextender: created")
	return &thinAutoextender{c: c, busy: map[string]bool{}, last: map[string]time.Time{}, warnedVG: map[string]bool{}}
}

// Observed implements alert.Observer. It must not block the poll, so the
// growing itself runs in its own goroutine.
func (a *thinAutoextender) Observed(obs alert.Observation) {
	log := a.c.traceLogger()
	cfg := a.c.config.Storage.Thin
	if cfg.AutoextendThreshold <= 0 || !obs.Pools.Enabled || !obs.Pools.OK {
		log.Debug("thinAutoextender.Observed: skipped", zap.Int("threshold", cfg.AutoextendThreshold),
			zap.Bool("pools_enabled", obs.Pools.Enabled), zap.Bool("pools_ok", obs.Pools.OK))
		return
	}
	log.Debug("thinAutoextender.Observed: checking pools", zap.Int("count", len(obs.Pools.Items)),
		zap.Int("threshold", cfg.AutoextendThreshold))
	for _, p := range obs.Pools.Items {
		if p.ThinPool == "" || p.VDO {
			log.Debug("thinAutoextender.Observed: not a plain thin pool, skipped", zap.String("node", p.Node),
				zap.String("pool", p.Name), zap.Bool("vdo", p.VDO))
			continue
		}
		data := p.DataPercent >= float64(cfg.AutoextendThreshold)
		meta := p.MetaPercent >= float64(cfg.AutoextendThreshold)
		key := p.Node + "/" + p.Name
		a.mu.Lock()
		if !data && !meta {
			delete(a.warnedVG, key)
			a.mu.Unlock()
			log.Debug("thinAutoextender.Observed: below threshold", zap.String("pool", key),
				zap.Float64("data_percent", p.DataPercent), zap.Float64("meta_percent", p.MetaPercent))
			continue
		}
		if a.busy[key] || time.Since(a.last[key]) < autoextendCooldown {
			busy := a.busy[key]
			a.mu.Unlock()
			log.Debug("thinAutoextender.Observed: busy or cooling down", zap.String("pool", key),
				zap.Bool("busy", busy))
			continue
		}
		a.busy[key] = true
		a.mu.Unlock()
		log.Debug("thinAutoextender.Observed: growing pool", zap.String("pool", key),
			zap.Bool("grow_data", data), zap.Bool("grow_meta", meta),
			zap.Float64("data_percent", p.DataPercent), zap.Float64("meta_percent", p.MetaPercent))
		go func(p alert.PoolStatusInfo) {
			defer func() {
				a.mu.Lock()
				a.busy[key] = false
				a.last[key] = time.Now()
				a.mu.Unlock()
			}()
			a.extend(a.c.ctx, p, data, meta, key)
		}(p)
	}
}

// extendPlan is how much to grow a pool's data and metadata by, given its
// sizes and its group's free space. Metadata comes first: a pool whose
// metadata fills is lost to writes as surely as one whose data does.
func extendPlan(dataBytes, metaBytes, free uint64, growData, growMeta bool, percent int) (dataAdd, newMeta uint64) {
	if growMeta && metaBytes > 0 {
		target := min(metaBytes*2, thinMetadataCeiling)
		if target > metaBytes && target-metaBytes <= free {
			newMeta = target
			free -= target - metaBytes
		}
	}
	if growData && dataBytes > 0 {
		// Whole 4 MiB extents: lvextend refuses a size that is not.
		dataAdd = min(dataBytes*uint64(percent)/100, free) / (4 << 20) * (4 << 20)
		if dataAdd < minExtendBytes {
			dataAdd = 0
		}
	}
	zap.L().Debug("extendPlan: planned", zap.Uint64("data_bytes", dataBytes), zap.Uint64("meta_bytes", metaBytes),
		zap.Uint64("free_bytes", free), zap.Bool("grow_data", growData), zap.Bool("grow_meta", growMeta),
		zap.Int("percent", percent), zap.Uint64("data_add_bytes", dataAdd), zap.Uint64("new_meta_bytes", newMeta))
	return dataAdd, newMeta
}

func (a *thinAutoextender) extend(ctx context.Context, p alert.PoolStatusInfo, growData, growMeta bool, key string) {
	c := a.c
	log := c.traceLogger()
	host := c.ResolveHost(p.Node)
	log.Debug("thinAutoextender.extend: start", zap.String("pool", key), zap.String("host", host),
		zap.String("thin_pool", p.ThinPool), zap.Bool("grow_data", growData), zap.Bool("grow_meta", growMeta))
	dep := c.deployment
	start := time.Now()
	data, err1 := dep.LVSizeBytes(ctx, host, p.Name, p.ThinPool)
	meta, err2 := dep.LVSizeBytes(ctx, host, p.Name, p.ThinPool+"_tmeta")
	free, err3 := dep.VGFreeBytes(ctx, host, p.Name)
	log.Debug("thinAutoextender.extend: sized pool", zap.String("pool", key), zap.String("host", host),
		zap.Uint64("data_bytes", data), zap.Uint64("meta_bytes", meta), zap.Uint64("free_bytes", free),
		zap.Duration("took", time.Since(start)), zap.NamedError("data_error", err1),
		zap.NamedError("meta_error", err2), zap.NamedError("free_error", err3))
	if err1 != nil || err2 != nil || err3 != nil {
		c.logger.Warn("Thin autoextend: could not size the pool", zap.String("pool", key))
		return
	}
	dataAdd, newMeta := extendPlan(data, meta, free, growData, growMeta, c.config.Storage.Thin.AutoextendPercent)
	if dataAdd == 0 && newMeta == 0 {
		a.mu.Lock()
		warned := a.warnedVG[key]
		a.warnedVG[key] = true
		a.mu.Unlock()
		log.Debug("thinAutoextender.extend: no room to grow", zap.String("pool", key),
			zap.Uint64("free_bytes", free), zap.Bool("already_warned", warned))
		if !warned {
			a.publish(p, event.SeverityWarning, fmt.Sprintf("thin pool %s/%s on %s is at %.0f%% data / %.0f%% metadata and its volume group "+
				"has %s left, too little to grow it: add a disk with `haify pool add --pool %s --nodes %s --devices <disk>`",
				p.Name, p.ThinPool, p.Node, p.DataPercent, p.MetaPercent, formatBytes(free), p.Name, p.Node))
		}
		return
	}
	var did []string
	if newMeta > 0 {
		start := time.Now()
		_, err := dep.LVExtendThinPoolMetadata(ctx, []string{host}, p.Name, p.ThinPool, newMeta)
		log.Debug("thinAutoextender.extend: extend metadata", zap.String("pool", key), zap.String("host", host),
			zap.Uint64("new_meta_bytes", newMeta), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			c.logger.Warn("Thin autoextend: metadata", zap.String("pool", key), zap.Error(err))
		} else {
			did = append(did, fmt.Sprintf("metadata %s → %s (was %.0f%% used)", formatBytes(meta), formatBytes(newMeta), p.MetaPercent))
		}
	}
	if dataAdd > 0 {
		cmd := fmt.Sprintf("sudo lvextend -y -L +%dB %s/%s", dataAdd, p.Name, p.ThinPool)
		start := time.Now()
		res, err := dep.Exec(ctx, []string{host}, cmd, deployment.WithExecTimeout(2*time.Minute))
		ok := err == nil && res.AllSuccess()
		log.Debug("thinAutoextender.extend: lvextend data", zap.String("pool", key), zap.String("host", host),
			zap.Uint64("data_add_bytes", dataAdd), zap.Bool("success", ok), zap.Duration("took", time.Since(start)),
			zap.Error(err))
		if ok {
			did = append(did, fmt.Sprintf("data %s → %s (was %.0f%% used)", formatBytes(data), formatBytes(data+dataAdd), p.DataPercent))
		} else {
			out := ""
			if res != nil && res.Hosts[host] != nil {
				out = strings.TrimSpace(res.Hosts[host].Output)
			}
			c.logger.Warn("Thin autoextend: data", zap.String("pool", key), zap.Error(err), zap.String("output", out))
			a.mu.Lock()
			warned := a.warnedVG[key]
			a.warnedVG[key] = true
			a.mu.Unlock()
			if !warned {
				a.publish(p, event.SeverityWarning, fmt.Sprintf("thin pool %s/%s on %s is at %.0f%% and growing it failed: %s",
					p.Name, p.ThinPool, p.Node, p.DataPercent, out))
			}
		}
	}
	if len(did) == 0 {
		log.Debug("thinAutoextender.extend: nothing grown", zap.String("pool", key))
		return
	}
	start = time.Now()
	left, lerr := dep.VGFreeBytes(ctx, host, p.Name)
	log.Debug("thinAutoextender.extend: free space after growing", zap.String("pool", key), zap.String("host", host),
		zap.Uint64("free_bytes", left), zap.Duration("took", time.Since(start)), zap.Error(lerr))
	a.publish(p, event.SeverityInfo, fmt.Sprintf("grew thin pool %s/%s on %s before it filled: %s; %s of its volume group left",
		p.Name, p.ThinPool, p.Node, strings.Join(did, ", "), formatBytes(left)))
}

func (a *thinAutoextender) publish(p alert.PoolStatusInfo, sev event.Severity, msg string) {
	a.c.logger.Info(msg)
	if a.c.events != nil {
		a.c.traceLogger().Debug("thinAutoextender.publish: publishing event", zap.String("node", p.Node),
			zap.String("pool", p.Name), zap.String("severity", string(sev)))
		a.c.events.Publish(event.Event{Type: event.TypePoolExtended, Severity: sev, Status: event.StatusInfo,
			Node: p.Node, Message: msg, Details: map[string]string{"pool": p.Name, "thin_pool": p.ThinPool}})
	} else {
		a.c.traceLogger().Debug("thinAutoextender.publish: no event bus", zap.String("pool", p.Name))
	}
}
