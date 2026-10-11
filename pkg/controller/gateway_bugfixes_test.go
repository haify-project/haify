package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/deployment"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resWithVolumes builds a controller + BBolt DB with a two-node resource and the
// given persisted volumes, plus a fake whose `cat <res>.res` returns cfg.
func resWithVolumes(t *testing.T, cfg string, dep *fakeDeploymentClient, volumes ...*database.Volume) *Controller {
	t.Helper()
	ctrl := newBasicTestController(dep)
	db := newTestDB(t)
	ctrl.db = db
	require.NoError(t, ctrl.db.SaveResource(context.Background(), &database.Resource{
		Name: "res1", Port: 7001, Nodes: "node1,node2", Protocol: "C", Replicas: 2,
	}))
	for _, v := range volumes {
		require.NoError(t, ctrl.db.SaveVolume(context.Background(), v))
	}
	ctrl.nodes.nodes["10.0.0.1"] = &NodeInfo{Name: "node1", Address: "10.0.0.1"}
	ctrl.nodes.nodes["10.0.0.2"] = &NodeInfo{Name: "node2", Address: "10.0.0.2"}
	ctrl.hostsMap["node1"] = "10.0.0.1"
	ctrl.hostsMap["node2"] = "10.0.0.2"
	return ctrl
}

// BUG 1: a retried gateway state-volume add must NOT append a second volume
// block for a disk the config already references — that is what produced
// "conflicting use of disk" after a few retries. The add is idempotent.
func TestAddVolumeIdempotentWhenDiskAlreadyReferenced(t *testing.T) {
	// Config already carries volume 1 pointing at the state LV.
	config := "resource res1 {\n" +
		"    volume 0 {\n        device    minor 1;\n        disk      /dev/haify_data-pool/res1_data;\n        meta-disk internal;\n    }\n" +
		"    volume 1 {\n        device    minor 2;\n        disk      /dev/haify_data-pool/res1_state1;\n        meta-disk internal;\n    }\n}\n"
	dep := &fakeDeploymentClient{
		execFunc: func(ctx context.Context, hosts []string, cmd string, opts ...deployment.ExecOption) (*deployment.ExecResult, error) {
			if strings.HasPrefix(cmd, "cat /etc/drbd.d/res1.res") {
				return successExecResult(hosts, config), nil
			}
			return successExecResult(hosts, ""), nil
		},
	}
	ctrl := resWithVolumes(t, config, dep)

	err := ctrl.resources.AddVolume(context.Background(), "res1", "res1_state1", "data-pool", 1)
	require.NoError(t, err)

	// No new config must be distributed and no create-md/adjust issued: the
	// volume is already present, so the add is a no-op.
	assert.Empty(t, dep.distributedConfigs, "must not re-distribute config for an already-present volume")
	for _, call := range dep.execCalls {
		assert.NotContains(t, call.cmd, "create-md", "must not create-md a duplicate volume")
	}
}

// BUG 1: when a step after the config append fails (create-md), the just-added
// volume block must be rolled back (original config restored) and the created
// LV removed, so a retry starts from a clean .res instead of stacking blocks.
func TestAddVolumeRollsBackAppendedBlockAndLVOnFailure(t *testing.T) {
	config := "resource res1 {\n    volume 0 {\n        device    minor 1;\n        disk      /dev/haify_data-pool/res1_data;\n        meta-disk internal;\n    }\n}\n"
	dep := &fakeDeploymentClient{
		execFunc: func(ctx context.Context, hosts []string, cmd string, opts ...deployment.ExecOption) (*deployment.ExecResult, error) {
			if strings.HasPrefix(cmd, "cat /etc/drbd.d/res1.res") {
				return successExecResult(hosts, config), nil
			}
			if strings.Contains(cmd, "create-md") {
				// create-md fails on every host (the failure BUG 1 rolls back).
				res := &deployment.ExecResult{Hosts: make(map[string]*deployment.HostResult, len(hosts))}
				for _, h := range hosts {
					res.Hosts[h] = &deployment.HostResult{Host: h, Success: false, Output: "conflicting use of disk"}
				}
				return res, nil
			}
			return successExecResult(hosts, ""), nil
		},
	}
	ctrl := resWithVolumes(t, config, dep)

	err := ctrl.resources.AddVolume(context.Background(), "res1", "res1_state1", "data-pool", 1)
	require.Error(t, err)

	// Two distributes: the forward append (with the new block), then the
	// rollback that restores the original config.
	require.Len(t, dep.distributedConfigs, 2)
	assert.Contains(t, dep.distributedConfigs[0].content, "res1_state1", "forward config must add the block")
	assert.Equal(t, config, dep.distributedConfigs[1].content, "rollback must restore the original config")
	assert.NotContains(t, dep.distributedConfigs[1].content, "res1_state1")

	// The LV created for the failed add must be removed.
	var removedStateLV bool
	for _, call := range dep.lvRemoveCalls {
		if call.lvPath == "/dev/haify_data-pool/res1_state1" {
			removedStateLV = true
		}
	}
	assert.True(t, removedStateLV, "rollback must lvremove the created state LV")

	// The DB must not record a volume for a rolled-back add.
	volumes, err := ctrl.db.ListVolumes(context.Background(), "res1")
	require.NoError(t, err)
	assert.Empty(t, volumes)
}

// BUG 2: deleting a resource must remove the backing LVs for ALL of its
// volumes, including auto-provisioned gateway state volumes, and sweep any
// orphaned "<res>_state*" LV that has no volume record.
func TestDeleteResourceRemovesStateVolumeLVs(t *testing.T) {
	dep := &fakeDeploymentClient{}
	ctrl := resWithVolumes(t, "", dep,
		&database.Volume{ResourceName: "res1", VolumeName: "res1_data", VolumeID: 0, Pool: "haify_data-pool", SizeGB: 10, Device: "/dev/haify_data-pool/res1_data"},
		&database.Volume{ResourceName: "res1", VolumeName: "res1_state1", VolumeID: 1, Pool: "haify_data-pool", SizeGB: 1, Device: "/dev/haify_data-pool/res1_state1"},
	)

	err := ctrl.resources.DeleteResource(context.Background(), "res1", true)
	require.NoError(t, err)

	var removedData, removedState, sawSweep bool
	for _, call := range dep.execCalls {
		if strings.Contains(call.cmd, "lvremove -f haify_data-pool/res1_data") {
			removedData = true
		}
		if strings.Contains(call.cmd, "lvremove -f haify_data-pool/res1_state1") {
			removedState = true
		}
		// The orphan sweep enumerates the pool for leftover state LVs.
		if strings.Contains(call.cmd, "grep -E '^res1_state[0-9]+$'") {
			sawSweep = true
			assert.Equal(t, []string{"10.0.0.1", "10.0.0.2"}, call.hosts)
		}
	}
	assert.True(t, removedData, "data volume LV must be removed")
	assert.True(t, removedState, "state volume LV must be removed")
	assert.True(t, sawSweep, "must sweep the pool for orphaned <res>_state* LVs")

	volumes, err := ctrl.db.ListVolumes(context.Background(), "res1")
	require.NoError(t, err)
	assert.Empty(t, volumes)
}

// BUG 3 (approach B): CreateResource must establish the initial UpToDate
// generation at create time by force-promoting the first diskful node once and
// demoting it back to Secondary, so a fresh resource is UpToDate before any
// gateway state volume is added and a later gateway promote is a plain promote.
func TestCreateResourceEstablishesInitialSync(t *testing.T) {
	var forces []bool
	var demoted bool
	dep := &fakeDeploymentClient{
		drbdPrimaryFunc: func(_ context.Context, host, resource string, force bool) (*deployment.HostResult, error) {
			forces = append(forces, force)
			return &deployment.HostResult{Host: host, Success: true}, nil
		},
		drbdSecondaryFunc: func(_ context.Context, host, resource string) (*deployment.HostResult, error) {
			demoted = true
			return &deployment.HostResult{Host: host, Success: true}, nil
		},
	}
	ctrl := newBasicTestController(dep)
	ctrl.nodes.nodes["10.0.0.1"] = &NodeInfo{Name: "node1", Address: "10.0.0.1"}
	ctrl.nodes.nodes["10.0.0.2"] = &NodeInfo{Name: "node2", Address: "10.0.0.2"}
	ctrl.hostsMap["node1"] = "10.0.0.1"
	ctrl.hostsMap["node2"] = "10.0.0.2"

	err := ctrl.resources.CreateResource(context.Background(), "res1", 7001, []string{"node1", "node2"}, "", 10, "data-pool", "lvm", nil)
	require.NoError(t, err)
	assert.Equal(t, []bool{true}, forces, "create must force-promote exactly once to seed the initial UpToDate generation")
	assert.True(t, demoted, "create must demote back to Secondary after seeding UpToDate")
}

// BUG 3 (approach B guard): if the initial force-promote fails, resource create
// must fail and roll back rather than leave an un-promotable resource behind.
func TestCreateResourceFailsWhenInitialSyncFails(t *testing.T) {
	dep := &fakeDeploymentClient{
		drbdPrimaryFunc: func(_ context.Context, host, resource string, force bool) (*deployment.HostResult, error) {
			return &deployment.HostResult{Host: host, Success: false, Output: "boom"}, nil
		},
	}
	ctrl := newBasicTestController(dep)
	ctrl.nodes.nodes["10.0.0.1"] = &NodeInfo{Name: "node1", Address: "10.0.0.1"}
	ctrl.nodes.nodes["10.0.0.2"] = &NodeInfo{Name: "node2", Address: "10.0.0.2"}
	ctrl.hostsMap["node1"] = "10.0.0.1"
	ctrl.hostsMap["node2"] = "10.0.0.2"

	err := ctrl.resources.CreateResource(context.Background(), "res1", 7001, []string{"node1", "node2"}, "", 10, "data-pool", "lvm", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "initial sync")
	// Rollback must have torn the half-created resource down.
	require.NotEmpty(t, dep.drbdDownCalls, "failed create must roll back with a DRBD down")
}

// BUG 3 (pure parse): the initial-force decision is PER VOLUME, and must not be
// fooled by a sibling volume being UpToDate.
func TestSafeToForceInitialSync(t *testing.T) {
	// Single volume, both local and peer Inconsistent -> fresh, force needed.
	force, err := safeToForceInitialSync(`[{"name":"data","role":"Secondary","devices":[{"volume":0,"disk-state":"Inconsistent"}],"connections":[{"name":"node2","connection-state":"Connected","peer-role":"Secondary","peer_devices":[{"volume":0,"replication-state":"Established","peer-disk-state":"Inconsistent"}]}]}]`)
	require.NoError(t, err)
	assert.True(t, force)

	// Single volume, local UpToDate -> nothing to do.
	force, err = safeToForceInitialSync(`[{"name":"data","role":"Primary","devices":[{"volume":0,"disk-state":"UpToDate"}],"connections":[]}]`)
	require.NoError(t, err)
	assert.False(t, force)

	// Single volume, peer UpToDate (normal failover) -> must NOT force.
	force, err = safeToForceInitialSync(`[{"name":"data","role":"Secondary","devices":[{"volume":0,"disk-state":"Inconsistent"}],"connections":[{"name":"node2","connection-state":"Connected","peer-role":"Primary","peer_devices":[{"volume":0,"replication-state":"Established","peer-disk-state":"UpToDate"}]}]}]`)
	require.NoError(t, err)
	assert.False(t, force)

	// The coordinator's real case: a MIXED multi-volume resource where the
	// gateway state volume (1) is UpToDate but the DATA volume (0) has no
	// UpToDate copy anywhere. The UpToDate volume 1 must NOT mask volume 0:
	// force IS needed (and safe — volume 1 is locally UpToDate, volume 0 has no
	// data to lose).
	force, err = safeToForceInitialSync(`[{"name":"vtest","role":"Secondary","devices":[{"volume":0,"disk-state":"Inconsistent"},{"volume":1,"disk-state":"UpToDate"}],"connections":[{"name":"node-b","connection-state":"Connected","peer-role":"Secondary","peer_devices":[{"volume":0,"peer-disk-state":"Inconsistent"},{"volume":1,"peer-disk-state":"UpToDate"}]},{"name":"node-c","connection-state":"Connected","peer-role":"Secondary","peer_devices":[{"volume":0,"peer-disk-state":"Diskless"},{"volume":1,"peer-disk-state":"Diskless"}]}]}]`)
	require.NoError(t, err)
	assert.True(t, force, "an UpToDate state volume must not suppress the force the data volume needs")

	// Data-loss guard, multi-volume: volume 0 has a real UpToDate copy on a peer
	// (would be overwritten by a blanket force) while volume 1 is fresh. The
	// unsafe volume 0 must veto the whole force.
	force, err = safeToForceInitialSync(`[{"name":"vtest","role":"Secondary","devices":[{"volume":0,"disk-state":"Inconsistent"},{"volume":1,"disk-state":"Inconsistent"}],"connections":[{"name":"node-b","connection-state":"Connected","peer-role":"Primary","peer_devices":[{"volume":0,"peer-disk-state":"UpToDate"},{"volume":1,"peer-disk-state":"Inconsistent"}]}]}]`)
	require.NoError(t, err)
	assert.False(t, force, "a peer's real UpToDate data on any volume must veto a blanket force")

	// Unparseable -> error (fail closed).
	_, err = safeToForceInitialSync("")
	require.Error(t, err)
}

// BUG 3: a plain (non-forced) promote of a freshly created, all-Inconsistent
// resource fails with "Need access to UpToDate data"; SetPrimary must detect
// the no-UpToDate-anywhere state and escalate to a single force-promote.
func TestSetPrimaryForcesInitialSyncWhenNoUpToDate(t *testing.T) {
	var forces []bool
	dep := &fakeDeploymentClient{
		drbdPrimaryFunc: func(_ context.Context, host, resource string, force bool) (*deployment.HostResult, error) {
			forces = append(forces, force)
			if !force {
				return &deployment.HostResult{Host: host, Success: false, Output: "State change failed: (-2) Need access to UpToDate data"}, nil
			}
			return &deployment.HostResult{Host: host, Success: true}, nil
		},
		drbdStatusJSONFunc: func(_ context.Context, hosts []string, resource string) (*deployment.ExecResult, error) {
			return successExecResult(hosts, `[{"name":"data","role":"Secondary","devices":[{"volume":0,"disk-state":"Inconsistent"}],"connections":[{"name":"node2","connection-state":"Connected","peer-role":"Secondary","peer_devices":[{"volume":0,"replication-state":"Established","peer-disk-state":"Inconsistent"}]}]}]`), nil
		},
	}
	ctrl := newBasicTestController(dep)

	err := ctrl.resources.SetPrimary(context.Background(), "data", "node1", false)
	require.NoError(t, err)
	assert.Equal(t, []bool{false, true}, forces, "must try non-forced, then force once the fresh initial-sync state is confirmed")
}

// BUG 3 (guard): a failed non-forced promote where a replica is still UpToDate
// (a normal failover, not initial sync) must NOT be escalated to force.
func TestSetPrimaryDoesNotForceWhenPeerUpToDate(t *testing.T) {
	var forces []bool
	dep := &fakeDeploymentClient{
		drbdPrimaryFunc: func(_ context.Context, host, resource string, force bool) (*deployment.HostResult, error) {
			forces = append(forces, force)
			return &deployment.HostResult{Host: host, Success: false, Output: "State change was refused by peer node"}, nil
		},
		drbdStatusJSONFunc: func(_ context.Context, hosts []string, resource string) (*deployment.ExecResult, error) {
			return successExecResult(hosts, `[{"name":"data","role":"Secondary","devices":[{"volume":0,"disk-state":"Inconsistent"}],"connections":[{"name":"node2","connection-state":"Connected","peer-role":"Primary","peer_devices":[{"volume":0,"replication-state":"Established","peer-disk-state":"UpToDate"}]}]}]`), nil
		},
	}
	ctrl := newBasicTestController(dep)

	err := ctrl.resources.SetPrimary(context.Background(), "data", "node1", false)
	require.Error(t, err)
	assert.Equal(t, []bool{false}, forces, "must NOT force-promote when a replica is still UpToDate")
}

// A peer that is not connected may hold the only copy with data; an Outdated
// or Consistent local copy has history. Neither is an initial sync.
func TestSafeToForceInitialSyncRefusesUnknownPeersAndHistory(t *testing.T) {
	for name, js := range map[string]string{
		"peer unreachable": `[{"name":"d","devices":[{"volume":0,"disk-state":"Inconsistent"}],"connections":[{"name":"n2","connection-state":"Connecting"}]}]`,
		"local Outdated":   `[{"name":"d","devices":[{"volume":0,"disk-state":"Outdated"}],"connections":[{"name":"n2","connection-state":"Connected","peer_devices":[{"volume":0,"peer-disk-state":"Diskless"}]}]}]`,
		"local Consistent": `[{"name":"d","devices":[{"volume":0,"disk-state":"Consistent"}],"connections":[]}]`,
		"peer Outdated":    `[{"name":"d","devices":[{"volume":0,"disk-state":"Inconsistent"}],"connections":[{"name":"n2","connection-state":"Connected","peer_devices":[{"volume":0,"peer-disk-state":"Outdated"}]}]}]`,
	} {
		force, err := safeToForceInitialSync(js)
		require.NoError(t, err, name)
		assert.False(t, force, name)
	}
}
