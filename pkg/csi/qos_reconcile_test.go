package csi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestEffectiveQoSLabelsWinOverTheVolumeContext(t *testing.T) {
	ctx := map[string]string{"readIOPS": "100", "writeIOPS": "50", "readBytesPerSecond": "1000"}
	labels := map[string]string{
		"haify.csi/qos-readIOPS":            "900",
		"haify.csi/qos-writeIOPS":           "max",
		"haify.csi/qos-writeBytesPerSecond": "2048",
		"unrelated":                         "x",
	}
	assert.Equal(t, map[string]string{"readIOPS": "900", "readBytesPerSecond": "1000", "writeBytesPerSecond": "2048"},
		effectiveQoS(ctx, labels))
	assert.Equal(t, ctx, effectiveQoS(ctx, nil))
	assert.Empty(t, effectiveQoS(nil, nil))
}

func TestIOMaxLimitsListsEveryKey(t *testing.T) {
	assert.Equal(t, "rbps=1000 wbps=max riops=max wiops=7", ioMaxLimits(map[string]string{"readBytesPerSecond": "1000", "writeIOPS": "7"}))
	assert.Equal(t, "rbps=max wbps=max riops=max wiops=max", ioMaxLimits(nil))
}

// recordingWrites swaps the tracker's io.max writer for one that records.
func recordingWrites(tr *qosTracker, fail *error) *[]string {
	var got []string
	tr.write = func(device, target, limits string) error {
		if *fail != nil {
			return *fail
		}
		got = append(got, device+" "+target+" "+limits)
		return nil
	}
	return &got
}

func TestQoSTrackerWritesOnlyChanges(t *testing.T) {
	tr := newQoSTracker(zap.NewNop())
	var fail error
	writes := recordingWrites(tr, &fail)
	tr.track(qosRecord{VolumeID: "v", Target: "/t"})

	tr.apply("/dev/drbd1", "/t", nil)
	assert.Empty(t, *writes, "never limited: io.max is left alone")

	tr.apply("/dev/drbd1", "/t", map[string]string{"readIOPS": "10"})
	tr.apply("/dev/drbd1", "/t", map[string]string{"readIOPS": "10"})
	assert.Equal(t, []string{"/dev/drbd1 /t rbps=max wbps=max riops=10 wiops=max"}, *writes, "unchanged is not rewritten")

	tr.apply("/dev/drbd1", "/t", nil)
	assert.Equal(t, "/dev/drbd1 /t rbps=max wbps=max riops=max wiops=max", (*writes)[1], "a lifted limit is cleared")

	fail = errors.New("no io controller")
	tr.apply("/dev/drbd1", "/t", map[string]string{"readIOPS": "20"})
	fail = nil
	tr.apply("/dev/drbd1", "/t", map[string]string{"readIOPS": "20"})
	assert.Len(t, *writes, 3, "a failed write is retried on the next pass")
}

func TestReconcileQoSFollowsLabelChanges(t *testing.T) {
	b := newFakeBackend("n1")
	b.resources["pvc_a"] = &haifypb.ResourceInfo{Name: "pvc_a", Volumes: []*haifypb.VolumeInfo{{Device: "/dev/drbd7"}}}
	s := NewNodeServer(b, NewNopMounter(), "n1", zap.NewNop()).(*nodeServer)
	var fail error
	writes := recordingWrites(s.qos, &fail)

	target := filepath.Join(t.TempDir(), "mount")
	require.NoError(t, os.MkdirAll(target, 0o755))
	s.publishQoS(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId: "pvc_a", TargetPath: target, VolumeContext: map[string]string{"writeIOPS": "100"},
	}, "")
	require.Equal(t, []string{"/dev/drbd7 " + target + " rbps=max wbps=max riops=max wiops=100"}, *writes)

	s.reconcileQoS(context.Background())
	assert.Len(t, *writes, 1, "nothing changed")

	b.resources["pvc_a"].Labels = map[string]string{"haify.csi/qos-writeIOPS": "max", "haify.csi/qos-readIOPS": "5"}
	s.reconcileQoS(context.Background())
	require.Len(t, *writes, 2)
	assert.Equal(t, "/dev/drbd7 "+target+" rbps=max wbps=max riops=5 wiops=max", (*writes)[1])

	require.NoError(t, os.RemoveAll(target))
	s.reconcileQoS(context.Background())
	assert.Empty(t, s.qos.records, "an unpublished target is forgotten")
}

func TestPublishQoSUsesLabelsAlready(t *testing.T) {
	b := newFakeBackend("n1")
	b.resources["pvc_a"] = &haifypb.ResourceInfo{Name: "pvc_a",
		Labels:  map[string]string{"haify.csi/qos-readIOPS": "42"},
		Volumes: []*haifypb.VolumeInfo{{Device: "/dev/drbd7"}}}
	s := NewNodeServer(b, NewNopMounter(), "n1", zap.NewNop()).(*nodeServer)
	var fail error
	writes := recordingWrites(s.qos, &fail)
	s.publishQoS(context.Background(), &csi.NodePublishVolumeRequest{VolumeId: "pvc_a", TargetPath: "/t"}, "/dev/drbd9")
	assert.Equal(t, []string{"/dev/drbd9 /t rbps=max wbps=max riops=42 wiops=max"}, *writes,
		"a volume provisioned without limits gets the class's at publish")
}

func TestQoSRecordsSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	tr := newQoSTracker(zap.NewNop())
	tr.dir = dir
	tr.track(qosRecord{VolumeID: "pvc_a", Target: "/var/lib/kubelet/pods/x/mount", Context: map[string]string{"readIOPS": "1"}})
	tr.track(qosRecord{VolumeID: "pvc_b", Target: "/var/lib/kubelet/pods/y/mount"})
	require.NoError(t, os.WriteFile(filepath.Join(dir, "junk.json"), []byte("{"), 0o600))

	again := newQoSTracker(zap.NewNop())
	again.dir = dir
	again.load()
	require.Len(t, again.records, 2)
	assert.Equal(t, "1", again.records["/var/lib/kubelet/pods/x/mount"].Context["readIOPS"])

	again.forget("/var/lib/kubelet/pods/x/mount")
	third := newQoSTracker(zap.NewNop())
	third.dir = dir
	third.load()
	assert.Len(t, third.records, 1)
	_, err := os.Stat(filepath.Join(dir, "junk.json"))
	assert.True(t, os.IsNotExist(err), "an unreadable record is dropped")
}

func TestEnableQoSReconcileIgnoresOtherServers(t *testing.T) {
	EnableQoSReconcile(context.Background(), nil, "", 0)
	s := NewNodeServer(newFakeBackend("n1"), NewNopMounter(), "n1", zap.NewNop())
	EnableQoSReconcile(context.Background(), s, "", 0)
}
