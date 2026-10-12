package database

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
)

// ==================== RESOURCE ====================

// Resource represents a DRBD resource
type Resource struct {
	Name     string
	Port     int
	Nodes    string
	Protocol string
	Replicas int
	// Profile is the resource profile the resource is a member of: options
	// set on the profile are applied to it, and adjusting the profile brings
	// its replica count into line. Empty for a resource in no profile.
	Profile string
	// Labels are arbitrary resource metadata used for organization and search.
	Labels map[string]string
	// DisklessNodes is a comma-separated list of node names that participate
	// in the resource as diskless quorum tiebreakers (they vote in quorum but
	// store no data). Empty for ordinary all-diskful resources.
	DisklessNodes string
	// DisklessClients is a comma-separated list of node names attached to the
	// resource as diskless data clients: they carry no local replica but connect
	// over DRBD and can be promoted Primary to read/write the volume over the
	// network (LINSTOR's "diskless client"). Distinct from DisklessNodes, which
	// are quorum-only tiebreakers that must never be promoted or mounted. Empty
	// for resources with no diskless clients.
	DisklessClients string
	// StaleConfigNodes lists members whose copy of the DRBD config was not
	// updated because they were unreachable at the time (add-replica with
	// allow_unreachable). They are repaired when they answer again.
	StaleConfigNodes string `json:",omitempty"`
	// MoveFrom is the node a move-replica takes this resource's replica off
	// once the new one, on MoveTo, is UpToDate (pkg/controller/self_heal_move.go).
	MoveFrom string `json:",omitempty"`
	MoveTo   string `json:",omitempty"`
	// WAN replication (opt-in). All zero-valued for an ordinary LAN resource, so
	// existing records deserialize as LAN and every WAN code path stays gated
	// behind WANMode. See docs/design/wan-replication.md.
	WANMode    bool   // false = LAN (default); true routes DRBD via a per-resource haify-proxy pair
	DRNode     string // the DR-site node name (WAN only)
	DREndpoint string // the DR site's public WAN address the primary dials (WAN only)
	WANPort    int    // WAN mTLS port the DR acceptor listens on (WAN only)
	// WANEgressAddress optionally pins the source address the primary's proxy
	// binds before dialing the DR site (WAN only). Empty = routing table decides.
	// Persisted because the proxy config is re-rendered from this record, so a
	// controller restart would otherwise silently drop the pinned egress.
	WANEgressAddress string
	// Encrypted records that every replica's backing volume is a LUKS2
	// container, so DRBD consumes /dev/mapper/<container> instead of the LV or
	// zvol directly. False for every record written before this existed, which
	// is exactly right: those resources are not encrypted.
	//
	// No key material is stored here, or anywhere else in the controller. Each
	// node generates and keeps its own key; see pkg/controller/encryption.go.
	Encrypted bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ResourceProfile contains defaults applied when a resource is created.
type ResourceProfile struct {
	Name        string
	Protocol    string
	StorageType string
	Pool        string
	Replicas    int
	OnDifferent []string
	OnSame      []string
	DRBDOptions map[string]string
	Labels      map[string]string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// SaveResourceProfile saves or updates a resource profile.
func (db *DB) SaveResourceProfile(ctx context.Context, profile *ResourceProfile) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	now := time.Now()
	if profile.CreatedAt.IsZero() {
		profile.CreatedAt = now
	}
	profile.UpdatedAt = now

	data, err := json.Marshal(profile)
	if err != nil {
		return fmt.Errorf("failed to marshal resource profile: %w", err)
	}

	return db.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(profilesBucket)).Put([]byte(profile.Name), data)
	})
}

// GetResourceProfile retrieves a resource profile by name.
func (db *DB) GetResourceProfile(ctx context.Context, name string) (*ResourceProfile, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	var profile ResourceProfile
	err := db.db.View(func(tx *bolt.Tx) error {
		data := tx.Bucket([]byte(profilesBucket)).Get([]byte(name))
		if data == nil {
			return fmt.Errorf("resource profile not found")
		}
		return json.Unmarshal(data, &profile)
	})
	if err != nil {
		return nil, err
	}
	return &profile, nil
}

// ListResourceProfiles lists all resource profiles.
func (db *DB) ListResourceProfiles(ctx context.Context) ([]*ResourceProfile, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	profiles := make([]*ResourceProfile, 0)
	err := db.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(profilesBucket)).ForEach(func(_, value []byte) error {
			var profile ResourceProfile
			if err := json.Unmarshal(value, &profile); err != nil {
				return err
			}
			profiles = append(profiles, &profile)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return profiles, nil
}

// DeleteResourceProfile deletes a resource profile by name.
func (db *DB) DeleteResourceProfile(ctx context.Context, name string) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	return db.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(profilesBucket)).Delete([]byte(name))
	})
}

// SaveResource saves or updates a resource
func (db *DB) SaveResource(ctx context.Context, resource *Resource) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	now := time.Now()
	if resource.CreatedAt.IsZero() {
		resource.CreatedAt = now
	}
	resource.UpdatedAt = now

	data, err := json.Marshal(resource)
	if err != nil {
		return fmt.Errorf("failed to marshal resource: %w", err)
	}

	return db.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(resourcesBucket))
		return b.Put([]byte(resource.Name), data)
	})
}

// GetResource retrieves a resource by name
func (db *DB) GetResource(ctx context.Context, name string) (*Resource, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	var resource Resource
	err := db.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(resourcesBucket))
		data := b.Get([]byte(name))
		if data == nil {
			return fmt.Errorf("resource not found")
		}
		return json.Unmarshal(data, &resource)
	})

	if err != nil {
		return nil, err
	}
	return &resource, nil
}

// ListResources lists all resources
func (db *DB) ListResources(ctx context.Context) ([]*Resource, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	var resources []*Resource
	err := db.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(resourcesBucket))
		return b.ForEach(func(k, v []byte) error {
			var resource Resource
			if err := json.Unmarshal(v, &resource); err != nil {
				return err
			}
			resources = append(resources, &resource)
			return nil
		})
	})

	return resources, err
}

// DeleteResource deletes a resource by name
func (db *DB) DeleteResource(ctx context.Context, name string) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	return db.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(resourcesBucket))
		return b.Delete([]byte(name))
	})
}

// ==================== VOLUME ====================

// Volume represents a volume in a resource
type Volume struct {
	ID           int64
	ResourceName string
	VolumeName   string
	VolumeID     int
	Pool         string
	SizeGB       int
	// SizeBytes is the exact size the DRBD device presents, when the volume
	// was created or resized to one; zero for whole-GiB volumes.
	SizeBytes int64 `json:",omitempty"`
	Device    string
	CreatedAt time.Time
}

// SaveVolume saves or updates a volume
func (db *DB) SaveVolume(ctx context.Context, volume *Volume) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	if volume.CreatedAt.IsZero() {
		volume.CreatedAt = time.Now()
	}
	if volume.ID == 0 {
		volume.ID = time.Now().UnixNano()
	}

	data, err := json.Marshal(volume)
	if err != nil {
		return fmt.Errorf("failed to marshal volume: %w", err)
	}

	key := fmt.Sprintf("%s:%s", volume.ResourceName, volume.VolumeName)
	return db.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(volumesBucket))
		return b.Put([]byte(key), data)
	})
}

// ListVolumes lists all volumes for a resource
func (db *DB) ListVolumes(ctx context.Context, resourceName string) ([]*Volume, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	var volumes []*Volume
	err := db.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(volumesBucket))
		c := b.Cursor()
		// Keys are "<resource>:<volume>". The separator belongs in the
		// prefix: without it, the volumes of "db" included those of "db2",
		// which sort first, so a lookup of db's volume 0 returned db2's.
		prefix := []byte(resourceName + ":")
		for k, v := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
			var volume Volume
			if err := json.Unmarshal(v, &volume); err != nil {
				return err
			}
			volumes = append(volumes, &volume)
		}
		return nil
	})

	return volumes, err
}

// DeleteVolume deletes a volume
func (db *DB) DeleteVolume(ctx context.Context, resourceName, volumeName string) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	key := fmt.Sprintf("%s:%s", resourceName, volumeName)
	return db.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(volumesBucket))
		return b.Delete([]byte(key))
	})
}
