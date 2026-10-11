package controller

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The config of a real resource with a diskless client (pve-c), as the
// nodes hold it — replication paths expanded — and as the controller reads it.
func TestDropDisklessOverride(t *testing.T) {
	raw, err := os.ReadFile("testdata/res_diskless_client.res")
	require.NoError(t, err)
	conf := collapseReplicationPaths(string(raw))

	out, err := dropDisklessOverride(conf, "pve-c")
	require.NoError(t, err)
	assert.NotContains(t, out, "disk      none;")
	assert.Contains(t, out, "    on pve-c {\n        address   192.168.123.113:7090;\n        node-id   2;\n    }")
	assert.Contains(t, out, "disk      /dev/haify_vg0/tgl_data;", "the resource-level disk stays")
	assert.Equal(t, strings.Count(conf, "\n")-4, strings.Count(out, "\n"), "only the four override lines go")
	assert.Contains(t, out, "connection-mesh", "the mesh is untouched")

	_, err = dropDisklessOverride(out, "pve-c")
	assert.ErrorContains(t, err, "no diskless override")
	_, err = dropDisklessOverride(conf, "pve-a")
	assert.ErrorContains(t, err, "pve-a has no diskless override", "a replica is not a diskless member")
}
