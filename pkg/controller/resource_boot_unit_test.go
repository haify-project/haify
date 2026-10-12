package controller

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/deployment"
)

// A node can join a resource without `resource create`: add-replica, an
// in-place diskless-to-diskful conversion, attach-diskless, set-tiebreaker.
// Create used to be the only path that installed haify-drbd-up.service, so a
// node that got its first resource any of those ways stayed down after a
// reboot. These tests pin every such path to installing it on the newcomer.

// bootFixture is res1 (drbdResFixture) on n1 and n2, with n3 registered and
// free to join.
type bootFixture struct {
	ctrl     *Controller
	dep      *fakeDeploymentClient
	config   string
	failUnit bool

	mu           sync.Mutex
	installHosts [][]string
}

func newBootFixture(t *testing.T, res *database.Resource) *bootFixture {
	t.Helper()
	f := &bootFixture{config: drbdResFixture}
	f.dep = &fakeDeploymentClient{}
	f.dep.execFunc = func(_ context.Context, hosts []string, cmd string, _ ...deployment.ExecOption) (*deployment.ExecResult, error) {
		switch {
		case strings.Contains(cmd, drbdBootUnitPath):
			f.mu.Lock()
			f.installHosts = append(f.installHosts, append([]string(nil), hosts...))
			f.mu.Unlock()
			if f.failUnit {
				return failedResult(hosts, "tee: /etc/systemd/system: Read-only file system"), nil
			}
		case strings.HasPrefix(cmd, "cat /etc/drbd.d/"):
			return successExecResult(hosts, f.config), nil
		case strings.Contains(cmd, "dump-md"):
			return successExecResult(hosts, "7\n"), nil
		case strings.Contains(cmd, "blockdev --getsize64"):
			return successExecResult(hosts, "1073741824\n"), nil
		case strings.Contains(cmd, "command -v cryptsetup"):
			return successExecResult(hosts, "ok"), nil
		}
		return successExecResult(hosts, ""), nil
	}
	f.ctrl = newBasicTestController(f.dep)
	f.ctrl.db = newTestDB(t)
	registerNodes(f.ctrl, map[string]string{"n1": "10.0.0.1", "n2": "10.0.0.2", "n3": "10.0.0.3"})
	if res.Port == 0 {
		res.Port = 7001
	}
	require.NoError(t, f.ctrl.db.SaveResource(context.Background(), res))
	require.NoError(t, f.ctrl.db.SaveVolume(context.Background(), &database.Volume{
		ResourceName: res.Name, VolumeID: 0, Pool: "vg0", VolumeName: "res1_data", SizeGB: 1,
	}))
	return f
}

// installedOn reports whether the boot unit install ran with host among its
// targets.
func (f *bootFixture) installedOn(host string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, hosts := range f.installHosts {
		for _, h := range hosts {
			if h == host {
				return true
			}
		}
	}
	return false
}

func TestAddReplicaInstallsTheBootUnitOnTheNewReplica(t *testing.T) {
	f := newBootFixture(t, &database.Resource{Name: "res1", Nodes: "n1,n2"})

	require.NoError(t, f.ctrl.resources.AddReplicaWith(context.Background(), "res1", "n3",
		AddReplicaOpts{IgnoreFreeSpace: true}))

	assert.True(t, f.installedOn("10.0.0.3"), "the new replica must get the boot unit")
	got, err := f.ctrl.db.GetResource(context.Background(), "res1")
	require.NoError(t, err)
	assert.Equal(t, "n1,n2,n3", got.Nodes)
}

// A plaintext replica that is up is worth keeping even when the unit could
// not be installed; an encrypted one is not, because the unit is what opens
// its container at boot. The failed add is rolled back.
func TestAddReplicaFailsOnlyForAnEncryptedResourceWhenTheBootUnitFails(t *testing.T) {
	f := newBootFixture(t, &database.Resource{Name: "res1", Nodes: "n1,n2"})
	f.failUnit = true
	require.NoError(t, f.ctrl.resources.AddReplicaWith(context.Background(), "res1", "n3",
		AddReplicaOpts{IgnoreFreeSpace: true}))

	f = newBootFixture(t, &database.Resource{Name: "res1", Nodes: "n1,n2", Encrypted: true})
	f.failUnit = true
	err := f.ctrl.resources.AddReplicaWith(context.Background(), "res1", "n3",
		AddReplicaOpts{IgnoreFreeSpace: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "opens the LUKS containers")
	assert.NotEqual(t, -1, execIndexOf(f.dep, "drbdadm down res1"), "the new replica is torn down again")
	got, gerr := f.ctrl.db.GetResource(context.Background(), "res1")
	require.NoError(t, gerr)
	assert.Equal(t, "n1,n2", got.Nodes, "the record still describes the old membership")
}

// A diskless member that is given a disk may never have had the unit: it
// joined by attach-diskless or set-tiebreaker.
func TestConvertingADisklessClientInstallsTheBootUnit(t *testing.T) {
	f := newBootFixture(t, &database.Resource{Name: "res1", Nodes: "n1,n2", DisklessClients: "n3"})
	withClient, err := addDisklessClientBlock(drbdResFixture, "n3", "10.0.0.3", 7001)
	require.NoError(t, err)
	f.config = withClient

	require.NoError(t, f.ctrl.resources.AddReplicaWith(context.Background(), "res1", "n3",
		AddReplicaOpts{IgnoreFreeSpace: true}))

	assert.True(t, f.installedOn("10.0.0.3"))
	got, err := f.ctrl.db.GetResource(context.Background(), "res1")
	require.NoError(t, err)
	assert.Equal(t, "n1,n2,n3", got.Nodes)
	assert.Empty(t, got.DisklessClients)
}

// The DR site is usually a node holding nothing else of the cluster.
func TestAddDRInstallsTheBootUnitOnTheDRNode(t *testing.T) {
	withTempPKI(t)
	f := newBootFixture(t, &database.Resource{Name: "res1", Nodes: "n1,n2"})

	require.NoError(t, f.ctrl.resources.AddDR(context.Background(), "res1", "n3", "203.0.113.7", 47000, ""))

	assert.True(t, f.installedOn("10.0.0.3"))
	got, err := f.ctrl.db.GetResource(context.Background(), "res1")
	require.NoError(t, err)
	assert.Equal(t, "n3", got.DRNode)
}

func TestAttachDisklessClientInstallsTheBootUnit(t *testing.T) {
	f := newBootFixture(t, &database.Resource{Name: "res1", Nodes: "n1,n2"})

	require.NoError(t, f.ctrl.resources.AttachDisklessClient(context.Background(), "res1", "n3"))

	assert.True(t, f.installedOn("10.0.0.3"), "the client must get the boot unit")
	assert.False(t, f.installedOn("10.0.0.1"), "the replicas already have it from create")
}

// A client has no container to open, so a unit that cannot be installed does
// not undo the attach.
func TestAttachDisklessClientToleratesAMissingBootUnit(t *testing.T) {
	f := newBootFixture(t, &database.Resource{Name: "res1", Nodes: "n1,n2"})
	f.failUnit = true

	require.NoError(t, f.ctrl.resources.AttachDisklessClient(context.Background(), "res1", "n3"))

	got, err := f.ctrl.db.GetResource(context.Background(), "res1")
	require.NoError(t, err)
	assert.Equal(t, "n3", got.DisklessClients)
}

func TestTiebreakerBecomingAClientInstallsTheBootUnit(t *testing.T) {
	f := newBootFixture(t, &database.Resource{Name: "res1", Nodes: "n1,n2", DisklessNodes: "n3"})

	require.NoError(t, f.ctrl.resources.AttachDisklessClient(context.Background(), "res1", "n3"))

	assert.True(t, f.installedOn("10.0.0.3"))
}

func TestSetTiebreakerInstallsTheBootUnitOnTheNewTiebreaker(t *testing.T) {
	f := newBootFixture(t, &database.Resource{Name: "res1", Nodes: "n1,n2"})

	require.NoError(t, f.ctrl.resources.SetTiebreaker(context.Background(), "res1", "n3"))

	assert.True(t, f.installedOn("10.0.0.3"))
	got, err := f.ctrl.db.GetResource(context.Background(), "res1")
	require.NoError(t, err)
	assert.Equal(t, "n3", got.DisklessNodes)
}

// Repair heals clusters whose members joined before those paths installed
// the unit: every participant gets it, the tiebreaker included.
func TestRepairInstallsTheBootUnitOnEveryParticipant(t *testing.T) {
	f := newBootFixture(t, &database.Resource{Name: "res1", Nodes: "n1,n2", DisklessNodes: "n3"})
	withTiebreaker, err := addDisklessClientBlock(drbdResFixture, "n3", "10.0.0.3", 7001)
	require.NoError(t, err)
	f.config = withTiebreaker

	require.NoError(t, f.ctrl.resources.RepairResourceConfig(context.Background(), "res1"))

	for _, h := range []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"} {
		assert.True(t, f.installedOn(h), "repair must install the boot unit on %s", h)
	}
}

func TestRepairReportsAMissingBootUnitOnlyForAnEncryptedResource(t *testing.T) {
	f := newBootFixture(t, &database.Resource{Name: "res1", Nodes: "n1,n2"})
	f.failUnit = true
	require.NoError(t, f.ctrl.resources.RepairResourceConfig(context.Background(), "res1"))

	f = newBootFixture(t, &database.Resource{Name: "res1", Nodes: "n1,n2", Encrypted: true})
	f.failUnit = true
	err := f.ctrl.resources.RepairResourceConfig(context.Background(), "res1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "opens the LUKS containers")
}

// Installing the unit is the same command every time, so a node that joins a
// second resource, or is repaired twice, ends up exactly where it was.
func TestBootUnitInstallIsTheSameEveryTime(t *testing.T) {
	cmd := drbdBootUnitInstallCmd()
	assert.Equal(t, cmd, drbdBootUnitInstallCmd())
	assert.Contains(t, cmd, "sudo tee "+drbdBootUnitPath)
	assert.Contains(t, cmd, "systemctl enable "+drbdBootUnit)
	assert.Contains(t, cmd, "sh-resources", "the unit brings up whatever the node holds at boot, not a fixed list")
	assert.NotContains(t, cmd, ">>", "nothing is appended, so a second install does not grow a file")
}
