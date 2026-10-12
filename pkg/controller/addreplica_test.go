package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haify-project/haify/pkg/deployment"
)

// A LAN resource gains a plain mesh member: real address, no explicit
// connection, no loopback anywhere.
func TestAddReplicaToConfigLAN(t *testing.T) {
	rm := addDRTestFixture(t)
	// node-e is the fixture's tiebreaker and already in the config; a fresh node
	// is what an operator would actually be adding.
	rm.controller.nodes.nodes["192.168.1.30"] = &NodeInfo{
		Name: "node-d", Address: "192.168.1.30", Hostname: "haify-d", State: NodeStateOnline,
	}
	rm.controller.hostsMap["node-d"] = "192.168.1.30"

	out, err := rm.addReplicaToConfig(lanResConfig, "openclaw", "node-d", "192.168.1.30",
		addDRVolumes, 7300, 2, 0, false, "", false)
	require.NoError(t, err)

	assert.Contains(t, out, "on haify-d {")
	assert.Contains(t, out, "address   192.168.1.30:7300;")
	assert.Contains(t, out, "node-id   3;", "next free id after 0,1,2")
	// Every replica of a resource shares one minor.
	assert.Equal(t, 4, strings.Count(out, "device    minor 12;"))
	assert.NotContains(t, out, "connection {", "a LAN member needs no explicit connection")
	assert.NotContains(t, out, "127.0.0.1")

	mesh := out[strings.Index(out, "connection-mesh"):]
	mesh = mesh[:strings.Index(mesh, ";")]
	for _, h := range []string{"haify-a", "haify-b", "haify-e", "haify-d"} {
		assert.Contains(t, mesh, h)
	}
}

// On a two-site resource the newcomer must also get its own leg. DRBD 9 is a
// full mesh: a replica the DR cannot reach ends replication the moment it is
// promoted, and nothing would report that until a failover landed there.
func TestAddReplicaToConfigTwoSiteGetsItsOwnLeg(t *testing.T) {
	rm := addDRTestFixture(t)
	twoSite, err := rm.addDRToConfig(lanResConfig, "openclaw", "node-c",
		[]string{"node-a", "node-b"}, []string{"node-e"}, "203.0.113.7", addDRVolumes, 7300,
		[]int{7900, 7901}, false)
	require.NoError(t, err)

	// node-d joins as a third primary-site replica: leg index 2.
	rm.controller.nodes.nodes["192.168.1.30"] = &NodeInfo{
		Name: "node-d", Address: "192.168.1.30", Hostname: "haify-d", State: NodeStateOnline,
	}
	rm.controller.hostsMap["node-d"] = "192.168.1.30"

	out, err := rm.addReplicaToConfig(twoSite, "openclaw", "node-d", "192.168.1.30",
		addDRVolumes, 7300, 2, 7902, true, "node-c", false)
	require.NoError(t, err)

	// Its LAN identity is a real address; only the leg uses loopback.
	assert.Contains(t, out, "address   192.168.1.30:7300;")
	assert.Contains(t, out, "host haify-d address 127.0.0.1:7902;")
	assert.Contains(t, out, "host haify-c address 127.0.0.1:7302;", "leg index 2 → port+2")
	assert.Equal(t, 3, strings.Count(out, "connection {"), "one leg per primary-site replica")

	// The DR stays out of the LAN mesh; the newcomer joins it.
	mesh := out[strings.Index(out, "connection-mesh"):]
	mesh = mesh[:strings.Index(mesh, ";")]
	assert.Contains(t, mesh, "haify-d")
	assert.NotContains(t, mesh, "haify-c")
}

func TestAddReplicaToConfigRejectsExistingNode(t *testing.T) {
	rm := addDRTestFixture(t)
	_, err := rm.addReplicaToConfig(lanResConfig, "openclaw", "node-b", "192.168.1.11",
		addDRVolumes, 7300, 1, 0, false, "", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already in the config")
}

// Growing the primary site moves its majority, so the number has to be
// recomputed — a stale one no longer describes the site it is meant to protect.
func TestAddReplicaKeepsQuorumDescribingThePrimarySite(t *testing.T) {
	// Two local replicas plus a DR: local majority is 2.
	cfg := "resource r {\n    options {\n        quorum 2;\n    }\n}\n"
	// A third local replica: majority of three is still 2.
	assert.Contains(t, setLocalSiteQuorum(cfg, 3), "quorum 2;")
	// A fourth: majority of four is 3.
	assert.Contains(t, setLocalSiteQuorum(cfg, 4), "quorum 3;")
}

// A WAN leg on a node with no proxy binary produces a systemd unit that
// crash-loops with 203/EXEC while DRBD reports only "Connecting" — a symptom
// that points nowhere near the cause, and shows up on a different machine from
// the failure. Refuse with the cause named instead.
func TestAssertWANProxyBinaryRejectsMissing(t *testing.T) {
	fake := &fakeDeploymentClient{}
	fake.execFunc = func(ctx context.Context, hosts []string, cmd string, opts ...deployment.ExecOption) (*deployment.ExecResult, error) {
		if hosts[0] == "192.168.1.20" {
			return successExecResult(hosts, "missing\n"), nil
		}
		return successExecResult(hosts, "present\n"), nil
	}
	rm := newBasicTestController(fake).resources

	require.NoError(t, rm.assertWANProxyBinary(context.Background(),
		[]string{"192.168.1.10"}, []string{"node-a"}))

	err := rm.assertWANProxyBinary(context.Background(),
		[]string{"192.168.1.10", "192.168.1.20"}, []string{"node-a", "node-e"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "node-e")
	assert.Contains(t, err.Error(), "203/EXEC")
}
