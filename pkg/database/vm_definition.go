package database

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
	"go.uber.org/zap"
)

// The libvirt definition of a guest that `ha create --vm` handed to
// drbd-reactor. Every replica that may run the guest needs the same
// definition under the same UUID; this record is the copy the controller
// defines on a replica added later, and what each node's own copy is compared
// with.
//
// XML can hold credentials (a VNC or SPICE password), so nothing here traces
// it: only its size.

const vmDefinitionsBucket = "vm_definitions"

// VMDefinition is one HA guest's definition, keyed by its resource.
type VMDefinition struct {
	Resource string
	// Domain is the libvirt name and UUID its identity on every node.
	Domain string
	UUID   string
	// XML is the definition as the source node's libvirt wrote it.
	XML string
	// SourceNode is the node XML was taken from.
	SourceNode string
	// NodeHashes is the digest of each node's own copy as it was right after
	// the last sync. libvirt rewrites what it is given, so two nodes' copies
	// of one definition need not be byte-equal; a node whose copy no longer
	// matches its own entry has been edited there.
	NodeHashes map[string]string `json:",omitempty"`
	// Placed names the nodes that had no definition until Haify defined one.
	// Only those are cleaned up: a definition the operator made is theirs.
	Placed    []string `json:",omitempty"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

// SaveVMDefinition creates or replaces a guest's definition record.
func (db *DB) SaveVMDefinition(ctx context.Context, d *VMDefinition) error {
	if d == nil || d.Resource == "" {
		db.log().Debug("DB.SaveVMDefinition: refusing a record without a resource", zap.Bool("nil_record", d == nil))
		return fmt.Errorf("resource is required")
	}
	db.log().Debug("DB.SaveVMDefinition: saving", zap.String("resource", d.Resource), zap.String("domain", d.Domain),
		zap.String("uuid", d.UUID), zap.String("source_node", d.SourceNode), zap.Int("xml_bytes", len(d.XML)),
		zap.Strings("placed", d.Placed), zap.Int("node_hashes", len(d.NodeHashes)))
	db.mu.Lock()
	defer db.mu.Unlock()

	now := time.Now()
	if d.CreatedAt.IsZero() {
		d.CreatedAt = now
	}
	d.UpdatedAt = now
	data, err := json.Marshal(d)
	if err != nil {
		db.log().Debug("DB.SaveVMDefinition: marshal failed", zap.String("resource", d.Resource), zap.Error(err))
		return fmt.Errorf("failed to marshal the guest definition: %w", err)
	}
	start := time.Now()
	err = db.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(vmDefinitionsBucket))
		if err != nil {
			return err
		}
		return b.Put([]byte(d.Resource), data)
	})
	db.log().Debug("DB.SaveVMDefinition: put", zap.String("bucket", vmDefinitionsBucket), zap.String("key", d.Resource),
		zap.Int("size_bytes", len(data)), zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}

// GetVMDefinition returns the definition kept for resource, or nil.
func (db *DB) GetVMDefinition(ctx context.Context, resource string) (*VMDefinition, error) {
	db.log().Debug("DB.GetVMDefinition: reading", zap.String("resource", resource))
	db.mu.RLock()
	defer db.mu.RUnlock()

	var d *VMDefinition
	size := 0
	start := time.Now()
	err := db.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(vmDefinitionsBucket))
		if b == nil {
			return nil
		}
		data := b.Get([]byte(resource))
		if data == nil {
			return nil
		}
		size = len(data)
		d = &VMDefinition{}
		return json.Unmarshal(data, d)
	})
	db.log().Debug("DB.GetVMDefinition: get", zap.String("bucket", vmDefinitionsBucket), zap.String("key", resource),
		zap.Bool("found", d != nil), zap.Int("size_bytes", size), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, fmt.Errorf("failed to read the guest definition of %s: %w", resource, err)
	}
	return d, nil
}

// ListVMDefinitions returns every kept guest definition.
func (db *DB) ListVMDefinitions(ctx context.Context) ([]*VMDefinition, error) {
	db.log().Debug("DB.ListVMDefinitions: listing")
	db.mu.RLock()
	defer db.mu.RUnlock()

	var out []*VMDefinition
	start := time.Now()
	err := db.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(vmDefinitionsBucket))
		if b == nil {
			db.log().Debug("DB.ListVMDefinitions: bucket absent", zap.String("bucket", vmDefinitionsBucket))
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			var d VMDefinition
			if err := json.Unmarshal(v, &d); err != nil {
				return fmt.Errorf("guest definition %s: %w", k, err)
			}
			out = append(out, &d)
			return nil
		})
	})
	db.log().Debug("DB.ListVMDefinitions: scan", zap.String("bucket", vmDefinitionsBucket), zap.Int("count", len(out)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	return out, err
}

// DeleteVMDefinition removes a guest's definition record. Removing one that
// does not exist is not an error.
func (db *DB) DeleteVMDefinition(ctx context.Context, resource string) error {
	db.log().Debug("DB.DeleteVMDefinition: deleting", zap.String("resource", resource))
	db.mu.Lock()
	defer db.mu.Unlock()
	start := time.Now()
	err := db.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(vmDefinitionsBucket))
		if b == nil {
			return nil
		}
		return b.Delete([]byte(resource))
	})
	db.log().Debug("DB.DeleteVMDefinition: delete", zap.String("bucket", vmDefinitionsBucket), zap.String("key", resource),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}
