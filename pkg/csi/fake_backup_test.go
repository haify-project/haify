package csi

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/client"
	"github.com/stretchr/testify/require"
)

// The real client serves the backup API the driver asserts for.
var _ backupBackend = (*client.HaifyClient)(nil)

// fakeBackupBackend is a fakeBackend with the controller's backup API. Its
// backup methods run on the driver's background goroutines, so they touch only
// their own state, under mu.
type fakeBackupBackend struct {
	*fakeBackend

	mu        sync.Mutex
	targets   []string
	backups   map[string]*haifypb.BackupInfo
	uploadErr error
	// gate, when set, holds every upload and restore until it is closed.
	gate       chan struct{}
	uploads    []string // "<resource>/<snapshot>@<node> -> <target>"
	restores   []string // "<backup> -> <resource>"
	restoreErr error
	deleted    []string // "<backup> force=<bool>"
	unlabelled []string // resources whose restoring label was removed
}

func newFakeBackupBackend(nodes ...string) *fakeBackupBackend {
	return &fakeBackupBackend{fakeBackend: newFakeBackend(nodes...), targets: []string{"offsite"},
		backups: map[string]*haifypb.BackupInfo{}}
}

func (f *fakeBackupBackend) wait(ctx context.Context) error {
	f.mu.Lock()
	gate := f.gate
	f.mu.Unlock()
	if gate == nil {
		return nil
	}
	select {
	case <-gate:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *fakeBackupBackend) ListBackupTargets(context.Context) ([]*haifypb.BackupTargetInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*haifypb.BackupTargetInfo, 0, len(f.targets))
	for _, t := range f.targets {
		out = append(out, &haifypb.BackupTargetInfo{Name: t})
	}
	return out, nil
}

func (f *fakeBackupBackend) CreateBackupFromSnapshot(ctx context.Context, resource, target, node, snapshot string) (*haifypb.BackupInfo, error) {
	id := resource + "_" + snapshot
	f.mu.Lock()
	f.uploads = append(f.uploads, fmt.Sprintf("%s/%s@%s -> %s", resource, snapshot, node, target))
	rec := &haifypb.BackupInfo{Id: id, Resource: resource, Target: target, Node: node, State: backupStateRunning,
		StartedAt: "2026-10-15T08:00:00Z", Volumes: []*haifypb.BackupVolumeInfo{{Bytes: 2*giB - 4<<20}}}
	f.backups[id] = rec
	f.mu.Unlock()

	err := f.wait(ctx)
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil {
		err = f.uploadErr
	}
	if err != nil {
		rec.State = backupStateFailed
		return nil, err
	}
	rec.State = backupStateCompleted
	return rec, nil
}

func (f *fakeBackupBackend) ListBackups(_ context.Context, resource, target string) ([]*haifypb.BackupInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*haifypb.BackupInfo
	for _, b := range f.backups {
		if (resource == "" || b.GetResource() == resource) && (target == "" || b.GetTarget() == target) {
			out = append(out, &haifypb.BackupInfo{Id: b.Id, Resource: b.Resource, Target: b.Target, Node: b.Node,
				State: b.State, StartedAt: b.StartedAt, Volumes: b.Volumes})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetId() < out[j].GetId() })
	return out, nil
}

func (f *fakeBackupBackend) RestoreBackup(ctx context.Context, id, resource, _ string) (*haifypb.BackupInfo, error) {
	f.mu.Lock()
	f.restores = append(f.restores, id+" -> "+resource)
	f.mu.Unlock()
	if err := f.wait(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.restoreErr != nil {
		return nil, f.restoreErr
	}
	return f.backups[id], nil
}

func (f *fakeBackupBackend) DeleteBackup(_ context.Context, id, _ string, force bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, fmt.Sprintf("%s force=%t", id, force))
	delete(f.backups, id)
	return nil
}

func (f *fakeBackupBackend) SetResourceLabels(_ context.Context, resource string, _ map[string]string, remove []string) (map[string]string, error) {
	r, ok := f.resources[resource]
	if !ok {
		return nil, fmt.Errorf("resource %q not found", resource)
	}
	for _, k := range remove {
		delete(r.Labels, k)
	}
	f.mu.Lock()
	f.unlabelled = append(f.unlabelled, resource)
	f.mu.Unlock()
	return r.Labels, nil
}

// putBackup records a backup as if the controller (or an import) had made it.
func (f *fakeBackupBackend) putBackup(b *haifypb.BackupInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.backups[b.GetId()] = b
}

func (f *fakeBackupBackend) backupState(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.backups[id].GetState()
}

// waitOp waits until the background operation key is no longer running.
func waitOp(t *testing.T, s *controllerServer, key string) {
	t.Helper()
	require.Eventually(t, func() bool {
		st, _ := s.ops.state(key)
		return st != opRunning
	}, 5*time.Second, 5*time.Millisecond, "operation %s still running", key)
}
