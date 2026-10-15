package csi

import (
	"context"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestParseDRParams(t *testing.T) {
	dr, err := parseDRParams(map[string]string{"pool": "vg0"})
	require.NoError(t, err)
	assert.Equal(t, drParams{}, dr, "no DR replica unless asked")

	dr, err = parseDRParams(map[string]string{paramDRNode: "dr1", paramDREndpoint: "dr.example.com",
		paramDRWANPort: "37901", paramDREgressAddress: "203.0.113.20"})
	require.NoError(t, err)
	assert.Equal(t, drParams{node: "dr1", endpoint: "dr.example.com", wanPort: 37901, egress: "203.0.113.20"}, dr)

	dr, err = parseDRParams(map[string]string{paramDRNode: "dr1", paramDREndpoint: "2001:db8::1"})
	require.NoError(t, err, "an IPv6 address is an address, not a host with a port")
	assert.Equal(t, "2001:db8::1", dr.endpoint)

	for name, p := range map[string]map[string]string{
		"node without endpoint": {paramDRNode: "dr1"},
		"endpoint without node": {paramDREndpoint: "dr.example.com"},
		"endpoint with a port":  {paramDRNode: "dr1", paramDREndpoint: "dr.example.com:7000"},
		"endpoint with scheme":  {paramDRNode: "dr1", paramDREndpoint: "tcp://dr.example.com"},
		"common port":           {paramDRNode: "dr1", paramDREndpoint: "dr.example.com", paramDRWANPort: "443"},
		"not a port":            {paramDRNode: "dr1", paramDREndpoint: "dr.example.com", paramDRWANPort: "x"},
	} {
		_, err := parseDRParams(p)
		assert.Errorf(t, err, "%s should be refused", name)
	}
}

func drCreateReq(params map[string]string) *csi.CreateVolumeRequest {
	req := validCreateReq("pvc-dr")
	for k, v := range params {
		req.Parameters[k] = v
	}
	return req
}

func TestCreateVolumeWithDRReplica(t *testing.T) {
	b := newFakeBackend("n1", "n2", "n3", "dr1")
	s := newTestController(b)

	resp, err := s.CreateVolume(context.Background(), drCreateReq(map[string]string{
		"replicas": "3", paramDRNode: "dr1", paramDREndpoint: "dr.example.com"}))
	require.NoError(t, err)

	require.Len(t, b.requestCalls, 1)
	req := b.requestCalls[0]
	assert.True(t, req.GetWan())
	assert.Equal(t, "dr1", req.GetDrNode())
	assert.Equal(t, "dr.example.com", req.GetDrEndpoint())
	assert.ElementsMatch(t, []string{"n1", "n2", "n3"}, req.GetNodes(), "the DR node is never a local replica")
	for _, topo := range resp.GetVolume().GetAccessibleTopology() {
		assert.NotEqual(t, "dr1", topo.GetSegments()[TopologyKeyNode], "no Pod is scheduled onto the DR node")
	}
}

func TestCreateVolumeDRRefusals(t *testing.T) {
	cases := map[string]struct {
		params map[string]string
		setup  func(*fakeBackend)
		pinned string
		code   codes.Code
		msg    string
	}{
		"unknown DR node": {
			params: map[string]string{paramDRNode: "nowhere", paramDREndpoint: "dr.example.com"},
			code:   codes.InvalidArgument, msg: "not a registered Haify node",
		},
		"DR node without the pool": {
			params: map[string]string{paramDRNode: "dr1", paramDREndpoint: "dr.example.com"},
			setup:  func(b *fakeBackend) { b.onlyPoolOnNodes("n1", "n2") },
			code:   codes.InvalidArgument, msg: "does not host pool",
		},
		"scheduler picked the DR node": {
			params: map[string]string{paramDRNode: "dr1", paramDREndpoint: "dr.example.com"},
			pinned: "dr1",
			code:   codes.ResourceExhausted, msg: "cannot also hold a local replica",
		},
		"endpoint missing": {
			params: map[string]string{paramDRNode: "dr1"},
			code:   codes.InvalidArgument, msg: paramDREndpoint,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			b := newFakeBackend("n1", "n2", "dr1")
			if tc.setup != nil {
				tc.setup(b)
			}
			req := drCreateReq(tc.params)
			if tc.pinned != "" {
				req.AccessibilityRequirements = &csi.TopologyRequirement{
					Preferred: []*csi.Topology{{Segments: map[string]string{TopologyKeyNode: tc.pinned}}},
				}
			}
			_, err := newTestController(b).CreateVolume(context.Background(), req)
			require.Error(t, err)
			assert.Equal(t, tc.code, status.Code(err))
			assert.Contains(t, err.Error(), tc.msg)
			assert.Empty(t, b.requestCalls, "nothing is created")
		})
	}
}

func TestLocalReplicasLeaveOutTheDRNode(t *testing.T) {
	lan := &haifypb.ResourceInfo{Nodes: []string{"n1", "n2"}}
	assert.Equal(t, []string{"n1", "n2"}, localReplicas(lan))

	wan := &haifypb.ResourceInfo{Nodes: []string{"n1", "n2", "dr1"}, WanMode: true, DrNode: "dr1"}
	assert.Equal(t, []string{"n1", "n2"}, localReplicas(wan))
}

func TestExistingDRVolumeAnswersWithLocalTopology(t *testing.T) {
	b := newFakeBackend("n1", "n2", "dr1")
	b.resources["pvc_dr"] = &haifypb.ResourceInfo{Name: "pvc_dr", Nodes: []string{"n1", "n2", "dr1"}, WanMode: true, DrNode: "dr1"}
	s := newTestController(b)

	resp, err := s.CreateVolume(context.Background(), drCreateReq(nil))
	require.NoError(t, err)
	assert.Equal(t, accessibleTopology([]string{"n1", "n2"}), resp.GetVolume().GetAccessibleTopology())
}
