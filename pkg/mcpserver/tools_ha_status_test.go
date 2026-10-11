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

func (m *mockExtraClient) GetHaStatus(_ context.Context, resource string) ([]*haifypb.HaPromoterStatus, error) {
	return []*haifypb.HaPromoterStatus{{
		DrbdResource: resource, Status: "active", PrimaryOn: "sds-e", ActiveNode: "node-e",
		Target: &haifypb.HaServiceStatus{Name: "drbd-services@openclaw.target", Status: "active", Kind: "target"},
		Deps: []*haifypb.HaServiceStatus{
			{Name: "var-lib-openclaw.mount", Status: "active", Kind: "mount", Label: "/var/lib/openclaw",
				SubState: "mounted", Since: "2026-10-08T08:06:41Z"},
			{Name: "openclaw.service", Status: "active", Kind: "service", Label: "OpenClaw Gateway", MainPid: 42},
		},
		Standbys: []*haifypb.HaStandby{{Node: "node-b", Reasons: []string{"disk Outdated"}, DiskState: "Outdated"}},
		Outside: []*haifypb.HaOutsideService{{Unit: "backup-agent.service", Description: "Nightly backup agent",
			Processes: 2, MountPoints: []string{"/var/lib/openclaw"}}},
		OutsideSessions: 1, OutsideSessionProcesses: 3,
	}}, nil
}

func callStructured(t *testing.T, session *mcp.ClientSession, tool string, args map[string]any, into any) {
	t.Helper()
	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: tool, Arguments: args})
	require.NoError(t, err)
	require.False(t, res.IsError, "%v", res.Content)
	raw, err := json.Marshal(res.StructuredContent)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, into))
}

func TestMCPHaStatusCarriesTheChain(t *testing.T) {
	session := connect(t, &mockExtraClient{}, true)

	var promoters haStatusOut
	callStructured(t, session, "haify_ha_promoter_status", map[string]any{"resource": "openclaw"}, &promoters)
	require.Len(t, promoters.Promoters, 1)
	p := promoters.Promoters[0]
	assert.Equal(t, "node-e", p.ActiveNode)
	require.Len(t, p.Chain, 3)
	assert.Equal(t, haServiceOut{Unit: "var-lib-openclaw.mount", Kind: "mount", Label: "/var/lib/openclaw",
		Status: "active", SubState: "mounted", Since: "2026-10-08T08:06:41Z"}, p.Chain[0])
	assert.Equal(t, int32(42), p.Chain[1].MainPID)
	assert.Equal(t, "drbd-services@openclaw.target", p.Chain[2].Unit, "the target comes last")
	assert.Equal(t, []haStandbyOut{{Node: "node-b", Reasons: []string{"disk Outdated"}, DiskState: "Outdated"}}, p.Standbys)
	assert.Equal(t, []haOutsideOut{{Unit: "backup-agent.service", Description: "Nightly backup agent",
		Processes: 2, MountPoints: []string{"/var/lib/openclaw"}}}, p.Outside)
	assert.Equal(t, int32(1), p.OutsideSessions)
	assert.Equal(t, int32(3), p.OutsideSessionProcesses)

	var cfg haConfigStatusOut
	callStructured(t, session, "haify_ha_status", map[string]any{"resource": "openclaw"}, &cfg)
	assert.Equal(t, "openclaw", cfg.Resource)
	require.NotNil(t, cfg.Promoter)
	assert.Equal(t, "node-e", cfg.Promoter.ActiveNode)
	assert.Len(t, cfg.Promoter.Chain, 3)
}
