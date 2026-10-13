package wansizing

import (
	"sort"
	"time"
)

// KeepDays is how many days of write history a resource keeps.
const KeepDays = 14

// Day is one UTC day of a resource's write rate, built from the controller's
// health polls: each poll turns the change in DRBD's written counter into one
// rate over the interval since the previous poll.
type Day struct {
	Date string `json:"date"` // YYYY-MM-DD, UTC
	// Seconds and Bytes are the time covered and the bytes written in it, so
	// the day's average is Bytes/Seconds however irregular the polls were.
	Seconds float64 `json:"seconds"`
	Bytes   float64 `json:"bytes"`
	// Samples is the number of intervals, PeakBps the busiest of them.
	Samples int     `json:"samples"`
	PeakBps float64 `json:"peak_bps"`
	// Rates counts the intervals by rate, for percentiles across days.
	Rates Histogram `json:"rates"`
}

// History is a resource's recent days, oldest first.
type History struct {
	Days []Day `json:"days"`
	// Node is the replica whose counter was read last. The controller reads
	// whichever replica answers its status query, normally the first one listed.
	Node string `json:"node,omitempty"`
}

// Add records bytes written over the interval of dt seconds that ended at t.
func (h *History) Add(t time.Time, dt, bytes float64, node string) {
	if dt <= 0 || bytes < 0 {
		return
	}
	date := t.UTC().Format("2006-01-02")
	idx := h.dayIndex(date)
	if idx < 0 {
		h.Days = append(h.Days, Day{Date: date})
		sort.SliceStable(h.Days, func(i, j int) bool { return h.Days[i].Date < h.Days[j].Date })
		idx = h.dayIndex(date)
	}
	d := &h.Days[idx]
	rate := bytes / dt
	d.Seconds += dt
	d.Bytes += bytes
	d.Samples++
	if rate > d.PeakBps {
		d.PeakBps = rate
	}
	d.Rates.Add(rate)
	h.Node = node
	if over := len(h.Days) - KeepDays; over > 0 {
		h.Days = append([]Day(nil), h.Days[over:]...)
	}
}

func (h *History) dayIndex(date string) int {
	for i := range h.Days {
		if h.Days[i].Date == date {
			return i
		}
	}
	return -1
}

// Window is what the history says about the days it covers.
type Window struct {
	// Days is how many calendar days contributed; Seconds the time covered.
	Days    int
	Seconds float64
	Samples int
	// ResolutionSecs is the average interval: a burst shorter than it is
	// averaged into its interval, so PeakBps and P99Bps understate short bursts.
	ResolutionSecs float64
	AvgBps         float64
	P99Bps         float64
	PeakBps        float64
	First, Last    string
	Node           string
}

// Summary covers the last `days` days up to now (all kept days when days <= 0).
// Nil when there is nothing in the window.
func (h *History) Summary(now time.Time, days int) *Window {
	if h == nil || len(h.Days) == 0 {
		return nil
	}
	cutoff := ""
	if days > 0 {
		cutoff = now.UTC().AddDate(0, 0, -(days - 1)).Format("2006-01-02")
	}
	w := &Window{Node: h.Node}
	var bytes float64
	var rates Histogram
	for _, d := range h.Days {
		if d.Date < cutoff || d.Samples == 0 {
			continue
		}
		if w.First == "" {
			w.First = d.Date
		}
		w.Last = d.Date
		w.Days++
		w.Seconds += d.Seconds
		w.Samples += d.Samples
		bytes += d.Bytes
		if d.PeakBps > w.PeakBps {
			w.PeakBps = d.PeakBps
		}
		rates.Merge(d.Rates)
	}
	if w.Samples == 0 || w.Seconds <= 0 {
		return nil
	}
	w.AvgBps = bytes / w.Seconds
	w.ResolutionSecs = w.Seconds / float64(w.Samples)
	w.P99Bps = rates.Quantile(0.99)
	// A bucket's upper bound can exceed the busiest interval actually seen.
	if w.P99Bps > w.PeakBps {
		w.P99Bps = w.PeakBps
	}
	return w
}
