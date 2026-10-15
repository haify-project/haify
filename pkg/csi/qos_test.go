package csi

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

func TestParseQoS(t *testing.T) {
	q, err := parseQoS(map[string]string{"writeBytesPerSecond": "100Mi", "readIOPS": "5000"})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"writeBytesPerSecond": "104857600", "readIOPS": "5000"}, q)
	_, err = parseQoS(map[string]string{"readIOPS": "lots"})
	assert.Error(t, err)
	p, err := ParseVolumeParams(map[string]string{"pool": "vg0", "writeIOPS": "100"})
	require.NoError(t, err)
	assert.Equal(t, "100", volumeContextFor(false, p.QoS)["writeIOPS"])
	assert.Nil(t, volumeContextFor(false, nil))
}

// The limits go into the pod-level cgroup kubelet made for the pod, found by
// its UID under either cgroup driver, as "<major>:<minor> <limits>".
func TestApplyQoSWritesIOMax(t *testing.T) {
	root := t.TempDir()
	orig := cgroupRoot
	cgroupRoot = root
	t.Cleanup(func() { cgroupRoot = orig })
	uid := "0f4c6a2e-1111-2222-3333-444455556666"
	pod := filepath.Join(root, "kubepods.slice", "kubepods-burstable.slice",
		"kubepods-burstable-pod0f4c6a2e_1111_2222_3333_444455556666.slice")
	require.NoError(t, os.MkdirAll(pod, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "kubepods.slice", "cgroup.procs"), nil, 0o644))

	tr := newQoSTracker(zap.NewNop())
	target := "/var/lib/kubelet/pods/" + uid + "/volumes/kubernetes.io~csi/pvc-1/mount"
	tr.track(qosRecord{VolumeID: "pvc_1", Target: target})
	tr.apply("/dev/null", target, map[string]string{"writeIOPS": "100", "readBytesPerSecond": "1048576"})
	got, err := os.ReadFile(filepath.Join(pod, "io.max"))
	require.NoError(t, err)
	// /dev/null is 1:3 on Linux but not elsewhere (3:2 on macOS).
	var st unix.Stat_t
	require.NoError(t, unix.Stat("/dev/null", &st))
	dev := fmt.Sprintf("%d:%d", unix.Major(uint64(st.Rdev)), unix.Minor(uint64(st.Rdev)))
	// Every key is written, the unlimited ones as "max", so a lifted limit
	// really goes away when the line is rewritten.
	assert.Equal(t, dev+" rbps=1048576 wbps=max riops=max wiops=100", string(got))
}
