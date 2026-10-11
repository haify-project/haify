package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type profileMockClient struct {
	mockClient
	profiles       map[string]*haifypb.ResourceProfile
	profileErr     error
	deletedProfile string
	createRequest  *haifypb.CreateResourceRequest
	operations     []string
}

func (m *profileMockClient) RegisterNode(_ context.Context, name, address string) (*haifypb.NodeInfo, error) {
	m.operations = append(m.operations, "register:"+name)
	return &haifypb.NodeInfo{Name: name, Address: address, State: "online"}, nil
}

func (m *profileMockClient) DeletePool(_ context.Context, name, node string) error {
	m.operations = append(m.operations, "delete-pool:"+name+":"+node)
	return nil
}

func (m *profileMockClient) AddDiskToPool(_ context.Context, pool, disk, node string, _ bool) error {
	m.operations = append(m.operations, "add-disk:"+pool+":"+node+":"+disk)
	return nil
}

func (m *profileMockClient) AddVolume(_ context.Context, resource, volume, pool string, size uint32) error {
	m.operations = append(m.operations, "add:"+resource+":"+volume+":"+pool)
	return nil
}

func (m *profileMockClient) UpdateResourceOptions(_ context.Context, resource string, options map[string]string) error {
	m.operations = append(m.operations, "options:"+resource+":"+options["net/max-buffers"])
	return nil
}

func (m *profileMockClient) RemoveVolume(_ context.Context, resource string, volumeID uint32) error {
	m.operations = append(m.operations, "remove:"+resource)
	return nil
}

func (m *profileMockClient) ResizeVolume(_ context.Context, resource string, volumeID, size uint32) error {
	m.operations = append(m.operations, "resize:"+resource)
	return nil
}

func (m *profileMockClient) CreateFilesystem(_ context.Context, resource string, volumeID uint32, node, fstype string) error {
	m.operations = append(m.operations, "fs:"+resource+":"+fstype)
	return nil
}

func (m *profileMockClient) MountResource(_ context.Context, resource string, volumeID uint32, path, node, fstype string) error {
	m.operations = append(m.operations, "mount:"+resource+":"+fstype)
	return nil
}

func (m *profileMockClient) UnmountResource(_ context.Context, resource string, volumeID uint32, node string) error {
	m.operations = append(m.operations, "unmount:"+resource)
	return nil
}

func (m *profileMockClient) SetPrimary(_ context.Context, resource, node string, force bool) error {
	m.operations = append(m.operations, "primary:"+resource)
	return nil
}

func (m *profileMockClient) SetSecondary(_ context.Context, resource, node string) error {
	m.operations = append(m.operations, "secondary:"+resource)
	return nil
}

func (m *profileMockClient) CreateResourceProfile(_ context.Context, profile *haifypb.ResourceProfile) (*haifypb.ResourceProfile, error) {
	if m.profileErr != nil {
		return nil, m.profileErr
	}
	m.profiles[profile.Name] = profile
	return profile, nil
}

func (m *profileMockClient) GetResourceProfile(_ context.Context, name string) (*haifypb.ResourceProfile, error) {
	if m.profileErr != nil {
		return nil, m.profileErr
	}
	profile, ok := m.profiles[name]
	if !ok {
		return nil, errors.New("profile not found")
	}
	return profile, nil
}

func (m *profileMockClient) ListResourceProfiles(context.Context) ([]*haifypb.ResourceProfile, error) {
	if m.profileErr != nil {
		return nil, m.profileErr
	}
	out := make([]*haifypb.ResourceProfile, 0, len(m.profiles))
	for _, profile := range m.profiles {
		out = append(out, profile)
	}
	return out, nil
}

func (m *profileMockClient) DeleteResourceProfile(_ context.Context, name string) error {
	if m.profileErr != nil {
		return m.profileErr
	}
	m.deletedProfile = name
	delete(m.profiles, name)
	return nil
}

func (m *profileMockClient) CreateResourceRequest(_ context.Context, req *haifypb.CreateResourceRequest) error {
	if m.profileErr != nil {
		return m.profileErr
	}
	m.createRequest = req
	return nil
}

func decodeStructured[T any](t *testing.T, result *mcp.CallToolResult) T {
	t.Helper()
	raw, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err)
	var out T
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

func TestResourceProfileToolsCRUD(t *testing.T) {
	mock := &profileMockClient{profiles: map[string]*haifypb.ResourceProfile{}}
	session := connect(t, mock, false)
	tools := listTools(t, session)
	for _, name := range []string{
		"haify_resource_profile_create", "haify_resource_profile_get",
		"haify_resource_profile_list", "haify_resource_profile_delete",
	} {
		assert.Contains(t, tools, name)
	}

	createdResult, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "haify_resource_profile_create",
		Arguments: map[string]any{
			"name": "production", "protocol": "C", "storage_type": "lvm-thin",
			"pool": "fast", "replicas": 3,
			"replicas_on_different": []string{"zone", "rack"},
			"replicas_on_same":      []string{"region"},
			"drbd_options":          map[string]string{"net/max-buffers": "8000"},
			"labels":                map[string]string{"env": "prod"},
		},
	})
	require.NoError(t, err)
	require.False(t, createdResult.IsError)
	created := decodeStructured[resourceProfileOut](t, createdResult)
	assert.Equal(t, "production", created.Name)
	assert.Equal(t, uint32(3), created.Replicas)
	assert.Equal(t, []string{"zone", "rack"}, created.ReplicasOnDifferent)
	assert.Equal(t, "8000", created.DrbdOptions["net/max-buffers"])

	getResult, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "haify_resource_profile_get", Arguments: map[string]any{"name": "production"},
	})
	require.NoError(t, err)
	require.False(t, getResult.IsError)
	assert.Equal(t, "fast", decodeStructured[resourceProfileOut](t, getResult).Pool)

	listResult, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "haify_resource_profile_list"})
	require.NoError(t, err)
	require.False(t, listResult.IsError)
	listed := decodeStructured[resourceProfileListOut](t, listResult)
	require.Len(t, listed.Profiles, 1)
	assert.Equal(t, "production", listed.Profiles[0].Name)

	deleteResult, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "haify_resource_profile_delete", Arguments: map[string]any{"name": "production"},
	})
	require.NoError(t, err)
	require.False(t, deleteResult.IsError)
	assert.Equal(t, "production", mock.deletedProfile)
	assert.Empty(t, mock.profiles)
}

func TestResourceProfileToolsErrors(t *testing.T) {
	mock := &profileMockClient{
		profiles:   map[string]*haifypb.ResourceProfile{},
		profileErr: errors.New("profile store unavailable"),
	}
	session := connect(t, mock, false)

	for _, call := range []*mcp.CallToolParams{
		{Name: "haify_resource_profile_create", Arguments: map[string]any{"name": "production"}},
		{Name: "haify_resource_profile_get", Arguments: map[string]any{"name": "production"}},
		{Name: "haify_resource_profile_list"},
		{Name: "haify_resource_profile_delete", Arguments: map[string]any{"name": "production"}},
	} {
		result, err := session.CallTool(t.Context(), call)
		require.NoError(t, err)
		assert.True(t, result.IsError, call.Name)
	}
}

func TestResourceCreateUsesMetadataAwareRequest(t *testing.T) {
	mock := &profileMockClient{profiles: map[string]*haifypb.ResourceProfile{}}
	session := connect(t, mock, false)

	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "haify_resource_create",
		Arguments: map[string]any{
			"name": "data", "port": 7001, "nodes": []string{"n1", "n2"}, "profile": "production",
			"labels":  map[string]string{"app": "postgres"},
			"volumes": []map[string]any{{"size_gb": 10, "pool": "fast"}, {"size_gb": 20}},
		},
	})
	require.NoError(t, err)
	require.False(t, result.IsError)
	require.NotNil(t, mock.createRequest)
	assert.Equal(t, "data", mock.createRequest.Name)
	assert.Equal(t, "production", mock.createRequest.Profile)
	assert.Equal(t, "postgres", mock.createRequest.Labels["app"])
	assert.Equal(t, "C", mock.createRequest.Protocol)
	assert.Equal(t, "lvm", mock.createRequest.StorageType)
	require.Len(t, mock.createRequest.Volumes, 2)
	assert.Equal(t, uint32(10), mock.createRequest.Volumes[0].SizeGb)
	assert.Equal(t, "fast", mock.createRequest.Volumes[0].Pool)
}

func TestResourceCreateRejectsMetadataOnLegacyClient(t *testing.T) {
	session := connect(t, &mockClient{}, false)
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "haify_resource_create",
		Arguments: map[string]any{
			"name": "data", "port": 7001, "nodes": []string{"n1"}, "profile": "production",
		},
	})
	require.NoError(t, err)
	assert.True(t, result.IsError)
}

func TestProfileOutNil(t *testing.T) {
	assert.Equal(t, resourceProfileOut{}, profileOut(nil))
}

func TestResourceMutationTools(t *testing.T) {
	mock := &profileMockClient{profiles: map[string]*haifypb.ResourceProfile{}}
	session := connect(t, mock, false)
	calls := []*mcp.CallToolParams{
		{Name: "haify_resource_add_volume", Arguments: map[string]any{"resource": "data", "volume": "logs", "pool": "fast", "size_gb": 10}},
		{Name: "haify_resource_set_options", Arguments: map[string]any{"resource": "data", "options": map[string]string{"net/max-buffers": "8000"}}},
		{Name: "haify_resource_remove_volume", Arguments: map[string]any{"resource": "data", "volume_id": 1}},
		{Name: "haify_resource_resize_volume", Arguments: map[string]any{"resource": "data", "volume_id": 0, "size_gb": 20}},
		{Name: "haify_resource_create_filesystem", Arguments: map[string]any{"resource": "data", "volume_id": 0, "node": "n1", "fstype": "xfs"}},
		{Name: "haify_resource_mount", Arguments: map[string]any{"resource": "data", "volume_id": 0, "path": "/data", "node": "n1"}},
		{Name: "haify_resource_unmount", Arguments: map[string]any{"resource": "data", "volume_id": 0, "node": "n1"}},
		{Name: "haify_resource_set_role", Arguments: map[string]any{"resource": "data", "node": "n1", "role": "primary"}},
		{Name: "haify_resource_set_role", Arguments: map[string]any{"resource": "data", "node": "n1", "role": "secondary"}},
	}
	for _, call := range calls {
		result, err := session.CallTool(t.Context(), call)
		require.NoError(t, err)
		assert.False(t, result.IsError, call.Name)
	}
	require.Len(t, mock.operations, len(calls))
	assert.Contains(t, mock.operations, "mount:data:ext4")

	for _, call := range []*mcp.CallToolParams{
		{Name: "haify_resource_set_options", Arguments: map[string]any{"resource": "data", "options": map[string]string{}}},
		{Name: "haify_resource_set_role", Arguments: map[string]any{"resource": "data", "node": "n1", "role": "invalid"}},
		{Name: "haify_resource_create", Arguments: map[string]any{"name": "data", "port": 7001}},
	} {
		result, err := session.CallTool(t.Context(), call)
		require.NoError(t, err)
		assert.True(t, result.IsError, call.Name)
	}
}

func TestClusterMutationTools(t *testing.T) {
	mock := &profileMockClient{profiles: map[string]*haifypb.ResourceProfile{}}
	session := connect(t, mock, false)
	calls := []*mcp.CallToolParams{
		{Name: "haify_node_register", Arguments: map[string]any{"name": "n1", "address": "10.0.0.1"}},
		{Name: "haify_node_unregister", Arguments: map[string]any{"address": "10.0.0.1"}},
		{Name: "haify_pool_delete", Arguments: map[string]any{"name": "fast", "node": "n1"}},
		{Name: "haify_pool_add_disk", Arguments: map[string]any{"pool": "fast", "nodes": []string{"n1", "n2"}, "devices": []string{"/dev/sdb", "/dev/sdc"}}},
	}
	for _, call := range calls {
		result, err := session.CallTool(t.Context(), call)
		require.NoError(t, err)
		assert.False(t, result.IsError, call.Name)
	}
	assert.Contains(t, mock.operations, "register:n1")
	assert.Contains(t, mock.operations, "delete-pool:fast:n1")
	assert.Contains(t, mock.operations, "add-disk:fast:n2:/dev/sdc")

	for _, call := range []*mcp.CallToolParams{
		{Name: "haify_pool_create", Arguments: map[string]any{"name": "fast", "type": "unknown", "nodes": []string{"n1"}, "devices": []string{"/dev/sdb"}}},
		{Name: "haify_pool_create", Arguments: map[string]any{"name": "fast", "type": "lvm"}},
		{Name: "haify_pool_add_disk", Arguments: map[string]any{"pool": "fast"}},
	} {
		result, err := session.CallTool(t.Context(), call)
		require.NoError(t, err)
		assert.True(t, result.IsError, call.Name)
	}
}

func TestGatewayActionValidation(t *testing.T) {
	session := connect(t, &mockExtraClient{}, false)
	calls := []*mcp.CallToolParams{
		{Name: "haify_nfs_exports", Arguments: map[string]any{"resource": "gw", "action": "add"}},
		{Name: "haify_nfs_exports", Arguments: map[string]any{"resource": "gw", "action": "remove"}},
		{Name: "haify_nfs_exports", Arguments: map[string]any{"resource": "gw", "action": "invalid"}},
		{Name: "haify_iscsi_luns", Arguments: map[string]any{"resource": "gw", "action": "add", "lun": 1}},
		{Name: "haify_iscsi_luns", Arguments: map[string]any{"resource": "gw", "action": "invalid"}},
		{Name: "haify_iscsi_initiators", Arguments: map[string]any{"resource": "gw", "action": "add"}},
		{Name: "haify_iscsi_initiators", Arguments: map[string]any{"resource": "gw", "action": "remove"}},
		{Name: "haify_iscsi_initiators", Arguments: map[string]any{"resource": "gw", "action": "invalid"}},
		{Name: "haify_iscsi_chap", Arguments: map[string]any{"resource": "gw", "action": "set"}},
		{Name: "haify_iscsi_chap", Arguments: map[string]any{"resource": "gw", "action": "invalid"}},
		{Name: "haify_nvme_namespaces", Arguments: map[string]any{"resource": "gw", "action": "add"}},
		{Name: "haify_nvme_namespaces", Arguments: map[string]any{"resource": "gw", "action": "invalid"}},
		{Name: "haify_nvme_hosts", Arguments: map[string]any{"resource": "gw", "action": "add"}},
		{Name: "haify_nvme_hosts", Arguments: map[string]any{"resource": "gw", "action": "remove"}},
		{Name: "haify_nvme_hosts", Arguments: map[string]any{"resource": "gw", "action": "invalid"}},
	}
	for _, call := range calls {
		result, err := session.CallTool(t.Context(), call)
		require.NoError(t, err)
		assert.True(t, result.IsError, call.Name)
	}
	assert.EqualError(t, badAction("x", "list"), `invalid action "x" (use list)`)
}

func TestSnapshotDatasetHelpers(t *testing.T) {
	assert.Equal(t, "tank/data_data", zfsDataset("tank", "data"))
	assert.Equal(t, "tank/tank/data_data", zfsDataset("tank", "tank/data"))
	assert.Equal(t, "/data_data", zfsDataset("", "data"))
}
