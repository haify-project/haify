package mcpserver

import (
	"context"
	"testing"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/client"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func (m *mockExtraClient) GetSelfHaStatus(_ context.Context) (*client.SelfHaStatus, error) {
	return &client.SelfHaStatus{Enabled: true, Resource: "haify-meta", VIP: "10.0.0.250"}, nil
}

func (m *mockExtraClient) EnableSelfHa(_ context.Context, _, _ string, _, _ uint32, _ []string) (string, string, error) {
	return "/etc/ha", "script", nil
}

func (m *mockExtraClient) DisableSelfHa(_ context.Context, _ string) error {
	return nil
}

func (m *mockExtraClient) ListNFSExports(_ context.Context, _ string) ([]*haifypb.NFSExportInfo, error) {
	return []*haifypb.NFSExportInfo{{Directory: "/export"}}, nil
}

func (m *mockExtraClient) AddNFSExport(_ context.Context, _, _ string, _ int32, _, _ string) error {
	return nil
}

func (m *mockExtraClient) RemoveNFSExport(_ context.Context, _, _ string) error {
	return nil
}

func (m *mockExtraClient) ListISCSILUNs(_ context.Context, _ string) ([]*haifypb.ISCSILUNInfo, error) {
	return []*haifypb.ISCSILUNInfo{{Lun: 1, Device: "/dev/drbd0"}}, nil
}

func (m *mockExtraClient) AddISCSILUN(_ context.Context, _ string, _ int32, _ string) error {
	return nil
}

func (m *mockExtraClient) RemoveISCSILUN(_ context.Context, _ string, _ int32) error {
	return nil
}

func (m *mockExtraClient) ListNVMeNamespaces(_ context.Context, _ string) ([]*haifypb.NVMeNamespaceInfo, error) {
	return []*haifypb.NVMeNamespaceInfo{{NamespaceId: 1, BackingPath: "/dev/drbd0"}}, nil
}

func (m *mockExtraClient) AddNVMeNamespace(_ context.Context, _, _ string) error {
	return nil
}

func (m *mockExtraClient) RemoveNVMeNamespace(_ context.Context, _ string, _ int32) error {
	return nil
}

func TestMCPExtraSubTools(t *testing.T) {
	mc := &mockExtraClient{}
	session := connect(t, mc, false)

	// haify_self_ha_status
	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "haify_self_ha_status"})
	require.NoError(t, err)
	if res.IsError {
		t.Logf("tool error: %+v", res.Content[0])
	}
	assert.False(t, res.IsError)

	// haify_self_ha_enable
	res, err = session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "haify_self_ha_enable",
		Arguments: map[string]interface{}{
			"vip":  "10.0.0.250/24",
			"pool": "vg0",
		},
	})
	require.NoError(t, err)
	if res.IsError {
		t.Logf("tool error: %+v", res.Content[0])
	}
	assert.False(t, res.IsError)

	// haify_self_ha_disable
	res, err = session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "haify_self_ha_disable",
		Arguments: map[string]interface{}{
			"node": "n1",
		},
	})
	require.NoError(t, err)
	if res.IsError {
		t.Logf("tool error: %+v", res.Content[0])
	}
	assert.False(t, res.IsError)

	// haify_nfs_exports list, add, remove
	res, err = session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "haify_nfs_exports",
		Arguments: map[string]interface{}{"resource": "gw1", "action": "list"},
	})
	require.NoError(t, err)
	if res.IsError {
		t.Logf("tool error: %+v", res.Content[0])
	}
	assert.False(t, res.IsError)

	res, err = session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "haify_nfs_exports",
		Arguments: map[string]interface{}{
			"resource":    "gw1",
			"action":      "add",
			"export_path": "/export",
			"client_spec": "*",
		},
	})
	require.NoError(t, err)
	if res.IsError {
		t.Logf("tool error: %+v", res.Content[0])
	}
	assert.False(t, res.IsError)

	res, err = session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "haify_nfs_exports",
		Arguments: map[string]interface{}{
			"resource":    "gw1",
			"action":      "remove",
			"export_path": "/export",
		},
	})
	require.NoError(t, err)
	if res.IsError {
		t.Logf("tool error: %+v", res.Content[0])
	}
	assert.False(t, res.IsError)

	// haify_iscsi_luns list, add, remove
	res, err = session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "haify_iscsi_luns",
		Arguments: map[string]interface{}{"resource": "gw1", "action": "list"},
	})
	require.NoError(t, err)
	if res.IsError {
		t.Logf("tool error: %+v", res.Content[0])
	}
	assert.False(t, res.IsError)

	res, err = session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "haify_iscsi_luns",
		Arguments: map[string]interface{}{
			"resource": "gw1",
			"action":   "add",
			"lun":      1,
			"device":   "/dev/drbd0",
		},
	})
	require.NoError(t, err)
	if res.IsError {
		t.Logf("tool error: %+v", res.Content[0])
	}
	assert.False(t, res.IsError)

	res, err = session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "haify_iscsi_luns",
		Arguments: map[string]interface{}{
			"resource": "gw1",
			"action":   "remove",
			"lun":      1,
		},
	})
	require.NoError(t, err)
	if res.IsError {
		t.Logf("tool error: %+v", res.Content[0])
	}
	assert.False(t, res.IsError)

	// haify_nvme_namespaces list, add, remove
	res, err = session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "haify_nvme_namespaces",
		Arguments: map[string]interface{}{"resource": "gw1", "action": "list"},
	})
	require.NoError(t, err)
	if res.IsError {
		t.Logf("tool error: %+v", res.Content[0])
	}
	assert.False(t, res.IsError)

	res, err = session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "haify_nvme_namespaces",
		Arguments: map[string]interface{}{
			"resource": "gw1",
			"action":   "add",
			"device":   "/dev/drbd0",
		},
	})
	require.NoError(t, err)
	if res.IsError {
		t.Logf("tool error: %+v", res.Content[0])
	}
	assert.False(t, res.IsError)

	res, err = session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "haify_nvme_namespaces",
		Arguments: map[string]interface{}{
			"resource":     "gw1",
			"action":       "remove",
			"namespace_id": 1,
		},
	})
	require.NoError(t, err)
	if res.IsError {
		t.Logf("tool error: %+v", res.Content[0])
	}
	assert.False(t, res.IsError)
}

func (m *mockExtraClient) ListISCSIInitiators(_ context.Context, _ string) ([]string, error) {
	return []string{"iqn.init"}, nil
}

func (m *mockExtraClient) AddISCSIInitiator(_ context.Context, _, _ string) error { return nil }

func (m *mockExtraClient) RemoveISCSIInitiator(_ context.Context, _, _ string) error { return nil }

func (m *mockExtraClient) GetISCSIChap(_ context.Context, _ string) (*haifypb.GetISCSIChapResponse, error) {
	return &haifypb.GetISCSIChapResponse{Success: true, Username: "user"}, nil
}

func (m *mockExtraClient) SetISCSIChap(_ context.Context, _, _, _ string, _ bool) error { return nil }

func (m *mockExtraClient) ListNVMeHosts(_ context.Context, _ string) ([]string, error) {
	return []string{"nqn.host"}, nil
}

func (m *mockExtraClient) AddNVMeHost(_ context.Context, _, _ string) error { return nil }

func (m *mockExtraClient) RemoveNVMeHost(_ context.Context, _, _ string) error { return nil }

func (m *mockExtraClient) ListSnapshotSchedules(_ context.Context) ([]*haifypb.SnapshotScheduleInfo, error) {
	return []*haifypb.SnapshotScheduleInfo{{Resource: "res1", Cron: "0 * * * *"}}, nil
}

func (m *mockExtraClient) CreateSnapshotSchedule(_ context.Context, _, _ string, _ *haifypb.GFSRetention, _ bool) error {
	return nil
}

func (m *mockExtraClient) DeleteSnapshotSchedule(_ context.Context, _ string) error { return nil }

func (m *mockExtraClient) RestoreSnapshot(_ context.Context, _, _, _ string) error { return nil }

func (m *mockExtraClient) UpdateResourceOptions(_ context.Context, _ string, _ map[string]string) error {
	return nil
}

func (m *mockExtraClient) GetResourceOptions(_ context.Context, _ string) ([]*haifypb.ResourceOption, error) {
	return []*haifypb.ResourceOption{{Key: "net/max-buffers", Value: "8000"}, {Key: "net/shared-secret", Secret: true}}, nil
}

func (m *mockExtraClient) SetPrimary(_ context.Context, _, _ string, _ bool) error { return nil }

func (m *mockExtraClient) SetSecondary(_ context.Context, _, _ string) error { return nil }

func TestMCPExtraSubTools2(t *testing.T) {
	mc := &mockExtraClient{}
	session := connect(t, mc, false)

	// haify_iscsi_initiators list, add, remove
	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "haify_iscsi_initiators",
		Arguments: map[string]interface{}{"resource": "gw1", "action": "list"},
	})
	require.NoError(t, err)
	if res.IsError {
		t.Logf("tool error: %+v", res.Content[0])
	}
	assert.False(t, res.IsError)

	res, err = session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "haify_iscsi_initiators",
		Arguments: map[string]interface{}{"resource": "gw1", "action": "add", "iqn": "iqn.init"},
	})
	require.NoError(t, err)
	if res.IsError {
		t.Logf("tool error: %+v", res.Content[0])
	}
	assert.False(t, res.IsError)

	res, err = session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "haify_iscsi_initiators",
		Arguments: map[string]interface{}{"resource": "gw1", "action": "remove", "iqn": "iqn.init"},
	})
	require.NoError(t, err)
	if res.IsError {
		t.Logf("tool error: %+v", res.Content[0])
	}
	assert.False(t, res.IsError)

	// haify_iscsi_chap get, set
	res, err = session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "haify_iscsi_chap",
		Arguments: map[string]interface{}{"resource": "gw1", "action": "get"},
	})
	require.NoError(t, err)
	if res.IsError {
		t.Logf("tool error: %+v", res.Content[0])
	}
	assert.False(t, res.IsError)

	res, err = session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "haify_iscsi_chap",
		Arguments: map[string]interface{}{"resource": "gw1", "action": "set", "username": "user", "password": "pass"},
	})
	require.NoError(t, err)
	if res.IsError {
		t.Logf("tool error: %+v", res.Content[0])
	}
	assert.False(t, res.IsError)

	// haify_nvme_hosts list, add, remove
	res, err = session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "haify_nvme_hosts",
		Arguments: map[string]interface{}{"resource": "gw1", "action": "list"},
	})
	require.NoError(t, err)
	if res.IsError {
		t.Logf("tool error: %+v", res.Content[0])
	}
	assert.False(t, res.IsError)

	res, err = session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "haify_nvme_hosts",
		Arguments: map[string]interface{}{"resource": "gw1", "action": "add", "host_nqn": "nqn.host"},
	})
	require.NoError(t, err)
	if res.IsError {
		t.Logf("tool error: %+v", res.Content[0])
	}
	assert.False(t, res.IsError)

	res, err = session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "haify_nvme_hosts",
		Arguments: map[string]interface{}{"resource": "gw1", "action": "remove", "host_nqn": "nqn.host"},
	})
	require.NoError(t, err)
	if res.IsError {
		t.Logf("tool error: %+v", res.Content[0])
	}
	assert.False(t, res.IsError)

	// haify_snapshot_schedule_list, create, delete
	res, err = session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "haify_snapshot_schedule_list",
	})
	require.NoError(t, err)
	if res.IsError {
		t.Logf("tool error: %+v", res.Content[0])
	}
	assert.False(t, res.IsError)

	res, err = session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "haify_snapshot_schedule_create",
		Arguments: map[string]interface{}{"resource": "res1", "cron": "0 * * * *", "keep_daily": 7},
	})
	require.NoError(t, err)
	if res.IsError {
		t.Logf("tool error: %+v", res.Content[0])
	}
	assert.False(t, res.IsError)

	res, err = session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "haify_snapshot_schedule_delete",
		Arguments: map[string]interface{}{"name": "res1"},
	})
	require.NoError(t, err)
	if res.IsError {
		t.Logf("tool error: %+v", res.Content[0])
	}
	assert.False(t, res.IsError)

	// haify_snapshot_restore
	res, err = session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "haify_snapshot_restore",
		Arguments: map[string]interface{}{"resource": "res1", "name": "snap1", "node": "n1", "pool": "vg0"},
	})
	require.NoError(t, err)
	if res.IsError {
		t.Logf("tool error: %+v", res.Content[0])
	}
	assert.False(t, res.IsError)

	// haify_resource_get_options
	res, err = session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "haify_resource_get_options",
		Arguments: map[string]interface{}{"resource": "res1"},
	})
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Contains(t, res.Content[0].(*mcp.TextContent).Text, "net/max-buffers")

	// haify_resource_set_options
	res, err = session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "haify_resource_set_options",
		Arguments: map[string]interface{}{"resource": "res1", "options": map[string]string{"opt": "val"}},
	})
	require.NoError(t, err)
	if res.IsError {
		t.Logf("tool error: %+v", res.Content[0])
	}
	assert.False(t, res.IsError)
}
