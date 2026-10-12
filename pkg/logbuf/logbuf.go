// Package logbuf keeps the controller's most recent log lines in memory so the
// API can serve them.
//
// Reading them back from journald would mean shelling out as root on whichever
// node is currently active, and would tie the feature to systemd; a controller
// started any other way would have no logs at all. Teeing zap into a ring
// buffer costs one slot per line and works the same however the process was
// started.
//
// The buffer is deliberately not persisted. It describes what a running
// controller is doing right now; once the controller has moved, its old node's
// output is a different process's story and belongs in the audit trail instead.
package logbuf

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Entry is one captured log line.
type Entry struct {
	Time    time.Time
	Level   string
	Logger  string
	Caller  string
	Message string
	Fields  map[string]string
}

// Ring is a fixed-size circular buffer of log entries, safe for concurrent use.
type Ring struct {
	mu      sync.RWMutex
	entries []Entry
	// next is where the following write lands; once wrapped is true the buffer
	// is full and next is also the oldest entry.
	next    int
	wrapped bool
}

// DefaultSize is a few thousand lines: enough to cover the run-up to whatever
// made someone open the log view, and small enough to ignore.
const DefaultSize = 2000

// New creates a ring holding the last size entries.
func New(size int) *Ring {
	if size <= 0 {
		zap.L().Debug("New: size not positive, using the default", zap.Int("size", size), zap.Int("default", DefaultSize))
		size = DefaultSize
	}
	zap.L().Debug("New: creating log ring", zap.Int("size", size))
	return &Ring{entries: make([]Entry, size)}
}

func (r *Ring) append(e Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[r.next] = e
	r.next++
	if r.next == len(r.entries) {
		r.next = 0
		r.wrapped = true
	}
}

// Filter narrows a listing. Zero values mean "no constraint".
type Filter struct {
	Limit int
	// MinLevel drops anything less severe. Empty keeps everything.
	MinLevel zapcore.Level
	HasLevel bool
	Contains string
}

// DefaultLimit is returned when a caller asks for no particular number.
const DefaultLimit = 300

// List returns matching entries newest first, plus whether the buffer has
// wrapped — that is, whether lines older than these were discarded.
func (r *Ring) List(f Filter) ([]Entry, bool) {
	// Traced before the lock is taken and never while it is held: the
	// controller's logger tees into this ring, so a log line written under
	// r.mu would wait on r.mu in append.
	zap.L().Debug("Ring.List: start", zap.Int("limit", f.Limit), zap.Bool("has_level", f.HasLevel),
		zap.Stringer("min_level", f.MinLevel), zap.String("contains", f.Contains))
	r.mu.RLock()
	defer r.mu.RUnlock()

	limit := f.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}

	count := len(r.entries)
	if !r.wrapped {
		count = r.next
	}

	needle := strings.ToLower(f.Contains)
	out := make([]Entry, 0, min(limit, count))
	// Walk backwards from the newest entry.
	for i := 0; i < count && len(out) < limit; i++ {
		idx := r.next - 1 - i
		if idx < 0 {
			idx += len(r.entries)
		}
		e := r.entries[idx]
		if f.HasLevel && levelOf(e.Level) < f.MinLevel {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(e.Message), needle) {
			continue
		}
		out = append(out, e)
	}
	return out, r.wrapped
}

func levelOf(s string) zapcore.Level {
	var l zapcore.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		// An unparseable level must not be silently dropped by a level filter.
		return zapcore.DebugLevel
	}
	return l
}

// core is the zapcore.Core that feeds the ring.
type core struct {
	zapcore.LevelEnabler
	ring *Ring
	// with holds the fields accumulated by With() calls on this core, which is
	// how a named/derived logger's context reaches each entry.
	with []zapcore.Field
}

// Core returns a zapcore.Core that writes into the ring at or above enab.
// Tee it alongside the real output rather than replacing it:
//
//	zapConfig.Build(zap.WrapCore(func(c zapcore.Core) zapcore.Core {
//	        return zapcore.NewTee(c, ring.Core(zapConfig.Level))
//	}))
func (r *Ring) Core(enab zapcore.LevelEnabler) zapcore.Core {
	return &core{LevelEnabler: enab, ring: r}
}

func (c *core) With(fields []zapcore.Field) zapcore.Core {
	// Copy rather than append in place: derived cores share the backing array
	// otherwise, and one would overwrite another's fields.
	merged := make([]zapcore.Field, 0, len(c.with)+len(fields))
	merged = append(merged, c.with...)
	merged = append(merged, fields...)
	return &core{LevelEnabler: c.LevelEnabler, ring: c.ring, with: merged}
}

func (c *core) Check(ent zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(ent.Level) {
		return ce.AddCore(ent, c)
	}
	return ce
}

func (c *core) Write(ent zapcore.Entry, fields []zapcore.Field) error {
	enc := zapcore.NewMapObjectEncoder()
	for _, f := range c.with {
		f.AddTo(enc)
	}
	for _, f := range fields {
		f.AddTo(enc)
	}
	// Render to strings here rather than at read time: the values are only
	// valid while their source lives, and the API surface is strings anyway.
	rendered := make(map[string]string, len(enc.Fields))
	for k, v := range enc.Fields {
		rendered[k] = fmt.Sprint(v)
	}

	c.ring.append(Entry{
		Time:    ent.Time,
		Level:   ent.Level.String(),
		Logger:  ent.LoggerName,
		Caller:  ent.Caller.TrimmedPath(),
		Message: ent.Message,
		Fields:  rendered,
	})
	return nil
}

// Sync is a no-op: the ring is memory, there is nothing to flush.
func (c *core) Sync() error { return nil }
