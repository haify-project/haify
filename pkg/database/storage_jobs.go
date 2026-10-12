package database

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	bolt "go.etcd.io/bbolt"
	"go.uber.org/zap"
)

// Long storage operations — moving a disk's data off it, moving a volume to
// another pool — take minutes to hours, outlive the API call that started
// them, and must outlive a controller failover too. Each is a job here; the
// active controller resumes the running ones on start.

const storageJobsBucket = "storage_jobs"

// Storage job kinds.
const (
	JobRemoveDisk  = "remove-disk"
	JobReplaceDisk = "replace-disk"
	JobMoveVolume  = "move-volume"
	JobShip        = "ship"
)

// Storage job states.
const (
	JobRunning = "running"
	JobDone    = "done"
	JobFailed  = "failed"
)

// StorageJob is one long storage operation and how far it got.
type StorageJob struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	State string `json:"state"`

	// Disk jobs.
	Pool    string `json:"pool,omitempty"`
	Node    string `json:"node,omitempty"`
	Disk    string `json:"disk,omitempty"`
	NewDisk string `json:"new_disk,omitempty"`

	// Volume moves.
	Resource   string `json:"resource,omitempty"`
	VolumeID   int    `json:"volume_id,omitempty"`
	FromPool   string `json:"from_pool,omitempty"`
	TargetPool string `json:"target_pool,omitempty"`
	// Nodes is the order the volume moves in; Done the ones already moved.
	Nodes []string `json:"nodes,omitempty"`
	Done  []string `json:"done,omitempty"`

	// Ships: the receiving controller and the resource's name there.
	Target     string `json:"target,omitempty"`
	TargetName string `json:"target_name,omitempty"`

	Progress  string    `json:"progress,omitempty"`
	Message   string    `json:"message,omitempty"`
	StartedAt time.Time `json:"started_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SaveStorageJob creates or replaces a job.
func (db *DB) SaveStorageJob(ctx context.Context, j *StorageJob) error {
	db.log().Debug("DB.SaveStorageJob: saving job",
		zap.String("job", j.ID), zap.String("kind", j.Kind), zap.String("state", j.State),
		zap.String("pool", j.Pool), zap.String("node", j.Node), zap.String("resource", j.Resource),
		zap.Int("volume_id", j.VolumeID), zap.Strings("done", j.Done), zap.String("progress", j.Progress))
	j.UpdatedAt = time.Now()
	data, err := json.Marshal(j)
	if err != nil {
		db.log().Debug("DB.SaveStorageJob: marshal failed", zap.String("job", j.ID), zap.Error(err))
		return err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	start := time.Now()
	err = db.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(storageJobsBucket))
		if err != nil {
			return err
		}
		return b.Put([]byte(j.ID), data)
	})
	db.log().Debug("DB.SaveStorageJob: put", zap.String("bucket", storageJobsBucket), zap.String("key", j.ID),
		zap.Int("size_bytes", len(data)), zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}

// ListStorageJobs returns every job, newest first.
func (db *DB) ListStorageJobs(ctx context.Context) ([]*StorageJob, error) {
	db.log().Debug("DB.ListStorageJobs: listing jobs")
	db.mu.RLock()
	defer db.mu.RUnlock()
	var out []*StorageJob
	unreadable := 0
	start := time.Now()
	err := db.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(storageJobsBucket))
		if b == nil {
			db.log().Debug("DB.ListStorageJobs: jobs bucket absent", zap.String("bucket", storageJobsBucket))
			return nil
		}
		return b.ForEach(func(_, v []byte) error {
			var j StorageJob
			if err := json.Unmarshal(v, &j); err != nil {
				unreadable++
				return nil // a record this version cannot read is skipped, not fatal
			}
			out = append(out, &j)
			return nil
		})
	})
	db.log().Debug("DB.ListStorageJobs: scan", zap.String("bucket", storageJobsBucket), zap.Int("count", len(out)),
		zap.Int("unreadable", unreadable), zap.Duration("took", time.Since(start)), zap.Error(err))
	sort.Slice(out, func(i, k int) bool { return out[i].StartedAt.After(out[k].StartedAt) })
	return out, err
}

// PruneStorageJobs drops finished jobs older than keep.
func (db *DB) PruneStorageJobs(ctx context.Context, keep time.Duration) error {
	db.log().Debug("DB.PruneStorageJobs: pruning finished jobs", zap.Duration("keep", keep))
	jobs, err := db.ListStorageJobs(ctx)
	if err != nil {
		db.log().Debug("DB.PruneStorageJobs: listing jobs failed", zap.Error(err))
		return err
	}
	cutoff := time.Now().Add(-keep)
	db.mu.Lock()
	defer db.mu.Unlock()
	dropped := 0
	start := time.Now()
	err = db.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(storageJobsBucket))
		if b == nil {
			db.log().Debug("DB.PruneStorageJobs: jobs bucket absent", zap.String("bucket", storageJobsBucket))
			return nil
		}
		for _, j := range jobs {
			if j.State != JobRunning && j.UpdatedAt.Before(cutoff) {
				err := b.Delete([]byte(j.ID))
				db.log().Debug("DB.PruneStorageJobs: delete", zap.String("bucket", storageJobsBucket),
					zap.String("key", j.ID), zap.String("state", j.State), zap.Time("updated_at", j.UpdatedAt), zap.Error(err))
				if err != nil {
					return err
				}
				dropped++
			} else {
				db.log().Debug("DB.PruneStorageJobs: keeping job", zap.String("job", j.ID),
					zap.String("state", j.State), zap.Time("updated_at", j.UpdatedAt))
			}
		}
		return nil
	})
	db.log().Debug("DB.PruneStorageJobs: done", zap.Int("count", dropped), zap.Int("jobs", len(jobs)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}
