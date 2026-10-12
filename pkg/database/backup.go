package database

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	bolt "go.etcd.io/bbolt"
	"go.uber.org/zap"
)

// Backup state values. A backup is only usable for a restore in StateCompleted;
// everything else exists so a failed or interrupted run is visible instead of
// silently absent.
const (
	// BackupStateRunning is written before the first byte is uploaded. A record
	// stuck here means the controller died mid-backup — which is exactly what an
	// operator needs to see, and is never mistaken for a usable copy.
	BackupStateRunning = "running"
	// BackupStateCompleted means every volume image was uploaded AND verified
	// against the far end's reported size.
	BackupStateCompleted = "completed"
	// BackupStateFailed records a run that did not finish.
	BackupStateFailed = "failed"
)

// Backup kinds. An incremental backup holds only the blocks that changed since
// its Parent, so restoring it needs every backup down the chain to the full one.
const (
	BackupKindFull        = "full"
	BackupKindIncremental = "incremental"
)

// BackupTarget is a repository backups are shipped to.
//
// Secret is stored here in the clear and is deliberately never returned by the
// API. The protection boundary is the database file itself, which bbolt opens
// 0600 — the same boundary the WAN mTLS private key already relies on. Adding
// an encryption layer whose key sits next to the ciphertext on the same host
// would move the secret, not protect it.
type BackupTarget struct {
	Name string
	Kind string // s3 | smb | webdav

	Prefix string

	Bucket   string
	Endpoint string
	Region   string

	Host  string
	Share string

	// User is the S3 access key id, or the SMB/WebDAV username.
	User string
	// Secret is the S3 secret access key, or the SMB/WebDAV password.
	Secret string
	// SecretObscured marks Secret as already in rclone's obscured form.
	SecretObscured bool

	// LockMode ("governance", "compliance" or empty), LockDays and
	// FullEveryDays configure S3 Object Lock for what the target stores.
	LockMode      string `json:",omitempty"`
	LockDays      int    `json:",omitempty"`
	FullEveryDays int    `json:",omitempty"`

	CreatedAt time.Time
	UpdatedAt time.Time
}

// BackupVolume records one volume image inside a backup.
type BackupVolume struct {
	VolumeID uint32
	// BackingVolume and Pool identify the LV the snapshot was taken from, so a
	// restore can be reasoned about even when the resource has been recreated.
	BackingVolume string
	Pool          string
	// Object is the target-relative key holding the raw image.
	Object string
	// Bytes is the exact size of the image: the DRBD device's size at backup
	// time, NOT the backing volume's — see BackupManager for why those differ.
	Bytes uint64
	// Ranges is the object listing the "offset length" byte ranges an
	// incremental Object holds, in order. Empty for a full image.
	Ranges string
	// ChangedBytes is how much of the volume an incremental image carries.
	ChangedBytes uint64
	// Snapshot is the thin snapshot this image was read from, kept on Node as
	// the base the next incremental is computed against. Empty once released.
	Snapshot string
	// ReadThroughLUKS marks an image of an encrypted volume that was read
	// through the volume's LUKS container and therefore holds plaintext. An
	// encrypted volume's image without it was copied from the ciphertext by an
	// earlier version and cannot be restored.
	ReadThroughLUKS bool
}

// Backup is one point-in-time copy of a resource on a target.
type Backup struct {
	ID       string
	Resource string
	Target   string
	// Node is the node the snapshot was taken and read on.
	Node string
	// Backend records which implementation wrote the objects (e.g. "rclone").
	Backend string
	State   string
	// Error carries the failure reason for BackupStateFailed.
	Error string
	// Prefix is the target-relative directory holding this backup's objects.
	Prefix string
	// Kind is BackupKindFull or BackupKindIncremental; empty is a full backup
	// taken before incrementals existed.
	Kind string
	// Parent is the backup an incremental was computed against.
	Parent string
	// Schedule names the backup schedule that took this backup; empty for one
	// taken by hand. A schedule's retention only ever deletes its own.
	Schedule   string
	Volumes    []BackupVolume
	TotalBytes uint64
	StartedAt  time.Time
	FinishedAt time.Time

	// LockMode and RetainUntil are the Object Lock every object of the
	// backup carries: until then neither haify nor anyone with its credentials
	// can delete or overwrite them. Every backup of an incremental chain
	// shares the date, so no link expires before a backup built on it.
	LockMode    string    `json:",omitempty"`
	RetainUntil time.Time `json:",omitempty"`
	// ReadAt, when set, is the time the backup's objects are read as of —
	// the versions current then, whatever was written or deleted since. Set
	// for a backup imported with --as-of.
	ReadAt time.Time `json:",omitempty"`
}

// ==================== BACKUP TARGET ====================

// SaveBackupTarget saves or updates a backup target.
func (db *DB) SaveBackupTarget(ctx context.Context, t *BackupTarget) error {
	// User is an S3 access key id and Secret a key or password: only their
	// presence is traced.
	db.log().Debug("DB.SaveBackupTarget: saving backup target",
		zap.String("target", t.Name), zap.String("kind", t.Kind), zap.String("bucket_name", t.Bucket),
		zap.String("endpoint", t.Endpoint), zap.String("host", t.Host), zap.String("share", t.Share),
		zap.Bool("has_user", t.User != ""), zap.Bool("has_secret", t.Secret != ""),
		zap.String("lock_mode", t.LockMode), zap.Int("lock_days", t.LockDays))
	db.mu.Lock()
	defer db.mu.Unlock()

	now := time.Now()
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	t.UpdatedAt = now

	data, err := json.Marshal(t)
	if err != nil {
		db.log().Debug("DB.SaveBackupTarget: marshal failed", zap.String("target", t.Name), zap.Error(err))
		return fmt.Errorf("failed to marshal backup target: %w", err)
	}
	start := time.Now()
	err = db.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(backupTargetsBucket)).Put([]byte(t.Name), data)
	})
	db.log().Debug("DB.SaveBackupTarget: put", zap.String("bucket", backupTargetsBucket), zap.String("key", t.Name),
		zap.Int("size_bytes", len(data)), zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}

// GetBackupTarget retrieves a backup target by name.
func (db *DB) GetBackupTarget(ctx context.Context, name string) (*BackupTarget, error) {
	db.log().Debug("DB.GetBackupTarget: reading backup target", zap.String("target", name))
	db.mu.RLock()
	defer db.mu.RUnlock()

	var t BackupTarget
	found, size := false, 0
	start := time.Now()
	err := db.db.View(func(tx *bolt.Tx) error {
		data := tx.Bucket([]byte(backupTargetsBucket)).Get([]byte(name))
		if data == nil {
			return fmt.Errorf("backup target %q not found", name)
		}
		found, size = true, len(data)
		return json.Unmarshal(data, &t)
	})
	db.log().Debug("DB.GetBackupTarget: get", zap.String("bucket", backupTargetsBucket), zap.String("key", name),
		zap.Bool("found", found), zap.Int("size_bytes", size),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// ListBackupTargets lists all backup targets, name-ordered.
func (db *DB) ListBackupTargets(ctx context.Context) ([]*BackupTarget, error) {
	db.log().Debug("DB.ListBackupTargets: listing backup targets")
	db.mu.RLock()
	defer db.mu.RUnlock()

	targets := make([]*BackupTarget, 0)
	start := time.Now()
	err := db.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(backupTargetsBucket)).ForEach(func(k, v []byte) error {
			var t BackupTarget
			if err := json.Unmarshal(v, &t); err != nil {
				db.log().Debug("DB.ListBackupTargets: decoding entry failed", zap.ByteString("key", k), zap.Error(err))
				return err
			}
			targets = append(targets, &t)
			return nil
		})
	})
	db.log().Debug("DB.ListBackupTargets: scan", zap.String("bucket", backupTargetsBucket),
		zap.Int("count", len(targets)), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, err
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].Name < targets[j].Name })
	return targets, nil
}

// DeleteBackupTarget deletes a backup target by name.
func (db *DB) DeleteBackupTarget(ctx context.Context, name string) error {
	db.log().Debug("DB.DeleteBackupTarget: deleting backup target", zap.String("target", name))
	db.mu.Lock()
	defer db.mu.Unlock()

	start := time.Now()
	err := db.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(backupTargetsBucket)).Delete([]byte(name))
	})
	db.log().Debug("DB.DeleteBackupTarget: delete", zap.String("bucket", backupTargetsBucket), zap.String("key", name),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}

// ==================== BACKUP ====================

// SaveBackup saves or updates a backup record.
func (db *DB) SaveBackup(ctx context.Context, b *Backup) error {
	db.log().Debug("DB.SaveBackup: saving backup record",
		zap.String("backup", b.ID), zap.String("resource", b.Resource), zap.String("target", b.Target),
		zap.String("node", b.Node), zap.String("state", b.State), zap.String("kind", b.Kind),
		zap.String("parent", b.Parent), zap.Int("volumes", len(b.Volumes)), zap.Uint64("total_bytes", b.TotalBytes))
	db.mu.Lock()
	defer db.mu.Unlock()

	if b.ID == "" {
		db.log().Debug("DB.SaveBackup: refusing record without id", zap.String("resource", b.Resource))
		return fmt.Errorf("backup id is required")
	}
	data, err := json.Marshal(b)
	if err != nil {
		db.log().Debug("DB.SaveBackup: marshal failed", zap.String("backup", b.ID), zap.Error(err))
		return fmt.Errorf("failed to marshal backup: %w", err)
	}
	start := time.Now()
	err = db.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(backupsBucket)).Put([]byte(b.ID), data)
	})
	db.log().Debug("DB.SaveBackup: put", zap.String("bucket", backupsBucket), zap.String("key", b.ID),
		zap.Int("size_bytes", len(data)), zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}

// GetBackup retrieves a backup record by id.
func (db *DB) GetBackup(ctx context.Context, id string) (*Backup, error) {
	db.log().Debug("DB.GetBackup: reading backup record", zap.String("backup", id))
	db.mu.RLock()
	defer db.mu.RUnlock()

	var b Backup
	found, size := false, 0
	start := time.Now()
	err := db.db.View(func(tx *bolt.Tx) error {
		data := tx.Bucket([]byte(backupsBucket)).Get([]byte(id))
		if data == nil {
			return fmt.Errorf("backup %q not found", id)
		}
		found, size = true, len(data)
		return json.Unmarshal(data, &b)
	})
	db.log().Debug("DB.GetBackup: get", zap.String("bucket", backupsBucket), zap.String("key", id),
		zap.Bool("found", found), zap.Int("size_bytes", size),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// ListBackups lists backup records, newest first. An empty resource or target
// matches everything.
func (db *DB) ListBackups(ctx context.Context, resource, target string) ([]*Backup, error) {
	db.log().Debug("DB.ListBackups: listing backup records", zap.String("resource", resource), zap.String("target", target))
	db.mu.RLock()
	defer db.mu.RUnlock()

	backups := make([]*Backup, 0)
	scanned := 0
	start := time.Now()
	err := db.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(backupsBucket)).ForEach(func(k, v []byte) error {
			scanned++
			var b Backup
			if err := json.Unmarshal(v, &b); err != nil {
				db.log().Debug("DB.ListBackups: decoding entry failed", zap.ByteString("key", k), zap.Error(err))
				return err
			}
			if resource != "" && b.Resource != resource {
				return nil
			}
			if target != "" && b.Target != target {
				return nil
			}
			backups = append(backups, &b)
			return nil
		})
	})
	db.log().Debug("DB.ListBackups: scan", zap.String("bucket", backupsBucket), zap.Int("scanned", scanned),
		zap.Int("count", len(backups)), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, err
	}
	sort.Slice(backups, func(i, j int) bool { return backups[i].StartedAt.After(backups[j].StartedAt) })
	return backups, nil
}

// DeleteBackup deletes a backup record by id.
func (db *DB) DeleteBackup(ctx context.Context, id string) error {
	db.log().Debug("DB.DeleteBackup: deleting backup record", zap.String("backup", id))
	db.mu.Lock()
	defer db.mu.Unlock()

	start := time.Now()
	err := db.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(backupsBucket)).Delete([]byte(id))
	})
	db.log().Debug("DB.DeleteBackup: delete", zap.String("bucket", backupsBucket), zap.String("key", id),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}
