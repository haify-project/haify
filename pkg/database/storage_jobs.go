package database

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	bolt "go.etcd.io/bbolt"
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
	j.UpdatedAt = time.Now()
	data, err := json.Marshal(j)
	if err != nil {
		return err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(storageJobsBucket))
		if err != nil {
			return err
		}
		return b.Put([]byte(j.ID), data)
	})
}

// ListStorageJobs returns every job, newest first.
func (db *DB) ListStorageJobs(ctx context.Context) ([]*StorageJob, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	var out []*StorageJob
	err := db.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(storageJobsBucket))
		if b == nil {
			return nil
		}
		return b.ForEach(func(_, v []byte) error {
			var j StorageJob
			if err := json.Unmarshal(v, &j); err != nil {
				return nil // a record this version cannot read is skipped, not fatal
			}
			out = append(out, &j)
			return nil
		})
	})
	sort.Slice(out, func(i, k int) bool { return out[i].StartedAt.After(out[k].StartedAt) })
	return out, err
}

// PruneStorageJobs drops finished jobs older than keep.
func (db *DB) PruneStorageJobs(ctx context.Context, keep time.Duration) error {
	jobs, err := db.ListStorageJobs(ctx)
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-keep)
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(storageJobsBucket))
		if b == nil {
			return nil
		}
		for _, j := range jobs {
			if j.State != JobRunning && j.UpdatedAt.Before(cutoff) {
				if err := b.Delete([]byte(j.ID)); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
