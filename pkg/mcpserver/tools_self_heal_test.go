package mcpserver

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	haifypb "github.com/haify-project/haify/api/proto/v1"
)

type selfHealMock struct {
	mockClient
	status *haifypb.GetSelfHealStatusResponse
}

func (m *selfHealMock) GetSelfHealStatus(context.Context) (*haifypb.GetSelfHealStatusResponse, error) {
	return m.status, nil
}

func (m *selfHealMock) ResizeVolumeWith(_ context.Context, req *haifypb.ResizeVolumeRequest) (string, error) {
	return "Volume resized successfully; guest web1 on n1 was not told the new size (no agent); run `virsh blockresize web1 vdb <size>` on n1", nil
}

func (m *selfHealMock) CreateZFSSnapshotNoted(context.Context, string, string, string) (string, error) {
	return "ZFS snapshot created successfully (crash-consistent: guest web1 has no guest agent)", nil
}

// The status tool is read-only and passes on what the controller reports,
// with times a reader can use.
func TestSelfHealStatusTool(t *testing.T) {
	half := 42.5
	mc := &selfHealMock{status: &haifypb.GetSelfHealStatusResponse{Success: true,
		AutoEvict: &haifypb.AutoEvictStatus{Mode: "off"},
		AutoDiskful: &haifypb.AutoDiskfulStatus{Mode: "on", AfterMinutes: 60, CooldownHours: 24,
			Clients: []*haifypb.AutoDiskfulClient{{Resource: "vol1", Node: "c", InUseSince: 1794643200, InUseSeconds: 4000,
				State: "skipped", Skip: "pinned", Reason: "its PersistentVolume is pinned"}},
			Swaps: []*haifypb.AutoDiskfulSwap{{Resource: "vol2", Node: "d", Phase: "syncing", SyncPercent: &half,
				ReplicasNow: 3, ReplicasTarget: 2}},
			Cooldowns: []*haifypb.AutoDiskfulCooldown{{Resource: "vol3", MovedAt: 1794643200, Until: 1794729600}}},
	}}
	session := connect(t, mc, true)
	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "haify_self_heal_status"})
	require.NoError(t, err)
	require.False(t, res.IsError, "a read-only server still serves it")
	raw, err := json.Marshal(res.StructuredContent)
	require.NoError(t, err)
	var out selfHealStatusOut
	require.NoError(t, json.Unmarshal(raw, &out))
	assert.Equal(t, "on", out.AutoDiskful.Mode)
	require.Len(t, out.AutoDiskful.Clients, 1)
	assert.Equal(t, "skipped", out.AutoDiskful.Clients[0].State)
	assert.Equal(t, "pinned", out.AutoDiskful.Clients[0].Skip)
	assert.Equal(t, "2026-11-14T08:00:00Z", out.AutoDiskful.Clients[0].InUseSince)
	require.Len(t, out.AutoDiskful.Swaps, 1)
	require.NotNil(t, out.AutoDiskful.Swaps[0].SyncPercent)
	assert.InDelta(t, 42.5, *out.AutoDiskful.Swaps[0].SyncPercent, 0.001)
	assert.Equal(t, "2026-11-15T08:00:00Z", out.AutoDiskful.Cooldowns[0].Until)
	assert.NotNil(t, out.Moves, "an empty list, not null")
}

// What the controller said about a resize or a snapshot reaches the caller.
func TestToolsPassOnTheControllersNote(t *testing.T) {
	session := connect(t, &selfHealMock{}, false)
	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "haify_resource_resize_volume",
		Arguments: map[string]any{"resource": "web1-data", "volume_id": 0, "size_gb": 20}})
	require.NoError(t, err)
	require.False(t, res.IsError)
	raw, _ := json.Marshal(res.StructuredContent)
	assert.Contains(t, string(raw), "virsh blockresize web1 vdb")

	res, err = session.CallTool(t.Context(), &mcp.CallToolParams{Name: "haify_snapshot_create",
		Arguments: map[string]any{"resource": "web1-data", "name": "s1", "node": "n1", "pool": "tank", "storage_type": "zfs"}})
	require.NoError(t, err)
	require.False(t, res.IsError)
	raw, _ = json.Marshal(res.StructuredContent)
	assert.Contains(t, string(raw), "crash-consistent: guest web1 has no guest agent")

	assert.Equal(t, "done", withControllerNote("done", "  "))
}
