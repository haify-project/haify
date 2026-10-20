package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/deployment"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResourceManagerMakeHaUsesResourceNodesOnly(t *testing.T) {
	dep := &fakeDeploymentClient{
		execFunc: func(ctx context.Context, hosts []string, cmd string, opts ...deployment.ExecOption) (*deployment.ExecResult, error) {
			switch {
			case strings.Contains(cmd, "drbdadm status"):
				return successExecResult(hosts, "res1 role:Primary\n"), nil
			default:
				return successExecResult(hosts, ""), nil
			}
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

	configPath, err := ctrl.resources.MakeHa(context.Background(), "res1", nil, "", "", "", nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "/etc/drbd-reactor.d/haify-ha-res1.toml", configPath)
	require.Len(t, dep.distributedConfigs, 1)
	assert.Equal(t, []string{"10.0.0.1", "10.0.0.2"}, dep.distributedConfigs[0].hosts)
}

func TestResourceManagerRemoveHaUsesResourceNodesOnly(t *testing.T) {
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
	require.NoError(t, ctrl.db.SaveHaConfig(context.Background(), &database.HaConfig{
		Resource: "res1",
	}))
	ctrl.nodes.nodes["10.0.0.1"] = &NodeInfo{Name: "node1", Address: "10.0.0.1"}
	ctrl.nodes.nodes["10.0.0.2"] = &NodeInfo{Name: "node2", Address: "10.0.0.2"}
	ctrl.hostsMap["node1"] = "10.0.0.1"
	ctrl.hostsMap["node2"] = "10.0.0.2"
	ctrl.resources.mu.Lock()
	ctrl.resources.hosts = []string{"wrong-host"}
	ctrl.resources.mu.Unlock()

	err := ctrl.resources.RemoveHa(context.Background(), "res1")
	require.NoError(t, err)
	require.Len(t, dep.deleteConfigCalls, 1)
	assert.Equal(t, []string{"10.0.0.1", "10.0.0.2"}, dep.deleteConfigCalls[0].hosts)
	_, err = ctrl.db.GetHaConfig(context.Background(), "res1")
	assert.ErrorContains(t, err, "not found")
}

func TestResourceManagerRemoveHaStopsVIP(t *testing.T) {
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
	require.NoError(t, ctrl.db.SaveHaConfig(context.Background(), &database.HaConfig{
		Resource: "res1",
		VIP:      "192.168.1.50/24",
	}))
	ctrl.nodes.nodes["10.0.0.1"] = &NodeInfo{Name: "node1", Address: "10.0.0.1"}
	ctrl.nodes.nodes["10.0.0.2"] = &NodeInfo{Name: "node2", Address: "10.0.0.2"}
	ctrl.hostsMap["node1"] = "10.0.0.1"
	ctrl.hostsMap["node2"] = "10.0.0.2"

	require.NoError(t, ctrl.resources.RemoveHa(context.Background(), "res1"))

	var sawStop bool
	for _, call := range dep.execCalls {
		if call.cmd == "systemctl stop service-ip@192.168.1.50-24.service" {
			sawStop = true
			assert.Equal(t, []string{"10.0.0.1", "10.0.0.2"}, call.hosts)
		}
	}
	assert.True(t, sawStop, "RemoveHa must explicitly stop the VIP service-ip unit; exec calls: %+v", dep.execCalls)
}

func TestSelfHaDisableScriptStopsVIP(t *testing.T) {
	script := generateSelfHaDisableScript([]string{"10.0.0.2"}, "10.0.0.1", "10.0.0.1", "192.168.1.50/24")
	assert.Contains(t, script, "systemctl stop service-ip@192.168.1.50-24.service",
		"self-HA disable script must explicitly stop the VIP service-ip unit")

	// A missing VIP (disable retry) must not emit a bogus stop command.
	scriptNoVIP := generateSelfHaDisableScript([]string{"10.0.0.2"}, "10.0.0.1", "10.0.0.1", "")
	assert.NotContains(t, scriptNoVIP, "stop service-ip@")
}

func TestVipServiceIPInstance(t *testing.T) {
	assert.Equal(t, "192.168.1.50-24", vipServiceIPInstance("192.168.1.50/24"))
	assert.Equal(t, "192.168.1.50-32", vipServiceIPInstance("192.168.1.50"))
	assert.Equal(t, "", vipServiceIPInstance(""))
	assert.Equal(t, "", vipServiceIPInstance("  "))
}

func TestResourceManagerEvictHaUsesResourceNodesOnly(t *testing.T) {
	var evictTimeoutSeen time.Duration
	dep := &fakeDeploymentClient{
		reactorPromoterStatusByResourceFunc: func(ctx context.Context, host, resource string) (*deployment.ReactorPromoterStatus, error) {
			if host == "10.0.0.1" {
				return &deployment.ReactorPromoterStatus{
					DRBDResource: resource,
					PrimaryOn:    "node1",
					Status:       "active",
				}, nil
			}
			return nil, assert.AnError
		},
		execFunc: func(ctx context.Context, hosts []string, cmd string, opts ...deployment.ExecOption) (*deployment.ExecResult, error) {
			if strings.Contains(cmd, base64Std(evictScript("res1"))) {
				evictTimeoutSeen = deployment.ExecTimeoutOf(opts...)
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
	ctrl.resources.mu.Lock()
	ctrl.resources.hosts = []string{"wrong-host"}
	ctrl.resources.mu.Unlock()

	err := ctrl.resources.EvictHa(context.Background(), "res1")
	require.NoError(t, err)

	var sawRemoteStatus, sawEvict bool
	for _, call := range dep.execCalls {
		if call.cmd == "drbdadm status res1" {
			sawRemoteStatus = true
			assert.Equal(t, []string{"10.0.0.1"}, call.hosts)
		}
		if strings.Contains(call.cmd, base64Std(evictScript("res1"))) {
			sawEvict = true
			assert.Equal(t, []string{"10.0.0.1"}, call.hosts)
		}
	}
	assert.False(t, sawRemoteStatus)
	assert.True(t, sawEvict)
	// Cut off at the 30-second default, drbd-reactorctl was interrupted after
	// stopping the services and before a peer took over.
	assert.Equal(t, evictTimeout, evictTimeoutSeen)
	assert.Greater(t, evictTimeoutSeen, 30*time.Second)
}
