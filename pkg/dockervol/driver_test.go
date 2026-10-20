package dockervol

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/csi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeBackend struct {
	res      map[string]*haifypb.ResourceInfo
	calls    []string
	created  *haifypb.CreateResourceRequest
	failNext error
}

func (f *fakeBackend) CreateResourceRequest(_ context.Context, req *haifypb.CreateResourceRequest) error {
	f.created = req
	f.res[req.Name] = &haifypb.ResourceInfo{Name: req.Name, Nodes: []string{"s1", "s2"}, Labels: req.Labels}
	return nil
}
func (f *fakeBackend) GetResource(_ context.Context, name string) (*haifypb.ResourceInfo, error) {
	if r := f.res[name]; r != nil {
		return r, nil
	}
	return nil, fmt.Errorf("not found")
}
func (f *fakeBackend) ListResources(context.Context) ([]*haifypb.ResourceInfo, error) {
	var out []*haifypb.ResourceInfo
	for _, r := range f.res {
		out = append(out, r)
	}
	return out, nil
}
func (f *fakeBackend) DeleteResource(_ context.Context, name string) error {
	delete(f.res, name)
	f.calls = append(f.calls, "delete "+name)
	return nil
}
func (f *fakeBackend) AttachDisklessClient(_ context.Context, res, node string) error {
	f.calls = append(f.calls, "attach "+res+" "+node)
	f.res[res].DisklessClients = append(f.res[res].DisklessClients, node)
	return nil
}
func (f *fakeBackend) PromoteForNode(_ context.Context, res, node string) error {
	f.calls = append(f.calls, "promote "+res+" "+node)
	return f.failNext
}
func (f *fakeBackend) SetSecondary(_ context.Context, res, node string) error {
	f.calls = append(f.calls, "secondary "+res+" "+node)
	return nil
}

type fakeMounter struct {
	csi.Mounter
	mounted map[string]string // target -> fs
}

func (m *fakeMounter) FormatAndMount(_, target, fs string, _ []string) error {
	m.mounted[target] = fs
	return nil
}
func (m *fakeMounter) Unmount(target string) error         { delete(m.mounted, target); return nil }
func (m *fakeMounter) IsMountPoint(t string) (bool, error) { _, ok := m.mounted[t]; return ok, nil }
func (m *fakeMounter) EnsureDir(string) error              { return nil }

func newTestDriver(t *testing.T) (*Driver, *fakeBackend, *fakeMounter, http.Handler) {
	t.Helper()
	b := &fakeBackend{res: map[string]*haifypb.ResourceInfo{}}
	m := &fakeMounter{mounted: map[string]string{}}
	d := New(b, m, "web1", "/var/lib/haify-docker/volumes", Defaults{SizeBytes: 10 << 30, Pool: "vg0", Replicas: 2, FS: "ext4"})
	dev := filepath.Join(t.TempDir(), "dev")
	require.NoError(t, os.WriteFile(dev, nil, 0o600))
	d.device = func(string) string { return dev }
	return d, b, m, Handler(d)
}

func call(t *testing.T, h http.Handler, path string, body any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw)))
	out := map[string]any{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out), w.Body.String())
	return out
}

// The life of a volume as Docker drives it, on a host with no replica.
func TestVolumeLifecycle(t *testing.T) {
	_, b, m, h := newTestDriver(t)

	assert.Equal(t, []any{"VolumeDriver"}, call(t, h, "/Plugin.Activate", nil)["Implements"])
	assert.Equal(t, map[string]any{"Scope": "global"}, call(t, h, "/VolumeDriver.Capabilities", nil)["Capabilities"])

	out := call(t, h, "/VolumeDriver.Create", map[string]any{"Name": "pg.data", "Opts": map[string]string{"size": "20G", "fs": "xfs"}})
	assert.Empty(t, out["Err"])
	require.NotNil(t, b.created)
	assert.Equal(t, "docker-pg_data", b.created.Name, "dots become underscores in the DRBD name")
	assert.Equal(t, uint64(20<<30), b.created.SizeBytes)
	assert.Equal(t, "vg0", b.created.Pool)
	assert.Equal(t, uint32(2), b.created.Replicas)
	assert.Equal(t, "pg.data", b.created.Labels[LabelVolume])

	assert.Empty(t, call(t, h, "/VolumeDriver.Create", map[string]any{"Name": "pg.data"})["Err"],
		"creating an existing volume again is how docker run -v reuses it")

	out = call(t, h, "/VolumeDriver.Mount", map[string]any{"Name": "pg.data", "ID": "c1"})
	assert.Equal(t, "/var/lib/haify-docker/volumes/pg.data", out["Mountpoint"])
	assert.Equal(t, []string{"attach docker-pg_data web1", "promote docker-pg_data web1"}, b.calls,
		"a host without a replica joins as a diskless client before it promotes")
	assert.Equal(t, "xfs", m.mounted["/var/lib/haify-docker/volumes/pg.data"])

	call(t, h, "/VolumeDriver.Mount", map[string]any{"Name": "pg.data", "ID": "c2"})
	assert.Len(t, b.calls, 2, "a second container shares the mount")
	call(t, h, "/VolumeDriver.Unmount", map[string]any{"Name": "pg.data", "ID": "c1"})
	assert.Len(t, m.mounted, 1, "still used by c2")
	call(t, h, "/VolumeDriver.Unmount", map[string]any{"Name": "pg.data", "ID": "c2"})
	assert.Empty(t, m.mounted)
	assert.Equal(t, "secondary docker-pg_data web1", b.calls[len(b.calls)-1], "the last one out gives up Primary")

	vols := call(t, h, "/VolumeDriver.List", nil)["Volumes"].([]any)
	require.Len(t, vols, 1)
	assert.Equal(t, "pg.data", vols[0].(map[string]any)["Name"])

	assert.Empty(t, call(t, h, "/VolumeDriver.Remove", map[string]any{"Name": "pg.data"})["Err"])
	assert.Contains(t, call(t, h, "/VolumeDriver.Get", map[string]any{"Name": "pg.data"})["Err"], "no such volume")
}

// Podman takes any 200 answer as success, so a failure has to be a 500 too.
func TestFailureIsHTTP500(t *testing.T) {
	_, _, _, h := newTestDriver(t)
	raw, _ := json.Marshal(map[string]any{"Name": "missing"})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/VolumeDriver.Get", bytes.NewReader(raw)))
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "no such volume")

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/Plugin.Activate", nil))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestMountFailureReleasesTheRole(t *testing.T) {
	d, b, m, _ := newTestDriver(t)
	require.NoError(t, d.Create(context.Background(), "v", nil))
	b.res["docker-v"].Nodes = []string{"web1", "s2"}
	d.device = func(string) string { return "/nonexistent/device" }
	d.deviceWait = 0
	_, err := d.Mount(context.Background(), "v", "c1")
	require.Error(t, err)
	assert.Equal(t, []string{"promote docker-v web1", "secondary docker-v web1"}, b.calls,
		"a replica host promotes without attaching, and gives the role back when the mount fails")
	assert.Empty(t, m.mounted)
}

func TestCreateRejectsBadInput(t *testing.T) {
	d, _, _, _ := newTestDriver(t)
	for name, opts := range map[string]map[string]string{
		"v": {"size": "lots"}, "w": {"replicas": "0"}, "x": {"fs": "btrfs"}, "y": {"colour": "red"},
	} {
		assert.Error(t, d.Create(context.Background(), name, opts), name)
	}
	assert.Error(t, d.Create(context.Background(), "../etc", nil))
	d.defaults.Pool = ""
	assert.ErrorContains(t, d.Create(context.Background(), "z", nil), "no pool")
}
