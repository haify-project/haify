package mcpserver

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	haifypb "github.com/haify-project/haify/api/proto/v1"
)

func (m *mockExtraClient) MakeHaWith(_ context.Context, req *haifypb.MakeHaRequest) (string, error) {
	return "/etc/ha", nil
}

// mockGuestClient is a controller with one HA guest, web1 on k1.
type mockGuestClient struct {
	mockExtraClient
	made        *haifypb.MakeHaRequest
	evicted     []string
	liveTo      string
	liveTimeout time.Duration
	migration   *haifypb.HaMigration
	syncFrom    string
}

func (m *mockGuestClient) MakeHaWith(_ context.Context, req *haifypb.MakeHaRequest) (string, error) {
	m.made = req
	return "/etc/drbd-reactor.d/haify-ha-" + req.GetResource() + ".toml", nil
}

func (m *mockGuestClient) EvictHa(_ context.Context, resource string) error {
	m.evicted = append(m.evicted, resource)
	return nil
}

func (m *mockGuestClient) EvictHaLive(_ context.Context, resource, target string, timeout time.Duration) (*haifypb.EvictHaResponse, error) {
	if resource != "web1" {
		return nil, fmt.Errorf("%s runs no libvirt guest", resource)
	}
	m.liveTo, m.liveTimeout = target, timeout
	return &haifypb.EvictHaResponse{Success: true, Started: true, SourceNode: "k1", TargetNode: "k2",
		Message: "moving web1 from k1 to k2 without stopping it"}, nil
}

func (m *mockGuestClient) GetHaMigration(_ context.Context, _ string) (*haifypb.HaMigration, error) {
	return m.migration, nil
}

func (m *mockGuestClient) WaitHaMigration(ctx context.Context, resource string, _ time.Duration, _ func(*haifypb.HaMigration)) (*haifypb.HaMigration, error) {
	return m.GetHaMigration(ctx, resource)
}

func (m *mockGuestClient) GetVmDefinition(_ context.Context, resource string) (*haifypb.VmDefinitionResponse, error) {
	return &haifypb.VmDefinitionResponse{Success: true, Resource: resource, Domain: "web1", Uuid: "6f1b2c3d", SourceNode: "k1",
		Message: "k3: the guest is not defined", Nodes: []*haifypb.VmDefinitionNode{
			{Node: "k1", State: "ok", Uuid: "6f1b2c3d", Running: true},
			{Node: "k3", State: "missing", Detail: "no definition"}}}, nil
}

func (m *mockGuestClient) SyncVmDefinition(_ context.Context, resource, from string) (*haifypb.VmDefinitionResponse, error) {
	m.syncFrom = from
	return &haifypb.VmDefinitionResponse{Success: true, Resource: resource, Domain: "web1", Consistent: true,
		Nodes: []*haifypb.VmDefinitionNode{{Node: "k1", State: "ok"}, {Node: "k3", State: "ok"}}}, nil
}

func TestMCPHaEvictLiveFollowsTheMove(t *testing.T) {
	c := &mockGuestClient{migration: &haifypb.HaMigration{Resource: "web1", Domain: "web1", SourceNode: "k1", TargetNode: "k2",
		Phase: "done", Done: true, Outcome: "moved", ActiveNode: "k2", ProgressPercent: 100,
		Message: "web1 now runs on k2. It was not stopped, and drbd-reactor manages it there"}}
	session := connect(t, c, false)

	var out haEvictOut
	callStructured(t, session, "haify_ha_evict", map[string]any{"resource": "web1", "live": true, "to": "k2", "timeout_seconds": 600}, &out)
	assert.Equal(t, "ok", out.Status)
	assert.Equal(t, "k2", c.liveTo)
	assert.Equal(t, 10*time.Minute, c.liveTimeout)
	assert.Empty(t, c.evicted, "a live move is not an eviction")
	require.NotNil(t, out.Migration)
	assert.True(t, out.Migration.Done)
	assert.Equal(t, "moved", out.Migration.Outcome)
	assert.Equal(t, "k2", out.Migration.ActiveNode)
	assert.Contains(t, out.Detail, "was not stopped")

	var st haMigrationStatusOut
	callStructured(t, session, "haify_ha_migration_status", map[string]any{"resource": "web1"}, &st)
	assert.True(t, st.Found)
	assert.Equal(t, "k1", st.Migration.SourceNode)
}

func TestMCPHaEvictWithoutLiveStaysAnEviction(t *testing.T) {
	c := &mockGuestClient{}
	session := connect(t, c, false)

	var out haEvictOut
	callStructured(t, session, "haify_ha_evict", map[string]any{"resource": "data"}, &out)
	assert.Equal(t, []string{"data"}, c.evicted)
	assert.Nil(t, out.Migration)

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "haify_ha_evict",
		Arguments: map[string]any{"resource": "data", "to": "k2"}})
	require.NoError(t, err)
	assert.True(t, res.IsError, "a target without live is refused, not ignored")
	assert.Equal(t, []string{"data"}, c.evicted)

	res, err = session.CallTool(t.Context(), &mcp.CallToolParams{Name: "haify_ha_evict",
		Arguments: map[string]any{"resource": "data", "live": true}})
	require.NoError(t, err)
	assert.True(t, res.IsError, "a resource that runs no guest cannot move live")

	var st haMigrationStatusOut
	callStructured(t, session, "haify_ha_migration_status", map[string]any{"resource": "data"}, &st)
	assert.False(t, st.Found)
}

func TestMCPGuestDefinitionTools(t *testing.T) {
	c := &mockGuestClient{}
	session := connect(t, c, false)

	var st vmDefinitionOut
	callStructured(t, session, "haify_vm_definition_status", map[string]any{"resource": "web1"}, &st)
	assert.False(t, st.Consistent)
	require.Len(t, st.Nodes, 2)
	assert.Equal(t, vmDefinitionNodeOut{Node: "k3", State: "missing", Detail: "no definition"}, st.Nodes[1])
	assert.True(t, st.Nodes[0].Running)

	var synced vmDefinitionOut
	callStructured(t, session, "haify_vm_definition_sync", map[string]any{"resource": "web1", "from": "k1"}, &synced)
	assert.True(t, synced.Consistent)
	assert.Equal(t, "k1", c.syncFrom)

	var made opResult
	callStructured(t, session, "haify_ha_create", map[string]any{"resource": "web1", "vm": "web1", "vm_from": "k1"}, &made)
	require.NotNil(t, c.made)
	assert.Equal(t, "k1", c.made.GetVmDefinitionNode())
	require.Len(t, c.made.GetOcfAgents(), 1)
	assert.Equal(t, "VirtualDomain", c.made.GetOcfAgents()[0].GetName())
}

func TestMCPGuestDefinitionSyncIsAWriteTool(t *testing.T) {
	ro := toolNames(t, New(&mockExtraClient{}, zap.NewNop(), Options{ReadOnly: true}))
	assert.True(t, ro["haify_vm_definition_status"])
	assert.True(t, ro["haify_ha_migration_status"])
	assert.False(t, ro["haify_vm_definition_sync"])
	assert.False(t, ro["haify_ha_evict"])
}

func (m *mockGuestClient) SetResourceLabels(_ context.Context, _ string, set map[string]string, _ []string) (map[string]string, error) {
	return set, nil
}
