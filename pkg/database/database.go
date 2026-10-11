package database

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	bolt "go.etcd.io/bbolt"
	"go.uber.org/zap"
)

// Bucket names
const (
	nodesBucket     = "nodes"
	poolsBucket     = "pools"
	resourcesBucket = "resources"
	profilesBucket  = "resource_profiles"
	volumesBucket   = "volumes"
	gatewaysBucket  = "gateways"
	haConfigsBucket = "ha_configs"
	rbacBucket      = "rbac"
	schedulesBucket = "snapshot_schedules"
	// Off-cluster backup shipping: the repositories (targets) and the metadata
	// of each copy pushed to them. See pkg/backup.
	backupTargetsBucket = "backup_targets"
	backupsBucket       = "backups"
	// backupSchedulesBucket holds cron-driven backups, keyed resource@target.
	backupSchedulesBucket = "backup_schedules"

	// notifyChannelsBucket holds where alerts are delivered. Kept in the
	// database so a channel can be changed from the UI without restarting the
	// controller — an alerting path must not need an outage to reconfigure.
	notifyChannelsBucket = "notify_channels"
)

// rbacStateKey is the single key under rbacBucket holding the serialized RBAC
// snapshot (policies, role assignments and token digests).
const rbacStateKey = "state"

// DB holds the database connection
type DB struct {
	db     *bolt.DB
	path   string
	logger *zap.Logger
	mu     sync.RWMutex
	// auditCap bounds the audit trail; zero means DefaultAuditRetention.
	auditCap int
	// auditMaxAge is how long audit entries are kept; zero keeps them until
	// the cap drops them.
	auditMaxAge time.Duration
	// auditTruncated counts entries the cap dropped before auditMaxAge.
	auditTruncated atomic.Uint64
}

// Config holds database configuration
type Config struct {
	Path string // Database file path
	// AuditRetention bounds the number of audit entries kept. Zero selects
	// DefaultAuditRetention.
	AuditRetention int
	// AuditMaxAge is how long audit entries are kept. Zero: until the cap.
	AuditMaxAge time.Duration
}

// Default database path
const DefaultDBPath = "/var/lib/haify/haify.db"

// closeAfterFailedOpen releases the file when Open gives up after bolt has
// already taken it.
//
// bolt holds an exclusive flock for as long as the handle lives, so returning
// an error without closing turns the operator's next start into a lock timeout
// on a database nothing is using — an error that says nothing about the schema
// or bucket problem that actually stopped this one. The close error is logged
// rather than returned because the error Open is already carrying is the one
// worth reading; nothing has been written that a failed close could lose, since
// bolt fsyncs at commit.
func closeAfterFailedOpen(db *bolt.DB, path string, logger *zap.Logger) {
	if logger == nil {
		// Open takes the logger from its caller and does not check it; matching
		// ensureSchema's guard keeps a cleanup path from panicking on the way
		// out of an error the caller still needs to see.
		logger = zap.NewNop()
	}
	if err := db.Close(); err != nil {
		logger.Warn("Failed to close the database after an aborted open; the file may stay locked",
			zap.String("path", path), zap.Error(err))
	}
}

// Open opens the database connection
func Open(cfg *Config, logger *zap.Logger) (*DB, error) {
	if cfg == nil {
		cfg = &Config{Path: DefaultDBPath}
	}

	// Ensure directory exists
	dir := filepath.Dir(cfg.Path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create database directory: %w", err)
	}

	// Sample this before opening: bolt.Open creates the file, after which there
	// is no way left to tell an unversioned production database apart from one
	// this process just made. A size check rather than bare existence, so a
	// stray touch(1) on the path is not mistaken for a database with history.
	preexisting := false
	if fi, statErr := os.Stat(cfg.Path); statErr == nil && fi.Size() > 0 {
		preexisting = true
	}

	// Open database
	db, err := bolt.Open(cfg.Path, 0600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Version gate before anything is written, including bucket creation: a
	// database from a newer controller must be left exactly as it was found.
	// Migrations run before the bucket initialization below so each step sees
	// the layout that was actually on disk, not one already patched up by
	// CreateBucketIfNotExists.
	if err := ensureSchema(db, cfg.Path, preexisting, logger); err != nil {
		closeAfterFailedOpen(db, cfg.Path, logger)
		return nil, err
	}

	// Initialize buckets
	if err := db.Update(func(tx *bolt.Tx) error {
		buckets := []string{nodesBucket, poolsBucket, resourcesBucket, profilesBucket, volumesBucket, gatewaysBucket, haConfigsBucket, rbacBucket, schedulesBucket, backupTargetsBucket, backupsBucket, backupSchedulesBucket, notifyChannelsBucket}
		for _, bucket := range buckets {
			_, err := tx.CreateBucketIfNotExists([]byte(bucket))
			if err != nil {
				return fmt.Errorf("failed to create bucket %s: %w", bucket, err)
			}
		}
		return nil
	}); err != nil {
		closeAfterFailedOpen(db, cfg.Path, logger)
		return nil, fmt.Errorf("failed to initialize buckets: %w", err)
	}

	database := &DB{
		db:          db,
		path:        cfg.Path,
		logger:      logger,
		auditCap:    cfg.AuditRetention,
		auditMaxAge: cfg.AuditMaxAge,
	}

	logger.Info("Database opened", zap.String("path", cfg.Path))

	return database, nil
}

// Close closes the database connection
func (db *DB) Close() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.db.Close()
}

// ==================== NODE ====================

// Node represents a stored node
type Node struct {
	Name    string
	Address string
	// ReplicationAddress is the address DRBD uses for this node in generated
	// .res files. Empty means it shares Address — which is how every node
	// registered before this field existed deserializes, so the single-network
	// setup keeps working untouched.
	ReplicationAddress string
	// ReplicationPaths are further addresses DRBD may use for this node, one
	// per additional replication network.
	ReplicationPaths []string `json:",omitempty"`
	Hostname         string
	State            string
	LastSeen         time.Time
	Version          string
	// Labels is a JSON-encoded map[string]string of arbitrary node tags (e.g.
	// {"rack":"A","zone":"east"}) used by placement constraints. Empty for nodes
	// registered before labels existed — deserializes as no labels.
	Labels string
	// OfflineSince is when the node stopped answering, zero while it does.
	// Self-healing (auto-evict) counts from it, across controller failovers.
	OfflineSince time.Time `json:",omitempty"`
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// SaveNode saves or updates a node
func (db *DB) SaveNode(ctx context.Context, node *Node) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	now := time.Now()
	if node.CreatedAt.IsZero() {
		node.CreatedAt = now
	}
	node.UpdatedAt = now

	data, err := json.Marshal(node)
	if err != nil {
		return fmt.Errorf("failed to marshal node: %w", err)
	}

	return db.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(nodesBucket))
		return b.Put([]byte(node.Address), data)
	})
}

// GetNode retrieves a node by address
func (db *DB) GetNode(ctx context.Context, address string) (*Node, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	var node Node
	err := db.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(nodesBucket))
		data := b.Get([]byte(address))
		if data == nil {
			return fmt.Errorf("node not found")
		}
		return json.Unmarshal(data, &node)
	})

	if err != nil {
		return nil, err
	}
	return &node, nil
}

// ListNodes lists all nodes
func (db *DB) ListNodes(ctx context.Context) ([]*Node, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	var nodes []*Node
	err := db.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(nodesBucket))
		return b.ForEach(func(k, v []byte) error {
			var node Node
			if err := json.Unmarshal(v, &node); err != nil {
				return err
			}
			nodes = append(nodes, &node)
			return nil
		})
	})

	return nodes, err
}

// DeleteNode deletes a node by address
func (db *DB) DeleteNode(ctx context.Context, address string) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	return db.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(nodesBucket))
		return b.Delete([]byte(address))
	})
}

// ==================== POOL ====================

// Pool represents a storage pool
type Pool struct {
	Name      string
	Type      string
	Node      string
	TotalGB   int
	FreeGB    int
	Devices   string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// SavePool saves or updates a pool
func (db *DB) SavePool(ctx context.Context, pool *Pool) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	now := time.Now()
	if pool.CreatedAt.IsZero() {
		pool.CreatedAt = now
	}
	pool.UpdatedAt = now

	data, err := json.Marshal(pool)
	if err != nil {
		return fmt.Errorf("failed to marshal pool: %w", err)
	}

	return db.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(poolsBucket))
		return b.Put([]byte(pool.Name), data)
	})
}

// GetPool retrieves a pool by name
func (db *DB) GetPool(ctx context.Context, name string) (*Pool, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	var pool Pool
	err := db.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(poolsBucket))
		data := b.Get([]byte(name))
		if data == nil {
			return fmt.Errorf("pool not found")
		}
		return json.Unmarshal(data, &pool)
	})

	if err != nil {
		return nil, err
	}
	return &pool, nil
}

// ListPools lists all pools
func (db *DB) ListPools(ctx context.Context) ([]*Pool, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	var pools []*Pool
	err := db.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(poolsBucket))
		return b.ForEach(func(k, v []byte) error {
			var pool Pool
			if err := json.Unmarshal(v, &pool); err != nil {
				return err
			}
			pools = append(pools, &pool)
			return nil
		})
	})

	return pools, err
}

// DeletePool deletes a pool by name
func (db *DB) DeletePool(ctx context.Context, name string) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	return db.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(poolsBucket))
		return b.Delete([]byte(name))
	})
}

// ==================== RBAC ====================

// SaveRBAC persists the serialized RBAC snapshot (opaque to the database; the
// rbac package owns the encoding).
func (db *DB) SaveRBAC(ctx context.Context, data []byte) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	// Copy: bbolt does not retain the caller's slice past the transaction.
	buf := make([]byte, len(data))
	copy(buf, data)
	return db.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(rbacBucket)).Put([]byte(rbacStateKey), buf)
	})
}

// LoadRBAC returns the stored RBAC snapshot, or nil if none has been saved yet.
func (db *DB) LoadRBAC(ctx context.Context) ([]byte, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	var out []byte
	err := db.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket([]byte(rbacBucket)).Get([]byte(rbacStateKey))
		if v != nil {
			out = make([]byte, len(v))
			copy(out, v)
		}
		return nil
	})
	return out, err
}
