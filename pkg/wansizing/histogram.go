// Package wansizing answers the questions an operator has before and while
// replicating a resource across a WAN: is the link fast enough, does the proxy
// need a bigger buffer, is compression worth its CPU, or is the workload too
// heavy for the link and better served by shipping snapshots.
//
// It is pure: the controller gathers the measurements (its own history of each
// resource's DRBD write counter, the proxy's published rates, an optional
// sample of the volume's compressibility and the operator's link bandwidth),
// and Decide turns them into a verdict that says what it rests on.
package wansizing

import "math"

// subBuckets per power of two: buckets about 19% wide, far finer than any
// sizing decision needs, at 161 counters for 1 B/s to 1 TiB/s.
const subBuckets = 4

// Buckets is the number of histogram counters; index 0 holds idle intervals.
const Buckets = 1 + 40*subBuckets

// Histogram counts rates (bytes per second) on a log scale. Its layout matches
// haify-proxy's, so the two report percentiles the same way.
type Histogram []uint32

func bucketOf(rate float64) int {
	if rate < 1 || math.IsNaN(rate) || math.IsInf(rate, 0) {
		return 0
	}
	i := 1 + int(math.Floor(math.Log2(rate)*subBuckets))
	if i >= Buckets {
		return Buckets - 1
	}
	return i
}

// upperBound is the top of bucket i. Percentiles report it, which errs towards
// the larger figure: the safe side when the number sizes a link or a buffer.
func upperBound(i int) float64 {
	if i == 0 {
		return 0
	}
	return math.Pow(2, float64(i)/subBuckets)
}

// Add counts one rate.
func (h *Histogram) Add(rate float64) {
	if len(*h) != Buckets {
		grown := make(Histogram, Buckets)
		copy(grown, *h)
		*h = grown
	}
	(*h)[bucketOf(rate)]++
}

// Merge adds o's counts into h.
func (h *Histogram) Merge(o Histogram) {
	if len(o) == 0 {
		return
	}
	if len(*h) != Buckets {
		grown := make(Histogram, Buckets)
		copy(grown, *h)
		*h = grown
	}
	for i := 0; i < len(o) && i < Buckets; i++ {
		(*h)[i] += o[i]
	}
}

// Total is the number of rates counted.
func (h Histogram) Total() uint64 {
	var n uint64
	for _, c := range h {
		n += uint64(c)
	}
	return n
}

// Quantile returns the q quantile (0..1) as its bucket's upper bound, or 0
// when nothing was counted.
func (h Histogram) Quantile(q float64) float64 {
	total := h.Total()
	if total == 0 {
		return 0
	}
	q = math.Max(0, math.Min(1, q))
	want := uint64(math.Ceil(q * float64(total)))
	if want == 0 {
		want = 1
	}
	var seen uint64
	for i, c := range h {
		seen += uint64(c)
		if seen >= want {
			return upperBound(i)
		}
	}
	return upperBound(len(h) - 1)
}
