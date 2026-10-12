package database

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
	"go.uber.org/zap"
)

// ==================== GATEWAY ====================

// GatewayType represents the gateway type
type GatewayType string

const (
	GatewayTypeNFS    GatewayType = "nfs"
	GatewayTypeISCSI  GatewayType = "iscsi"
	GatewayTypeNVMEOF GatewayType = "nvmeof"
	GatewayTypeSMB    GatewayType = "smb"
)

// Gateway represents a storage gateway
type Gateway struct {
	ID         int64
	Name       string
	Resource   string
	Type       GatewayType
	Config     map[string]interface{}
	Status     string
	ActiveNode string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// SaveGateway saves or updates a gateway
func (db *DB) SaveGateway(ctx context.Context, gateway *Gateway) error {
	// Config can hold CHAP secrets and SMB passwords: only its key count is traced.
	db.log().Debug("DB.SaveGateway: saving gateway",
		zap.String("gateway", gateway.Name), zap.String("resource", gateway.Resource),
		zap.String("type", string(gateway.Type)), zap.String("status", gateway.Status),
		zap.String("active_node", gateway.ActiveNode), zap.Int("config_keys", len(gateway.Config)))
	db.mu.Lock()
	defer db.mu.Unlock()

	now := time.Now()
	if gateway.CreatedAt.IsZero() {
		db.log().Debug("DB.SaveGateway: new gateway, assigning id", zap.String("gateway", gateway.Name))
		gateway.CreatedAt = now
		gateway.ID = time.Now().UnixNano()
	}
	gateway.UpdatedAt = now

	data, err := json.Marshal(gateway)
	if err != nil {
		db.log().Debug("DB.SaveGateway: marshal failed", zap.String("gateway", gateway.Name), zap.Error(err))
		return fmt.Errorf("failed to marshal gateway: %w", err)
	}

	start := time.Now()
	err = db.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(gatewaysBucket))
		return b.Put([]byte(gateway.Name), data)
	})
	db.log().Debug("DB.SaveGateway: put", zap.String("bucket", gatewaysBucket), zap.String("key", gateway.Name),
		zap.Int("size_bytes", len(data)), zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}

// GetGateway retrieves a gateway by name
func (db *DB) GetGateway(ctx context.Context, name string) (*Gateway, error) {
	db.log().Debug("DB.GetGateway: reading gateway", zap.String("gateway", name))
	db.mu.RLock()
	defer db.mu.RUnlock()

	var gateway Gateway
	found, size := false, 0
	start := time.Now()
	err := db.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(gatewaysBucket))
		data := b.Get([]byte(name))
		if data == nil {
			return fmt.Errorf("gateway not found")
		}
		found, size = true, len(data)
		return json.Unmarshal(data, &gateway)
	})
	db.log().Debug("DB.GetGateway: get", zap.String("bucket", gatewaysBucket), zap.String("key", name),
		zap.Bool("found", found), zap.Int("size_bytes", size),
		zap.Duration("took", time.Since(start)), zap.Error(err))

	if err != nil {
		return nil, err
	}
	return &gateway, nil
}

// GetGatewayByResource retrieves a gateway by resource name.
func (db *DB) GetGatewayByResource(ctx context.Context, resource string) (*Gateway, error) {
	db.log().Debug("DB.GetGatewayByResource: looking up gateway", zap.String("resource", resource))
	db.mu.RLock()
	defer db.mu.RUnlock()

	var result *Gateway
	scanned := 0
	start := time.Now()
	err := db.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(gatewaysBucket))
		return b.ForEach(func(k, v []byte) error {
			scanned++
			var gateway Gateway
			if err := json.Unmarshal(v, &gateway); err != nil {
				db.log().Debug("DB.GetGatewayByResource: decoding entry failed", zap.ByteString("key", k), zap.Error(err))
				return err
			}
			if gateway.Resource != resource && gateway.Name != resource {
				return nil
			}
			db.log().Debug("DB.GetGatewayByResource: matched", zap.ByteString("key", k),
				zap.String("type", string(gateway.Type)))
			copyGateway := gateway
			result = &copyGateway
			return nil
		})
	})
	db.log().Debug("DB.GetGatewayByResource: scan", zap.String("bucket", gatewaysBucket), zap.String("resource", resource),
		zap.Int("scanned", scanned), zap.Bool("found", result != nil),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, fmt.Errorf("gateway not found")
	}
	return result, nil
}

// ListGateways lists all gateways
func (db *DB) ListGateways(ctx context.Context) ([]*Gateway, error) {
	db.log().Debug("DB.ListGateways: listing gateways")
	db.mu.RLock()
	defer db.mu.RUnlock()

	var gateways []*Gateway
	start := time.Now()
	err := db.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(gatewaysBucket))
		return b.ForEach(func(k, v []byte) error {
			var gateway Gateway
			if err := json.Unmarshal(v, &gateway); err != nil {
				db.log().Debug("DB.ListGateways: decoding entry failed", zap.ByteString("key", k), zap.Error(err))
				return err
			}
			gateways = append(gateways, &gateway)
			return nil
		})
	})
	db.log().Debug("DB.ListGateways: scan", zap.String("bucket", gatewaysBucket),
		zap.Int("count", len(gateways)), zap.Duration("took", time.Since(start)), zap.Error(err))

	return gateways, err
}

// DeleteGateway deletes a gateway by name
func (db *DB) DeleteGateway(ctx context.Context, name string) error {
	db.log().Debug("DB.DeleteGateway: deleting gateway", zap.String("gateway", name))
	db.mu.Lock()
	defer db.mu.Unlock()

	start := time.Now()
	err := db.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(gatewaysBucket))
		return b.Delete([]byte(name))
	})
	db.log().Debug("DB.DeleteGateway: delete", zap.String("bucket", gatewaysBucket), zap.String("key", name),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}

// DeleteGatewayByResource deletes all gateways matching the resource name.
func (db *DB) DeleteGatewayByResource(ctx context.Context, resource string) error {
	db.log().Debug("DB.DeleteGatewayByResource: deleting gateways of resource", zap.String("resource", resource))
	db.mu.Lock()
	defer db.mu.Unlock()

	start := time.Now()
	err := db.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(gatewaysBucket))
		var keys [][]byte
		err := b.ForEach(func(k, v []byte) error {
			var gateway Gateway
			if err := json.Unmarshal(v, &gateway); err != nil {
				db.log().Debug("DB.DeleteGatewayByResource: decoding entry failed", zap.ByteString("key", k), zap.Error(err))
				return err
			}
			if gateway.Resource == resource || gateway.Name == resource {
				keys = append(keys, append([]byte(nil), k...))
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, key := range keys {
			err := b.Delete(key)
			db.log().Debug("DB.DeleteGatewayByResource: delete", zap.String("bucket", gatewaysBucket),
				zap.ByteString("key", key), zap.Error(err))
			if err != nil {
				return err
			}
		}
		return nil
	})
	db.log().Debug("DB.DeleteGatewayByResource: done", zap.String("resource", resource),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}
