package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/alert"
)

// The cluster's totals over time, for the dashboard's charts.
//
// Every health poll adds one sample: write and read throughput, data out of
// sync, capacity, and the API's request and error rates. Three tiers keep the
// last day at full resolution, the last 30 days as ten-minute averages and
// the last 90 days as hourly ones, which bounds the document to a few
// thousand samples however long the controller runs. It is saved to the
// database every few minutes, so a restart loses at most that much.

const (
	historySaveEvery = 5 * time.Minute
	rawKeep          = 24 * time.Hour
	tenMinKeep       = 30 * 24 * time.Hour
	hourKeep         = 90 * 24 * time.Hour
)

// clusterSample is one moment of the history. JSON names are short because a
// day of samples is stored as one document.
type clusterSample struct {
	T     int64   `json:"t"`
	W     float64 `json:"w"`
	R     float64 `json:"r"`
	OOS   uint64  `json:"o"`
	Used  uint64  `json:"u"`
	Total uint64  `json:"c"`
	Req   float64 `json:"q"`
	Err   float64 `json:"e"`
}

// sampleSum accumulates samples into one average for a coarser tier.
type sampleSum struct {
	start int64
	n     int
	s     clusterSample
}

func (a *sampleSum) add(s clusterSample) {
	a.n++
	a.s.W += s.W
	a.s.R += s.R
	a.s.OOS += s.OOS
	a.s.Used += s.Used
	a.s.Total += s.Total
	a.s.Req += s.Req
	a.s.Err += s.Err
}

func (a *sampleSum) mean() clusterSample {
	n := float64(a.n)
	u := uint64(a.n)
	return clusterSample{T: a.start, W: a.s.W / n, R: a.s.R / n, OOS: a.s.OOS / u, Used: a.s.Used / u,
		Total: a.s.Total / u, Req: a.s.Req / n, Err: a.s.Err / n}
}

type historyTiers struct {
	Raw    []clusterSample `json:"raw"`
	TenMin []clusterSample `json:"m10"`
	Hour   []clusterSample `json:"h1"`
}

type clusterHistoryStore interface {
	SaveClusterHistory(ctx context.Context, data []byte) error
	LoadClusterHistory(ctx context.Context) ([]byte, error)
}

type clusterHistory struct {
	view   *clusterView
	store  clusterHistoryStore
	api    func() (uint64, uint64)
	ctx    context.Context
	log    *zap.Logger
	now    func() time.Time
	loaded bool

	mu         sync.RWMutex
	tiers      historyTiers
	ten, hour  sampleSum
	lastAPI    [2]uint64
	lastAPIAt  time.Time
	lastSaveAt time.Time
}

func newClusterHistory(c *Controller, view *clusterView) *clusterHistory {
	h := &clusterHistory{view: view, ctx: c.ctx, log: c.traceLogger(), now: time.Now}
	if h.ctx == nil {
		h.ctx = context.Background()
	}
	if c.db != nil {
		h.store = c.db
	}
	if c.metrics != nil {
		h.api = c.metrics.GRPCTotals
		// The baseline is taken now, so the first poll already has a rate.
		req, errs := h.api()
		h.lastAPI, h.lastAPIAt = [2]uint64{req, errs}, h.now()
	}
	h.log.Debug("newClusterHistory: built", zap.Bool("has_store", h.store != nil), zap.Bool("has_api_totals", h.api != nil))
	return h
}

// Observed implements alert.Observer. It runs after the view's own Observed,
// so the view already holds this poll.
func (h *clusterHistory) Observed(alert.Observation) {
	snap := h.view.snapshot()
	now := h.now()
	s := clusterSample{T: now.Unix(), W: max(snap.WriteBps, 0), R: max(snap.ReadBps, 0)}
	for _, rv := range snap.Resources {
		s.OOS += rv.OutOfSyncBytes
	}
	s.Used, s.Total = poolTotals(snap.Pools)

	h.mu.Lock()
	defer h.mu.Unlock()
	h.load()
	if h.api != nil {
		req, errs := h.api()
		if !h.lastAPIAt.IsZero() && req >= h.lastAPI[0] && errs >= h.lastAPI[1] {
			if dt := now.Sub(h.lastAPIAt).Seconds(); dt > 0 {
				s.Req = float64(req-h.lastAPI[0]) / dt
				s.Err = float64(errs-h.lastAPI[1]) / dt
			}
		}
		h.lastAPI, h.lastAPIAt = [2]uint64{req, errs}, now
	}
	h.tiers.Raw = append(trimBefore(h.tiers.Raw, now.Add(-rawKeep).Unix()), s)
	h.tiers.TenMin = rollUp(&h.ten, s, 600, h.tiers.TenMin, now.Add(-tenMinKeep).Unix())
	h.tiers.Hour = rollUp(&h.hour, s, 3600, h.tiers.Hour, now.Add(-hourKeep).Unix())
	h.log.Debug("clusterHistory.Observed: sampled", zap.Float64("write_bps", s.W), zap.Float64("read_bps", s.R),
		zap.Uint64("out_of_sync_bytes", s.OOS), zap.Uint64("used_bytes", s.Used), zap.Uint64("total_bytes", s.Total),
		zap.Float64("api_rps", s.Req), zap.Int("raw", len(h.tiers.Raw)))
	if now.Sub(h.lastSaveAt) >= historySaveEvery {
		h.save()
		h.lastSaveAt = now
	}
}

// rollUp adds s to the window it belongs to, and closes the previous window
// into the tier when s starts a new one.
func rollUp(acc *sampleSum, s clusterSample, width int64, tier []clusterSample, keepFrom int64) []clusterSample {
	start := s.T - s.T%width
	if acc.n > 0 && acc.start != start {
		tier = append(trimBefore(tier, keepFrom), acc.mean())
		*acc = sampleSum{}
	}
	if acc.n == 0 {
		acc.start = start
	}
	acc.add(s)
	return tier
}

func trimBefore(samples []clusterSample, from int64) []clusterSample {
	i := 0
	for i < len(samples) && samples[i].T < from {
		i++
	}
	return samples[i:]
}

// poolTotals is the capacity used and in all over the pools: a thin pool's
// data space, or a thick group's own size.
func poolTotals(pools []alert.PoolStatusInfo) (used, total uint64) {
	for _, p := range pools {
		u, t := poolUsage(p)
		used += u
		total += t
	}
	return used, total
}

func poolUsage(p alert.PoolStatusInfo) (used, total uint64) {
	if p.ThinPool != "" && p.ThinSizeBytes > 0 {
		return uint64(float64(p.ThinSizeBytes) * p.DataPercent / 100), p.ThinSizeBytes
	}
	if p.TotalBytes >= p.FreeBytes {
		return p.TotalBytes - p.FreeBytes, p.TotalBytes
	}
	return 0, p.TotalBytes
}

// load reads the stored history once, on the first poll.
func (h *clusterHistory) load() {
	if h.loaded || h.store == nil {
		h.loaded = true
		return
	}
	h.loaded = true
	data, err := h.store.LoadClusterHistory(h.ctx)
	if err != nil || data == nil {
		h.log.Debug("clusterHistory.load: nothing stored", zap.Error(err))
		return
	}
	if err := json.Unmarshal(data, &h.tiers); err != nil {
		h.log.Debug("clusterHistory.load: unreadable, starting over", zap.Error(err))
		h.tiers = historyTiers{}
		return
	}
	h.log.Debug("clusterHistory.load: loaded", zap.Int("raw", len(h.tiers.Raw)), zap.Int("ten_min", len(h.tiers.TenMin)),
		zap.Int("hour", len(h.tiers.Hour)))
}

// flush saves the history now.
func (h *clusterHistory) flush() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.loaded {
		h.save()
	}
}

func (h *clusterHistory) save() {
	if h.store == nil {
		return
	}
	data, err := json.Marshal(h.tiers)
	if err == nil {
		err = h.store.SaveClusterHistory(h.ctx, data)
	}
	h.log.Debug("clusterHistory.save: saved", zap.Int("size_bytes", len(data)), zap.Error(err))
}

// historyRanges maps an API range to its length and the tier that serves it.
var historyRanges = map[string]struct {
	span time.Duration
	tier string
}{
	"1h": {time.Hour, "raw"}, "6h": {6 * time.Hour, "raw"}, "24h": {24 * time.Hour, "raw"},
	"7d": {7 * 24 * time.Hour, "m10"}, "30d": {30 * 24 * time.Hour, "m10"}, "90d": {90 * 24 * time.Hour, "h1"},
}

// maxHistoryPoints bounds what one query returns: a chart has no use for more
// points than it has pixels.
const maxHistoryPoints = 360

// query returns the samples of one range, averaged down to at most
// maxHistoryPoints.
func (h *clusterHistory) query(rng string) ([]clusterSample, error) {
	if rng == "" {
		rng = "24h"
	}
	r, ok := historyRanges[rng]
	if !ok {
		return nil, fmt.Errorf("unknown range %q: use 1h, 6h, 24h, 7d, 30d or 90d", rng)
	}
	h.mu.RLock()
	// A coarse tier ends where its last window closed; the time since is in
	// the finer tiers, so a long range reaches up to now from the first day.
	tiers := [][]clusterSample{h.tiers.Raw}
	widths := []int64{0}
	switch r.tier {
	case "m10":
		tiers = [][]clusterSample{h.tiers.TenMin, h.tiers.Raw}
		widths = []int64{600, 0}
	case "h1":
		tiers = [][]clusterSample{h.tiers.Hour, h.tiers.TenMin, h.tiers.Raw}
		widths = []int64{3600, 600, 0}
	}
	from := h.now().Add(-r.span).Unix()
	var picked []clusterSample
	for i, tier := range tiers {
		cut := from
		if n := len(picked); n > 0 {
			cut = max(cut, picked[n-1].T+widths[i-1])
		}
		picked = append(picked, trimBefore(tier, cut)...)
	}
	h.mu.RUnlock()
	if len(picked) <= maxHistoryPoints {
		return picked, nil
	}
	width := (int64(r.span.Seconds()) + maxHistoryPoints - 1) / maxHistoryPoints
	var out []clusterSample
	var acc sampleSum
	for _, s := range picked {
		out = rollUp(&acc, s, width, out, 0)
	}
	if acc.n > 0 {
		out = append(out, acc.mean())
	}
	return out, nil
}
