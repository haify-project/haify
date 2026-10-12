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

// A LAN resource as it exists on disk before any DR is attached: two diskful
// replicas plus a diskless tiebreaker, wired by connection-mesh.
const lanResConfig = `resource openclaw {
    protocol C;

    options {
        quorum majority;
        on-no-quorum io-error;
    }

    on haify-a {
        address   192.168.1.10:7300;
        node-id   0;
        volume 0 {
            device    minor 12;
            disk      /dev/vg0/openclaw_data;
            meta-disk internal;
        }
    }

    on haify-b {
        address   192.168.1.11:7300;
        node-id   1;
        volume 0 {
            device    minor 12;
            disk      /dev/vg0/openclaw_data;
            meta-disk internal;
        }
    }

    on haify-e {
        address   192.168.1.20:7300;
        node-id   2;
        volume 0 {
            device    minor 12;
            disk      none;
            meta-disk internal;
        }
    }

    connection-mesh {
        hosts haify-a haify-b haify-e;
    }
}
`

func addDRTestFixture(t *testing.T) *ResourceManager {
	t.Helper()
	ctrl := newBasicTestController(&fakeDeploymentClient{})
	for _, n := range []struct{ name, addr, host string }{
		{"node-a", "192.168.1.10", "haify-a"},
		{"node-b", "192.168.1.11", "haify-b"},
		{"node-e", "192.168.1.20", "haify-e"},
		{"node-c", "203.0.113.7", "haify-c"},
	} {
		ctrl.nodes.nodes[n.addr] = &NodeInfo{
			Name: n.name, Address: n.addr, Hostname: n.host, State: NodeStateOnline,
		}
		ctrl.hostsMap[n.name] = n.addr
	}
	return ctrl.resources
}

var addDRVolumes = []*database.Volume{
	{VolumeID: 0, VolumeName: "openclaw_data", Pool: "vg0", SizeGB: 20},
}

// Attaching a DR must leave the primary site's replication untouched — its
// addresses, its synchronous protocol, and its minors are what the running
// resource is already using, and changing any of them mid-flight would drop the
// connection or, worse, point a replica at the wrong device.
func TestAddDRToConfigPreservesPrimarySite(t *testing.T) {
	rm := addDRTestFixture(t)

	out, err := rm.addDRToConfig(lanResConfig, "openclaw", "node-c",
		[]string{"node-a", "node-b"}, []string{"node-e"}, "203.0.113.7", addDRVolumes, 7300, nil, false)
	require.NoError(t, err)

	assert.Contains(t, out, "address   192.168.1.10:7300;")
	assert.Contains(t, out, "address   192.168.1.11:7300;")
	assert.Contains(t, out, "protocol C;", "the LAN stays synchronous")
	assert.Contains(t, out, "quorum majority;", "existing options survive the rewrite")
	// Every replica of a resource shares one minor; the DR must reuse it, not
	// invent one.
	assert.Equal(t, 4, strings.Count(out, "device    minor 12;"))
}

// The DR joins as a full replica with its own node-id and backing volume, on a
// loopback address because its only path in is the proxy leg.
func TestAddDRToConfigAddsDRStanza(t *testing.T) {
	rm := addDRTestFixture(t)

	out, err := rm.addDRToConfig(lanResConfig, "openclaw", "node-c",
		[]string{"node-a", "node-b"}, []string{"node-e"}, "203.0.113.7", addDRVolumes, 7300, nil, false)
	require.NoError(t, err)

	assert.Contains(t, out, "on haify-c {")
	assert.Contains(t, out, "node-id   3;", "next free id after 0,1,2")
	assert.Contains(t, out, "disk      /dev/vg0/openclaw_data;")
	// The DR's own stanza binds the leg port; only the connection sections use
	// the +100 offset, and only for the primaries.
	drStanza := out[strings.Index(out, "on haify-c {"):]
	drStanza = drStanza[:strings.Index(drStanza, "\n    }")]
	assert.Contains(t, drStanza, "address   127.0.0.1:7300;")
}

// A mesh entry for the DR would pair it with each replica on that replica's LAN
// address, which is unroutable from the other site. The mesh therefore has to
// shrink to the primary site and the WAN legs be spelled out explicitly.
func TestAddDRToConfigNarrowsMeshAndAddsLegs(t *testing.T) {
	rm := addDRTestFixture(t)

	out, err := rm.addDRToConfig(lanResConfig, "openclaw", "node-c",
		[]string{"node-a", "node-b"}, []string{"node-e"}, "203.0.113.7", addDRVolumes, 7300, nil, false)
	require.NoError(t, err)

	require.Equal(t, 1, strings.Count(out, "connection-mesh"))
	mesh := out[strings.Index(out, "connection-mesh"):]
	mesh = mesh[:strings.Index(mesh, ";")]
	assert.Contains(t, mesh, "haify-a")
	assert.Contains(t, mesh, "haify-b")
	assert.Contains(t, mesh, "haify-e", "the tiebreaker still votes over the LAN")
	assert.NotContains(t, mesh, "haify-c", "the DR must not join the LAN mesh")

	// One leg per diskful replica: DRBD 9 is a full mesh, so whichever replica
	// is Primary after a local failover must have a path to the DR.
	assert.Equal(t, 2, strings.Count(out, "connection {"))
	assert.Contains(t, out, "host haify-a address 127.0.0.1:7400;")
	assert.Contains(t, out, "host haify-c address 127.0.0.1:7300;")
	assert.Contains(t, out, "host haify-b address 127.0.0.1:7401;")
	assert.Contains(t, out, "host haify-c address 127.0.0.1:7301;")

	// Each leg is async and yields under congestion, so a saturated or flapping
	// WAN link cannot stall writes in the primary site.
	assert.Equal(t, 2, strings.Count(out, "protocol A;"))
	assert.Equal(t, 2, strings.Count(out, "on-congestion pull-ahead;"))
	assert.Equal(t, 2, strings.Count(out, "csums-alg sha256;"), "every WAN leg resyncs by checksum")

	// No tiebreaker leg: a diskless voter reachable only over the WAN would make
	// quorum depend on the link.
	assert.NotContains(t, out, "host haify-e address 127.0.0.1")
}

// The port layout must match what generateDrbdConfig produces for a resource
// created two-site from the start, or the two paths diverge and only one works.
func TestAddDRToConfigMatchesCreateTimeLayout(t *testing.T) {
	rm := addDRTestFixture(t)

	added, err := rm.addDRToConfig(lanResConfig, "openclaw", "node-c",
		[]string{"node-a", "node-b"}, nil, "203.0.113.7", addDRVolumes, 7300, nil, false)
	require.NoError(t, err)

	created := rm.generateDrbdConfig(
		"openclaw", 7300,
		[]resolvedVolume{{id: 0, volumeName: "openclaw_data", pool: "vg0", minor: 12, sizeGB: 20}},
		[]string{"node-a", "node-b", "node-c"}, nil,
		"C", "lvm", nil,
		&wanConfig{DRNode: "node-c", PrimaryNodes: []string{"node-a", "node-b"}},
	)

	for _, line := range []string{
		"host haify-a address 127.0.0.1:7400;",
		"host haify-b address 127.0.0.1:7401;",
		"host haify-c address 127.0.0.1:7300;",
		"host haify-c address 127.0.0.1:7301;",
	} {
		assert.Contains(t, created, line, "create-time config")
		assert.Contains(t, added, line, "add-dr config")
	}
}

// Re-running add-dr must not silently produce a config with the DR twice.
func TestAddDRToConfigRejectsDuplicateNode(t *testing.T) {
	rm := addDRTestFixture(t)

	once, err := rm.addDRToConfig(lanResConfig, "openclaw", "node-c",
		[]string{"node-a", "node-b"}, []string{"node-e"}, "203.0.113.7", addDRVolumes, 7300, nil, false)
	require.NoError(t, err)

	_, err = rm.addDRToConfig(once, "openclaw", "node-c",
		[]string{"node-a", "node-b"}, []string{"node-e"}, "203.0.113.7", addDRVolumes, 7300, nil, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already in the config")
}

// A resource with no mesh (two nodes wired implicitly) must still gain its legs.
func TestAddDRToConfigWithoutExistingMesh(t *testing.T) {
	rm := addDRTestFixture(t)
	noMesh := lanResConfig[:strings.Index(lanResConfig, "    connection-mesh")] + "}\n"

	out, err := rm.addDRToConfig(noMesh, "openclaw", "node-c",
		[]string{"node-a", "node-b"}, []string{"node-e"}, "203.0.113.7", addDRVolumes, 7300, nil, false)
	require.NoError(t, err)

	assert.Contains(t, out, "on haify-c {")
	assert.Equal(t, 2, strings.Count(out, "connection {"))
	// The primary site is now spelled out, since the DR's arrival means the
	// implicit all-pairs reading of the file would include it.
	assert.Contains(t, out, "hosts haify-a haify-b haify-e;")
}

func TestMinorForVolume(t *testing.T) {
	assert.Equal(t, 12, minorForVolume(lanResConfig, 0))

	twoVol := `resource r {
    on haify-a {
        volume 0 {
            device    minor 5;
        }
        volume 1 {
            device    minor 6;
        }
    }
}
`
	assert.Equal(t, 5, minorForVolume(twoVol, 0))
	assert.Equal(t, 6, minorForVolume(twoVol, 1))
}

// The whole point of the preflight is that DRBD hands out bitmap slots once, at
// create-md time. A resource created with two nodes has room for exactly one
// diskful peer, so a DR is the node that does not fit.
func TestAssertBitmapSlotFreeRejectsExhaustedMetadata(t *testing.T) {
	fake := &fakeDeploymentClient{}
	fake.execFunc = func(ctx context.Context, hosts []string, cmd string, opts ...deployment.ExecOption) (*deployment.ExecResult, error) {
		return successExecResult(hosts, "1\n"), nil
	}
	rm := newBasicTestController(fake).resources

	err := rm.assertBitmapSlotFree(context.Background(),
		[]string{"192.168.1.10", "192.168.1.11"}, []string{"node-a", "node-b"}, "openclaw")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "metadata for 1 peer(s) but 2 are needed")
	// The message has to carry the way out: there is no online fix, and an
	// operator who does not know that will go looking for a flag that does not
	// exist.
	assert.Contains(t, err.Error(), "--max-peers")
	assert.Contains(t, err.Error(), "one node at a time")
}

func TestAssertBitmapSlotFreeAcceptsHeadroom(t *testing.T) {
	fake := &fakeDeploymentClient{}
	fake.execFunc = func(ctx context.Context, hosts []string, cmd string, opts ...deployment.ExecOption) (*deployment.ExecResult, error) {
		return successExecResult(hosts, "7\n"), nil
	}
	rm := newBasicTestController(fake).resources

	require.NoError(t, rm.assertBitmapSlotFree(context.Background(),
		[]string{"192.168.1.10", "192.168.1.11"}, []string{"node-a", "node-b"}, "openclaw"))
}

// A multi-volume resource is only as extensible as its tightest volume.
func TestAssertBitmapSlotFreeUsesLowestVolume(t *testing.T) {
	fake := &fakeDeploymentClient{}
	fake.execFunc = func(ctx context.Context, hosts []string, cmd string, opts ...deployment.ExecOption) (*deployment.ExecResult, error) {
		return successExecResult(hosts, "7\n1\n7\n"), nil
	}
	rm := newBasicTestController(fake).resources

	err := rm.assertBitmapSlotFree(context.Background(),
		[]string{"192.168.1.10", "192.168.1.11"}, []string{"node-a", "node-b"}, "openclaw")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "metadata for 1 peer(s)")
}

// Metadata that cannot be read is not evidence of a problem; refusing on it
// would block the operation on any node whose drbdmeta behaves differently.
func TestAssertBitmapSlotFreeProceedsOnUnreadableMetadata(t *testing.T) {
	fake := &fakeDeploymentClient{}
	fake.execFunc = func(ctx context.Context, hosts []string, cmd string, opts ...deployment.ExecOption) (*deployment.ExecResult, error) {
		return successExecResult(hosts, "open failed: Device or resource busy\n"), nil
	}
	rm := newBasicTestController(fake).resources

	require.NoError(t, rm.assertBitmapSlotFree(context.Background(),
		[]string{"192.168.1.10"}, []string{"node-a"}, "openclaw"))
}

// Metadata must be created with room to grow. DRBD fixes the bitmap-slot count
// at create-md time, so sizing it to today's peer count means the first node
// added later cannot join — the exact failure add-dr exists to avoid. The
// backing volume is already sized for minMetadataPeers slots, so the headroom
// costs nothing that has not already been paid for.
func TestCreateResourceReservesBitmapHeadroom(t *testing.T) {
	fake := &fakeDeploymentClient{}
	ctrl := newBasicTestController(fake)
	registerWANNodes(ctrl)

	_ = ctrl.resources.CreateResource(context.Background(), "data", 7300,
		[]string{"node-a", "node-b"}, "C", 1, "vg0", "lvm", nil)

	require.NotEmpty(t, fake.drbdCreateMDCalls, "create should have made metadata")
	assert.Equal(t, minMetadataPeers, fake.drbdCreateMDCalls[0].maxPeers,
		"a two-node resource must still leave slots for a DR or a third replica")
}

// A fixed bind offset collides across resources: a resource on 7300 binds 7400,
// which is the leg port of a resource on 7400. The probed port has to win.
func TestAddDRToConfigHonoursProbedBindPorts(t *testing.T) {
	rm := addDRTestFixture(t)

	out, err := rm.addDRToConfig(lanResConfig, "openclaw", "node-c",
		[]string{"node-a", "node-b"}, []string{"node-e"}, "203.0.113.7", addDRVolumes, 7300,
		[]int{7900, 7901}, false)
	require.NoError(t, err)

	assert.Contains(t, out, "host haify-a address 127.0.0.1:7900;")
	assert.Contains(t, out, "host haify-b address 127.0.0.1:7901;")
	assert.NotContains(t, out, "127.0.0.1:7400", "the colliding default must not survive")
	// The DR side is untouched: it binds the leg port, where its acceptor dials.
	assert.Contains(t, out, "host haify-c address 127.0.0.1:7300;")
	assert.Contains(t, out, "host haify-c address 127.0.0.1:7301;")
}

// Same requirement on the create-time path, which generates the identical shape.
func TestGenerateDrbdConfigHonoursProbedBindPorts(t *testing.T) {
	ctrl := newBasicTestController(&fakeDeploymentClient{})
	registerWANNodes(ctrl)

	cfg := ctrl.resources.generateDrbdConfig(
		"data", 7300,
		[]resolvedVolume{{id: 0, volumeName: "data_data", pool: "vg0", minor: 0, sizeGB: 1}},
		[]string{"node-a", "node-b", "node-dr"}, nil,
		"C", "lvm", nil,
		&wanConfig{DRNode: "node-dr", PrimaryNodes: []string{"node-a", "node-b"}, BindPorts: []int{7900, 7901}},
	)

	assert.Contains(t, cfg, "host haify-a address 127.0.0.1:7900;")
	assert.Contains(t, cfg, "host haify-b address 127.0.0.1:7901;")
	assert.NotContains(t, cfg, "127.0.0.1:7400")
}

// The DR must be built to the size the primaries actually export. Metadata is
// carved out of the same device, so a replica whose volume was extended exports
// more than its recorded size — and DRBD refuses a peer that is even one sector
// short.
func TestPrimaryBackingSizesTakesLargest(t *testing.T) {
	fake := &fakeDeploymentClient{}
	byHost := map[string]string{
		"192.168.1.10": "3233808384\n",
		"192.168.1.11": "3225419776\n",
	}
	fake.execFunc = func(ctx context.Context, hosts []string, cmd string, opts ...deployment.ExecOption) (*deployment.ExecResult, error) {
		return successExecResult(hosts, byHost[hosts[0]]), nil
	}
	rm := newBasicTestController(fake).resources

	sizes, err := rm.primaryBackingSizes(context.Background(),
		[]string{"192.168.1.10", "192.168.1.11"}, "openclaw", 1)
	require.NoError(t, err)
	assert.Equal(t, []uint64{3233808384}, sizes)
}

func TestPrimaryBackingSizesFailsWhenUnreadable(t *testing.T) {
	fake := &fakeDeploymentClient{}
	fake.execFunc = func(ctx context.Context, hosts []string, cmd string, opts ...deployment.ExecOption) (*deployment.ExecResult, error) {
		return successExecResult(hosts, ""), nil
	}
	rm := newBasicTestController(fake).resources

	_, err := rm.primaryBackingSizes(context.Background(), []string{"192.168.1.10"}, "openclaw", 1)
	require.Error(t, err)
}

func TestPickWANBindPortsUsesProbeAndFallsBack(t *testing.T) {
	fake := &fakeDeploymentClient{}
	fake.execFunc = func(ctx context.Context, hosts []string, cmd string, opts ...deployment.ExecOption) (*deployment.ExecResult, error) {
		if hosts[0] == "192.168.1.10" {
			return successExecResult(hosts, "7405\n"), nil
		}
		return successExecResult(hosts, ""), nil // nothing free reported
	}
	rm := newBasicTestController(fake).resources

	ports, err := rm.pickWANBindPorts(context.Background(),
		[]string{"192.168.1.10", "192.168.1.11"}, 7300)
	require.NoError(t, err)
	assert.Equal(t, 7405, ports[0], "the probe's answer wins")
	// An unusable probe result must not abort the operation; the plain offset is
	// what the code did before the probe existed and is right when nothing else
	// holds the port.
	assert.Equal(t, 7401, ports[1], "falls back to offset + leg index")
}

// Adding or removing a diskless node must not drag the DR into the LAN mesh.
// The tiebreaker code predates two-site resources and rebuilt the mesh from
// every `on` stanza, which would pair the DR with each replica on that replica's
// LAN address — unroutable from the other site, so both WAN legs drop.
func TestDisklessMeshRebuildExcludesWANHosts(t *testing.T) {
	rm := addDRTestFixture(t)
	twoSite, err := rm.addDRToConfig(lanResConfig, "openclaw", "node-c",
		[]string{"node-a", "node-b"}, []string{"node-e"}, "203.0.113.7", addDRVolumes, 7300,
		[]int{7900, 7901}, false)
	require.NoError(t, err)

	out, err := removeDisklessClientBlock(twoSite, "haify-e")
	require.NoError(t, err)

	require.Equal(t, 1, strings.Count(out, "connection-mesh"))
	mesh := out[strings.Index(out, "connection-mesh"):]
	mesh = mesh[:strings.Index(mesh, ";")]
	assert.Contains(t, mesh, "haify-a")
	assert.Contains(t, mesh, "haify-b")
	assert.NotContains(t, mesh, "haify-e", "the removed tiebreaker is gone")
	assert.NotContains(t, mesh, "haify-c", "the DR must stay out of the LAN mesh")

	// And the WAN legs survive untouched.
	assert.Equal(t, 2, strings.Count(out, "connection {"))
	assert.Contains(t, out, "host haify-a address 127.0.0.1:7900;")
	assert.Contains(t, out, "host haify-c address 127.0.0.1:7301;")
}

func TestDisklessAddExcludesWANHostsFromMesh(t *testing.T) {
	rm := addDRTestFixture(t)
	// Two replicas plus a DR, no tiebreaker yet.
	noTB := lanResConfig[:strings.Index(lanResConfig, "    on haify-e {")] +
		lanResConfig[strings.Index(lanResConfig, "    connection-mesh"):]
	noTB = strings.Replace(noTB, "hosts haify-a haify-b haify-e;", "hosts haify-a haify-b;", 1)
	twoSite, err := rm.addDRToConfig(noTB, "openclaw", "node-c",
		[]string{"node-a", "node-b"}, nil, "203.0.113.7", addDRVolumes, 7300, nil, false)
	require.NoError(t, err)

	out, err := addDisklessClientBlock(twoSite, "haify-e", "192.168.1.20", 7300)
	require.NoError(t, err)

	mesh := out[strings.Index(out, "connection-mesh"):]
	mesh = mesh[:strings.Index(mesh, ";")]
	assert.Contains(t, mesh, "haify-e", "the new tiebreaker joins the LAN mesh")
	assert.NotContains(t, mesh, "haify-c", "the DR does not")
}

// Removing the tiebreaker is only dangerous when too few replicas are left.
// Warning either way trains the operator to ignore the message — and on a
// two-site resource the warning is simply false.
func TestSetTiebreakerRemovalMessageDependsOnReplicaCount(t *testing.T) {
	for _, tc := range []struct {
		name    string
		nodes   string
		wantSub string
	}{
		{"two replicas still need the vote", "node-a,node-b", "will now suspend I/O"},
		{"three replicas stand on their own", "node-a,node-b,node-c", "still give a quorum majority"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeDeploymentClient{}
			fake.execFunc = func(ctx context.Context, hosts []string, cmd string, opts ...deployment.ExecOption) (*deployment.ExecResult, error) {
				if strings.HasPrefix(cmd, "cat ") {
					return successExecResult(hosts, lanResConfig), nil
				}
				return successExecResult(hosts, ""), nil
			}
			ctrl := addDRTestFixture(t).controller
			ctrl.deployment = fake
			ctrl.resources.SetDeployment(fake)
			ctrl.db = newTestDB(t)
			require.NoError(t, ctrl.db.SaveResource(context.Background(), &database.Resource{
				Name: "data", Port: 7300, Nodes: tc.nodes, DisklessNodes: "node-e",
			}))

			srv := &Server{ctrl: ctrl, resources: ctrl.resources}
			resp, err := srv.SetTiebreaker(context.Background(),
				&haifypb.SetTiebreakerRequest{Resource: "data", Node: ""})
			require.NoError(t, err)
			assert.Contains(t, resp.Message, tc.wantSub)
		})
	}
}
