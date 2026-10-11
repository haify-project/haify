package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haify-project/haify/pkg/deployment"
)

func TestParseZFSPoolProperties(t *testing.T) {
	props := parseZFSPoolProperties("haify_tank\tcompression\tzstd\nhaify_tank\tcompressratio\t1.85\n" +
		"haify_fast\tcompression\tlz4\nhaify_fast\tcompressratio\t1.00x\nnoise line\n")
	assert.Equal(t, zfsPoolProps{compression: "zstd", ratio: 1.85}, props["haify_tank"])
	assert.Equal(t, zfsPoolProps{compression: "lz4", ratio: 1.00}, props["haify_fast"])
	assert.Len(t, props, 2)
}

// Pools say how they compress, and how well, instead of nothing.
func TestListZFSPoolsReportsCompression(t *testing.T) {
	dep := &fakeDeploymentClient{
		execFunc: func(_ context.Context, hosts []string, cmd string, _ ...deployment.ExecOption) (*deployment.ExecResult, error) {
			if strings.Contains(cmd, "base64 -d") {
				return successExecResult(hosts, "haify_tank\tcompression\tzstd\nhaify_tank\tcompressratio\t2.10\n"), nil
			}
			return successExecResult(hosts, ""), nil
		},
		zfsListPoolsFunc: func(_ context.Context, hosts []string) (*deployment.ExecResult, error) {
			return successExecResult(hosts, "haify_tank\t1000\t400\t600\t60\n"), nil
		},
	}
	ctrl := newBasicTestController(dep)
	ctrl.hosts = []string{"10.0.0.1"}

	pools, err := ctrl.storage.ListZFSpools(context.Background())
	require.NoError(t, err)
	require.Len(t, pools, 1)
	assert.Equal(t, "zstd", pools[0].Compression)
	assert.InDelta(t, 2.10, pools[0].CompressRatio, 0.001)
}

func TestCreateZFSPoolRefusesAnUnknownAlgorithm(t *testing.T) {
	dep := &fakeDeploymentClient{}
	ctrl := newBasicTestController(dep)
	err := ctrl.storage.CreateZFSPool(context.Background(), "tank", "node1", []string{"/dev/sdb"}, "snappy", false, false)
	require.Error(t, err)
	assert.Empty(t, dep.zfsCreatePoolCalls, "refused before anything is created")
}
