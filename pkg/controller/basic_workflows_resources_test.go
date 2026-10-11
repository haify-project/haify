package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/deployment"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResourceManagerCreateResourceUsesNormalizedPool(t *testing.T) {
	dep := &fakeDeploymentClient{}
	ctrl := newBasicTestController(dep)
	ctrl.nodes.nodes["10.0.0.1"] = &NodeInfo{Name: "node1", Address: "10.0.0.1"}
	ctrl.nodes.nodes["10.0.0.2"] = &NodeInfo{Name: "node2", Address: "10.0.0.2"}
	ctrl.hostsMap["node1"] = "10.0.0.1"
	ctrl.hostsMap["node2"] = "10.0.0.2"

	err := ctrl.resources.CreateResource(context.Background(), "res1", 7001, []string{"node1", "node2"}, "", 10, "data-pool", "lvm", nil)
	require.NoError(t, err)
	require.Len(t, dep.lvCreateCalls, 2)
	assert.Equal(t, "haify_data-pool", dep.lvCreateCalls[0].vgName)
	assert.Equal(t, "res1_data", dep.lvCreateCalls[0].lvName)
	assert.Equal(t, []string{"10.0.0.1"}, dep.lvCreateCalls[0].hosts)
	require.Len(t, dep.distributedConfigs, 1)
	assert.Contains(t, dep.distributedConfigs[0].content, "/dev/haify_data-pool/res1_data")
	assert.Contains(t, dep.distributedConfigs[0].remotePath, "/etc/drbd.d/res1.res")
}

func TestResourceManagerCreateResourcePersistsInitialVolume(t *testing.T) {
	dep := &fakeDeploymentClient{}
	ctrl := newBasicTestController(dep)
	ctrl.nodes.nodes["10.0.0.1"] = &NodeInfo{Name: "node1", Address: "10.0.0.1"}
	ctrl.nodes.nodes["10.0.0.2"] = &NodeInfo{Name: "node2", Address: "10.0.0.2"}
	ctrl.hostsMap["node1"] = "10.0.0.1"
	ctrl.hostsMap["node2"] = "10.0.0.2"

	db := newTestDB(t)
	ctrl.db = db

	err := ctrl.resources.CreateResourceWithVolumesMetadata(context.Background(), "res1", 7001, []string{"node1", "node2"}, "", "lvm", nil,
		[]VolumeSpec{{SizeGB: 10, Pool: "data-pool"}}, nil, ResourceMetadata{
			Profile: "production",
			Labels:  map[string]string{"app": "postgres"},
		})
	require.NoError(t, err)
	resource, err := ctrl.db.GetResource(context.Background(), "res1")
	require.NoError(t, err)
	assert.Equal(t, "production", resource.Profile)
	assert.Equal(t, map[string]string{"app": "postgres"}, resource.Labels)

	volumes, err := ctrl.db.ListVolumes(context.Background(), "res1")
	require.NoError(t, err)
	require.Len(t, volumes, 1)
	assert.Equal(t, "res1_data", volumes[0].VolumeName)
	assert.Equal(t, 0, volumes[0].VolumeID)
	assert.Equal(t, "haify_data-pool", volumes[0].Pool)
	assert.Equal(t, 10, volumes[0].SizeGB)
	assert.Equal(t, "/dev/haify_data-pool/res1_data", volumes[0].Device)
}

func TestResourceManagerCreateResourceEnablesDRBDBootUnit(t *testing.T) {
	// After a successful create the native haify-drbd-up.service oneshot must be
	// installed AND enabled on every diskful node, so a rebooted node re-runs
	// `drbdadm adjust all` and rejoins replication without a manual `drbdadm
	// adjust`. We must NOT rely on the packaged drbd.service: it is an LSB unit
	// that cannot be enabled (empty Default-Start).
	dep := &fakeDeploymentClient{}
	ctrl := newBasicTestController(dep)
	ctrl.nodes.nodes["10.0.0.1"] = &NodeInfo{Name: "node1", Address: "10.0.0.1"}
	ctrl.nodes.nodes["10.0.0.2"] = &NodeInfo{Name: "node2", Address: "10.0.0.2"}
	ctrl.hostsMap["node1"] = "10.0.0.1"
	ctrl.hostsMap["node2"] = "10.0.0.2"

	err := ctrl.resources.CreateResource(context.Background(), "res1", 7001, []string{"node1", "node2"}, "", 10, "data-pool", "lvm", nil)
	require.NoError(t, err)

	var installHosts []string
	var installCmd string
	for _, call := range dep.execCalls {
		// The single install command both writes the unit file to
		// /etc/systemd/system/haify-drbd-up.service and enables it.
		if strings.Contains(call.cmd, "/etc/systemd/system/haify-drbd-up.service") &&
			strings.Contains(call.cmd, "systemctl enable haify-drbd-up.service") {
			installHosts = call.hosts
			installCmd = call.cmd
			break
		}
	}
	require.NotNil(t, installHosts,
		"expected an exec call that writes and enables haify-drbd-up.service; got %+v", dep.execCalls)
	// The boot bring-up must install the helper script, activate LVM first
	// (so backing devices exist), and adjust each resource INDEPENDENTLY.
	// It must not use `up all`/`adjust all`, which abort on a foreign resource
	// that is already up ("minor exists") and leave the rest Diskless.
	assert.Contains(t, installCmd, drbdBootScriptPath)
	assert.Contains(t, installCmd, "vgchange -ay")
	assert.Contains(t, installCmd, `adjust "$res"`)
	assert.NotContains(t, installCmd, "adjust all")
	assert.NotContains(t, installCmd, "up all\n")
	// The un-enableable LSB drbd.service must never be the mechanism.
	for _, call := range dep.execCalls {
		assert.NotContains(t, call.cmd, "systemctl enable drbd.service",
			"must not attempt to enable the un-enableable LSB drbd.service")
	}
	assert.ElementsMatch(t, []string{"10.0.0.1", "10.0.0.2"}, installHosts,
		"boot unit must be installed and enabled on both diskful nodes")
}

func TestResourceManagerCreateResourceSucceedsWhenBootUnitEnableFails(t *testing.T) {
	// Installing/enabling the boot unit is best-effort: a failure must never
	// fail resource creation, since the resource is already up at that point.
	dep := &fakeDeploymentClient{
		execFunc: func(ctx context.Context, hosts []string, cmd string, opts ...deployment.ExecOption) (*deployment.ExecResult, error) {
			if strings.Contains(cmd, "haify-drbd-up.service") {
				return nil, context.DeadlineExceeded
			}
			return successExecResult(hosts, ""), nil
		},
	}
	ctrl := newBasicTestController(dep)
	ctrl.nodes.nodes["10.0.0.1"] = &NodeInfo{Name: "node1", Address: "10.0.0.1"}
	ctrl.nodes.nodes["10.0.0.2"] = &NodeInfo{Name: "node2", Address: "10.0.0.2"}
	ctrl.hostsMap["node1"] = "10.0.0.1"
	ctrl.hostsMap["node2"] = "10.0.0.2"

	err := ctrl.resources.CreateResource(context.Background(), "res1", 7001, []string{"node1", "node2"}, "", 10, "data-pool", "lvm", nil)
	require.NoError(t, err, "boot-unit enable failure must not fail resource create")
}

func TestResourceManagerDeleteResourceRemovesDatabaseRecord(t *testing.T) {
	dep := &fakeDeploymentClient{}
	ctrl := newBasicTestController(dep)

	db := newTestDB(t)
	ctrl.db = db

	require.NoError(t, ctrl.db.SaveResource(context.Background(), &database.Resource{
		Name:     "res1",
		Port:     7001,
		Nodes:    "node1,node2",
		Protocol: "C",
		Replicas: 2,
	}))
	require.NoError(t, ctrl.db.SaveVolume(context.Background(), &database.Volume{
		ResourceName: "res1",
		VolumeName:   "res1_data",
		VolumeID:     0,
		Pool:         "haify_data-pool",
		SizeGB:       10,
		Device:       "/dev/haify_data-pool/res1_data",
	}))

	ctrl.nodes.nodes["10.0.0.1"] = &NodeInfo{Name: "node1", Address: "10.0.0.1"}
	ctrl.nodes.nodes["10.0.0.2"] = &NodeInfo{Name: "node2", Address: "10.0.0.2"}
	ctrl.hostsMap["node1"] = "10.0.0.1"
	ctrl.hostsMap["node2"] = "10.0.0.2"

	ctrl.resources.mu.Lock()
	ctrl.resources.hosts = []string{"wrong-host"}
	ctrl.resources.mu.Unlock()

	err := ctrl.resources.DeleteResource(context.Background(), "res1", true)
	require.NoError(t, err)
	require.Len(t, dep.drbdDownCalls, 1)
	assert.Equal(t, []string{"10.0.0.1", "10.0.0.2"}, dep.drbdDownCalls[0].hosts)
	require.Len(t, dep.deleteConfigCalls, 1)
	assert.Equal(t, []string{"10.0.0.1", "10.0.0.2"}, dep.deleteConfigCalls[0].hosts)
	_, err = ctrl.db.GetResource(context.Background(), "res1")
	assert.ErrorContains(t, err, "not found")
	volumes, err := ctrl.db.ListVolumes(context.Background(), "res1")
	require.NoError(t, err)
	assert.Empty(t, volumes)

	// The backing LV must be removed on all resource hosts — the database
	// records were the last knowledge of which LVs belonged to the resource.
	foundLvremove := false
	for _, call := range dep.execCalls {
		if strings.Contains(call.cmd, "lvremove -f haify_data-pool/res1_data") {
			foundLvremove = true
			assert.Equal(t, []string{"10.0.0.1", "10.0.0.2"}, call.hosts)
		}
	}
	assert.True(t, foundLvremove, "backing LV was not removed")
}

func TestResourceManagerGetResourceUsesResourceSpecificHosts(t *testing.T) {
	dep := &fakeDeploymentClient{
		drbdStatusFunc: func(ctx context.Context, hosts []string, resource string) (*deployment.ExecResult, error) {
			return successExecResult(hosts, "resource role:Primary\n"), nil
		},
	}
	ctrl := newBasicTestController(dep)

	db := newTestDB(t)
	ctrl.db = db

	require.NoError(t, ctrl.db.SaveResource(context.Background(), &database.Resource{
		Name:     "res1",
		Port:     7001,
		Nodes:    "node1,node2",
		Protocol: "C",
		Replicas: 2,
	}))

	ctrl.nodes.nodes["10.0.0.1"] = &NodeInfo{Name: "node1", Address: "10.0.0.1"}
	ctrl.nodes.nodes["10.0.0.2"] = &NodeInfo{Name: "node2", Address: "10.0.0.2"}
	ctrl.hostsMap["node1"] = "10.0.0.1"
	ctrl.hostsMap["node2"] = "10.0.0.2"

	ctrl.resources.mu.Lock()
	ctrl.resources.hosts = []string{"wrong-host"}
	ctrl.resources.mu.Unlock()

	_, err := ctrl.resources.GetResource(context.Background(), "res1")
	require.NoError(t, err)
	require.Len(t, dep.drbdStatusCalls, 1)
	assert.Equal(t, []string{"10.0.0.1"}, dep.drbdStatusCalls[0].hosts)
}

func TestResourceManagerGetResourceFallsBackToPersistedVolumes(t *testing.T) {
	dep := &fakeDeploymentClient{
		drbdStatusFunc: func(ctx context.Context, hosts []string, resource string) (*deployment.ExecResult, error) {
			return successExecResult(hosts, ""), nil
		},
	}
	ctrl := newBasicTestController(dep)

	db := newTestDB(t)
	ctrl.db = db

	require.NoError(t, ctrl.db.SaveResource(context.Background(), &database.Resource{
		Name:     "res1",
		Port:     7001,
		Nodes:    "node1,node2",
		Protocol: "C",
		Replicas: 2,
	}))
	require.NoError(t, ctrl.db.SaveVolume(context.Background(), &database.Volume{
		ResourceName: "res1",
		VolumeName:   "res1_data",
		VolumeID:     0,
		Pool:         "haify_data-pool",
		SizeGB:       10,
		Device:       "/dev/haify_data-pool/res1_data",
	}))

	ctrl.nodes.nodes["10.0.0.1"] = &NodeInfo{Name: "node1", Address: "10.0.0.1"}
	ctrl.nodes.nodes["10.0.0.2"] = &NodeInfo{Name: "node2", Address: "10.0.0.2"}
	ctrl.hostsMap["node1"] = "10.0.0.1"
	ctrl.hostsMap["node2"] = "10.0.0.2"

	info, err := ctrl.resources.GetResource(context.Background(), "res1")
	require.NoError(t, err)
	require.Len(t, info.Volumes, 1)
	assert.Equal(t, uint32(0), info.Volumes[0].VolumeID)
	assert.Equal(t, uint64(10), info.Volumes[0].SizeGB)
	assert.Equal(t, "/dev/drbd/by-res/res1/0", info.Volumes[0].Device)
}

func TestResourceManagerAddVolumePersistsMetadata(t *testing.T) {
	config := "resource res1 {\n    volume 0 {\n        device    minor 1;\n        disk      /dev/haify_data-pool/res1_data;\n        meta-disk internal;\n    }\n}\n"
	dep := &fakeDeploymentClient{
		execFunc: func(ctx context.Context, hosts []string, cmd string, opts ...deployment.ExecOption) (*deployment.ExecResult, error) {
			if strings.HasPrefix(cmd, "cat /etc/drbd.d/res1.res") {
				return successExecResult(hosts, config), nil
			}
			if out, ok := settledVolumeStatus(cmd); ok {
				return successExecResult(hosts, out), nil
			}
			return successExecResult(hosts, ""), nil
		},
	}
	ctrl := newBasicTestController(dep)

	db := newTestDB(t)
	ctrl.db = db

	require.NoError(t, ctrl.db.SaveResource(context.Background(), &database.Resource{
		Name:     "res1",
		Port:     7001,
		Nodes:    "node1,node2",
		Protocol: "C",
		Replicas: 2,
	}))
	ctrl.nodes.nodes["10.0.0.1"] = &NodeInfo{Name: "node1", Address: "10.0.0.1"}
	ctrl.nodes.nodes["10.0.0.2"] = &NodeInfo{Name: "node2", Address: "10.0.0.2"}
	ctrl.hostsMap["node1"] = "10.0.0.1"
	ctrl.hostsMap["node2"] = "10.0.0.2"

	err := ctrl.resources.AddVolume(context.Background(), "res1", "res1_logs", "data-pool", 20)
	require.NoError(t, err)
	require.Len(t, dep.distributedConfigs, 1)
	assert.Contains(t, dep.distributedConfigs[0].content, "volume 1 {")
	assert.Contains(t, dep.distributedConfigs[0].content, "disk      /dev/haify_data-pool/res1_logs;")
	var sawAdjust bool
	for _, call := range dep.execCalls {
		if call.cmd == "sudo drbdadm adjust res1" {
			sawAdjust = true
			assert.Equal(t, []string{"10.0.0.1", "10.0.0.2"}, call.hosts)
		}
	}
	assert.True(t, sawAdjust)
	volumes, err := ctrl.db.ListVolumes(context.Background(), "res1")
	require.NoError(t, err)
	require.Len(t, volumes, 1)
	assert.Equal(t, "res1_logs", volumes[0].VolumeName)
	assert.Equal(t, 1, volumes[0].VolumeID)
	assert.Equal(t, 20, volumes[0].SizeGB)
}

func TestResourceManagerRemoveVolumeUpdatesConfigAndDatabase(t *testing.T) {
	config := "resource res1 {\n    volume 0 {\n        device    minor 1;\n        disk      /dev/haify_data-pool/res1_data;\n        meta-disk internal;\n    }\n\n    volume 1 {\n        device    minor 2;\n        disk      /dev/haify_data-pool/res1_logs;\n        meta-disk internal;\n    }\n}\n"
	dep := &fakeDeploymentClient{
		execFunc: func(ctx context.Context, hosts []string, cmd string, opts ...deployment.ExecOption) (*deployment.ExecResult, error) {
			if strings.HasPrefix(cmd, "cat /etc/drbd.d/res1.res") {
				return successExecResult(hosts, config), nil
			}
			if out, ok := settledVolumeStatus(cmd); ok {
				return successExecResult(hosts, out), nil
			}
			return successExecResult(hosts, ""), nil
		},
	}
	ctrl := newBasicTestController(dep)

	db := newTestDB(t)
	ctrl.db = db

	require.NoError(t, ctrl.db.SaveResource(context.Background(), &database.Resource{
		Name:     "res1",
		Port:     7001,
		Nodes:    "node1,node2",
		Protocol: "C",
		Replicas: 2,
	}))
	require.NoError(t, ctrl.db.SaveVolume(context.Background(), &database.Volume{
		ResourceName: "res1",
		VolumeName:   "res1_logs",
		VolumeID:     1,
		Pool:         "haify_data-pool",
		SizeGB:       20,
		Device:       "/dev/haify_data-pool/res1_logs",
	}))
	ctrl.nodes.nodes["10.0.0.1"] = &NodeInfo{Name: "node1", Address: "10.0.0.1"}
	ctrl.nodes.nodes["10.0.0.2"] = &NodeInfo{Name: "node2", Address: "10.0.0.2"}
	ctrl.hostsMap["node1"] = "10.0.0.1"
	ctrl.hostsMap["node2"] = "10.0.0.2"

	err := ctrl.resources.RemoveVolume(context.Background(), "res1", 1)
	require.NoError(t, err)
	require.Len(t, dep.distributedConfigs, 1)
	assert.NotContains(t, dep.distributedConfigs[0].content, "volume 1 {")
	require.NotEmpty(t, dep.execCalls)
	var sawAdjust, sawLVRemove bool
	for _, call := range dep.execCalls {
		if call.cmd == "sudo drbdadm adjust res1" {
			sawAdjust = true
		}
		if strings.Contains(call.cmd, "lvremove -f /dev/haify_data-pool/res1_logs") {
			sawLVRemove = true
		}
	}
	assert.True(t, sawAdjust)
	assert.True(t, sawLVRemove)
	volumes, err := ctrl.db.ListVolumes(context.Background(), "res1")
	require.NoError(t, err)
	assert.Empty(t, volumes)
}

// A failed lvremove on any node must surface as an error, not a silent success
// that leaves an orphaned LV and a lopsided DRBD resource.
func TestResourceManagerRemoveVolumeErrorsWhenBackingRemovalFails(t *testing.T) {
	config := "resource res1 {\n    volume 1 {\n        device    minor 2;\n        disk      /dev/haify_data-pool/res1_logs;\n        meta-disk internal;\n    }\n}\n"
	dep := &fakeDeploymentClient{
		execFunc: func(ctx context.Context, hosts []string, cmd string, opts ...deployment.ExecOption) (*deployment.ExecResult, error) {
			if strings.HasPrefix(cmd, "cat /etc/drbd.d/res1.res") {
				return successExecResult(hosts, config), nil
			}
			if strings.Contains(cmd, "lvremove") {
				res := successExecResult(hosts, "")
				for _, h := range res.Hosts {
					h.Success = false
					h.Output = "Logical volume is used by another device."
				}
				return res, nil
			}
			return successExecResult(hosts, ""), nil
		},
	}
	ctrl := newBasicTestController(dep)
	db := newTestDB(t)
	ctrl.db = db
	require.NoError(t, ctrl.db.SaveResource(context.Background(), &database.Resource{
		Name: "res1", Port: 7001, Nodes: "node1,node2", Protocol: "C", Replicas: 2,
	}))
	ctrl.nodes.nodes["10.0.0.1"] = &NodeInfo{Name: "node1", Address: "10.0.0.1"}
	ctrl.hostsMap["node1"] = "10.0.0.1"

	err := ctrl.resources.RemoveVolume(context.Background(), "res1", 1)
	require.Error(t, err, "RemoveVolume must not report success when lvremove fails")
}

func TestResourceManagerResizeVolumeUpdatesBackendAndMetadata(t *testing.T) {
	config := "resource res1 {\n    volume 1 {\n        device    minor 2;\n        disk      /dev/haify_data-pool/res1_logs;\n        meta-disk internal;\n    }\n}\n"
	dep := &fakeDeploymentClient{
		execFunc: func(ctx context.Context, hosts []string, cmd string, opts ...deployment.ExecOption) (*deployment.ExecResult, error) {
			if strings.HasPrefix(cmd, "cat /etc/drbd.d/res1.res") {
				return successExecResult(hosts, config), nil
			}
			if out, ok := settledVolumeStatus(cmd); ok {
				return successExecResult(hosts, out), nil
			}
			return successExecResult(hosts, ""), nil
		},
	}
	ctrl := newBasicTestController(dep)

	db := newTestDB(t)
	ctrl.db = db

	require.NoError(t, ctrl.db.SaveResource(context.Background(), &database.Resource{
		Name:     "res1",
		Port:     7001,
		Nodes:    "node1,node2",
		Protocol: "C",
		Replicas: 2,
	}))
	require.NoError(t, ctrl.db.SaveVolume(context.Background(), &database.Volume{
		ResourceName: "res1",
		VolumeName:   "res1_logs",
		VolumeID:     1,
		Pool:         "haify_data-pool",
		SizeGB:       20,
		Device:       "/dev/haify_data-pool/res1_logs",
	}))
	ctrl.nodes.nodes["10.0.0.1"] = &NodeInfo{Name: "node1", Address: "10.0.0.1"}
	ctrl.nodes.nodes["10.0.0.2"] = &NodeInfo{Name: "node2", Address: "10.0.0.2"}
	ctrl.hostsMap["node1"] = "10.0.0.1"
	ctrl.hostsMap["node2"] = "10.0.0.2"

	err := ctrl.resources.ResizeVolume(context.Background(), "res1", 1, 50)
	require.NoError(t, err)
	var sawLVResize, sawDRBDResize bool
	for _, call := range dep.execCalls {
		// The LV grows by DRBD's metadata too, so the device reaches 50 GiB.
		if call.cmd == fmt.Sprintf("sudo lvresize -L %dB -y /dev/haify_data-pool/res1_logs", backingVolumeSizeBytes(50, 1, false)) {
			sawLVResize = true
		}
		if call.cmd == "sudo drbdadm resize res1/1" {
			sawDRBDResize = true
		}
	}
	assert.True(t, sawLVResize)
	assert.True(t, sawDRBDResize)
	volumes, err := ctrl.db.ListVolumes(context.Background(), "res1")
	require.NoError(t, err)
	require.Len(t, volumes, 1)
	assert.Equal(t, 50, volumes[0].SizeGB)
}

func TestResourceManagerCreateFilesystemOnlySupportsBtrfs(t *testing.T) {
	dep := &fakeDeploymentClient{
		execFunc: func(ctx context.Context, hosts []string, cmd string, opts ...deployment.ExecOption) (*deployment.ExecResult, error) {
			require.Equal(t, []string{"10.0.0.1"}, hosts)
			require.Equal(t, "sudo mkfs.btrfs -f /dev/drbd/by-res/res1/0", cmd)
			return successExecResult(hosts, ""), nil
		},
	}
	ctrl := newBasicTestController(dep)
	ctrl.nodes.nodes["10.0.0.1"] = &NodeInfo{Name: "node1", Address: "10.0.0.1"}
	ctrl.hostsMap["node1"] = "10.0.0.1"

	err := ctrl.resources.CreateFilesystemOnly(context.Background(), "res1", 0, "btrfs", "node1")
	require.NoError(t, err)
	require.Len(t, dep.execCalls, 1)
}

// A diskless data client holds the config and a kernel resource too. Left
// behind, its file claimed the resource's port, and the next resource given
// that port could not come up on the node.
func TestDeleteResourceTearsDownDisklessClients(t *testing.T) {
	dep := &fakeDeploymentClient{}
	ctrl := newBasicTestController(dep)
	ctrl.db = newTestDB(t)
	require.NoError(t, ctrl.db.SaveResource(context.Background(), &database.Resource{
		Name: "res1", Port: 7004, Nodes: "node1,node2", DisklessClients: "node3", Protocol: "C", Replicas: 2,
	}))
	for i, n := range []string{"node1", "node2", "node3"} {
		addr := fmt.Sprintf("10.0.0.%d", i+1)
		ctrl.nodes.nodes[addr] = &NodeInfo{Name: n, Address: addr}
		ctrl.hostsMap[n] = addr
	}
	require.NoError(t, ctrl.resources.DeleteResource(context.Background(), "res1", false))
	require.NotEmpty(t, dep.drbdDownCalls)
	assert.Contains(t, dep.drbdDownCalls[0].hosts, "10.0.0.3", "the client is brought down")
	require.NotEmpty(t, dep.deleteConfigCalls)
	assert.Contains(t, dep.deleteConfigCalls[0].hosts, "10.0.0.3", "and its config removed")
}
