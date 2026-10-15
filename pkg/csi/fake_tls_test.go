package csi

import (
	"context"
	"fmt"

	haifypb "github.com/haify-project/haify/api/proto/v1"
)

// tlsFakeBackend is a fakeBackend whose controller also answers the
// replication TLS calls, as *client.HaifyClient does.
type tlsFakeBackend struct {
	*fakeBackend
	// notReady maps a node to the problem ReplicationTLSStatus reports for
	// it; every other node is ready.
	notReady  map[string]string
	statusErr error
	setErr    error
	// setCalls records "resource=true|false" and, after it, how many
	// populate copies had run by then, so a test can tell the links were
	// encrypted before any data was copied.
	setCalls    []string
	statusCalls [][]string
}

func newTLSFakeBackend(nodeNames ...string) *tlsFakeBackend {
	return &tlsFakeBackend{fakeBackend: newFakeBackend(nodeNames...), notReady: map[string]string{}}
}

func (f *tlsFakeBackend) ReplicationTLSStatus(_ context.Context, nodes []string) ([]*haifypb.NodeTLSInfo, error) {
	f.statusCalls = append(f.statusCalls, nodes)
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	out := make([]*haifypb.NodeTLSInfo, 0, len(nodes))
	for _, n := range nodes {
		problem, bad := f.notReady[n]
		out = append(out, &haifypb.NodeTLSInfo{Node: n, Ready: !bad, Problem: problem})
	}
	return out, nil
}

func (f *tlsFakeBackend) SetResourceTLS(_ context.Context, resource string, enabled bool) (string, error) {
	f.setCalls = append(f.setCalls, fmt.Sprintf("%s=%t populated=%d", resource, enabled, len(f.populated)))
	if f.setErr != nil {
		return "", f.setErr
	}
	return "ok", nil
}

var _ replicationTLSBackend = (*tlsFakeBackend)(nil)
