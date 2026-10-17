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

func TestValidateDrbdOptions(t *testing.T) {
	ok := map[string]string{
		"net/max-buffers":               "8000",
		"on-no-quorum":                  "suspend-io",
		"disk/resync-rate":              "100M",
		"handlers/split-brain":          `"/usr/lib/drbd/notify-split-brain.sh root"`,
		"handlers/fence-peer":           "/usr/lib/drbd/crm-fence-peer.9.sh",
		"net/shared-secret":             "s3cr3t",
		"disk/rs-discard-granularity":   "", // removal
		"options/on-no-data-accessible": "io-error",
	}
	require.NoError(t, validateDrbdOptions(ok))

	for k, v := range map[string]string{
		"net/max-buffers":         "8000; } resource evil {",
		"net/verify-alg":          "crc32c # note",
		"net/x":                   "two words",
		"handlers/split-brain":    `"rm -rf /; echo"`,
		"bogus/key":               "1",
		"net/Max_Buffers":         "1",
		"options/auto-promote":    "yes",
		"net/allow-two-primaries": "yes",
		"net/tls":                 "yes",
	} {
		assert.Error(t, validateDrbdOptions(map[string]string{k: v}), "%s=%s", k, v)
	}
}

func TestApplyDrbdOptionsRemoves(t *testing.T) {
	withDisk, err := applyDrbdOptions(sampleRes, map[string]string{"disk/on-io-error": "detach", "net/max-buffers": "8000"})
	require.NoError(t, err)
	out, err := applyDrbdOptions(withDisk, map[string]string{
		"on-no-quorum":     "",
		"disk/on-io-error": "",
		"net/max-buffers":  "",
		"net/never-set":    "",
	})
	require.NoError(t, err)
	assert.NotContains(t, out, "on-no-quorum")
	assert.NotContains(t, out, "on-io-error")
	assert.NotContains(t, out, "max-buffers")
	assert.Contains(t, out, "quorum majority;", "only the named keys go")
	assert.Contains(t, out, "rr-conflict retry-connect;")
	assert.Equal(t, strings.Count(sampleRes, "{"), strings.Count(out, "{")-2, "the two disk blocks stay, empty")
}

func TestReadDrbdOptions(t *testing.T) {
	conf, err := applyDrbdOptions(sampleRes, map[string]string{
		"disk/on-io-error":              "detach",
		"net/shared-secret":             "s3cr3t",
		"handlers/before-resync-target": `"/usr/lib/drbd/snapshot-resync-target-lvm.sh -p 15"`,
	})
	require.NoError(t, err)
	opts, err := readDrbdOptions(conf)
	require.NoError(t, err)
	got := map[string]ResourceOption{}
	for _, o := range opts {
		got[o.Key] = o
	}
	assert.Equal(t, "majority", got["options/quorum"].Value)
	assert.Equal(t, "C", got["net/protocol"].Value)
	assert.Equal(t, "detach", got["disk/on-io-error"].Value)
	assert.Equal(t, `"/usr/lib/drbd/snapshot-resync-target-lvm.sh -p 15"`, got["handlers/before-resync-target"].Value)
	assert.True(t, got["net/shared-secret"].Secret)
	assert.Empty(t, got["net/shared-secret"].Value)
	_, hasDevice := got["disk/disk"]
	assert.False(t, hasDevice, "volume layout is not an option")
}

// setOptionsController is a resource on two nodes whose drbdadm answers as
// check and adjust say.
func setOptionsController(t *testing.T, check, adjust func(cmd string) *deployment.ExecResult) (*Controller, *fakeDeploymentClient) {
	t.Helper()
	ctx := context.Background()
	dep := &fakeDeploymentClient{}
	dep.execFunc = func(_ context.Context, hosts []string, cmd string, _ ...deployment.ExecOption) (*deployment.ExecResult, error) {
		switch {
		case strings.HasPrefix(cmd, "cat /etc/drbd.d/res1.res"):
			// The node holds whatever was last installed there.
			conf := drbdResFixture
			for _, d := range dep.distributedConfigs {
				if d.remotePath == "/etc/drbd.d/res1.res" {
					conf = d.content
				}
			}
			return successExecResult(hosts, conf), nil
		case strings.Contains(cmd, "drbdadm -c"):
			return check(cmd), nil
		case strings.Contains(cmd, "drbdadm adjust"):
			return adjust(cmd), nil
		}
		return successExecResult(hosts, ""), nil
	}
	ctrl := newBasicTestController(dep)
	ctrl.db = newTestDB(t)
	require.NoError(t, ctrl.db.SaveResource(ctx, &database.Resource{Name: "res1", Nodes: "n1,n2"}))
	ctrl.hostsMap["n1"] = "10.0.0.1"
	ctrl.hostsMap["n2"] = "10.0.0.2"
	return ctrl, dep
}

func installed(dep *fakeDeploymentClient) []string {
	var out []string
	for _, d := range dep.distributedConfigs {
		if d.remotePath == "/etc/drbd.d/res1.res" {
			out = append(out, d.content)
		}
	}
	return out
}

// A config the node's drbdadm rejects is never installed, and the error says
// why without repeating a secret.
func TestSetOptionsRejectedByDrbdadm(t *testing.T) {
	ctrl, dep := setOptionsController(t,
		func(string) *deployment.ExecResult {
			return failedExecResult([]string{"10.0.0.1"},
				"/var/lib/haify/drbd-check/res1.res:12: Parse error: while parsing value ('toolongsecret') for shared-secret")
		},
		func(string) *deployment.ExecResult { return successExecResult([]string{"10.0.0.1"}, "") })
	err := ctrl.resources.SetOptions(context.Background(), "res1", map[string]string{"net/shared-secret": "toolongsecret"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "DRBD rejects the options")
	assert.Contains(t, err.Error(), "res1.res:12")
	assert.NotContains(t, err.Error(), "toolongsecret")
	assert.Empty(t, installed(dep))
}

// A config that does not apply on a node is replaced by the one before it.
func TestSetOptionsRestoresWhenAdjustFails(t *testing.T) {
	adjusts := 0
	ctrl, dep := setOptionsController(t,
		func(string) *deployment.ExecResult { return successExecResult([]string{"10.0.0.1"}, "") },
		func(string) *deployment.ExecResult {
			adjusts++
			if adjusts == 1 {
				return failedExecResult([]string{"10.0.0.1"}, "drbdsetup net-options failed")
			}
			return successExecResult([]string{"10.0.0.1"}, "")
		})
	err := ctrl.resources.SetOptions(context.Background(), "res1", map[string]string{"net/max-buffers": "8000"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "previous config is back in place")
	got := installed(dep)
	require.Len(t, got, 2)
	assert.Contains(t, got[0], "max-buffers 8000;")
	assert.NotContains(t, got[1], "max-buffers")
}

func TestParseErrorText(t *testing.T) {
	out := "/var/lib/haify/drbd-check/r.res:16: Parse error: while parsing value ('bogus')\n" +
		"for on-congestion. Value is not valid.\n*** buffer overflow detected ***: terminated\n" +
		"bash: line 1: 2435584 Aborted                 sudo drbdadm -c x -d adjust r\nAborted\n"
	assert.Equal(t, "r.res:16: Parse error: while parsing value ('bogus')\nfor on-congestion. Value is not valid.",
		parseErrorText(out, "/var/lib/haify/drbd-check/r.res", "r"))
}
