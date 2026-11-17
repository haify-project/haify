package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/deployment"
)

// The mark is read from among an LV's other tags, and only a value this
// controller writes is passed on.
func TestConsistencyOfTags(t *testing.T) {
	assert.Equal(t, "filesystem", consistencyOfTags("haify.consistency=filesystem"))
	assert.Equal(t, "crash", consistencyOfTags(" backup,haify.consistency=crash ,other"))
	assert.Empty(t, consistencyOfTags(""))
	assert.Empty(t, consistencyOfTags("backup,other"))
	assert.Empty(t, consistencyOfTags("haify.consistency=perfect"))
	assert.Empty(t, knownConsistency("-"), "zfs prints - for a property that is not set")
	assert.Equal(t, "crash", knownConsistency("crash\n"))
}

// Both listings of LVM snapshots carry the mark, and a snapshot without one
// reports nothing rather than "crash".
func TestSnapshotListsCarryConsistency(t *testing.T) {
	snap, ok := parseSnapshotLine("  web1_snap|2147483648|2026-11-01 10:00:00 +0000|web1_00000|haify.consistency=filesystem", "vg0")
	require.True(t, ok)
	assert.Equal(t, "filesystem", snap.Consistency)
	assert.Equal(t, "web1_00000", snap.Origin)
	snap, ok = parseSnapshotLine("  plain_snap|2147483648|2026-11-01 10:00:00 +0000|plain_00000|", "vg0")
	require.True(t, ok)
	assert.Empty(t, snap.Consistency)
	// A node that still prints four columns is read as before.
	snap, ok = parseSnapshotLine("old_snap|2147483648|2026-11-01 10:00:00 +0000|old_00000", "vg0")
	require.True(t, ok)
	assert.Empty(t, snap.Consistency)

	line := "tank/web1@s1\t0\t12288\t1791768600\tcrash"
	name, volume, created, ok := parseZFSSnapshotLine(line)
	require.True(t, ok)
	assert.Equal(t, "s1", name)
	assert.Equal(t, "tank/web1", volume)
	assert.NotContains(t, created, "crash", "the mark is not part of the creation time")
	assert.Equal(t, "crash", zfsSnapshotConsistency(line))
	assert.Empty(t, zfsSnapshotConsistency("tank/web1@s1\t0\t12288\t1791768600\t-"))
	assert.Empty(t, zfsSnapshotConsistency("tank/web1@s1\t0\t12288\t1791768600"))
}

// A resource's snapshots are listed with their consistency, and the names
// alone stay in the reply for clients that read only those.
func TestListResourceSnapshotsCarriesConsistency(t *testing.T) {
	ctrl := guestFixture(t, &guestNode{})
	dep := ctrl.deployment.(*fakeDeploymentClient)
	dep.execFunc = func(_ context.Context, hosts []string, cmd string, _ ...deployment.ExecOption) (*deployment.ExecResult, error) {
		if strings.Contains(cmd, "lvs --noheadings") && strings.Contains(cmd, "lv_name,lv_tags") {
			return successExecResult(hosts, "  web1-root_00000|\n  web1-root_00000_snap_frozen|haify.consistency=filesystem\n"+
				"  web1-root_00000_snap_agentless|haify.consistency=crash\n  web1-root_00000_snap_old|\n  other_00000_snap_x|\n"), nil
		}
		return successExecResult(hosts, ""), nil
	}
	snaps, err := ctrl.resources.ListResourceSnapshots(context.Background(), "web1-root")
	require.NoError(t, err)
	assert.Equal(t, []ResourceSnapshot{{Name: "frozen", Consistency: "filesystem"}, {Name: "agentless", Consistency: "crash"}, {Name: "old"}}, snaps)

	resp, err := NewServer(ctrl).ListResourceSnapshots(context.Background(), &haifypb.ListResourceSnapshotsRequest{Resource: "web1-root"})
	require.NoError(t, err)
	require.True(t, resp.Success, resp.Message)
	assert.Equal(t, []string{"frozen", "agentless", "old"}, resp.Names)
	require.Len(t, resp.Snapshots, 3)
	assert.Equal(t, "filesystem", resp.Snapshots[0].Consistency)
	assert.Empty(t, resp.Snapshots[2].Consistency)
}

// A backup record says what its snapshots hold, and an import reads it back
// from the manifest.
func TestBackupConsistencyTravels(t *testing.T) {
	info := backupToProto(nil)
	assert.Nil(t, info)
	rec, err := backupFromManifest(`{"version":2,"id":"b1","resource":"web1-root","created_at":"2026-11-01T10:00:00Z",
		"consistency":"filesystem","volumes":[{"volume_id":0,"object":"vol0.img.gz","bytes":5}]}`, "web1-root/b1", "s3",
		map[string]uint64{"vol0.img.gz": 5})
	require.NoError(t, err)
	assert.Equal(t, "filesystem", rec.Consistency)
	assert.Equal(t, "filesystem", backupToProto(rec).Consistency)
}
