package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/deployment"
)

// keepDrain is a drain cluster whose node reports the device open or closed.
func keepDrain(t *testing.T, res *database.Resource, open string) *drainCluster {
	t.Helper()
	c := newDrainCluster(t, res, drainStatusJSON(drainPeer{"n2", "Connected", "UpToDate"}))
	dep := c.ctrl.deployment.(*fakeDeploymentClient)
	dep.execFunc = func(_ context.Context, hosts []string, cmd string, _ ...deployment.ExecOption) (*deployment.ExecResult, error) {
		c.execs = append(c.execs, strings.Join(hosts, ",")+": "+cmd)
		if strings.Contains(cmd, "drbdsetup status") && strings.Contains(cmd, "--verbose") {
			if open == "unreadable" {
				return failedExecResult(hosts, "ssh: connection reset"), nil
			}
			if open == "unreported" {
				return successExecResult(hosts, "res1 role:Primary\n  volume:0 minor:100 disk:UpToDate\n"), nil
			}
			return successExecResult(hosts, "res1 role:Primary\n  volume:0 minor:100 disk:UpToDate open:"+open+"\n"), nil
		}
		return successExecResult(hosts, ""), nil
	}
	return c
}

// A keep-in-use drain takes the node out of placement at once and leaves
// alone every resource another system keeps Primary.
func TestDrainKeepInUseLeavesExternallyPromoted(t *testing.T) {
	ctx := context.Background()
	for owner, labels := range map[string]map[string]string{
		"the CSI driver":   {csiManagedByLabel: "csi", csiPVCLabel: "db/data-0"},
		"Proxmox VE":       {pveManagedByLabel: "pve"},
		"OpenStack Cinder": {cinderManagedByLabel: "cinder"},
		"XCP-ng":           {xcpManagedByLabel: "xcp-ng"},
		"OpenNebula":       {openNebulaManagedByLabel: "opennebula"},
	} {
		t.Run(owner, func(t *testing.T) {
			c := keepDrain(t, &database.Resource{Name: "res1", Nodes: "n1,n2", Labels: labels}, "no")
			moved, left, err := c.ctrl.resources.DrainNodeWith(ctx, "n1", DrainOptions{KeepInUse: true, By: "kubernetes drain", Reason: "taint x"})
			require.NoError(t, err)
			assert.Empty(t, moved)
			require.Len(t, left, 1)
			assert.Equal(t, "res1", left[0].Resource)
			assert.Contains(t, left[0].Reason, owner+" decides where it is Primary")
			assert.Empty(t, c.secondary, "never demoted")
			assert.Empty(t, c.primary)
			assert.Equal(t, NodeStateMaintenance, c.state(t, "10.0.0.1"), "the node takes no new replicas from now on")
		})
	}
	// The claim is named, so the reader knows which workload holds it.
	c := keepDrain(t, &database.Resource{Name: "res1", Nodes: "n1,n2",
		Labels: map[string]string{csiManagedByLabel: "csi", csiPVCLabel: "db/data-0"}}, "no")
	_, left, err := c.ctrl.resources.DrainNodeWith(ctx, "n1", DrainOptions{KeepInUse: true})
	require.NoError(t, err)
	assert.Contains(t, left[0].Reason, "PVC db/data-0")
}

// What Haify itself keeps Primary moves, as in any drain: a gateway, an HA
// service, and a shared CSI volume, whose Primary is its gateway's.
func TestDrainKeepInUseMovesWhatHaifyPromotes(t *testing.T) {
	ctx := context.Background()
	c := keepDrain(t, &database.Resource{Name: "res1", Nodes: "n1,n2",
		Labels: map[string]string{csiManagedByLabel: "csi", csiSharedLabel: "nfs"}}, "yes")
	require.NoError(t, c.ctrl.db.SaveGateway(ctx, &database.Gateway{Name: "res1", Resource: "res1", Type: database.GatewayTypeNFS}))
	moved, left, err := c.ctrl.resources.DrainNodeWith(ctx, "n1", DrainOptions{KeepInUse: true})
	require.NoError(t, err)
	assert.Equal(t, []string{"res1"}, moved)
	assert.Empty(t, left)
	assert.Contains(t, strings.Join(c.execs, "\n"), base64Std(evictScript("res1")), "moved by the promoter's eviction")
	assert.Empty(t, c.secondary)
}

// A resource nobody is on record for is moved only when its device is seen
// closed; open, or unreadable, it stays.
func TestDrainKeepInUseLeavesOpenDevices(t *testing.T) {
	ctx := context.Background()
	c := keepDrain(t, &database.Resource{Name: "res1", Nodes: "n1,n2"}, "yes")
	moved, left, err := c.ctrl.resources.DrainNodeWith(ctx, "n1", DrainOptions{KeepInUse: true})
	require.NoError(t, err)
	assert.Empty(t, moved)
	require.Len(t, left, 1)
	assert.Contains(t, left[0].Reason, "its device is open on n1")
	assert.Empty(t, c.secondary)

	c = keepDrain(t, &database.Resource{Name: "res1", Nodes: "n1,n2"}, "unreadable")
	moved, left, err = c.ctrl.resources.DrainNodeWith(ctx, "n1", DrainOptions{KeepInUse: true})
	require.NoError(t, err)
	assert.Empty(t, moved)
	require.Len(t, left, 1)
	assert.Contains(t, left[0].Reason, "could not be read")
	assert.Empty(t, c.secondary)

	// A DRBD that does not say whether the device is open has not said it
	// is closed.
	c = keepDrain(t, &database.Resource{Name: "res1", Nodes: "n1,n2"}, "unreported")
	moved, left, err = c.ctrl.resources.DrainNodeWith(ctx, "n1", DrainOptions{KeepInUse: true})
	require.NoError(t, err)
	assert.Empty(t, moved)
	require.Len(t, left, 1)
	assert.Contains(t, left[0].Reason, "does not report whether its device is in use")
	assert.Empty(t, c.secondary)

	c = keepDrain(t, &database.Resource{Name: "res1", Nodes: "n1,n2"}, "no")
	moved, left, err = c.ctrl.resources.DrainNodeWith(ctx, "n1", DrainOptions{KeepInUse: true})
	require.NoError(t, err)
	assert.Equal(t, []string{"res1"}, moved)
	assert.Empty(t, left)
	assert.Equal(t, []string{"10.0.0.1"}, c.secondary)
	assert.Equal(t, []string{"10.0.0.2"}, c.primary)

	// Without the option the drain is what it always was: the open device is
	// not asked about, and the resource is moved.
	c = keepDrain(t, &database.Resource{Name: "res1", Nodes: "n1,n2"}, "yes")
	moved, err = c.ctrl.resources.DrainNode(ctx, "n1")
	require.NoError(t, err)
	assert.Equal(t, []string{"res1"}, moved)
	assert.NotContains(t, strings.Join(c.execs, "\n"), "--verbose")
}

// The node says who drained it and why while it is drained, the first drain's
// word stands, and an undrain clears it.
func TestDrainRecordsWhoDrained(t *testing.T) {
	ctx := context.Background()
	c := keepDrain(t, &database.Resource{Name: "res1", Nodes: "n2,n3"}, "no")
	srv := NewServer(c.ctrl)
	resp, err := srv.DrainNode(ctx, &haifypb.DrainNodeRequest{Name: "n1", KeepInUse: true, RequestedBy: "kubernetes drain", Reason: "taint medik8s.io/drain"})
	require.NoError(t, err)
	require.True(t, resp.Success, resp.Message)

	node := func() *haifypb.NodeInfo {
		list, lerr := srv.ListNodes(ctx, &haifypb.ListNodesRequest{})
		require.NoError(t, lerr)
		for _, n := range list.Nodes {
			if n.Name == "n1" {
				return n
			}
		}
		t.Fatal("n1 not listed")
		return nil
	}
	n := node()
	assert.Equal(t, "maintenance", n.State)
	assert.Equal(t, "kubernetes drain", n.MaintenanceBy)
	assert.Equal(t, "taint medik8s.io/drain", n.MaintenanceReason)
	assert.NotZero(t, n.MaintenanceSince)
	since := n.MaintenanceSince
	rec, err := c.ctrl.db.GetNode(ctx, "10.0.0.1")
	require.NoError(t, err)
	assert.Equal(t, "kubernetes drain", rec.MaintenanceBy, "it survives a controller failover")

	// The drain that finishes the job, and an operator's on top, change nothing.
	_, err = srv.DrainNode(ctx, &haifypb.DrainNodeRequest{Name: "n1", RequestedBy: "admin"})
	require.NoError(t, err)
	n = node()
	assert.Equal(t, "kubernetes drain", n.MaintenanceBy)
	assert.Equal(t, since, n.MaintenanceSince)

	_, err = srv.UndrainNode(ctx, &haifypb.UndrainNodeRequest{Name: "n1"})
	require.NoError(t, err)
	n = node()
	assert.Equal(t, "online", n.State)
	assert.Empty(t, n.MaintenanceBy)
	assert.Empty(t, n.MaintenanceReason)
	assert.Zero(t, n.MaintenanceSince)
	rec, err = c.ctrl.db.GetNode(ctx, "10.0.0.1")
	require.NoError(t, err)
	assert.Empty(t, rec.MaintenanceBy)
}

// The reply of a keep-in-use drain lists what stayed and why.
func TestDrainNodeReplyListsWhatWasLeft(t *testing.T) {
	c := keepDrain(t, &database.Resource{Name: "res1", Nodes: "n1,n2", Labels: map[string]string{csiManagedByLabel: "csi"}}, "yes")
	resp, err := NewServer(c.ctrl).DrainNode(context.Background(), &haifypb.DrainNodeRequest{Name: "n1", KeepInUse: true})
	require.NoError(t, err)
	require.True(t, resp.Success, resp.Message)
	assert.Empty(t, resp.ResourcesMoved)
	require.Len(t, resp.ResourcesLeft, 1)
	assert.Equal(t, "res1", resp.ResourcesLeft[0].Resource)
	assert.Contains(t, resp.Message, "1 still Primary on it and left where they are")
}
