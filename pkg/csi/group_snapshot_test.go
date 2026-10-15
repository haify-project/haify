package csi

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// seedCSIVolume registers a volume this driver provisioned.
func seedCSIVolume(b *fakeBackend, name string, nodes ...string) {
	seedSnapshotSource(b, name, nodes...)
	b.resources[name].Labels = map[string]string{managedByLabel: managedByValue}
}

func groupFixture() (*fakeBackend, *controllerServer) {
	b := newFakeBackend("n1", "n2", "n3")
	seedCSIVolume(b, "pvc_db", "n1", "n2")
	seedCSIVolume(b, "pvc_wal", "n2", "n3")
	return b, newTestController(b)
}

func createGroup(t *testing.T, s *controllerServer, name string, sources ...string) *csi.VolumeGroupSnapshot {
	t.Helper()
	resp, err := s.CreateVolumeGroupSnapshot(context.Background(), &csi.CreateVolumeGroupSnapshotRequest{
		Name: name, SourceVolumeIds: sources})
	require.NoError(t, err)
	return resp.GetGroupSnapshot()
}

func codeOf(err error) codes.Code { return status.Code(err) }

func memberIDs(g *csi.VolumeGroupSnapshot) []string {
	var ids []string
	for _, s := range g.GetSnapshots() {
		ids = append(ids, s.GetSnapshotId())
	}
	return ids
}

// One controller call takes every member, each an ordinary snapshot of this
// driver on its volume's first replica node.
func TestCreateVolumeGroupSnapshotTakesAllAtOnce(t *testing.T) {
	b, s := groupFixture()
	g := createGroup(t, s, "groupsnapshot-1", "pvc_db", "pvc_wal")

	require.Len(t, b.groupCalls, 1, "one group call, so the controller suspends everything together")
	require.Len(t, b.groupCalls[0], 2)
	assert.True(t, g.GetReadyToUse())
	assert.NotNil(t, g.GetCreationTime())
	groupSnap, err := parseGroupSnapshotID(g.GetGroupSnapshotId())
	require.NoError(t, err)
	assert.Equal(t, "haifysnap_groupsnapshot_1", groupSnap)

	require.Len(t, g.GetSnapshots(), 2)
	byVolume := map[string]*csi.Snapshot{}
	for _, snap := range g.GetSnapshots() {
		byVolume[snap.GetSourceVolumeId()] = snap
		assert.Equal(t, g.GetGroupSnapshotId(), snap.GetGroupSnapshotId())
		assert.True(t, snap.GetReadyToUse())
		assert.Equal(t, int64(2*giB), snap.GetSizeBytes())
	}
	res, node, name, err := parseSnapshotID(byVolume["pvc_wal"].GetSnapshotId())
	require.NoError(t, err, "a member ID is an ordinary snapshot ID")
	assert.Equal(t, "pvc_wal", res)
	assert.Equal(t, "n2", node)
	assert.True(t, strings.HasPrefix(name, snapshotNamePrefix))
	assert.ElementsMatch(t, []string{
		"haify_vg0/pvc_db_data/" + memberSnapshotName(groupSnap, "pvc_db") + "@n1",
		"haify_vg0/pvc_wal_data/" + memberSnapshotName(groupSnap, "pvc_wal") + "@n2",
	}, b.snapCreated)
	assert.NotEqual(t, memberSnapshotName(groupSnap, "pvc_db"), memberSnapshotName(groupSnap, "pvc_wal"),
		"two members in one pool need different LV names")
}

// A retry with the same name returns the same group without taking it again.
func TestCreateVolumeGroupSnapshotIdempotent(t *testing.T) {
	b, s := groupFixture()
	first := createGroup(t, s, "g", "pvc_db", "pvc_wal")
	again := createGroup(t, s, "g", "pvc_wal", "pvc_db")
	assert.Len(t, b.groupCalls, 1)
	assert.Equal(t, first.GetGroupSnapshotId(), again.GetGroupSnapshotId())
	assert.ElementsMatch(t, memberIDs(first), memberIDs(again))
}

// A member left by an attempt that died half-way is not from the new instant:
// it is removed and the whole group taken again.
func TestCreateVolumeGroupSnapshotRetakesPartialGroup(t *testing.T) {
	b, s := groupFixture()
	groupSnap := sanitizeSnapshotName("g")
	require.NoError(t, b.CreateSnapshot(context.Background(), "haify_vg0/pvc_db_data", memberSnapshotName(groupSnap, "pvc_db"), "n1"))
	b.snapCreated = nil

	createGroup(t, s, "g", "pvc_db", "pvc_wal")
	assert.Equal(t, []string{"haify_vg0/pvc_db_data/" + memberSnapshotName(groupSnap, "pvc_db") + "@n1"}, b.snapDeleted)
	require.Len(t, b.groupCalls, 1)
	assert.Len(t, b.groupCalls[0], 2)
}

// The same name for a group of other volumes is a conflict.
func TestCreateVolumeGroupSnapshotNameUsedForOtherVolumes(t *testing.T) {
	b, s := groupFixture()
	seedCSIVolume(b, "pvc_other", "n1", "n3")
	createGroup(t, s, "g", "pvc_other")
	_, err := s.CreateVolumeGroupSnapshot(context.Background(), &csi.CreateVolumeGroupSnapshotRequest{
		Name: "g", SourceVolumeIds: []string{"pvc_db", "pvc_wal"}})
	assert.Equal(t, codes.AlreadyExists, codeOf(err), "%v", err)
}

func TestCreateVolumeGroupSnapshotRefusals(t *testing.T) {
	b, s := groupFixture()
	seedSnapshotSource(b, "not_csi", "n1", "n2") // a Haify resource, but not one this driver provisioned
	cases := []struct {
		name    string
		req     *csi.CreateVolumeGroupSnapshotRequest
		want    codes.Code
		message string
	}{
		{"no name", &csi.CreateVolumeGroupSnapshotRequest{SourceVolumeIds: []string{"pvc_db"}}, codes.InvalidArgument, ""},
		{"no sources", &csi.CreateVolumeGroupSnapshotRequest{Name: "g"}, codes.InvalidArgument, ""},
		{"repeated source", &csi.CreateVolumeGroupSnapshotRequest{Name: "g", SourceVolumeIds: []string{"pvc_db", "pvc_db"}},
			codes.InvalidArgument, "twice"},
		{"unknown volume", &csi.CreateVolumeGroupSnapshotRequest{Name: "g", SourceVolumeIds: []string{"pvc_db", "pvc_gone"}},
			codes.NotFound, "pvc_gone"},
		{"not a CSI volume", &csi.CreateVolumeGroupSnapshotRequest{Name: "g", SourceVolumeIds: []string{"pvc_db", "not_csi"}},
			codes.InvalidArgument, "not provisioned by " + DriverName},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := s.CreateVolumeGroupSnapshot(context.Background(), c.req)
			assert.Equal(t, c.want, codeOf(err), "%v", err)
			assert.Contains(t, status.Convert(err).Message(), c.message)
		})
	}
	assert.Empty(t, b.groupCalls, "nothing is suspended for a refused group")
}

func TestCreateVolumeGroupSnapshotControllerFailure(t *testing.T) {
	b, s := groupFixture()
	b.groupErr = errors.New("the group took 58s")
	_, err := s.CreateVolumeGroupSnapshot(context.Background(), &csi.CreateVolumeGroupSnapshotRequest{
		Name: "g", SourceVolumeIds: []string{"pvc_db", "pvc_wal"}})
	assert.Equal(t, codes.Internal, codeOf(err))
	assert.Contains(t, err.Error(), "58s")
	assert.Empty(t, b.snapCreated)
}

func TestGetVolumeGroupSnapshot(t *testing.T) {
	b, s := groupFixture()
	g := createGroup(t, s, "g", "pvc_db", "pvc_wal")
	ctx := context.Background()

	got, err := s.GetVolumeGroupSnapshot(ctx, &csi.GetVolumeGroupSnapshotRequest{
		GroupSnapshotId: g.GetGroupSnapshotId(), SnapshotIds: memberIDs(g)})
	require.NoError(t, err)
	assert.ElementsMatch(t, memberIDs(g), memberIDs(got.GetGroupSnapshot()))
	assert.True(t, got.GetGroupSnapshot().GetReadyToUse())

	// Without the member IDs the driver finds them itself.
	got, err = s.GetVolumeGroupSnapshot(ctx, &csi.GetVolumeGroupSnapshotRequest{GroupSnapshotId: g.GetGroupSnapshotId()})
	require.NoError(t, err)
	assert.ElementsMatch(t, memberIDs(g), memberIDs(got.GetGroupSnapshot()))

	// A member of another group is a mismatch.
	other := createGroup(t, s, "other", "pvc_db")
	_, err = s.GetVolumeGroupSnapshot(ctx, &csi.GetVolumeGroupSnapshotRequest{
		GroupSnapshotId: g.GetGroupSnapshotId(), SnapshotIds: memberIDs(other)})
	assert.Equal(t, codes.InvalidArgument, codeOf(err))

	_, err = s.GetVolumeGroupSnapshot(ctx, &csi.GetVolumeGroupSnapshotRequest{GroupSnapshotId: "nonsense"})
	assert.Equal(t, codes.NotFound, codeOf(err))
	_, err = s.GetVolumeGroupSnapshot(ctx, &csi.GetVolumeGroupSnapshotRequest{})
	assert.Equal(t, codes.InvalidArgument, codeOf(err))

	// A group missing a member is not found.
	_, _, name, _ := parseSnapshotID(memberIDs(g)[0])
	for k, snaps := range b.snapshots {
		for _, sn := range snaps {
			if sn.GetName() == name {
				require.NoError(t, b.DeleteSnapshot(ctx, strings.Split(k, "|")[0], name, strings.Split(k, "|")[1]))
			}
		}
	}
	_, err = s.GetVolumeGroupSnapshot(ctx, &csi.GetVolumeGroupSnapshotRequest{
		GroupSnapshotId: g.GetGroupSnapshotId(), SnapshotIds: memberIDs(g)})
	assert.Equal(t, codes.NotFound, codeOf(err))
}

func TestDeleteVolumeGroupSnapshotDeletesMembers(t *testing.T) {
	b, s := groupFixture()
	ctx := context.Background()
	g := createGroup(t, s, "g", "pvc_db", "pvc_wal")
	other := createGroup(t, s, "other", "pvc_db")

	_, err := s.DeleteVolumeGroupSnapshot(ctx, &csi.DeleteVolumeGroupSnapshotRequest{
		GroupSnapshotId: g.GetGroupSnapshotId(), SnapshotIds: memberIDs(other)})
	assert.Equal(t, codes.InvalidArgument, codeOf(err), "a member of another group is a mismatch")
	assert.Empty(t, b.snapDeleted)

	_, err = s.DeleteVolumeGroupSnapshot(ctx, &csi.DeleteVolumeGroupSnapshotRequest{
		GroupSnapshotId: g.GetGroupSnapshotId(), SnapshotIds: memberIDs(g)})
	require.NoError(t, err)
	assert.Len(t, b.snapDeleted, 2)

	// Again: idempotent, and the other group is untouched.
	_, err = s.DeleteVolumeGroupSnapshot(ctx, &csi.DeleteVolumeGroupSnapshotRequest{GroupSnapshotId: g.GetGroupSnapshotId()})
	require.NoError(t, err)
	_, err = s.GetVolumeGroupSnapshot(ctx, &csi.GetVolumeGroupSnapshotRequest{GroupSnapshotId: other.GetGroupSnapshotId()})
	require.NoError(t, err)

	// Without member IDs the driver finds them itself.
	_, err = s.DeleteVolumeGroupSnapshot(ctx, &csi.DeleteVolumeGroupSnapshotRequest{GroupSnapshotId: other.GetGroupSnapshotId()})
	require.NoError(t, err)
	assert.Len(t, b.snapDeleted, 3)

	_, err = s.DeleteVolumeGroupSnapshot(ctx, &csi.DeleteVolumeGroupSnapshotRequest{GroupSnapshotId: "nonsense"})
	require.NoError(t, err, "an ID never handed out has nothing to delete")
	_, err = s.DeleteVolumeGroupSnapshot(ctx, &csi.DeleteVolumeGroupSnapshotRequest{})
	assert.Equal(t, codes.InvalidArgument, codeOf(err))
}

// A member restores like any VolumeSnapshot.
func TestGroupMemberRestoresAsOrdinarySnapshot(t *testing.T) {
	b, s := groupFixture()
	g := createGroup(t, s, "g", "pvc_db", "pvc_wal")
	var walID string
	for _, snap := range g.GetSnapshots() {
		if snap.GetSourceVolumeId() == "pvc_wal" {
			walID = snap.GetSnapshotId()
		}
	}
	_, err := s.CreateVolume(context.Background(), restoreReq("pvc-restored", walID))
	require.NoError(t, err)
	require.Len(t, b.populated, 1)
	assert.Contains(t, b.populated[0], "pvc_wal/0@"+memberSnapshotName(sanitizeSnapshotName("g"), "pvc_wal"))
}

func TestGroupControllerIsServedAndAdvertised(t *testing.T) {
	_, ok := NewControllerServer(newFakeBackend(), nil).(csi.GroupControllerServer)
	assert.True(t, ok, "the driver registers the group service only for a controller that implements it")

	caps, err := newTestController(newFakeBackend()).GroupControllerGetCapabilities(context.Background(),
		&csi.GroupControllerGetCapabilitiesRequest{})
	require.NoError(t, err)
	require.Len(t, caps.GetCapabilities(), 1)
	assert.Equal(t, csi.GroupControllerServiceCapability_RPC_CREATE_DELETE_GET_VOLUME_GROUP_SNAPSHOT,
		caps.GetCapabilities()[0].GetRpc().GetType())

	resp, err := NewIdentityServer().GetPluginCapabilities(context.Background(), &csi.GetPluginCapabilitiesRequest{})
	require.NoError(t, err)
	var types []csi.PluginCapability_Service_Type
	for _, c := range resp.GetCapabilities() {
		types = append(types, c.GetService().GetType())
	}
	assert.Contains(t, types, csi.PluginCapability_Service_GROUP_CONTROLLER_SERVICE)
}

func TestGroupSnapshotIDs(t *testing.T) {
	id := makeGroupSnapshotID("haifysnap_g")
	got, err := parseGroupSnapshotID(id)
	require.NoError(t, err)
	assert.Equal(t, "haifysnap_g", got)
	for _, bad := range []string{"", "haifygroup", "haifygroup/", "haifygroup/x", "other/haifysnap_g", "haifygroup/haifysnap_g/x",
		makeSnapshotID("pvc", "n1", "haifysnap_g")} {
		_, err := parseGroupSnapshotID(bad)
		assert.Errorf(t, err, "%q should be rejected", bad)
	}
	_, _, _, err = parseSnapshotID(id)
	assert.Error(t, err, "a group ID is not a snapshot ID")
}
