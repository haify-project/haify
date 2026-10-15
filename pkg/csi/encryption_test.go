package csi

import (
	"context"
	"errors"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// withParams returns req with the given StorageClass parameters added.
func withParams(req *csi.CreateVolumeRequest, kv ...string) *csi.CreateVolumeRequest {
	for i := 0; i+1 < len(kv); i += 2 {
		req.Parameters[kv[i]] = kv[i+1]
	}
	return req
}

func TestParseSecurityParams(t *testing.T) {
	p, err := ParseVolumeParams(map[string]string{"pool": "vg0"})
	require.NoError(t, err)
	assert.False(t, p.Encryption, "encryption is off unless asked for")
	assert.False(t, p.ReplicationTLS)

	p, err = ParseVolumeParams(map[string]string{"pool": "vg0", "encryption": "true", "replicationTLS": "TRUE"})
	require.NoError(t, err)
	assert.True(t, p.Encryption)
	assert.True(t, p.ReplicationTLS)

	p, err = ParseVolumeParams(map[string]string{"pool": "vg0", "encryption": "false", "replicationTLS": ""})
	require.NoError(t, err)
	assert.False(t, p.Encryption)
	assert.False(t, p.ReplicationTLS)

	for _, bad := range []map[string]string{
		{"pool": "vg0", "encryption": "yes please"},
		{"pool": "vg0", "replicationTLS": "on"},
	} {
		_, err := ParseVolumeParams(bad)
		assert.Error(t, err, "%v", bad)
	}
}

// An encrypted StorageClass asks the controller for an encrypted resource and
// says so in the volume's context, which becomes the PV's volumeAttributes.
func TestCreateVolumeEncrypted(t *testing.T) {
	b := newFakeBackend("n1", "n2", "n3")
	resp, err := newTestController(b).CreateVolume(context.Background(), withParams(validCreateReq("pvc-enc"), "encryption", "true"))
	require.NoError(t, err)
	require.Len(t, b.requestCalls, 1)
	assert.True(t, b.requestCalls[0].GetEncrypt())
	assert.Equal(t, "true", resp.GetVolume().GetVolumeContext()["encryption"])
	_, hasTLS := resp.GetVolume().GetVolumeContext()["replicationTLS"]
	assert.False(t, hasTLS, "encryption at rest does not claim encrypted replication")
}

func TestCreateVolumePlaintextByDefault(t *testing.T) {
	b := newFakeBackend("n1", "n2", "n3")
	resp, err := newTestController(b).CreateVolume(context.Background(), validCreateReq("pvc-plain"))
	require.NoError(t, err)
	require.Len(t, b.requestCalls, 1)
	assert.False(t, b.requestCalls[0].GetEncrypt())
	assert.Empty(t, resp.GetVolume().GetVolumeContext())
}

// Where the controller would refuse encryption, CreateVolume refuses first,
// with InvalidArgument so the reason lands on the PVC, and creates nothing.
func TestCreateVolumeEncryptionRefusedWhereUnsupported(t *testing.T) {
	cases := map[string]func(b *fakeBackend, req *csi.CreateVolumeRequest){
		"zfs storage type": func(_ *fakeBackend, req *csi.CreateVolumeRequest) { req.Parameters["storageType"] = "zfs" },
		"zfs pool": func(b *fakeBackend, _ *csi.CreateVolumeRequest) {
			for _, p := range b.pools {
				p.Type = "zfs"
			}
		},
		"vdo pool": func(b *fakeBackend, _ *csi.CreateVolumeRequest) { b.pools[0].HasVdo = true },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			b := newFakeBackend("n1", "n2", "n3")
			req := withParams(validCreateReq("pvc-enc"), "encryption", "true")
			mutate(b, req)
			_, err := newTestController(b).CreateVolume(context.Background(), req)
			require.Error(t, err)
			assert.Equal(t, codes.InvalidArgument, status.Code(err))
			assert.Empty(t, b.createCalls)
		})
	}
	// The same pools are fine without encryption.
	b := newFakeBackend("n1", "n2", "n3")
	b.pools[0].HasVdo = true
	_, err := newTestController(b).CreateVolume(context.Background(), validCreateReq("pvc-plain"))
	require.NoError(t, err)
}

// A backend without the full create request cannot honour encryption; the
// volume is refused rather than created in plaintext.
type poolAndTypeOnlyBackend struct{ HaifyBackend }

func TestCreateVolumeEncryptionNeedsFullRequest(t *testing.T) {
	b := newFakeBackend("n1", "n2", "n3")
	_, err := newTestController(poolAndTypeOnlyBackend{b}).CreateVolume(context.Background(),
		withParams(validCreateReq("pvc-enc"), "encryption", "true"))
	require.Error(t, err)
	assert.Empty(t, b.createCalls)
}

// replicationTLS switches the new resource's links to TLS after creating it,
// and says so in the volume context.
func TestCreateVolumeReplicationTLS(t *testing.T) {
	b := newTLSFakeBackend("n1", "n2", "n3")
	resp, err := newTestController(b).CreateVolume(context.Background(),
		withParams(validCreateReq("pvc-tls"), "replicationTLS", "true", "encryption", "true"))
	require.NoError(t, err)
	require.Len(t, b.createCalls, 1)
	assert.Equal(t, []string{"pvc_tls=true populated=0"}, b.setCalls)
	vc := resp.GetVolume().GetVolumeContext()
	assert.Equal(t, "true", vc["replicationTLS"])
	assert.Equal(t, "true", vc["encryption"])
}

func TestCreateVolumeReplicationTLSNeedsBackendSupport(t *testing.T) {
	b := newFakeBackend("n1", "n2", "n3")
	_, err := newTestController(b).CreateVolume(context.Background(), withParams(validCreateReq("pvc-tls"), "replicationTLS", "true"))
	require.Error(t, err)
	assert.Empty(t, b.createCalls, "a volume that cannot get TLS is not created in the clear")
}

// Nodes that cannot carry an encrypted link are not placed on; too few ready
// nodes is refused before anything is created, with the remedy in the message.
func TestCreateVolumeReplicationTLSPlacement(t *testing.T) {
	b := newTLSFakeBackend("n1", "n2", "n3")
	b.notReady["n2"] = "no certificate"
	_, err := newTestController(b).CreateVolume(context.Background(), withParams(validCreateReq("pvc-tls"), "replicationTLS", "true"))
	require.NoError(t, err)
	require.Len(t, b.createCalls, 1)
	assert.ElementsMatch(t, []string{"n1", "n3"}, b.createCalls[0].nodes)

	b = newTLSFakeBackend("n1", "n2", "n3")
	b.notReady["n2"] = "no certificate"
	b.notReady["n3"] = "tlshd not running"
	_, err = newTestController(b).CreateVolume(context.Background(), withParams(validCreateReq("pvc-tls"), "replicationTLS", "true"))
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	assert.Contains(t, err.Error(), "n2 (no certificate)")
	assert.Contains(t, err.Error(), "haify replication-tls setup")
	assert.Empty(t, b.createCalls)
	assert.Empty(t, b.setCalls)
}

// The node the scheduler picked must not be dropped quietly: the Pod would be
// left on a node with no replica. ResourceExhausted lets it reschedule.
func TestCreateVolumeReplicationTLSPinnedNodeNotReady(t *testing.T) {
	b := newTLSFakeBackend("n1", "n2", "n3")
	b.notReady["n1"] = "no certificate"
	req := withParams(validCreateReq("pvc-tls"), "replicationTLS", "true")
	req.AccessibilityRequirements = &csi.TopologyRequirement{Preferred: []*csi.Topology{{Segments: map[string]string{TopologyKeyNode: "n1"}}}}
	_, err := newTestController(b).CreateVolume(context.Background(), req)
	require.Error(t, err)
	assert.Equal(t, codes.ResourceExhausted, status.Code(err))
	assert.Contains(t, err.Error(), `node "n1"`)
	assert.Empty(t, b.createCalls)
}

func TestCreateVolumeReplicationTLSStatusUnreadable(t *testing.T) {
	b := newTLSFakeBackend("n1", "n2", "n3")
	b.statusErr = errors.New("controller unavailable")
	_, err := newTestController(b).CreateVolume(context.Background(), withParams(validCreateReq("pvc-tls"), "replicationTLS", "true"))
	require.Error(t, err)
	assert.Empty(t, b.createCalls)
}

// A volume whose TLS switch failed is removed again, not handed out
// replicating in the clear.
func TestCreateVolumeReplicationTLSSwitchFailureRollsBack(t *testing.T) {
	b := newTLSFakeBackend("n1", "n2", "n3")
	b.setErr = errors.New("not every node can carry an encrypted connection: n4 (no certificate)")
	_, err := newTestController(b).CreateVolume(context.Background(), withParams(validCreateReq("pvc-tls"), "replicationTLS", "true"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "n4 (no certificate)")
	assert.Contains(t, err.Error(), "haify replication-tls setup")
	require.Len(t, b.createCalls, 1)
	_, exists := b.resources["pvc_tls"]
	assert.False(t, exists, "the half-made volume is deleted")
}

// A retry for a volume that already exists finishes the TLS switch (an
// earlier attempt may have stopped before it) and reports the flags.
func TestCreateVolumeExistingFinishesTLS(t *testing.T) {
	b := newTLSFakeBackend("n1", "n2", "n3")
	b.resources["pvc_tls"] = &haifypb.ResourceInfo{Name: "pvc_tls", Nodes: []string{"n1", "n2"}, Encrypted: true}
	resp, err := newTestController(b).CreateVolume(context.Background(),
		withParams(validCreateReq("pvc-tls"), "replicationTLS", "true", "encryption", "true"))
	require.NoError(t, err)
	assert.Empty(t, b.createCalls)
	assert.Equal(t, []string{"pvc_tls=true populated=0"}, b.setCalls)
	assert.Equal(t, "true", resp.GetVolume().GetVolumeContext()["encryption"])
	assert.Equal(t, "true", resp.GetVolume().GetVolumeContext()["replicationTLS"])
}

// An existing unencrypted volume does not satisfy a class that asks for
// encryption, and encryption cannot be added in place.
func TestCreateVolumeExistingUnencryptedRefused(t *testing.T) {
	b := newFakeBackend("n1", "n2", "n3")
	b.resources["pvc_enc"] = &haifypb.ResourceInfo{Name: "pvc_enc", Nodes: []string{"n1", "n2"}}
	_, err := newTestController(b).CreateVolume(context.Background(), withParams(validCreateReq("pvc-enc"), "encryption", "true"))
	require.Error(t, err)
	assert.Equal(t, codes.AlreadyExists, status.Code(err))
}
