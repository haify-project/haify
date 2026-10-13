package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/deployment"
	"github.com/haify-project/haify/pkg/wanproxy"
)

// A two-endpoint WAN config as versions before this one wrote it.
const oldTwoEndpointWAN = `resource wan1 {

    net {
        protocol A;
        verify-alg crc32c;
        congestion-extents 500;
        congestion-fill 2M;
        csums-alg sha256;
        on-congestion pull-ahead;
        ping-timeout 20;
        rr-conflict retry-connect;
    }

    volume 0 {
        device    minor 4;
        disk      /dev/vg0/wan1_data;
        meta-disk internal;
        disk {
            rs-discard-granularity 65536;
        }
    }

    on p1 {
        address   127.0.0.1:7250;
        node-id   0;
    }

    on d1 {
        address   127.0.0.1:7150;
        node-id   1;
    }
}`

// A multi-replica WAN config as earlier versions wrote it: a synchronous LAN
// mesh plus one explicit connection per WAN leg.
const oldMultiLegWAN = `resource data {

    net {
        protocol C;
        verify-alg crc32c;
    }

    connection-mesh {
        hosts haify-a haify-b;
    }

    connection {
        host haify-a address 127.0.0.1:7400;
        host haify-dr address 127.0.0.1:7300;
        net {
            protocol A;
            on-congestion pull-ahead;
            congestion-fill 400M;
            csums-alg sha256;
        }
    }

    connection {
        host haify-b address 127.0.0.1:7401;
        host haify-dr address 127.0.0.1:7301;
        net {
            protocol A;
            on-congestion pull-ahead;
            congestion-fill 400M;
            csums-alg sha256;
        }
    }
}`

func TestWANCongestionFillFollowsTheBuffer(t *testing.T) {
	assert.Equal(t, "3686M", wanCongestionFill(wanproxy.Tuning{}), "90% of the 4 GiB default")
	assert.Equal(t, "921M", wanCongestionFill(wanproxy.Tuning{BufferBytes: 1 << 30}))
	assert.Equal(t, "9216M", wanCongestionFill(wanproxy.Tuning{BufferBytes: 10 << 30}))
}

func TestApplyWANOptionsConvergesATwoEndpointConfig(t *testing.T) {
	got := applyWANOptions(oldTwoEndpointWAN, wanproxy.Tuning{BufferBytes: 1 << 30})
	assert.Contains(t, got, "        congestion-fill 921M;")
	assert.NotContains(t, got, "congestion-fill 2M")
	assert.Contains(t, got, "        ko-count 0;")
	// The resync settings land beside the net block, at resource level.
	assert.Contains(t, got, "    }\n    disk {\n        c-plan-ahead 200;\n        c-fill-target 15M;\n    }")
	// The volume's own disk block is untouched.
	assert.Contains(t, got, "rs-discard-granularity 65536;")
	assert.Equal(t, got, applyWANOptions(got, wanproxy.Tuning{BufferBytes: 1 << 30}), "applying twice changes nothing")

	// A new buffer moves congestion-fill and nothing else.
	again := applyWANOptions(got, wanproxy.Tuning{})
	assert.Contains(t, again, "congestion-fill 3686M;")
	assert.Equal(t, 1, strings.Count(again, "c-fill-target"))
	assert.Equal(t, 1, strings.Count(again, "ko-count"))
}

func TestApplyWANOptionsTouchesOnlyTheWANLegsOfAMultiReplicaConfig(t *testing.T) {
	got := applyWANOptions(oldMultiLegWAN, wanproxy.Tuning{})
	assert.Equal(t, 2, strings.Count(got, "congestion-fill 3686M;"))
	assert.Equal(t, 2, strings.Count(got, "            ko-count 0;"), "per WAN connection")
	assert.Equal(t, 2, strings.Count(got, "        disk {\n            c-plan-ahead 200;"), "inside each WAN connection")
	// The synchronous LAN net block must not lose its peer kick-out.
	lan := got[:strings.Index(got, "connection-mesh")]
	assert.NotContains(t, lan, "ko-count")
	assert.NotContains(t, lan, "c-fill-target")
	assert.Equal(t, got, applyWANOptions(got, wanproxy.Tuning{}))
}

func TestApplyWANOptionsKeepsWhatTheOperatorSet(t *testing.T) {
	custom := strings.Replace(oldTwoEndpointWAN, "        ping-timeout 20;", "        ping-timeout 20;\n        ko-count 12;", 1)
	custom = strings.Replace(custom, "rs-discard-granularity 65536;", "rs-discard-granularity 65536;\n            c-fill-target 2M;", 1)
	got := applyWANOptions(custom, wanproxy.Tuning{})
	assert.Contains(t, got, "ko-count 12;")
	assert.NotContains(t, got, "ko-count 0;")
	assert.Equal(t, 1, strings.Count(got, "c-fill-target"), "an explicit c-fill-target means no disk block is added")
	assert.NotContains(t, got, "c-plan-ahead")
}

// What the generator writes for a new resource is already what the converger
// would produce, so tuning a new resource's buffer only moves congestion-fill.
func TestGeneratedWANConfigsAreAlreadyConverged(t *testing.T) {
	ctrl := newBasicTestController(&fakeDeploymentClient{})
	registerWANNodes(ctrl)
	vols := []resolvedVolume{{id: 0, volumeName: "data_data", pool: "vg0", minor: 0, sizeGB: 1}}
	single := ctrl.resources.generateDrbdConfig("data", 7300, vols, []string{"node-a", "node-dr"}, nil, "A", "lvm", nil,
		&wanConfig{DRNode: "node-dr", PrimaryNodes: []string{"node-a"}})
	multi := ctrl.resources.generateDrbdConfig("data", 7300, vols, []string{"node-a", "node-b", "node-dr"}, nil, "C", "lvm", nil,
		&wanConfig{DRNode: "node-dr", PrimaryNodes: []string{"node-a", "node-b"}})
	for name, cfg := range map[string]string{"single": single, "multi": multi} {
		assert.Contains(t, cfg, "congestion-fill 3686M;", name)
		assert.Contains(t, cfg, "ko-count 0;", name)
		assert.Contains(t, cfg, "c-fill-target 15M;", name)
		assert.Equal(t, cfg, applyWANOptions(cfg, wanproxy.Tuning{}), name)
	}
}

// SetWanTuning moves DRBD's congestion-fill with the buffer, on every member,
// and adjusts DRBD to it.
func TestSetWanTuningMovesCongestionFill(t *testing.T) {
	srv, _ := wanEndpointFixture(t, "203.0.113.7")
	adjusted := withLiveWANConfig(srv, oldTwoEndpointWAN)
	resp, err := srv.SetWanTuning(context.Background(), &pb.SetWanTuningRequest{Name: "wan1", Buffer: "1GiB"})
	require.NoError(t, err)
	require.True(t, resp.Success, resp.Message)
	res := lastResConfig(t, srv, "wan1")
	assert.Contains(t, res, "congestion-fill 921M;")
	assert.Contains(t, res, "ko-count 0;")
	assert.True(t, *adjusted, "drbdadm adjust must run")

	// Unchanged proxy tuning still brings an old config up to date.
	srv2, _ := wanEndpointFixture(t, "203.0.113.7")
	withLiveWANConfig(srv2, oldTwoEndpointWAN)
	resp, err = srv2.SetWanTuning(context.Background(), &pb.SetWanTuningRequest{Name: "wan1", Buffer: "default"})
	require.NoError(t, err)
	require.True(t, resp.Success, resp.Message)
	assert.Contains(t, resp.Message, "DRBD options brought up to date")
	assert.Contains(t, lastResConfig(t, srv2, "wan1"), "congestion-fill 3686M;")
}

// withLiveWANConfig makes `cat /etc/drbd.d/<res>.res` return cfg, and reports
// whether `drbdadm adjust` ran.
func withLiveWANConfig(srv *Server, cfg string) *bool {
	dep := srv.resources.deployment.(*fakeDeploymentClient)
	prev := dep.execFunc
	adjusted := false
	dep.execFunc = func(ctx context.Context, hosts []string, cmd string, opts ...deployment.ExecOption) (*deployment.ExecResult, error) {
		switch {
		case strings.HasPrefix(cmd, "cat /etc/drbd.d/"):
			return successExecResult(hosts, cfg), nil
		case strings.Contains(cmd, "drbdadm adjust"):
			adjusted = true
		}
		return prev(ctx, hosts, cmd, opts...)
	}
	return &adjusted
}

func lastResConfig(t *testing.T, srv *Server, resource string) string {
	t.Helper()
	dep := srv.resources.deployment.(*fakeDeploymentClient)
	for i := len(dep.distributedConfigs) - 1; i >= 0; i-- {
		if dep.distributedConfigs[i].remotePath == "/etc/drbd.d/"+resource+".res" {
			return dep.distributedConfigs[i].content
		}
	}
	t.Fatal("no resource config was written")
	return ""
}
