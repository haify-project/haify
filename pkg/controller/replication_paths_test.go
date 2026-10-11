package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/haify-project/haify/pkg/deployment"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const threeNodeRes = `resource data {
    net {
        protocol C;
    }
    on pve-a {
        address   192.168.123.111:7010;
        node-id   0;
        volume 0 {
            device    minor 10;
            disk      /dev/haify_vg0/data_00000;
            meta-disk internal;
        }
    }
    on pve-b {
        address   192.168.123.112:7010;
        node-id   1;
        volume 0 {
            device    minor 10;
            disk      /dev/haify_vg0/data_00000;
            meta-disk internal;
        }
    }
    on pve-c {
        address   192.168.123.113:7010;
        node-id   2;
        volume 0 {
            device    minor 10;
            disk      /dev/haify_vg0/data_00000;
            meta-disk internal;
        }
    }

    connection-mesh {
        hosts pve-a pve-b pve-c;
    }
}
`

func TestExpandReplicationPathsMesh(t *testing.T) {
	extra := map[string][]string{"pve-a": {"10.77.0.11"}, "pve-b": {"10.77.0.12"}, "pve-c": {"10.77.0.13"}}
	out := expandReplicationPaths(threeNodeRes, extra)

	assert.NotContains(t, out, "connection-mesh", "the mesh would create every peer a second time")
	assert.Equal(t, 3, strings.Count(out, "connection {"), "one connection per pair")
	assert.Equal(t, 6, strings.Count(out, "path {"), "two networks per pair")
	assert.Contains(t, out, "host pve-a address 192.168.123.111:7010;\n            host pve-b address 192.168.123.112:7010;")
	assert.Contains(t, out, "host pve-b address 10.77.0.12:7010;\n            host pve-c address 10.77.0.13:7010;")
	assert.True(t, strings.HasSuffix(strings.TrimSpace(out), "}"))

	// What the controller reads back is what it wrote before paths existed.
	assert.Equal(t, threeNodeRes, collapseReplicationPaths(out))
	// Expanding twice is the same as once.
	assert.Equal(t, out, expandReplicationPaths(out, extra))
}

// A node on only the first network still gets a path to everyone: its pairs
// get one path, the others two.
func TestExpandReplicationPathsPartial(t *testing.T) {
	out := expandReplicationPaths(threeNodeRes, map[string][]string{"pve-a": {"10.77.0.11"}, "pve-b": {"10.77.0.12"}})
	assert.Equal(t, 3, strings.Count(out, "connection {"))
	assert.Equal(t, 4, strings.Count(out, "path {"))
	assert.NotContains(t, out, "10.77.0.13")
}

func TestExpandReplicationPathsUnchangedWithoutSharedNetworks(t *testing.T) {
	assert.Equal(t, threeNodeRes, expandReplicationPaths(threeNodeRes, nil))
	assert.Equal(t, threeNodeRes, expandReplicationPaths(threeNodeRes, map[string][]string{"pve-a": {"10.77.0.11"}}),
		"one node on a second network shares it with nobody")
}

// Two nodes and no connection sections: DRBD pairs them implicitly, and the
// expansion has to make that pair explicit — and collapse back to nothing.
func TestExpandReplicationPathsImplicitPair(t *testing.T) {
	two := strings.Replace(threeNodeRes, "    connection-mesh {\n        hosts pve-a pve-b pve-c;\n    }\n", "", 1)
	two = two[:strings.Index(two, "    on pve-c")] + "}\n"
	out := expandReplicationPaths(two, map[string][]string{"pve-a": {"10.77.0.11"}, "pve-b": {"10.77.0.12"}})
	assert.Equal(t, 1, strings.Count(out, "connection {"))
	assert.Equal(t, 2, strings.Count(out, "path {"))
	assert.Contains(t, out, "# haify-paths: implicit")
	assert.Equal(t, two, collapseReplicationPaths(out))
}

// The WAN shapes keep their loopback legs; a pair involving one is left to
// them.
func TestExpandReplicationPathsLeavesLoopbackAlone(t *testing.T) {
	wan := strings.Replace(threeNodeRes, "192.168.123.113:7010", "127.0.0.1:7010", 1)
	out := expandReplicationPaths(wan, map[string][]string{"pve-a": {"10.77.0.11"}, "pve-b": {"10.77.0.12"}, "pve-c": {"10.77.0.13"}})
	assert.Equal(t, wan, out)
}

func TestValidateReplicationPaths(t *testing.T) {
	got, err := validateReplicationPaths([]string{" 10.77.0.11 ", "", "10.78.0.11"})
	require.NoError(t, err)
	assert.Equal(t, []string{"10.77.0.11", "10.78.0.11"}, got)
	for _, bad := range [][]string{{"x"}, {"127.0.0.2"}, {"fd00::1"}, {"10.77.0.11", "10.77.0.11"}} {
		_, err := validateReplicationPaths(bad)
		assert.Error(t, err, bad)
	}
}

// Every .res write is expanded and every cat of one collapsed, whoever calls.
func TestResPathsDeploymentWrapsWritesAndReads(t *testing.T) {
	var written string
	fake := &fakeDeploymentClient{
		distributeConfigFunc: func(_ context.Context, hosts []string, content, path string, _ ...deployment.ConfigOption) (*deployment.ConfigResult, error) {
			written = content
			return &deployment.ConfigResult{}, nil
		},
	}
	d := &resPathsDeployment{deploymentClient: fake, extra: func() map[string][]string {
		return map[string][]string{"pve-a": {"10.77.0.11"}, "pve-b": {"10.77.0.12"}, "pve-c": {"10.77.0.13"}}
	}}
	_, err := d.DistributeConfig(context.Background(), []string{"h"}, threeNodeRes, "/etc/drbd.d/data.res")
	require.NoError(t, err)
	assert.Contains(t, written, "10.77.0.12:7010")

	_, err = d.DistributeConfig(context.Background(), []string{"h"}, threeNodeRes, "/etc/drbd-reactor.d/haify-ha-data.toml")
	require.NoError(t, err)
	assert.Equal(t, threeNodeRes, written, "only DRBD configs are touched")

	fake.execFunc = func(_ context.Context, hosts []string, _ string, _ ...deployment.ExecOption) (*deployment.ExecResult, error) {
		return successExecResult(hosts, expandReplicationPaths(threeNodeRes, d.extra())), nil
	}
	res, err := d.Exec(context.Background(), []string{"h"}, "cat /etc/drbd.d/data.res")
	require.NoError(t, err)
	assert.Equal(t, threeNodeRes, res.Hosts["h"].Output)
}
