package csi

import (
	"testing"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestHAIdentityForCollectsAliases(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "k8s-a", Labels: map[string]string{corev1.LabelHostname: "k8s-a"}},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: "10.0.0.1"},
			{Type: corev1.NodeHostName, Address: "machine-a.lab.example"},
		}},
	}
	id := haIdentityFor(node, []*haifypb.NodeInfo{
		{Name: "k8s-a", Address: "10.0.0.1", Hostname: "vm-a", ReplicationAddress: "10.1.0.1"},
		{Name: "k8s-b", Address: "10.0.0.2", Hostname: "vm-b"},
	})
	for _, name := range []string{"k8s-a", "10.0.0.1", "machine-a.lab.example", "machine-a", "vm-a", "10.1.0.1"} {
		assert.True(t, id[name], name)
	}
	assert.False(t, id["vm-b"])
	assert.False(t, id["10"], "an IP is not shortened")
	assert.True(t, id.matches("VM-A", &haifypb.NodeResourceState{}))
	assert.True(t, id.matches("drbd-name", &haifypb.NodeResourceState{Node: "k8s-a"}))
}

func TestHAJudgeNodeLoss(t *testing.T) {
	id := haNodeIdentity{"node-a": true}
	cases := []struct {
		name   string
		states map[string]*haifypb.NodeResourceState
		lost   bool
	}{
		{"three replicas, one lost", survivorView("Connecting"), true},
		{"node connected", survivorView("Connected"), false},
		{"standalone", survivorView("StandAlone"), false},
		{"two replicas and a tiebreaker", map[string]*haifypb.NodeResourceState{
			"node-b": {DiskState: "UpToDate"},
			"tb":     {DiskState: "Diskless", Connection: "Connected"},
			"node-a": {Connection: "NetworkFailure"},
		}, true},
		{"two replicas, no tiebreaker", map[string]*haifypb.NodeResourceState{
			"node-b": {DiskState: "UpToDate"},
			"node-a": {Connection: "Connecting"},
		}, false},
		{"survivor not up to date", map[string]*haifypb.NodeResourceState{
			"node-b": {DiskState: "Outdated"},
			"tb":     {DiskState: "Diskless", Connection: "Connected"},
			"node-a": {Connection: "Connecting"},
		}, false},
		{"node answered itself", map[string]*haifypb.NodeResourceState{
			"node-a": {DiskState: "UpToDate"},
			"node-b": {Connection: "Connecting"},
			"node-c": {Connection: "Connecting"},
		}, false},
		{"answering node ambiguous", map[string]*haifypb.NodeResourceState{
			"node-b": {DiskState: "UpToDate"},
			"node-c": {DiskState: "UpToDate"},
			"node-a": {Connection: "Connecting"},
		}, false},
		{"node not in view", map[string]*haifypb.NodeResourceState{
			"node-b": {DiskState: "UpToDate"},
			"node-c": {DiskState: "UpToDate", Connection: "Connected"},
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := haJudgeNodeLoss(&haifypb.ResourceInfo{Name: "r", NodeStates: tc.states}, id)
			assert.Equal(t, tc.lost, v.lost, v.reason)
		})
	}
	assert.False(t, haJudgeNodeLoss(nil, id).lost)
}

func TestHAPodClaimsIncludesEphemeral(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"}, Spec: corev1.PodSpec{Volumes: []corev1.Volume{
		{Name: "a", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "c"}}},
		{Name: "scratch", VolumeSource: corev1.VolumeSource{Ephemeral: &corev1.EphemeralVolumeSource{}}},
		{Name: "cfg", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}}}
	assert.Equal(t, []string{"ns/c", "ns/p-scratch"}, haPodClaims(pod))
}

func TestHAReconnected(t *testing.T) {
	id := haNodeIdentity{"node-a": true}
	assert.True(t, haReconnected(&haifypb.ResourceInfo{NodeStates: survivorView("Connected")}, id))
	assert.False(t, haReconnected(&haifypb.ResourceInfo{NodeStates: survivorView("Connecting")}, id))
	assert.True(t, haReconnected(&haifypb.ResourceInfo{NodeStates: map[string]*haifypb.NodeResourceState{
		"node-a": {DiskState: "UpToDate"},
		"node-b": {Connection: "Connected"},
	}}, id), "node-a answering, connected to a peer")
}
