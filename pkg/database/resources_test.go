package database

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"
)

// ==================== Resource Tests ====================

func TestResourceCRUD(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	ctx := context.Background()

	// Create
	resource := &Resource{
		Name:     "data",
		Port:     7000,
		Nodes:    "node1,node2",
		Protocol: "C",
		Replicas: 2,
		Profile:  "production",
		Labels: map[string]string{
			"app": "postgres",
			"env": "prod",
		},
	}

	err := db.SaveResource(ctx, resource)
	require.NoError(t, err)

	// Read
	retrieved, err := db.GetResource(ctx, resource.Name)
	require.NoError(t, err)
	assert.Equal(t, resource.Name, retrieved.Name)
	assert.Equal(t, resource.Port, retrieved.Port)
	assert.Equal(t, resource.Nodes, retrieved.Nodes)
	assert.Equal(t, resource.Protocol, retrieved.Protocol)
	assert.Equal(t, resource.Replicas, retrieved.Replicas)
	assert.Equal(t, resource.Profile, retrieved.Profile)
	assert.Equal(t, resource.Labels, retrieved.Labels)

	// Update
	resource.Replicas = 3
	err = db.SaveResource(ctx, resource)
	require.NoError(t, err)

	retrieved, err = db.GetResource(ctx, resource.Name)
	require.NoError(t, err)
	assert.Equal(t, 3, retrieved.Replicas)

	// List
	resources, err := db.ListResources(ctx)
	require.NoError(t, err)
	assert.Len(t, resources, 1)

	// Delete
	err = db.DeleteResource(ctx, resource.Name)
	require.NoError(t, err)

	_, err = db.GetResource(ctx, resource.Name)
	assert.Error(t, err)
}

func TestResourceProfileCRUD(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	ctx := context.Background()
	profile := &ResourceProfile{
		Name:        "production",
		Protocol:    "C",
		StorageType: "lvm-thin",
		Pool:        "fast",
		Replicas:    3,
		OnDifferent: []string{"zone", "rack"},
		OnSame:      []string{"region"},
		DRBDOptions: map[string]string{"net/max-buffers": "8000"},
		Labels:      map[string]string{"env": "prod"},
	}

	require.NoError(t, db.SaveResourceProfile(ctx, profile))
	retrieved, err := db.GetResourceProfile(ctx, profile.Name)
	require.NoError(t, err)
	assert.Equal(t, profile.Name, retrieved.Name)
	assert.Equal(t, profile.Protocol, retrieved.Protocol)
	assert.Equal(t, profile.StorageType, retrieved.StorageType)
	assert.Equal(t, profile.Pool, retrieved.Pool)
	assert.Equal(t, profile.Replicas, retrieved.Replicas)
	assert.Equal(t, profile.OnDifferent, retrieved.OnDifferent)
	assert.Equal(t, profile.OnSame, retrieved.OnSame)
	assert.Equal(t, profile.DRBDOptions, retrieved.DRBDOptions)
	assert.Equal(t, profile.Labels, retrieved.Labels)
	assert.False(t, retrieved.CreatedAt.IsZero())
	assert.False(t, retrieved.UpdatedAt.IsZero())

	profile.Replicas = 2
	profile.Labels["tier"] = "critical"
	require.NoError(t, db.SaveResourceProfile(ctx, profile))
	retrieved, err = db.GetResourceProfile(ctx, profile.Name)
	require.NoError(t, err)
	assert.Equal(t, 2, retrieved.Replicas)
	assert.Equal(t, "critical", retrieved.Labels["tier"])

	profiles, err := db.ListResourceProfiles(ctx)
	require.NoError(t, err)
	require.Len(t, profiles, 1)
	assert.Equal(t, profile.Name, profiles[0].Name)

	require.NoError(t, db.DeleteResourceProfile(ctx, profile.Name))
	_, err = db.GetResourceProfile(ctx, profile.Name)
	assert.Error(t, err)
}

func TestResourceProfileErrors(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	ctx := context.Background()
	require.NoError(t, db.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(profilesBucket)).Put([]byte("corrupt"), []byte("{"))
	}))
	_, err := db.GetResourceProfile(ctx, "corrupt")
	require.Error(t, err)
	_, err = db.ListResourceProfiles(ctx)
	require.Error(t, err)
}

// ==================== Volume Tests ====================

func TestVolumeCRUD(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	ctx := context.Background()

	// Create
	volume := &Volume{
		ResourceName: "data",
		VolumeName:   "vol0",
		VolumeID:     0,
		Pool:         "vg-data",
		SizeGB:       100,
		Device:       "/dev/drbd0",
	}

	err := db.SaveVolume(ctx, volume)
	require.NoError(t, err)
	assert.NotZero(t, volume.ID)

	// List by resource
	volumes, err := db.ListVolumes(ctx, "data")
	require.NoError(t, err)
	assert.Len(t, volumes, 1)
	assert.Equal(t, volume.ResourceName, volumes[0].ResourceName)
	assert.Equal(t, volume.VolumeName, volumes[0].VolumeName)

	// Delete
	err = db.DeleteVolume(ctx, "data", "vol0")
	require.NoError(t, err)

	volumes, err = db.ListVolumes(ctx, "data")
	require.NoError(t, err)
	assert.Empty(t, volumes)
}

func TestListVolumesByResource(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()

	ctx := context.Background()

	// Create volumes for different resources
	for i := 0; i < 3; i++ {
		volume := &Volume{
			ResourceName: "resource1",
			VolumeName:   "vol" + string(rune('0'+i)),
			VolumeID:     i,
			Pool:         "vg-data",
			SizeGB:       100 * (i + 1),
			Device:       "/dev/drbd" + string(rune('0'+i)),
		}
		err := db.SaveVolume(ctx, volume)
		require.NoError(t, err)
	}

	for i := 0; i < 2; i++ {
		volume := &Volume{
			ResourceName: "resource2",
			VolumeName:   "vol" + string(rune('0'+i)),
			VolumeID:     i,
			Pool:         "vg-data",
			SizeGB:       50 * (i + 1),
			Device:       "/dev/drbd10" + string(rune('0'+i)),
		}
		err := db.SaveVolume(ctx, volume)
		require.NoError(t, err)
	}

	// List volumes for resource1
	volumes1, err := db.ListVolumes(ctx, "resource1")
	require.NoError(t, err)
	assert.Len(t, volumes1, 3)

	// List volumes for resource2
	volumes2, err := db.ListVolumes(ctx, "resource2")
	require.NoError(t, err)
	assert.Len(t, volumes2, 2)

	// List volumes for nonexistent resource
	volumes3, err := db.ListVolumes(ctx, "nonexistent")
	require.NoError(t, err)
	assert.Empty(t, volumes3)
}

// A resource whose name starts with another's must not lend it its volumes:
// "db2:..." sorts before "db:...", so a prefix without the separator returned
// db2's volume 0 for a lookup of db's.
func TestListVolumesDoesNotMatchLongerNames(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()

	for _, r := range []string{"db", "db2", "db_x"} {
		require.NoError(t, db.SaveVolume(ctx, &Volume{ResourceName: r, VolumeName: r + "_data", Pool: "vg0"}))
	}
	vols, err := db.ListVolumes(ctx, "db")
	require.NoError(t, err)
	require.Len(t, vols, 1)
	assert.Equal(t, "db_data", vols[0].VolumeName)
}
