package database

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
	"go.uber.org/zap"
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
	db.log().Debug("DB.SaveResourceProfile: saving profile",
		zap.String("profile", profile.Name), zap.String("pool", profile.Pool), zap.Int("replicas", profile.Replicas))
	db.mu.Lock()
	defer db.mu.Unlock()

	now := time.Now()
	if profile.CreatedAt.IsZero() {
		profile.CreatedAt = now
	}
	profile.UpdatedAt = now

	data, err := json.Marshal(profile)
	if err != nil {
		db.log().Debug("DB.SaveResourceProfile: marshal failed", zap.String("profile", profile.Name), zap.Error(err))
		return fmt.Errorf("failed to marshal resource profile: %w", err)
	}

	start := time.Now()
	err = db.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(profilesBucket)).Put([]byte(profile.Name), data)
	})
	db.log().Debug("DB.SaveResourceProfile: put", zap.String("bucket", profilesBucket), zap.String("key", profile.Name),
		zap.Int("size_bytes", len(data)), zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}

// GetResourceProfile retrieves a resource profile by name.
func (db *DB) GetResourceProfile(ctx context.Context, name string) (*ResourceProfile, error) {
	db.log().Debug("DB.GetResourceProfile: reading profile", zap.String("profile", name))
	db.mu.RLock()
	defer db.mu.RUnlock()

	var profile ResourceProfile
	found, size := false, 0
	start := time.Now()
	err := db.db.View(func(tx *bolt.Tx) error {
		data := tx.Bucket([]byte(profilesBucket)).Get([]byte(name))
		if data == nil {
			return fmt.Errorf("resource profile not found")
		}
		found, size = true, len(data)
		return json.Unmarshal(data, &profile)
	})
	db.log().Debug("DB.GetResourceProfile: get", zap.String("bucket", profilesBucket), zap.String("key", name),
		zap.Bool("found", found), zap.Int("size_bytes", size),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, err
	}
	return &profile, nil
}

// ListResourceProfiles lists all resource profiles.
func (db *DB) ListResourceProfiles(ctx context.Context) ([]*ResourceProfile, error) {
	db.log().Debug("DB.ListResourceProfiles: listing profiles")
	db.mu.RLock()
	defer db.mu.RUnlock()

	profiles := make([]*ResourceProfile, 0)
	start := time.Now()
	err := db.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(profilesBucket)).ForEach(func(key, value []byte) error {
			var profile ResourceProfile
			if err := json.Unmarshal(value, &profile); err != nil {
				db.log().Debug("DB.ListResourceProfiles: decoding entry failed", zap.ByteString("key", key), zap.Error(err))
				return err
			}
			profiles = append(profiles, &profile)
			return nil
		})
	})
	db.log().Debug("DB.ListResourceProfiles: scan", zap.String("bucket", profilesBucket),
		zap.Int("count", len(profiles)), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, err
	}
	return profiles, nil
}

// DeleteResourceProfile deletes a resource profile by name.
func (db *DB) DeleteResourceProfile(ctx context.Context, name string) error {
	db.log().Debug("DB.DeleteResourceProfile: deleting profile", zap.String("profile", name))
	db.mu.Lock()
	defer db.mu.Unlock()

	start := time.Now()
	err := db.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(profilesBucket)).Delete([]byte(name))
	})
	db.log().Debug("DB.DeleteResourceProfile: delete", zap.String("bucket", profilesBucket), zap.String("key", name),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}

// SaveResource saves or updates a resource
func (db *DB) SaveResource(ctx context.Context, resource *Resource) error {
	db.log().Debug("DB.SaveResource: saving resource",
		zap.String("resource", resource.Name), zap.String("nodes", resource.Nodes),
		zap.Int("port", resource.Port), zap.Int("replicas", resource.Replicas), zap.Bool("wan", resource.WANMode))
	db.mu.Lock()
	defer db.mu.Unlock()

	now := time.Now()
	if resource.CreatedAt.IsZero() {
		resource.CreatedAt = now
	}
	resource.UpdatedAt = now

	data, err := json.Marshal(resource)
	if err != nil {
		db.log().Debug("DB.SaveResource: marshal failed", zap.String("resource", resource.Name), zap.Error(err))
		return fmt.Errorf("failed to marshal resource: %w", err)
	}

	start := time.Now()
	err = db.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(resourcesBucket))
		return b.Put([]byte(resource.Name), data)
	})
	db.log().Debug("DB.SaveResource: put", zap.String("bucket", resourcesBucket), zap.String("key", resource.Name),
		zap.Int("size_bytes", len(data)), zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}

// GetResource retrieves a resource by name
func (db *DB) GetResource(ctx context.Context, name string) (*Resource, error) {
	db.log().Debug("DB.GetResource: reading resource", zap.String("resource", name))
	db.mu.RLock()
	defer db.mu.RUnlock()

	var resource Resource
	found, size := false, 0
	start := time.Now()
	err := db.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(resourcesBucket))
		data := b.Get([]byte(name))
		if data == nil {
			return fmt.Errorf("resource not found")
		}
		found, size = true, len(data)
		return json.Unmarshal(data, &resource)
	})
	db.log().Debug("DB.GetResource: get", zap.String("bucket", resourcesBucket), zap.String("key", name),
		zap.Bool("found", found), zap.Int("size_bytes", size),
		zap.Duration("took", time.Since(start)), zap.Error(err))

	if err != nil {
		return nil, err
	}
	return &resource, nil
}

// ListResources lists all resources
func (db *DB) ListResources(ctx context.Context) ([]*Resource, error) {
	db.log().Debug("DB.ListResources: listing resources")
	db.mu.RLock()
	defer db.mu.RUnlock()

	var resources []*Resource
	start := time.Now()
	err := db.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(resourcesBucket))
		return b.ForEach(func(k, v []byte) error {
			var resource Resource
			if err := json.Unmarshal(v, &resource); err != nil {
				db.log().Debug("DB.ListResources: decoding entry failed", zap.ByteString("key", k), zap.Error(err))
				return err
			}
			resources = append(resources, &resource)
			return nil
		})
	})
	db.log().Debug("DB.ListResources: scan", zap.String("bucket", resourcesBucket),
		zap.Int("count", len(resources)), zap.Duration("took", time.Since(start)), zap.Error(err))

	return resources, err
}

// DeleteResource deletes a resource by name
func (db *DB) DeleteResource(ctx context.Context, name string) error {
	db.log().Debug("DB.DeleteResource: deleting resource", zap.String("resource", name))
	db.mu.Lock()
	defer db.mu.Unlock()

	start := time.Now()
	err := db.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(resourcesBucket))
		return b.Delete([]byte(name))
	})
	db.log().Debug("DB.DeleteResource: delete", zap.String("bucket", resourcesBucket), zap.String("key", name),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
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
	db.log().Debug("DB.SaveVolume: saving volume",
		zap.String("resource", volume.ResourceName), zap.String("volume", volume.VolumeName),
		zap.Int("volume_id", volume.VolumeID), zap.String("pool", volume.Pool),
		zap.Int("size_gb", volume.SizeGB), zap.Int64("size_bytes", volume.SizeBytes))
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
		db.log().Debug("DB.SaveVolume: marshal failed", zap.String("resource", volume.ResourceName), zap.Error(err))
		return fmt.Errorf("failed to marshal volume: %w", err)
	}

	key := fmt.Sprintf("%s:%s", volume.ResourceName, volume.VolumeName)
	start := time.Now()
	err = db.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(volumesBucket))
		return b.Put([]byte(key), data)
	})
	db.log().Debug("DB.SaveVolume: put", zap.String("bucket", volumesBucket), zap.String("key", key),
		zap.Int("size_bytes", len(data)), zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}

// ListVolumes lists all volumes for a resource
func (db *DB) ListVolumes(ctx context.Context, resourceName string) ([]*Volume, error) {
	db.log().Debug("DB.ListVolumes: listing volumes", zap.String("resource", resourceName))
	db.mu.RLock()
	defer db.mu.RUnlock()

	var volumes []*Volume
	start := time.Now()
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
				db.log().Debug("DB.ListVolumes: decoding entry failed", zap.ByteString("key", k), zap.Error(err))
				return err
			}
			db.log().Debug("DB.ListVolumes: found volume", zap.ByteString("key", k), zap.Int("volume_id", volume.VolumeID))
			volumes = append(volumes, &volume)
		}
		return nil
	})
	db.log().Debug("DB.ListVolumes: scan", zap.String("bucket", volumesBucket), zap.String("prefix", resourceName+":"),
		zap.Int("count", len(volumes)), zap.Duration("took", time.Since(start)), zap.Error(err))

	return volumes, err
}

// DeleteVolume deletes a volume
func (db *DB) DeleteVolume(ctx context.Context, resourceName, volumeName string) error {
	db.log().Debug("DB.DeleteVolume: deleting volume", zap.String("resource", resourceName), zap.String("volume", volumeName))
	db.mu.Lock()
	defer db.mu.Unlock()

	key := fmt.Sprintf("%s:%s", resourceName, volumeName)
	start := time.Now()
	err := db.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(volumesBucket))
		return b.Delete([]byte(key))
	})
	db.log().Debug("DB.DeleteVolume: delete", zap.String("bucket", volumesBucket), zap.String("key", key),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}
