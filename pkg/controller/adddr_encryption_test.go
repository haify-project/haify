package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/deployment"
)

// add-dr on an encrypted resource used to give the DR node a plain LV as its
// DRBD disk, so the off-site copy was the one plaintext copy of the data.
// These tests pin the DR to what add-replica does for a local replica.

const drContainer = "haify_vg0_res1_data"

// drConfig is the last resource config add-dr distributed, the one that
// carries the DR's stanza.
func drConfig(t *testing.T, dep *fakeDeploymentClient) string {
	t.Helper()
	for i := len(dep.distributedConfigs) - 1; i >= 0; i-- {
		if strings.HasSuffix(dep.distributedConfigs[i].remotePath, "/res1.res") {
			return dep.distributedConfigs[i].content
		}
	}
	t.Fatal("add-dr distributed no resource config")
	return ""
}

// execOn returns the index of the first command on host containing substr.
func execOn(dep *fakeDeploymentClient, host, substr string) int {
	for i, c := range dep.execCalls {
		if strings.Contains(c.cmd, substr) && len(c.hosts) == 1 && c.hosts[0] == host {
			return i
		}
	}
	return -1
}

func TestAddDROnAnEncryptedResourceEncryptsTheDRCopy(t *testing.T) {
	withTempPKI(t)
	f := newBootFixture(t, &database.Resource{Name: "res1", Nodes: "n1,n2", Encrypted: true})

	require.NoError(t, f.ctrl.resources.AddDR(context.Background(), "res1", "n3", "203.0.113.7", 47000, ""))

	dep := f.dep
	assert.NotEqual(t, -1, execOn(dep, "10.0.0.3", "command -v cryptsetup"),
		"the DR node is checked for cryptsetup and dm-crypt before anything is built")

	format := execOn(dep, "10.0.0.3", "luksFormat")
	require.NotEqual(t, -1, format, "the DR's LV is formatted as a LUKS container on the DR node")
	provision := dep.execCalls[format].cmd
	assert.Contains(t, provision, "/dev/vg0/res1_data", "the container goes on the DR's own LV")
	assert.Contains(t, provision, "tee "+luksDevPointer(drContainer),
		"the pointer the boot unit opens containers from is written on the DR")
	assert.Contains(t, provision, "cryptsetup open --type luks --key-file "+luksKeyPath(drContainer))
	assert.Contains(t, provision, "of="+luksKeyPath(drContainer), "the key is generated on the DR node")

	createMD := execOn(dep, "10.0.0.3", "create-md")
	require.NotEqual(t, -1, createMD)
	assert.Less(t, format, createMD, "DRBD metadata goes inside the open container")

	cfg := drConfig(t, dep)
	assert.Equal(t, 1, strings.Count(cfg, "disk      "+luksMapperPath("vg0", "res1_data")+";"),
		"the DR stanza names the container, not the LV")
	assert.NotContains(t, cfg, "disk      /dev/vg0/res1_data;")

	// The primaries report their container's size, which is the LV less the
	// header; the DR's LV has to carry a header of its own on top.
	require.Len(t, dep.lvCreateCalls, 1)
	assert.Equal(t, fmt.Sprintf("%dB", uint64(1073741824)+luksHeaderBytes), dep.lvCreateCalls[0].size)

	assert.True(t, f.installedOn("10.0.0.3"), "the boot unit that opens the container at boot is on the DR")
	got, err := f.ctrl.db.GetResource(context.Background(), "res1")
	require.NoError(t, err)
	assert.Equal(t, "n3", got.DRNode)
}

func TestAddDROnAPlaintextResourceBuildsNoContainer(t *testing.T) {
	withTempPKI(t)
	f := newBootFixture(t, &database.Resource{Name: "res1", Nodes: "n1,n2"})

	require.NoError(t, f.ctrl.resources.AddDR(context.Background(), "res1", "n3", "203.0.113.7", 47000, ""))

	assert.Equal(t, -1, execIndexOf(f.dep, "luksFormat"), "no container is built")
	assert.Equal(t, -1, execIndexOf(f.dep, "command -v cryptsetup"), "no node is probed for encryption")
	cfg := drConfig(t, f.dep)
	assert.Contains(t, cfg, "disk      /dev/vg0/res1_data;")
	assert.NotContains(t, cfg, "/dev/mapper/")
	require.Len(t, f.dep.lvCreateCalls, 1)
	assert.Equal(t, "1073741824B", f.dep.lvCreateCalls[0].size)
}

// A DR node that cannot hold a container is refused before any storage,
// proxy or config is touched there.
func TestAddDRRefusesADRNodeWithoutCryptsetup(t *testing.T) {
	withTempPKI(t)
	f := newBootFixture(t, &database.Resource{Name: "res1", Nodes: "n1,n2", Encrypted: true})
	base := f.dep.execFunc
	f.dep.execFunc = func(ctx context.Context, hosts []string, cmd string, opts ...deployment.ExecOption) (*deployment.ExecResult, error) {
		if strings.Contains(cmd, "command -v cryptsetup") && hosts[0] == "10.0.0.3" {
			return successExecResult(hosts, "nocryptsetup"), nil
		}
		return base(ctx, hosts, cmd, opts...)
	}

	err := f.ctrl.resources.AddDR(context.Background(), "res1", "n3", "203.0.113.7", 47000, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no cryptsetup")
	assert.Empty(t, f.dep.lvCreateCalls, "no volume is made on the DR")
	assert.Empty(t, f.dep.distributedConfigs, "no config changes anywhere")
}

// A failed add-dr on an encrypted resource closes the DR's container and
// destroys its key before the LV goes, as a failed add-replica does.
func TestAddDRRollbackClosesTheDRContainer(t *testing.T) {
	withTempPKI(t)
	f := newBootFixture(t, &database.Resource{Name: "res1", Nodes: "n1,n2", Encrypted: true})
	base := f.dep.execFunc
	f.dep.execFunc = func(ctx context.Context, hosts []string, cmd string, opts ...deployment.ExecOption) (*deployment.ExecResult, error) {
		if strings.Contains(cmd, "create-md") {
			return failedResult(hosts, "create-md: no space"), nil
		}
		return base(ctx, hosts, cmd, opts...)
	}

	err := f.ctrl.resources.AddDR(context.Background(), "res1", "n3", "203.0.113.7", 47000, "")
	require.Error(t, err)

	closeIdx := execOn(f.dep, "10.0.0.3", "cryptsetup close "+drContainer)
	removeIdx := execOn(f.dep, "10.0.0.3", "lvremove -f vg0/res1_data")
	require.NotEqual(t, -1, closeIdx, "the DR's container is closed")
	require.NotEqual(t, -1, removeIdx, "the DR's LV is removed")
	assert.Less(t, closeIdx, removeIdx, "the open container would hold the LV")
	assert.Contains(t, f.dep.execCalls[closeIdx].cmd, "shred -u "+luksKeyPath(drContainer))

	got, gerr := f.ctrl.db.GetResource(context.Background(), "res1")
	require.NoError(t, gerr)
	assert.False(t, got.WANMode, "the record still describes a LAN resource")
}

func TestAddDRToConfigNamesTheContainerOnlyForTheDR(t *testing.T) {
	rm := addDRTestFixture(t)

	out, err := rm.addDRToConfig(lanResConfig, "openclaw", "node-c",
		[]string{"node-a", "node-b"}, []string{"node-e"}, "203.0.113.7", addDRVolumes, 7300, nil, true)
	require.NoError(t, err)

	drStanza := out[strings.Index(out, "on haify-c {"):]
	drStanza = drStanza[:strings.Index(drStanza, "\n    }")]
	assert.Contains(t, drStanza, "disk      "+luksMapperPath("vg0", "openclaw_data")+";")
	// The primaries' stanzas are what they were; add-dr does not rewrite them.
	assert.Equal(t, 2, strings.Count(out, "disk      /dev/vg0/openclaw_data;"))
}

func TestReplicaBackingSizesAddsTheHeaderOnlyWhenEncrypted(t *testing.T) {
	sizes := []uint64{1 << 30, 2 << 30}
	assert.Equal(t, sizes, replicaBackingSizes(sizes, false))
	assert.Equal(t, []uint64{1<<30 + luksHeaderBytes, 2<<30 + luksHeaderBytes}, replicaBackingSizes(sizes, true))
	assert.Equal(t, []uint64{1 << 30, 2 << 30}, sizes, "the input is not modified")
}

// Converting a pool to thin rebuilds the node's copy. On an encrypted resource
// the rebuilt copy is a container again: the old one is closed so its LV can
// go, and the new thin LV is wrapped before DRBD attaches.
func TestThinConversionRebuildsAnEncryptedCopyAsAContainer(t *testing.T) {
	var order []string
	dep := &fakeDeploymentClient{
		lvRemoveFunc: func(_ context.Context, hosts []string, _ string) (*deployment.ExecResult, error) {
			order = append(order, "lvremove")
			return successExecResult(hosts, ""), nil
		},
		lvCreateThinVolumeFunc: func(_ context.Context, hosts []string, _, _, _, _ string) (*deployment.ExecResult, error) {
			order = append(order, "lvcreate")
			return successExecResult(hosts, ""), nil
		},
		drbdCreateMDFunc: func(_ context.Context, hosts []string, _ string) (*deployment.ExecResult, error) {
			order = append(order, "create-md")
			return successExecResult(hosts, ""), nil
		},
	}
	dep.execFunc = func(_ context.Context, hosts []string, cmd string, _ ...deployment.ExecOption) (*deployment.ExecResult, error) {
		switch {
		case strings.Contains(cmd, "cryptsetup close"):
			order = append(order, "close")
		case strings.Contains(cmd, "luksFormat"):
			order = append(order, "luksFormat")
		}
		return successExecResult(hosts, ""), nil
	}
	plan := testConversionPlan()
	plan.Volumes[0].Encrypted = true

	require.NoError(t, convertTestManager(dep).applyThinConversion(context.Background(), "10.0.0.1", plan))
	assert.Equal(t, []string{"close", "lvremove", "lvcreate", "luksFormat", "create-md"}, order)
	provision := dep.execCalls[execIndexOf(dep, "luksFormat")].cmd
	assert.Contains(t, provision, "/dev/haify_haifypool/openclaw_data")
	assert.Contains(t, provision, luksDevPointer(luksContainerName("haify_haifypool", "openclaw_data")))
}

func TestThinConversionOfAPlaintextCopyRunsNoCryptsetup(t *testing.T) {
	dep := &fakeDeploymentClient{}
	require.NoError(t, convertTestManager(dep).applyThinConversion(context.Background(), "10.0.0.1", testConversionPlan()))
	assert.Equal(t, -1, execIndexOf(dep, "cryptsetup"))
}

func TestThinConversionPlanCarriesTheEncryptionFlag(t *testing.T) {
	in := thickPool(t, "node-e")
	in.Resources[0].Volumes[0].Encrypted = true
	plan, err := planThinConversion(in)
	require.NoError(t, err)
	require.Len(t, plan.Volumes, 1)
	assert.True(t, plan.Volumes[0].Encrypted)
}
