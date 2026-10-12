package controller

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/event"
)

// The clock locks are judged by.
//
// A snapshot or backup lock ends at a wall-clock time, so moving the wall
// clock forward — a spoofed NTP answer, a `date -s` — would end every lock at
// once, and the next delete would go through. Locks are therefore judged by
// lockNow: the wall time when the controller started, advanced by the
// monotonic clock, which a change of the system time does not move. A jump
// while the controller runs changes nothing about any lock, and is announced.
//
// What it does not cover: a controller started after the clock was moved
// takes that time as its start. Root on the controller host is outside what
// these locks defend against (snapshot_lock.go); the backups' own locks are
// kept by the object store's clock, not this one.

// lockClockStart carries a monotonic reading, so time.Since(lockClockStart)
// is immune to changes of the wall clock.
var lockClockStart = time.Now()

// lockNow is the time locks are judged by.
func lockNow() time.Time {
	return lockClockStart.Round(0).Add(time.Since(lockClockStart))
}

// clockJumpThreshold is how far the wall clock may drift from lockNow before
// it is reported; NTP slews by far less than this.
const clockJumpThreshold = 2 * time.Minute

// clockJump is how far the wall clock has moved from the lock clock.
func clockJump(wall, lock time.Time) time.Duration {
	return wall.Round(0).Sub(lock.Round(0))
}

// watchLockClock reports a jump of the wall clock once, and again only after
// it has come back.
func (c *Controller) watchLockClock(ctx context.Context) {
	log := c.traceLogger()
	log.Debug("watchLockClock: started", zap.Duration("threshold", clockJumpThreshold))
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	reported := false
	for {
		select {
		case <-ctx.Done():
			log.Debug("watchLockClock: stopped", zap.Error(ctx.Err()))
			return
		case <-t.C:
		}
		d := clockJump(time.Now(), lockNow())
		if d < 0 {
			d = -d
		}
		log.Debug("watchLockClock: tick", zap.Duration("drift", d), zap.Bool("reported", reported))
		switch {
		case d > clockJumpThreshold && !reported:
			log.Debug("watchLockClock: wall clock jumped, reporting", zap.Duration("drift", d),
				zap.Bool("has_events", c.events != nil))
			reported = true
			msg := fmt.Sprintf("the system clock moved %s away from the time this controller counts since it started; "+
				"snapshot and backup locks keep using the counted time", clockJump(time.Now(), lockNow()).Round(time.Second))
			c.logger.Warn(msg, zap.Time("system", time.Now()), zap.Time("lock_clock", lockNow()))
			if c.events != nil {
				c.events.Publish(event.Event{Type: event.TypeClockJumped, Severity: event.SeverityWarning,
					Status: event.StatusFiring, Message: msg})
			}
		case d <= clockJumpThreshold && reported:
			log.Debug("watchLockClock: wall clock back in agreement, resolving", zap.Duration("drift", d),
				zap.Bool("has_events", c.events != nil))
			reported = false
			if c.events != nil {
				c.events.Publish(event.Event{Type: event.TypeClockJumped, Severity: event.SeverityWarning,
					Status: event.StatusResolved, Message: "the system clock agrees with the lock clock again"})
			}
		}
	}
}
