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

// log returns the database's logger, or the process-wide one when it was
// opened without one, so a trace never panics on a nil logger.
func (db *DB) log() *zap.Logger {
	if db == nil || db.logger == nil {
		return zap.L()
	}
	return db.logger
}

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
	logger.Debug("closeAfterFailedOpen: closing database after aborted open", zap.String("path", path))
	if err := db.Close(); err != nil {
		logger.Warn("Failed to close the database after an aborted open; the file may stay locked",
			zap.String("path", path), zap.Error(err))
	}
}

// Open opens the database connection
func Open(cfg *Config, logger *zap.Logger) (*DB, error) {
	trace := logger
	if trace == nil {
		trace = zap.L()
	}
	if cfg == nil {
		trace.Debug("Open: no config given, using default path", zap.String("path", DefaultDBPath))
		cfg = &Config{Path: DefaultDBPath}
	}
	trace.Debug("Open: opening database",
		zap.String("path", cfg.Path),
		zap.Int("audit_retention", cfg.AuditRetention),
		zap.Duration("audit_max_age", cfg.AuditMaxAge))

	// Ensure directory exists
	dir := filepath.Dir(cfg.Path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		trace.Debug("Open: creating database directory failed", zap.String("dir", dir), zap.Error(err))
		return nil, fmt.Errorf("failed to create database directory: %w", err)
	}

	// Sample this before opening: bolt.Open creates the file, after which there
	// is no way left to tell an unversioned production database apart from one
	// this process just made. A size check rather than bare existence, so a
	// stray touch(1) on the path is not mistaken for a database with history.
	preexisting := false
	if fi, statErr := os.Stat(cfg.Path); statErr == nil && fi.Size() > 0 {
		preexisting = true
		trace.Debug("Open: database file already holds data",
			zap.String("path", cfg.Path), zap.Int64("size_bytes", fi.Size()))
	} else {
		trace.Debug("Open: database file is new or empty", zap.String("path", cfg.Path), zap.Error(statErr))
	}

	// Open database
	start := time.Now()
	db, err := bolt.Open(cfg.Path, 0600, &bolt.Options{Timeout: 5 * time.Second})
	trace.Debug("Open: bolt open", zap.String("path", cfg.Path),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Version gate before anything is written, including bucket creation: a
	// database from a newer controller must be left exactly as it was found.
	// Migrations run before the bucket initialization below so each step sees
	// the layout that was actually on disk, not one already patched up by
	// CreateBucketIfNotExists.
	if err := ensureSchema(db, cfg.Path, preexisting, logger); err != nil {
		trace.Debug("Open: schema check failed", zap.String("path", cfg.Path), zap.Error(err))
		closeAfterFailedOpen(db, cfg.Path, logger)
		return nil, err
	}

	// Initialize buckets
	start = time.Now()
	if err := db.Update(func(tx *bolt.Tx) error {
		buckets := []string{nodesBucket, poolsBucket, resourcesBucket, profilesBucket, volumesBucket, gatewaysBucket, haConfigsBucket, rbacBucket, schedulesBucket, backupTargetsBucket, backupsBucket, backupSchedulesBucket, notifyChannelsBucket}
		for _, bucket := range buckets {
			_, err := tx.CreateBucketIfNotExists([]byte(bucket))
			trace.Debug("Open: ensuring bucket", zap.String("bucket", bucket), zap.Error(err))
			if err != nil {
				return fmt.Errorf("failed to create bucket %s: %w", bucket, err)
			}
		}
		return nil
	}); err != nil {
		trace.Debug("Open: bucket initialization failed", zap.String("path", cfg.Path), zap.Error(err))
		closeAfterFailedOpen(db, cfg.Path, logger)
		return nil, fmt.Errorf("failed to initialize buckets: %w", err)
	}
	trace.Debug("Open: buckets initialized", zap.Duration("took", time.Since(start)))

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
	db.log().Debug("DB.Close: closing database", zap.String("path", db.path))
	db.mu.Lock()
	defer db.mu.Unlock()
	err := db.db.Close()
	db.log().Debug("DB.Close: closed", zap.String("path", db.path), zap.Error(err))
	return err
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
	// LocalAgent marks a node the controller does not reach over SSH: the
	// Haify CSI node plugin running there applies its DRBD configuration
	// locally and reports in over the API instead.
	LocalAgent bool `json:",omitempty"`
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// SaveNode saves or updates a node
func (db *DB) SaveNode(ctx context.Context, node *Node) error {
	db.log().Debug("DB.SaveNode: saving node",
		zap.String("node", node.Name), zap.String("address", node.Address), zap.String("state", node.State))
	db.mu.Lock()
	defer db.mu.Unlock()

	now := time.Now()
	if node.CreatedAt.IsZero() {
		node.CreatedAt = now
	}
	node.UpdatedAt = now

	data, err := json.Marshal(node)
	if err != nil {
		db.log().Debug("DB.SaveNode: marshal failed", zap.String("node", node.Name), zap.Error(err))
		return fmt.Errorf("failed to marshal node: %w", err)
	}

	start := time.Now()
	err = db.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(nodesBucket))
		return b.Put([]byte(node.Address), data)
	})
	db.log().Debug("DB.SaveNode: put", zap.String("bucket", nodesBucket), zap.String("key", node.Address),
		zap.Int("size_bytes", len(data)), zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}

// GetNode retrieves a node by address
func (db *DB) GetNode(ctx context.Context, address string) (*Node, error) {
	db.log().Debug("DB.GetNode: reading node", zap.String("address", address))
	db.mu.RLock()
	defer db.mu.RUnlock()

	var node Node
	found, size := false, 0
	start := time.Now()
	err := db.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(nodesBucket))
		data := b.Get([]byte(address))
		if data == nil {
			return fmt.Errorf("node not found")
		}
		found, size = true, len(data)
		return json.Unmarshal(data, &node)
	})
	db.log().Debug("DB.GetNode: get", zap.String("bucket", nodesBucket), zap.String("key", address),
		zap.Bool("found", found), zap.Int("size_bytes", size),
		zap.Duration("took", time.Since(start)), zap.Error(err))

	if err != nil {
		return nil, err
	}
	return &node, nil
}

// ListNodes lists all nodes
func (db *DB) ListNodes(ctx context.Context) ([]*Node, error) {
	db.log().Debug("DB.ListNodes: listing nodes")
	db.mu.RLock()
	defer db.mu.RUnlock()

	var nodes []*Node
	start := time.Now()
	err := db.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(nodesBucket))
		return b.ForEach(func(k, v []byte) error {
			var node Node
			if err := json.Unmarshal(v, &node); err != nil {
				db.log().Debug("DB.ListNodes: decoding entry failed", zap.ByteString("key", k), zap.Error(err))
				return err
			}
			nodes = append(nodes, &node)
			return nil
		})
	})
	db.log().Debug("DB.ListNodes: scan", zap.String("bucket", nodesBucket), zap.Int("count", len(nodes)),
		zap.Duration("took", time.Since(start)), zap.Error(err))

	return nodes, err
}

// DeleteNode deletes a node by address
func (db *DB) DeleteNode(ctx context.Context, address string) error {
	db.log().Debug("DB.DeleteNode: deleting node", zap.String("address", address))
	db.mu.Lock()
	defer db.mu.Unlock()

	start := time.Now()
	err := db.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(nodesBucket))
		return b.Delete([]byte(address))
	})
	db.log().Debug("DB.DeleteNode: delete", zap.String("bucket", nodesBucket), zap.String("key", address),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
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
	db.log().Debug("DB.SavePool: saving pool",
		zap.String("pool", pool.Name), zap.String("type", pool.Type), zap.String("node", pool.Node))
	db.mu.Lock()
	defer db.mu.Unlock()

	now := time.Now()
	if pool.CreatedAt.IsZero() {
		pool.CreatedAt = now
	}
	pool.UpdatedAt = now

	data, err := json.Marshal(pool)
	if err != nil {
		db.log().Debug("DB.SavePool: marshal failed", zap.String("pool", pool.Name), zap.Error(err))
		return fmt.Errorf("failed to marshal pool: %w", err)
	}

	start := time.Now()
	err = db.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(poolsBucket))
		return b.Put([]byte(pool.Name), data)
	})
	db.log().Debug("DB.SavePool: put", zap.String("bucket", poolsBucket), zap.String("key", pool.Name),
		zap.Int("size_bytes", len(data)), zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}

// GetPool retrieves a pool by name
func (db *DB) GetPool(ctx context.Context, name string) (*Pool, error) {
	db.log().Debug("DB.GetPool: reading pool", zap.String("pool", name))
	db.mu.RLock()
	defer db.mu.RUnlock()

	var pool Pool
	found, size := false, 0
	start := time.Now()
	err := db.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(poolsBucket))
		data := b.Get([]byte(name))
		if data == nil {
			return fmt.Errorf("pool not found")
		}
		found, size = true, len(data)
		return json.Unmarshal(data, &pool)
	})
	db.log().Debug("DB.GetPool: get", zap.String("bucket", poolsBucket), zap.String("key", name),
		zap.Bool("found", found), zap.Int("size_bytes", size),
		zap.Duration("took", time.Since(start)), zap.Error(err))

	if err != nil {
		return nil, err
	}
	return &pool, nil
}

// ListPools lists all pools
func (db *DB) ListPools(ctx context.Context) ([]*Pool, error) {
	db.log().Debug("DB.ListPools: listing pools")
	db.mu.RLock()
	defer db.mu.RUnlock()

	var pools []*Pool
	start := time.Now()
	err := db.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(poolsBucket))
		return b.ForEach(func(k, v []byte) error {
			var pool Pool
			if err := json.Unmarshal(v, &pool); err != nil {
				db.log().Debug("DB.ListPools: decoding entry failed", zap.ByteString("key", k), zap.Error(err))
				return err
			}
			pools = append(pools, &pool)
			return nil
		})
	})
	db.log().Debug("DB.ListPools: scan", zap.String("bucket", poolsBucket), zap.Int("count", len(pools)),
		zap.Duration("took", time.Since(start)), zap.Error(err))

	return pools, err
}

// DeletePool deletes a pool by name
func (db *DB) DeletePool(ctx context.Context, name string) error {
	db.log().Debug("DB.DeletePool: deleting pool", zap.String("pool", name))
	db.mu.Lock()
	defer db.mu.Unlock()

	start := time.Now()
	err := db.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(poolsBucket))
		return b.Delete([]byte(name))
	})
	db.log().Debug("DB.DeletePool: delete", zap.String("bucket", poolsBucket), zap.String("key", name),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}

// ==================== RBAC ====================

// SaveRBAC persists the serialized RBAC snapshot (opaque to the database; the
// rbac package owns the encoding).
func (db *DB) SaveRBAC(ctx context.Context, data []byte) error {
	// The snapshot holds token digests: its size is traced, never its bytes.
	db.log().Debug("DB.SaveRBAC: saving RBAC snapshot", zap.Int("size_bytes", len(data)))
	db.mu.Lock()
	defer db.mu.Unlock()

	// Copy: bbolt does not retain the caller's slice past the transaction.
	buf := make([]byte, len(data))
	copy(buf, data)
	start := time.Now()
	err := db.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(rbacBucket)).Put([]byte(rbacStateKey), buf)
	})
	db.log().Debug("DB.SaveRBAC: put", zap.String("bucket", rbacBucket), zap.String("key", rbacStateKey),
		zap.Int("size_bytes", len(buf)), zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}

// LoadRBAC returns the stored RBAC snapshot, or nil if none has been saved yet.
func (db *DB) LoadRBAC(ctx context.Context) ([]byte, error) {
	db.log().Debug("DB.LoadRBAC: loading RBAC snapshot")
	db.mu.RLock()
	defer db.mu.RUnlock()

	var out []byte
	start := time.Now()
	err := db.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket([]byte(rbacBucket)).Get([]byte(rbacStateKey))
		if v != nil {
			out = make([]byte, len(v))
			copy(out, v)
		}
		return nil
	})
	db.log().Debug("DB.LoadRBAC: get", zap.String("bucket", rbacBucket), zap.String("key", rbacStateKey),
		zap.Bool("found", out != nil), zap.Int("size_bytes", len(out)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	return out, err
}
