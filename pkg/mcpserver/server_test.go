package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/client"
)

// mockClient embeds ControllerClient so only the methods a test exercises
// need implementations; calling anything else panics, which is a test bug.
type mockClient struct {
	ControllerClient

	listNodesFn      func(ctx context.Context) ([]*haifypb.NodeInfo, error)
	listPoolsFn      func(ctx context.Context) ([]*haifypb.PoolInfo, error)
	createPoolCalls  []string
	deleteResourceFn func(ctx context.Context, name string) error
	healthCheckFn    func(ctx context.Context, node string) (*client.NodeHealthInfo, error)
	lvmSnapshots     []string

	resourceStatusFn   func(ctx context.Context, name string) (*haifypb.ResourceStatus, error)
	createVolumesCalls [][]*haifypb.VolumeSpec
	promoteForNode     []string
	adoptFn            func(ctx context.Context, name string, nodes []string, port uint32, protocol string) (*haifypb.AdoptResourceResponse, error)
}

func (m *mockClient) UnregisterNode(ctx context.Context, address string) error { return nil }
func (m *mockClient) ListNodes(ctx context.Context) ([]*haifypb.NodeInfo, error) {
	return m.listNodesFn(ctx)
}

func (m *mockClient) ListPools(ctx context.Context) ([]*haifypb.PoolInfo, error) {
	return m.listPoolsFn(ctx)
}

func (m *mockClient) CreatePool(_ context.Context, name, poolType, node string, _ []string, _ uint64, _ bool) error {
	m.createPoolCalls = append(m.createPoolCalls, name+"/"+poolType+"@"+node)
	return nil
}

func (m *mockClient) DeleteResource(ctx context.Context, name string) error {
	return m.deleteResourceFn(ctx, name)
}

func (m *mockClient) HealthCheck(ctx context.Context, node string) (*client.NodeHealthInfo, error) {
	return m.healthCheckFn(ctx, node)
}

func (m *mockClient) CreateLvmSnapshot(_ context.Context, pool, lvName, snapshotName, node, size string) error {
	m.lvmSnapshots = append(m.lvmSnapshots, strings.Join([]string{pool, lvName, snapshotName, node, size}, "|"))
	return nil
}

func (m *mockClient) ResourceStatus(ctx context.Context, name string) (*haifypb.ResourceStatus, error) {
	return m.resourceStatusFn(ctx, name)
}

func (m *mockClient) CreateResourceWithVolumes(_ context.Context, _ string, _ uint32, _ []string, _, _ string, _ map[string]string, volumes []*haifypb.VolumeSpec) error {
	m.createVolumesCalls = append(m.createVolumesCalls, volumes)
	return nil
}

func (m *mockClient) PromoteForNode(_ context.Context, resource, node string) error {
	m.promoteForNode = append(m.promoteForNode, resource+"@"+node)
	return nil
}

func (m *mockClient) AdoptResource(ctx context.Context, name string, nodes []string, port uint32, protocol string) (*haifypb.AdoptResourceResponse, error) {
	return m.adoptFn(ctx, name, nodes, port, protocol)
}

// connect spins up the MCP server against an in-memory transport and
// returns a connected client session.
func connect(t *testing.T, mock ControllerClient, readOnly bool) *mcp.ClientSession {
	t.Helper()
	srv := New(mock, zap.NewNop(), Options{ReadOnly: readOnly, Version: "test"}).MCPServer()

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(t.Context(), serverTransport, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	cli := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "test"}, nil)
	session, err := cli.Connect(t.Context(), clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func listTools(t *testing.T, session *mcp.ClientSession) map[string]*mcp.Tool {
	t.Helper()
	res, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	tools := make(map[string]*mcp.Tool, len(res.Tools))
	for _, tool := range res.Tools {
		tools[tool.Name] = tool
	}
	return tools
}

// Adding a backup target takes an object-store secret, and a tool argument is
// recorded by whatever called it. There must be no tool for it — the CLI reads
// the secret from an env var or a file instead.
func TestNoToolAcceptsABackupSecret(t *testing.T) {
	session := connect(t, &mockClient{}, false)
	for name := range listTools(t, session) {
		if name == "haify_backup_target_add" || name == "haify_backup_target_create" {
			t.Errorf("%s would carry a credential through a tool call", name)
		}
	}
}

func TestToolRegistration(t *testing.T) {
	session := connect(t, &mockClient{}, false)
	tools := listTools(t, session)

	if len(tools) == 0 {
		t.Fatal("no tools registered")
	}
	// Every tool follows the haify_ naming convention and carries annotations.
	for name, tool := range tools {
		if !strings.HasPrefix(name, "haify_") {
			t.Errorf("tool %q does not use the haify_ prefix", name)
		}
		if tool.Description == "" {
			t.Errorf("tool %q has no description", name)
		}
		if tool.Annotations == nil || tool.Annotations.Title == "" {
			t.Errorf("tool %q has no annotations/title", name)
		}
	}
	// Spot-check the full surface is present.
	for _, want := range []string{
		"haify_node_list", "haify_node_register", "haify_node_health_check",
		"haify_pool_list", "haify_pool_create", "haify_pool_delete",
		"haify_resource_list", "haify_resource_create", "haify_resource_status", "haify_resource_adopt",
		"haify_snapshot_create", "haify_snapshot_restore",
		"haify_gateway_list", "haify_gateway_create_nfs", "haify_gateway_create_iscsi", "haify_gateway_create_nvme",
		"haify_nfs_exports", "haify_iscsi_luns", "haify_iscsi_chap", "haify_nvme_namespaces",
		"haify_ha_create", "haify_ha_evict", "haify_self_ha_status", "haify_self_ha_enable",
		// Observability: what the cluster did, not just what it is. Without
		// these an assistant can describe a cluster but not explain how it got
		// there, which is most of what gets asked after an incident.
		"haify_event_list", "haify_audit_list", "haify_log_list",
		// Inspection: the stored verdict on everything around the alerts.
		"haify_inspect_report", "haify_inspect_run",
		// ZFS: the controller has had this surface for a long time and the
		// client interface already declared it; none of it was ever exposed.
		"haify_zfs_pool_list", "haify_zfs_dataset_create", "haify_zfs_volume_create",
		"haify_zfs_volume_resize", "haify_zfs_snapshot_clone",
		// Topology and node lifecycle.
		"haify_resource_add_replica", "haify_resource_remove_replica",
		"haify_resource_attach_diskless", "haify_resource_detach_diskless",
		"haify_resource_set_tiebreaker", "haify_resource_add_dr", "haify_wan_repair",
		"haify_node_drain", "haify_node_undrain", "haify_node_set_labels", "haify_resource_set_labels",
		"haify_pool_convert_thin",
		"haify_ha_get_toml", "haify_ha_promoter_status",
		"haify_ocf_agent_list", "haify_ocf_agent_metadata",
		// Fast tier and off-cluster backups.
		"haify_pool_add_cache", "haify_pool_remove_cache",
		"haify_backup_list", "haify_backup_create", "haify_backup_restore", "haify_backup_delete",
		"haify_backup_target_list", "haify_backup_target_delete",
		"haify_backup_schedule_list", "haify_backup_schedule_create", "haify_backup_schedule_delete",
		"haify_backup_import",
	} {
		if _, found := tools[want]; !found {
			t.Errorf("expected tool %q not registered", want)
		}
	}
}

func TestDestructiveAnnotations(t *testing.T) {
	session := connect(t, &mockClient{}, false)
	tools := listTools(t, session)

	destructive := []string{
		"haify_resource_delete", "haify_pool_delete", "haify_snapshot_delete",
		"haify_snapshot_restore", "haify_gateway_delete", "haify_ha_delete", "haify_ha_evict",
	}
	for _, name := range destructive {
		tool, found := tools[name]
		if !found {
			t.Errorf("tool %q not registered", name)
			continue
		}
		ann := tool.Annotations
		if ann.DestructiveHint == nil || !*ann.DestructiveHint {
			t.Errorf("tool %q must carry DestructiveHint=true", name)
		}
		if ann.ReadOnlyHint {
			t.Errorf("tool %q must not be read-only", name)
		}
	}
	for _, name := range []string{"haify_node_list", "haify_pool_list", "haify_resource_status"} {
		if !tools[name].Annotations.ReadOnlyHint {
			t.Errorf("tool %q must carry ReadOnlyHint=true", name)
		}
	}
}

func TestReadOnlyMode(t *testing.T) {
	session := connect(t, &mockClient{}, true)
	tools := listTools(t, session)

	if len(tools) == 0 {
		t.Fatal("read-only mode registered no tools")
	}
	for name, tool := range tools {
		if !tool.Annotations.ReadOnlyHint {
			t.Errorf("read-only mode registered mutating tool %q", name)
		}
	}
	if _, found := tools["haify_resource_delete"]; found {
		t.Error("read-only mode must not register haify_resource_delete")
	}
	if _, found := tools["haify_node_list"]; !found {
		t.Error("read-only mode must register haify_node_list")
	}
}

func TestNodeListCall(t *testing.T) {
	mock := &mockClient{
		listNodesFn: func(context.Context) ([]*haifypb.NodeInfo, error) {
			return []*haifypb.NodeInfo{
				{Name: "orange1", Address: "192.168.123.214", State: "online", Version: "1.6.0"},
				{Name: "orange2", Address: "192.168.123.215", State: "online", Version: "1.6.0"},
			}, nil
		},
	}
	session := connect(t, mock, false)

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "haify_node_list"})
	if err != nil {
		t.Fatalf("call haify_node_list: %v", err)
	}
	if res.IsError {
		t.Fatalf("haify_node_list returned tool error: %v", res.Content)
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var out nodeListOut
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal structured content: %v", err)
	}
	if len(out.Nodes) != 2 || out.Nodes[0].Name != "orange1" || out.Nodes[1].State != "online" {
		t.Fatalf("unexpected node list output: %+v", out)
	}
}

func TestPoolCreateTypeDispatch(t *testing.T) {
	mock := &mockClient{}
	session := connect(t, mock, false)

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "haify_pool_create",
		Arguments: map[string]any{
			"name":    "data-pool",
			"type":    "lvm-thin",
			"nodes":   []string{"orange1", "orange2"},
			"devices": []string{"/dev/sdb"},
			"size_gb": 10,
		},
	})
	if err != nil {
		t.Fatalf("call haify_pool_create: %v", err)
	}
	if res.IsError {
		t.Fatalf("haify_pool_create returned tool error: %v", res.Content)
	}
	// lvm-thin must be normalized to the backend type thin_pool on every node.
	want := []string{"data-pool/thin_pool@orange1", "data-pool/thin_pool@orange2"}
	if len(mock.createPoolCalls) != 2 || mock.createPoolCalls[0] != want[0] || mock.createPoolCalls[1] != want[1] {
		t.Fatalf("unexpected CreatePool calls: %v", mock.createPoolCalls)
	}
}

func TestSnapshotNamingConvention(t *testing.T) {
	mock := &mockClient{}
	session := connect(t, mock, false)

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "haify_snapshot_create",
		Arguments: map[string]any{
			"resource": "data",
			"name":     "snap1",
			"node":     "orange1",
			"pool":     "haify_vg0",
		},
	})
	if err != nil {
		t.Fatalf("call haify_snapshot_create: %v", err)
	}
	if res.IsError {
		t.Fatalf("haify_snapshot_create returned tool error: %v", res.Content)
	}
	// LVM snapshots target LV "<resource>_data" with a default 1G COW size.
	if len(mock.lvmSnapshots) != 1 || mock.lvmSnapshots[0] != "haify_vg0|data_data|snap1|orange1|1G" {
		t.Fatalf("unexpected snapshot call: %v", mock.lvmSnapshots)
	}
}

func TestToolErrorPropagation(t *testing.T) {
	mock := &mockClient{
		deleteResourceFn: func(_ context.Context, name string) error {
			return context.DeadlineExceeded
		},
	}
	session := connect(t, mock, false)

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "haify_resource_delete",
		Arguments: map[string]any{"name": "data"},
	})
	if err != nil {
		t.Fatalf("protocol error instead of tool error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected IsError=true when the controller call fails")
	}
}

func TestResourceCreateMultiVolume(t *testing.T) {
	mock := &mockClient{}
	session := connect(t, mock, false)

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "haify_resource_create",
		Arguments: map[string]any{
			"name":  "data",
			"port":  7001,
			"nodes": []string{"orange1", "orange2"},
			"volumes": []map[string]any{
				{"size_gb": 10, "pool": "vg0"},
				{"size_gb": 20},
			},
		},
	})
	if err != nil {
		t.Fatalf("call haify_resource_create: %v", err)
	}
	if res.IsError {
		t.Fatalf("haify_resource_create returned tool error: %v", res.Content)
	}
	if len(mock.createVolumesCalls) != 1 {
		t.Fatalf("expected 1 multi-volume create call, got %d", len(mock.createVolumesCalls))
	}
	vols := mock.createVolumesCalls[0]
	if len(vols) != 2 {
		t.Fatalf("expected 2 VolumeSpecs, got %d", len(vols))
	}
	if vols[0].SizeGb != 10 || vols[0].Pool != "vg0" {
		t.Fatalf("unexpected volume[0]: %+v", vols[0])
	}
	if vols[1].SizeGb != 20 || vols[1].Pool != "" {
		t.Fatalf("unexpected volume[1]: %+v", vols[1])
	}
}

func TestResourceStatusSyncPercent(t *testing.T) {
	mock := &mockClient{
		resourceStatusFn: func(_ context.Context, name string) (*haifypb.ResourceStatus, error) {
			return &haifypb.ResourceStatus{
				Name: name,
				Role: "Primary",
				NodeStates: map[string]*haifypb.NodeResourceState{
					"orange2": {
						Role:             "Secondary",
						DiskState:        "Inconsistent",
						ReplicationState: "SyncTarget",
						SyncPercent:      42.5,
					},
				},
			}, nil
		},
	}
	session := connect(t, mock, false)

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "haify_resource_status",
		Arguments: map[string]any{"name": "data"},
	})
	if err != nil {
		t.Fatalf("call haify_resource_status: %v", err)
	}
	if res.IsError {
		t.Fatalf("haify_resource_status returned tool error: %v", res.Content)
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var out resourceOut
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal structured content: %v", err)
	}
	if len(out.NodeStates) != 1 {
		t.Fatalf("expected 1 node state, got %d", len(out.NodeStates))
	}
	ns := out.NodeStates[0]
	if ns.ReplicationState != "SyncTarget" || ns.SyncPercent != 42.5 {
		t.Fatalf("resync progress not surfaced: %+v", ns)
	}
}

func TestResourceSetRoleQuorumGuarded(t *testing.T) {
	mock := &mockClient{}
	session := connect(t, mock, false)

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "haify_resource_set_role",
		Arguments: map[string]any{
			"resource":       "data",
			"node":           "orange1",
			"role":           "primary",
			"quorum_guarded": true,
		},
	})
	if err != nil {
		t.Fatalf("call haify_resource_set_role: %v", err)
	}
	if res.IsError {
		t.Fatalf("haify_resource_set_role returned tool error: %v", res.Content)
	}
	if len(mock.promoteForNode) != 1 || mock.promoteForNode[0] != "data@orange1" {
		t.Fatalf("quorum_guarded did not route to PromoteForNode: %v", mock.promoteForNode)
	}
}

func TestResourceAdopt(t *testing.T) {
	mock := &mockClient{
		adoptFn: func(_ context.Context, name string, _ []string, _ uint32, _ string) (*haifypb.AdoptResourceResponse, error) {
			return &haifypb.AdoptResourceResponse{
				Success:  true,
				Nodes:    []string{"orange1", "orange2"},
				Port:     7005,
				Protocol: "C",
				Volumes:  2,
			}, nil
		},
	}
	session := connect(t, mock, false)

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "haify_resource_adopt",
		Arguments: map[string]any{"resource": "legacy"},
	})
	if err != nil {
		t.Fatalf("call haify_resource_adopt: %v", err)
	}
	if res.IsError {
		t.Fatalf("haify_resource_adopt returned tool error: %v", res.Content)
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var out resourceAdoptOut
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal structured content: %v", err)
	}
	if out.Resource != "legacy" || out.Port != 7005 || out.Volumes != 2 || len(out.Nodes) != 2 {
		t.Fatalf("unexpected adopt output: %+v", out)
	}
}

func TestInvalidInputRejected(t *testing.T) {
	session := connect(t, &mockClient{}, false)

	// Missing required field "name" must be rejected by schema validation
	// before the handler runs.
	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "haify_resource_delete",
		Arguments: map[string]any{},
	})
	if err == nil && !res.IsError {
		t.Fatal("expected missing required argument to be rejected")
	}
}
