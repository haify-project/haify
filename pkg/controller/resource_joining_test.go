package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/database"
)

// A replica added to a resource is reported as joining until it is first
// seen UpToDate, and never again after that.
func TestJoiningReplicaIsRecordedAndSettled(t *testing.T) {
	fx := newFollowFixture(t)
	ctx := context.Background()
	rm := fx.ctrl.resources
	fx.save(func(r *database.Resource) { r.Nodes, r.DisklessClients = "a,b,c", "" })
	fx.set(func() { fx.roles["c"], fx.disks["c"] = "Secondary", "Inconsistent" })

	rm.markJoining(ctx, "vol1", "c", 2, 0)
	require.Len(t, fx.resource().Joining, 1)
	assert.Equal(t, 2, fx.resource().Joining[0].Committed, "the replicas it had before the add")

	info, err := rm.GetResource(ctx, "vol1")
	require.NoError(t, err)
	assert.Equal(t, []string{"c"}, info.JoiningNodes)
	assert.Equal(t, 2, info.CommittedReplicas)
	_, st := fx.ctrl.nodeStateOf(info, "c")
	require.NotNil(t, st)
	assert.True(t, st.Joining)
	_, st = fx.ctrl.nodeStateOf(info, "b")
	assert.False(t, st.Joining)

	// The API says the same, on the resource and on the replica's state.
	resp, err := NewServer(fx.ctrl).GetResource(ctx, &haifypb.GetResourceRequest{Name: "vol1"})
	require.NoError(t, err)
	require.True(t, resp.Success, resp.Message)
	assert.Equal(t, []string{"c"}, resp.Resource.JoiningNodes)
	assert.EqualValues(t, 2, resp.Resource.CommittedReplicas)
	assert.True(t, resp.Resource.NodeStates["c"].Joining)

	// The health poll hears it too.
	list, err := rm.GetResourceStatusList(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, 2, list[0].CommittedReplicas)
	assert.True(t, list[0].NodeStates["c"].Joining)
	assert.False(t, list[0].NodeStates["b"].Joining)

	// In sync: off the record, for good.
	fx.set(func() { fx.disks["c"] = "UpToDate" })
	info, err = rm.GetResource(ctx, "vol1")
	require.NoError(t, err)
	assert.Empty(t, info.JoiningNodes)
	assert.Zero(t, info.CommittedReplicas)
	assert.Empty(t, fx.resource().Joining)

	// A later resync of the same replica is a replica that fell behind.
	fx.set(func() { fx.disks["c"] = "Inconsistent" })
	info, err = rm.GetResource(ctx, "vol1")
	require.NoError(t, err)
	assert.Empty(t, info.JoiningNodes)
	_, st = fx.ctrl.nodeStateOf(info, "c")
	assert.False(t, st.Joining)
}

// The record follows what the node is: nothing for a node that did not
// become a replica, gone with a replica that was removed, and kept while the
// replica's state cannot be read.
func TestJoiningRecordFollowsTheReplica(t *testing.T) {
	fx := newFollowFixture(t)
	ctx := context.Background()
	rm := fx.ctrl.resources

	rm.markJoining(ctx, "vol1", "c", 2, 0)
	assert.Empty(t, fx.resource().Joining, "c is a client, not a replica")

	fx.save(func(r *database.Resource) { r.Nodes, r.DisklessClients = "a,b,c", "" })
	fx.set(func() { fx.roles["c"], fx.disks["c"] = "Secondary", "Inconsistent" })
	// A replacement for a replica that was already removed keeps the number
	// from before the removal.
	rm.markJoining(ctx, "vol1", "c", 2, 3)
	rm.markJoining(ctx, "vol1", "c", 2, 3)
	require.Len(t, fx.resource().Joining, 1, "recorded once")
	info, err := rm.GetResource(ctx, "vol1")
	require.NoError(t, err)
	assert.Equal(t, 3, info.CommittedReplicas)

	// The status cannot be read: nothing was learned, the record stays.
	fx.set(func() { fx.statusOK = false })
	_, _ = rm.GetResource(ctx, "vol1")
	assert.Len(t, fx.resource().Joining, 1)
	fx.set(func() { fx.statusOK = true })

	// The replica is removed again before it ever finished.
	fx.save(func(r *database.Resource) { r.Nodes = "a,b" })
	info, err = rm.GetResource(ctx, "vol1")
	require.NoError(t, err)
	assert.Empty(t, info.JoiningNodes)
	assert.Empty(t, fx.resource().Joining)
}

// Adding a replica to a node that already holds one records nothing.
func TestAddReplicaRefusedRecordsNothing(t *testing.T) {
	fx := newFollowFixture(t)
	ctx := context.Background()
	err := fx.ctrl.resources.AddReplica(ctx, "vol1", "a")
	require.Error(t, err)
	assert.Empty(t, fx.resource().Joining)
}

// The record exists before the add begins: a diskless client turned into a
// replica starts syncing while the add is still running, and a health check
// landing in that window must already find it declared as joining.
func TestJoiningIsAnnouncedBeforeTheAdd(t *testing.T) {
	fx := newFollowFixture(t)
	ctx := context.Background()
	rm := fx.ctrl.resources
	fx.save(func(r *database.Resource) { r.Nodes, r.DisklessClients = "a,b", "c" })
	fx.set(func() { fx.roles["c"], fx.disks["c"] = "Primary", "Inconsistent" })

	rm.announceJoining(ctx, "vol1", "c", 2, 0)
	require.Len(t, fx.resource().Joining, 1)
	assert.True(t, fx.resource().Joining[0].Pending)

	// Not yet listed as a replica, already syncing: joining, and still on
	// the record after the read.
	info, err := rm.GetResource(ctx, "vol1")
	require.NoError(t, err)
	assert.Equal(t, []string{"c"}, info.JoiningNodes)
	assert.Equal(t, 2, info.CommittedReplicas)
	require.Len(t, fx.resource().Joining, 1)

	// The add made it a replica: confirmed.
	fx.save(func(r *database.Resource) { r.Nodes, r.DisklessClients = "a,b,c", "" })
	rm.markJoining(ctx, "vol1", "c", 2, 0)
	require.Len(t, fx.resource().Joining, 1)
	assert.False(t, fx.resource().Joining[0].Pending)
}

// An add that failed before the node became a replica withdraws what it
// announced, so nothing is left to excuse a later sync.
func TestJoiningAnnouncementIsWithdrawnWhenTheAddFails(t *testing.T) {
	fx := newFollowFixture(t)
	ctx := context.Background()
	rm := fx.ctrl.resources
	fx.save(func(r *database.Resource) { r.Nodes, r.DisklessClients = "a,b", "c" })

	rm.announceJoining(ctx, "vol1", "c", 2, 0)
	require.Len(t, fx.resource().Joining, 1)
	rm.markJoining(ctx, "vol1", "c", 2, 0)
	assert.Empty(t, fx.resource().Joining)
}
