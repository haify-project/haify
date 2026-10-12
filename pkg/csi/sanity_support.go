package csi

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// sanityBackend is a concurrency-safe in-memory HaifyBackend for the CSI sanity
// suite. It is exported via NewSanityFakeBackend.
type sanityBackend struct {
	mu        sync.Mutex
	resources map[string]*haifypb.ResourceInfo
	nodes     []*haifypb.NodeInfo
	pools     []*haifypb.PoolInfo
	// snapshots is keyed "<backing volume>|<node>", matching the backend's
	// per-node snapshot namespace.
	snapshots map[string][]*haifypb.SnapshotInfo
}

// NewSanityFakeBackend builds a fake backend seeded with the given node names.
// Every node hosts the "vg0" pool the sanity suite provisions against.
func NewSanityFakeBackend(nodeNames ...string) HaifyBackend {
	zap.L().Debug("NewSanityFakeBackend: seeding", zap.Strings("nodes", nodeNames))
	b := &sanityBackend{resources: map[string]*haifypb.ResourceInfo{}}
	for i, n := range nodeNames {
		addr := fmt.Sprintf("10.0.0.%d", i+1)
		b.nodes = append(b.nodes, &haifypb.NodeInfo{
			Name:    n,
			Address: addr,
			State:   "online",
		})
		b.pools = append(b.pools, &haifypb.PoolInfo{Name: "haify_vg0", Node: addr})
	}
	return b
}

func (b *sanityBackend) CreateResourceWithPoolAndType(_ context.Context, name string, port uint32, nodes []string, _ string, sizeGB uint32, pool, _ string, _ map[string]string) error {
	zap.L().Debug("sanityBackend.CreateResourceWithPoolAndType: creating", zap.String("resource", name),
		zap.Strings("nodes", nodes), zap.Uint32("size_gb", sizeGB), zap.String("pool", pool))
	b.mu.Lock()
	defer b.mu.Unlock()
	b.resources[name] = &haifypb.ResourceInfo{
		Name:   name,
		Nodes:  nodes,
		Port:   port,
		Labels: map[string]string{managedByLabel: managedByValue},
		Volumes: []*haifypb.VolumeInfo{{
			VolumeId: 0,
			Device:   "/dev/drbd100",
			SizeGb:   uint64(sizeGB),
			Pool:     pool,
			// The controller names every backing LV "<resource>_data" and
			// reports it. Leaving it empty here made every snapshot and clone
			// path fail before reaching the code under test.
			BackingVolume: name + "_data",
		}},
	}
	return nil
}

func (b *sanityBackend) GetResource(_ context.Context, name string) (*haifypb.ResourceInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if r, ok := b.resources[name]; ok {
		zap.L().Debug("sanityBackend.GetResource: found", zap.String("resource", name))
		return r, nil
	}
	zap.L().Debug("sanityBackend.GetResource: not found", zap.String("resource", name))
	return nil, fmt.Errorf("resource %q not found", name)
}

func (b *sanityBackend) ListResources(context.Context) ([]*haifypb.ResourceInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]*haifypb.ResourceInfo, 0, len(b.resources))
	for _, r := range b.resources {
		out = append(out, r)
	}
	return out, nil
}

func (b *sanityBackend) DeleteResource(_ context.Context, name string) error {
	zap.L().Debug("sanityBackend.DeleteResource: deleting", zap.String("resource", name))
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.resources, name)
	return nil
}

func (b *sanityBackend) ListNodes(_ context.Context) ([]*haifypb.NodeInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]*haifypb.NodeInfo, len(b.nodes))
	copy(out, b.nodes)
	return out, nil
}

func (b *sanityBackend) ListPools(_ context.Context) ([]*haifypb.PoolInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]*haifypb.PoolInfo, len(b.pools))
	copy(out, b.pools)
	return out, nil
}

func (b *sanityBackend) RegisterNode(_ context.Context, name, address string) (*haifypb.NodeInfo, error) {
	return &haifypb.NodeInfo{Name: name, Address: address}, nil
}

func (b *sanityBackend) CreateSnapshot(_ context.Context, volume, snapshotName, node string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.snapshots == nil {
		b.snapshots = map[string][]*haifypb.SnapshotInfo{}
	}
	k := volume + "|" + node
	for _, s := range b.snapshots[k] {
		if s.GetName() == snapshotName {
			zap.L().Debug("sanityBackend.CreateSnapshot: exists (idempotent)", zap.String("volume", volume),
				zap.String("snapshot", snapshotName), zap.String("node", node))
			return nil // idempotent
		}
	}
	zap.L().Debug("sanityBackend.CreateSnapshot: creating", zap.String("volume", volume),
		zap.String("snapshot", snapshotName), zap.String("node", node))
	// The controller reports each snapshot's creation time (lv_time), and CSI
	// makes creation_time required, so the fake has to carry one too.
	b.snapshots[k] = append(b.snapshots[k], &haifypb.SnapshotInfo{Name: snapshotName, Volume: volume,
		CreatedAt: time.Now().UTC().Format(time.RFC3339)})
	return nil
}

func (b *sanityBackend) DeleteSnapshot(_ context.Context, volume, snapshotName, node string) error {
	zap.L().Debug("sanityBackend.DeleteSnapshot: deleting", zap.String("volume", volume),
		zap.String("snapshot", snapshotName), zap.String("node", node))
	b.mu.Lock()
	defer b.mu.Unlock()
	k := volume + "|" + node
	var kept []*haifypb.SnapshotInfo
	for _, s := range b.snapshots[k] {
		if s.GetName() != snapshotName {
			kept = append(kept, s)
		}
	}
	b.snapshots[k] = kept
	return nil
}

func (b *sanityBackend) ListSnapshots(_ context.Context, volume, node string) ([]*haifypb.SnapshotInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	src := b.snapshots[volume+"|"+node]
	out := make([]*haifypb.SnapshotInfo, len(src))
	copy(out, src)
	return out, nil
}

func (b *sanityBackend) PopulateVolumeFromSnapshot(_ context.Context, _ string, _ uint32, _ *haifypb.PopulateSnapshotSource, _ string) (uint64, error) {
	return 0, nil
}

func (b *sanityBackend) SetPrimary(_ context.Context, _, _ string, _ bool) error   { return nil }
func (b *sanityBackend) PromoteForNode(_ context.Context, _, _ string) error       { return nil }
func (b *sanityBackend) SetSecondary(_ context.Context, _, _ string) error         { return nil }
func (b *sanityBackend) AttachDisklessClient(_ context.Context, _, _ string) error { return nil }
func (b *sanityBackend) DetachDisklessClient(_ context.Context, _, _ string) error { return nil }
func (b *sanityBackend) ResizeVolume(_ context.Context, _ string, _ uint32, _ uint32) error {
	return nil
}

var _ HaifyBackend = (*sanityBackend)(nil)

// nopMounter is a no-op Mounter used in the sanity suite to satisfy the node
// server without performing real filesystem operations.
type nopMounter struct {
	mu     sync.Mutex
	mounts map[string]bool
}

// NewNopMounter returns a no-op Mounter for tests (no real filesystem ops).
func NewNopMounter() Mounter {
	return &nopMounter{mounts: map[string]bool{}}
}

func (m *nopMounter) FormatAndMount(_, target, _ string, _ []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mounts[target] = true
	return nil
}

func (m *nopMounter) Mount(_, target, _ string, _ []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mounts[target] = true
	return nil
}

func (m *nopMounter) Unmount(target string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.mounts, target)
	return nil
}

func (m *nopMounter) IsMountPoint(target string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mounts[target], nil
}

func (m *nopMounter) EnsureDir(target string) error {
	return os.MkdirAll(target, 0o750)
}

func (m *nopMounter) ResizeFS(_, _ string) error { return nil }

func (m *nopMounter) EnsureFile(target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return err
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_RDONLY, 0o640)
	if err != nil {
		return err
	}
	return f.Close()
}

// FSStats reports a fixed 1GiB filesystem, a quarter used: the sanity suite
// checks that stats come back for a published volume, not what they say.
func (m *nopMounter) FSStats(string) (FSStats, error) {
	return FSStats{TotalBytes: giB, AvailableBytes: 3 * giB / 4, UsedBytes: giB / 4,
		TotalInodes: 65536, AvailableInodes: 65000, UsedInodes: 536}, nil
}

func (m *nopMounter) BlockSize(string) (int64, error) { return giB, nil }

var _ Mounter = (*nopMounter)(nil)
