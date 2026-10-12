package database

import (
	"context"
	"encoding/json"
	"time"

	bolt "go.etcd.io/bbolt"
	"go.uber.org/zap"
)

// Delivery results per notification channel.
//
// A channel whose URL went stale (a receiver moved, a bot was deleted) fails
// every delivery with a log line nobody reads, and the alerts it was meant to
// carry vanish. Recording each channel's last outcome is what lets the
// inspection say "your only channel has failed its last 40 deliveries" instead
// of finding out from the outage.

const notifyDeliveryBucket = "notify_delivery"

// maxFailedEventIDs bounds the per-channel list of events given up on.
const maxFailedEventIDs = 200

// NotifyDelivery is the delivery record of one channel.
type NotifyDelivery struct {
	Channel             string
	LastSuccess         time.Time
	LastSuccessEvent    uint64
	LastFailure         time.Time
	LastFailureEvent    uint64
	LastError           string
	ConsecutiveFailures int
	// FailedEvents are the most recent event ids this channel gave up on,
	// oldest first.
	FailedEvents []uint64
}

// RecordNotifyDelivery records the final outcome of delivering one event to
// one channel; errMsg empty is a success.
func (db *DB) RecordNotifyDelivery(ctx context.Context, channel string, eventID uint64, at time.Time, errMsg string) error {
	// errMsg can quote the receiver URL, token included: only its presence is traced.
	db.log().Debug("DB.RecordNotifyDelivery: recording delivery outcome",
		zap.String("channel", channel), zap.Uint64("event_id", eventID), zap.Bool("failed", errMsg != ""))
	db.mu.Lock()
	defer db.mu.Unlock()
	var d NotifyDelivery
	start := time.Now()
	err := db.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(notifyDeliveryBucket))
		if err != nil {
			return err
		}
		d = NotifyDelivery{Channel: channel}
		if raw := b.Get([]byte(channel)); raw != nil {
			if uerr := json.Unmarshal(raw, &d); uerr != nil {
				db.log().Debug("DB.RecordNotifyDelivery: previous record unreadable, starting over",
					zap.String("channel", channel), zap.Error(uerr))
			}
		}
		if errMsg == "" {
			d.LastSuccess, d.LastSuccessEvent, d.ConsecutiveFailures = at, eventID, 0
		} else {
			d.LastFailure, d.LastFailureEvent, d.LastError = at, eventID, errMsg
			d.ConsecutiveFailures++
			d.FailedEvents = append(d.FailedEvents, eventID)
			if n := len(d.FailedEvents); n > maxFailedEventIDs {
				db.log().Debug("DB.RecordNotifyDelivery: trimming failed event list",
					zap.String("channel", channel), zap.Int("count", n), zap.Int("max", maxFailedEventIDs))
				d.FailedEvents = append([]uint64(nil), d.FailedEvents[n-maxFailedEventIDs:]...)
			}
		}
		data, err := json.Marshal(d)
		if err != nil {
			return err
		}
		return b.Put([]byte(channel), data)
	})
	db.log().Debug("DB.RecordNotifyDelivery: put", zap.String("bucket", notifyDeliveryBucket), zap.String("key", channel),
		zap.Int("consecutive_failures", d.ConsecutiveFailures), zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}

// ListNotifyDeliveries returns every channel's delivery record by name.
func (db *DB) ListNotifyDeliveries(ctx context.Context) (map[string]*NotifyDelivery, error) {
	db.log().Debug("DB.ListNotifyDeliveries: listing delivery records")
	db.mu.RLock()
	defer db.mu.RUnlock()
	out := map[string]*NotifyDelivery{}
	unreadable := 0
	start := time.Now()
	err := db.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(notifyDeliveryBucket))
		if b == nil {
			db.log().Debug("DB.ListNotifyDeliveries: delivery bucket absent", zap.String("bucket", notifyDeliveryBucket))
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			var d NotifyDelivery
			if err := json.Unmarshal(v, &d); err != nil {
				unreadable++
				return nil
			}
			out[string(k)] = &d
			return nil
		})
	})
	db.log().Debug("DB.ListNotifyDeliveries: scan", zap.String("bucket", notifyDeliveryBucket), zap.Int("count", len(out)),
		zap.Int("unreadable", unreadable), zap.Duration("took", time.Since(start)), zap.Error(err))
	return out, err
}
