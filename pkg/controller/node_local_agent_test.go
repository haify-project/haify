package controller

import (
	"context"
	"testing"

	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/deployment"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func agentTestDeployment(fake *fakeDeploymentClient) *localAgentDeployment {
	return &localAgentDeployment{deploymentClient: fake, isAgent: func(h string) bool { return h == "10.0.0.9" }}
}

// A command naming an agent node runs only on the others, and the agent node
// reads as done.
func TestLocalAgentDeploymentDropsAgentHosts(t *testing.T) {
	var ran [][]string
	fake := &fakeDeploymentClient{
		execFunc: func(_ context.Context, hosts []string, _ string, _ ...deployment.ExecOption) (*deployment.ExecResult, error) {
			ran = append(ran, hosts)
			return successExecResult(hosts, "out"), nil
		},
		distributeConfigFunc: func(_ context.Context, hosts []string, _, _ string, _ ...deployment.ConfigOption) (*deployment.ConfigResult, error) {
			ran = append(ran, hosts)
			return &deployment.ConfigResult{Success: true, Hosts: map[string]*deployment.HostResult{}}, nil
		},
	}
	d := agentTestDeployment(fake)

	res, err := d.Exec(context.Background(), []string{"10.0.0.1", "10.0.0.9"}, "drbdadm adjust r")
	require.NoError(t, err)
	assert.Equal(t, [][]string{{"10.0.0.1"}}, ran)
	assert.True(t, res.AllSuccess())
	assert.Equal(t, "out", res.Hosts["10.0.0.1"].Output)
	assert.Empty(t, res.Hosts["10.0.0.9"].Output)

	ran = nil
	res, err = d.Exec(context.Background(), []string{"10.0.0.9"}, "rm -f /etc/drbd.d/r.res")
	require.NoError(t, err)
	assert.Empty(t, ran, "nothing reaches SSH when every host is an agent")
	assert.True(t, res.AllSuccess())

	cfg, err := d.DistributeConfig(context.Background(), []string{"10.0.0.9", "10.0.0.2"}, "x", "/etc/drbd.d/r.res")
	require.NoError(t, err)
	assert.Equal(t, [][]string{{"10.0.0.2"}}, ran)
	assert.Contains(t, cfg.Hosts, "10.0.0.9")
}

// A status read from an agent node fails, so a caller asks another node.
func TestLocalAgentDeploymentStatusFailsOnAgent(t *testing.T) {
	d := agentTestDeployment(&fakeDeploymentClient{})
	res, err := d.DRBDStatusJSON(context.Background(), []string{"10.0.0.9"}, "r")
	require.NoError(t, err)
	assert.False(t, res.AllSuccess())
}

func TestLocalAgentDeploymentRefusesRoleChange(t *testing.T) {
	called := false
	d := agentTestDeployment(&fakeDeploymentClient{
		drbdPrimaryFunc: func(context.Context, string, string, bool) (*deployment.HostResult, error) {
			called = true
			return &deployment.HostResult{Success: true}, nil
		},
	})
	_, err := d.DRBDPrimary(context.Background(), "10.0.0.9", "r", false)
	assert.Error(t, err)
	assert.False(t, called)
	_, err = d.DRBDPrimary(context.Background(), "10.0.0.1", "r", false)
	assert.NoError(t, err)
	assert.True(t, called)
}

// An agent registers without SSH; a node the controller already reaches over
// SSH stays SSH-managed.
func TestRegisterLocalAgentNode(t *testing.T) {
	sshed := false
	ctrl := newBasicTestController(&fakeDeploymentClient{
		execFunc: func(_ context.Context, hosts []string, _ string, _ ...deployment.ExecOption) (*deployment.ExecResult, error) {
			sshed = true
			return successExecResult(hosts, ""), nil
		},
	})
	nm := ctrl.nodes

	n, err := nm.RegisterLocalAgentNode(context.Background(), "worker-1", "10.0.0.9", "worker-1.ocp")
	require.NoError(t, err)
	assert.True(t, n.LocalAgent)
	assert.False(t, sshed, "registering an agent never SSHes")
	assert.True(t, nm.isLocalAgentHost("10.0.0.9"))
	assert.True(t, nm.isLocalAgentHost("worker-1"))
	assert.True(t, nm.isLocalAgentHost("worker-1.ocp"))
	assert.NotContains(t, ctrl.hosts, "10.0.0.9", "an agent is not an SSH host")
	assert.Equal(t, "10.0.0.9", ctrl.ResolveHost("worker-1"))

	nm.nodes["10.0.0.5"] = &NodeInfo{Name: "storage-1", Address: "10.0.0.5", State: NodeStateOnline}
	n, err = nm.RegisterLocalAgentNode(context.Background(), "storage-1", "10.0.0.5", "storage-1")
	require.NoError(t, err)
	assert.False(t, n.LocalAgent)
	assert.False(t, nm.isLocalAgentHost("10.0.0.5"))

	_, err = nm.RegisterLocalAgentNode(context.Background(), "worker-1", "10.0.0.10", "worker-1.ocp")
	assert.Error(t, err, "the same name at another address")
	_, err = nm.RegisterLocalAgentNode(context.Background(), "worker-2", "not-an-ip", "")
	assert.Error(t, err)
}

func TestLocalAgentHealthFollowsReports(t *testing.T) {
	ctrl := newBasicTestController(&fakeDeploymentClient{})
	nm := ctrl.nodes
	_, err := nm.RegisterLocalAgentNode(context.Background(), "worker-1", "10.0.0.9", "worker-1")
	require.NoError(t, err)
	assert.NoError(t, nm.CheckNodeHealth(context.Background(), "10.0.0.9"))

	nm.nodes["10.0.0.9"].LastSeen = nm.nodes["10.0.0.9"].LastSeen.Add(-2 * localAgentTimeout)
	assert.Error(t, nm.CheckNodeHealth(context.Background(), "10.0.0.9"))
	assert.Equal(t, NodeStateOffline, nm.nodes["10.0.0.9"].State)

	nm.agentHeartbeat("10.0.0.9")
	assert.NoError(t, nm.CheckNodeHealth(context.Background(), "10.0.0.9"))
	assert.Equal(t, NodeStateOnline, nm.nodes["10.0.0.9"].State)
}

func TestAgentServesAndOwnBlock(t *testing.T) {
	node := &NodeInfo{Name: "worker-1", Hostname: "worker-1.ocp", Address: "10.0.0.9"}
	assert.True(t, agentServes(&database.Resource{DisklessClients: "other,worker-1"}, node))
	assert.False(t, agentServes(&database.Resource{DisklessClients: "other", Nodes: "worker-1"}, node))

	cfg := "resource r {\n  on worker-1.ocp {\n    node-id 3;\n  }\n}\n"
	assert.True(t, hasOnBlock(cfg, "worker-1.ocp"))
	assert.False(t, hasOnBlock(cfg, "worker-2"))
}

func TestTiebreakerSkipsLocalAgents(t *testing.T) {
	ctrl := newBasicTestController(&fakeDeploymentClient{})
	ctrl.nodes.nodes["10.0.0.1"] = &NodeInfo{Name: "a", Address: "10.0.0.1", State: NodeStateOnline}
	ctrl.nodes.nodes["10.0.0.2"] = &NodeInfo{Name: "b", Address: "10.0.0.2", State: NodeStateOnline}
	ctrl.nodes.nodes["10.0.0.9"] = &NodeInfo{Name: "agent", Address: "10.0.0.9", State: NodeStateOnline, LocalAgent: true}
	assert.Empty(t, ctrl.resources.selectTiebreaker(context.Background(), []string{"a", "b"}))
	ctrl.nodes.nodes["10.0.0.3"] = &NodeInfo{Name: "c", Address: "10.0.0.3", State: NodeStateOnline}
	assert.Equal(t, "c", ctrl.resources.selectTiebreaker(context.Background(), []string{"a", "b"}))
}
