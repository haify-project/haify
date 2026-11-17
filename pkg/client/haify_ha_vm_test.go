package client

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVirtualDomainAgent(t *testing.T) {
	a, err := VirtualDomainAgent(" web-01.prod ")
	require.NoError(t, err)
	assert.Equal(t, "heartbeat", a.Provider)
	assert.Equal(t, "VirtualDomain", a.Name)
	assert.Equal(t, "vm_web_01_prod", a.Instance, "an OCF instance id carries no dots or hyphens")
	assert.Equal(t, map[string]string{
		"config":     "/etc/libvirt/qemu/web-01.prod.xml",
		"hypervisor": "qemu:///system",
	}, a.Params)

	// The name ends up in a file path and an unquoted promoter line.
	for _, bad := range []string{"", "a b", "../x", "x/y", "-lead", "a;rm"} {
		_, err := VirtualDomainAgent(bad)
		assert.Error(t, err, bad)
	}
}

// migrationPolls counts GetHaMigration calls for the resource "moving", whose
// move ends on the third.
var migrationPolls atomic.Int32

func (m *mockServer) GetHaMigration(ctx context.Context, req *haifypb.GetHaMigrationRequest) (*haifypb.GetHaMigrationResponse, error) {
	switch req.Resource {
	case "never":
		return &haifypb.GetHaMigrationResponse{Success: true, Message: "no live move has run"}, nil
	case "broken":
		return &haifypb.GetHaMigrationResponse{Success: false, Message: "database not available"}, nil
	}
	n := migrationPolls.Add(1)
	mig := &haifypb.HaMigration{Resource: req.Resource, Domain: "web1", SourceNode: "k1", TargetNode: "k2",
		Phase: "migrating", ProgressPercent: uint32(n) * 30, Steps: []string{"migrating web1 to k2"}}
	if n >= 3 {
		mig.Phase, mig.Done, mig.Outcome, mig.ActiveNode = "done", true, "moved", "k2"
		mig.Steps = append(mig.Steps, "web1 now runs on k2")
	}
	return &haifypb.GetHaMigrationResponse{Success: true, Migration: mig}, nil
}

func (m *mockServer) GetVmDefinition(ctx context.Context, req *haifypb.GetVmDefinitionRequest) (*haifypb.VmDefinitionResponse, error) {
	if req.Resource == "plain" {
		return &haifypb.VmDefinitionResponse{Success: false, Message: "plain runs no libvirt guest"}, nil
	}
	return &haifypb.VmDefinitionResponse{Success: true, Resource: req.Resource, Domain: "web1", Consistent: true,
		Nodes: []*haifypb.VmDefinitionNode{{Node: "k1", State: "ok", Running: true}}}, nil
}

func (m *mockServer) SyncVmDefinition(ctx context.Context, req *haifypb.SyncVmDefinitionRequest) (*haifypb.VmDefinitionResponse, error) {
	if req.FromNode == "k9" {
		// A partial sync: the per-node result comes back with the failure.
		return &haifypb.VmDefinitionResponse{Success: false, Message: "k9 holds no replica of this resource", Resource: req.Resource,
			Nodes: []*haifypb.VmDefinitionNode{{Node: "k1", State: "ok"}, {Node: "k2", State: "missing"}}}, nil
	}
	return &haifypb.VmDefinitionResponse{Success: true, Resource: req.Resource, Domain: "web1", SourceNode: req.FromNode, Consistent: true}, nil
}

func TestHaGuestLiveMoveAndDefinition(t *testing.T) {
	c, cleanup := setupMockClient(t)
	defer cleanup()
	ctx := context.Background()

	started, err := c.EvictHaLive(ctx, "moving", "k2", 10*time.Minute)
	require.NoError(t, err)
	assert.True(t, started.Success)

	none, err := c.GetHaMigration(ctx, "never")
	require.NoError(t, err)
	assert.Nil(t, none)
	_, err = c.GetHaMigration(ctx, "broken")
	require.ErrorContains(t, err, "database not available")

	migrationPolls.Store(0)
	var seen []string
	m, err := c.WaitHaMigration(ctx, "moving", time.Millisecond, func(m *haifypb.HaMigration) {
		seen = append(seen, fmt.Sprintf("%s %d", m.GetPhase(), m.GetProgressPercent()))
	})
	require.NoError(t, err)
	assert.Equal(t, "moved", m.GetOutcome())
	assert.Equal(t, []string{"migrating 30", "migrating 60", "done 90"}, seen, "one call per change, the last when it is done")

	// A caller that gives up gets what it saw and its own context's error.
	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	migrationPolls.Store(-1000)
	m, err = c.WaitHaMigration(short, "moving", time.Millisecond, nil)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NotNil(t, m)
	assert.False(t, m.GetDone())

	d, err := c.GetVmDefinition(ctx, "web1")
	require.NoError(t, err)
	assert.True(t, d.GetConsistent())
	_, err = c.GetVmDefinition(ctx, "plain")
	require.ErrorContains(t, err, "runs no libvirt guest")

	d, err = c.SyncVmDefinition(ctx, "web1", "k1")
	require.NoError(t, err)
	assert.Equal(t, "k1", d.GetSourceNode())
	d, err = c.SyncVmDefinition(ctx, "web1", "k9")
	require.ErrorContains(t, err, "k9 holds no replica")
	require.NotNil(t, d, "what each node ended up with is returned alongside the error")
	assert.Len(t, d.GetNodes(), 2)

	path, err := c.MakeHaWith(ctx, &haifypb.MakeHaRequest{Resource: "res1", VmDefinitionNode: "k1"})
	require.NoError(t, err)
	assert.NotEmpty(t, path)
}
