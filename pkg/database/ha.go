package database

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
	"go.uber.org/zap"
)

// ==================== HA CONFIG ====================

// HaConfig represents a highly available configuration: what `ha create` was
// asked for. The promoter config on the nodes is what runs; this is the record
// of how it was made, kept whole so it can be shown and made again.
type HaConfig struct {
	Resource   string
	VIP        string
	MountPoint string
	FsType     string
	Services   []string
	// OcfAgents are the OCF agents appended after the services, in order.
	OcfAgents []HaOcfAgent `json:",omitempty"`
	// StartItems, when set, is the promoter's start list exactly as given;
	// it then replaces Services, MountPoint, VIP and OcfAgents as the order.
	StartItems []HaStartItem `json:",omitempty"`
	// PreferredNodes orders where drbd-reactor starts the resource
	// (preferred-nodes), and PreferredNodesPolicy says whether that also
	// moves it back ("always") or only picks where it starts ("start-only").
	PreferredNodes       []string `json:",omitempty"`
	PreferredNodesPolicy string   `json:",omitempty"`
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// HaOcfAgent is one OCF resource agent in an HA promoter's start list.
type HaOcfAgent struct {
	Provider string
	Name     string
	Instance string
	Params   map[string]string `json:",omitempty"`
}

// HaStartItem is one entry of an explicit start list: a systemd unit or an
// OCF agent.
type HaStartItem struct {
	SystemdUnit string      `json:",omitempty"`
	Ocf         *HaOcfAgent `json:",omitempty"`
}

// SaveHaConfig saves or updates an HA configuration
func (db *DB) SaveHaConfig(ctx context.Context, cfg *HaConfig) error {
	// OCF agent params can carry passwords: only counts are traced.
	db.log().Debug("DB.SaveHaConfig: saving HA config",
		zap.String("resource", cfg.Resource), zap.String("vip", cfg.VIP), zap.String("mount_point", cfg.MountPoint),
		zap.Strings("services", cfg.Services), zap.Int("ocf_agents", len(cfg.OcfAgents)),
		zap.Int("start_items", len(cfg.StartItems)), zap.Strings("preferred_nodes", cfg.PreferredNodes))
	db.mu.Lock()
	defer db.mu.Unlock()

	now := time.Now()
	if cfg.CreatedAt.IsZero() {
		cfg.CreatedAt = now
	}
	cfg.UpdatedAt = now

	data, err := json.Marshal(cfg)
	if err != nil {
		db.log().Debug("DB.SaveHaConfig: marshal failed", zap.String("resource", cfg.Resource), zap.Error(err))
		return fmt.Errorf("failed to marshal ha config: %w", err)
	}

	start := time.Now()
	err = db.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(haConfigsBucket))
		return b.Put([]byte(cfg.Resource), data)
	})
	db.log().Debug("DB.SaveHaConfig: put", zap.String("bucket", haConfigsBucket), zap.String("key", cfg.Resource),
		zap.Int("size_bytes", len(data)), zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}

// GetHaConfig retrieves an HA configuration by resource name
func (db *DB) GetHaConfig(ctx context.Context, resource string) (*HaConfig, error) {
	db.log().Debug("DB.GetHaConfig: reading HA config", zap.String("resource", resource))
	db.mu.RLock()
	defer db.mu.RUnlock()

	var cfg HaConfig
	found, size := false, 0
	start := time.Now()
	err := db.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(haConfigsBucket))
		data := b.Get([]byte(resource))
		if data == nil {
			return fmt.Errorf("ha config not found")
		}
		found, size = true, len(data)
		return json.Unmarshal(data, &cfg)
	})
	db.log().Debug("DB.GetHaConfig: get", zap.String("bucket", haConfigsBucket), zap.String("key", resource),
		zap.Bool("found", found), zap.Int("size_bytes", size),
		zap.Duration("took", time.Since(start)), zap.Error(err))

	if err != nil {
		return nil, err
	}
	return &cfg, nil
}

// ListHaConfigs lists all HA configurations
func (db *DB) ListHaConfigs(ctx context.Context) ([]*HaConfig, error) {
	db.log().Debug("DB.ListHaConfigs: listing HA configs")
	db.mu.RLock()
	defer db.mu.RUnlock()

	var configs []*HaConfig
	start := time.Now()
	err := db.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(haConfigsBucket))
		return b.ForEach(func(k, v []byte) error {
			var cfg HaConfig
			if err := json.Unmarshal(v, &cfg); err != nil {
				db.log().Debug("DB.ListHaConfigs: decoding entry failed", zap.ByteString("key", k), zap.Error(err))
				return err
			}
			configs = append(configs, &cfg)
			return nil
		})
	})
	db.log().Debug("DB.ListHaConfigs: scan", zap.String("bucket", haConfigsBucket),
		zap.Int("count", len(configs)), zap.Duration("took", time.Since(start)), zap.Error(err))

	return configs, err
}

// DeleteHaConfig deletes an HA configuration by resource name
func (db *DB) DeleteHaConfig(ctx context.Context, resource string) error {
	db.log().Debug("DB.DeleteHaConfig: deleting HA config", zap.String("resource", resource))
	db.mu.Lock()
	defer db.mu.Unlock()

	start := time.Now()
	err := db.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(haConfigsBucket))
		return b.Delete([]byte(resource))
	})
	db.log().Debug("DB.DeleteHaConfig: delete", zap.String("bucket", haConfigsBucket), zap.String("key", resource),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}
