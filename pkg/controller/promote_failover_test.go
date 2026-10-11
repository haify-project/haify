package controller

import (
	"context"
	"fmt"
	"testing"

	"github.com/haify-project/haify/pkg/deployment"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// jsonStatusWithQuorum builds a minimal `drbdsetup status <res> --json` payload
// with a single local device whose quorum flag is set as requested.
func jsonStatusWithQuorum(resource string, quorum bool) string {
	return fmt.Sprintf(`[{"name":%q,"role":"Secondary","devices":[{"volume":0,"disk-state":"UpToDate","quorum":%t}],"connections":[]}]`,
		resource, quorum)
}

// TestPromoteForNode_GracefulSucceedsWithoutForce verifies the safe common path:
// a normal (non-forced) promote succeeds, so no force is ever attempted and the
// quorum status is never even consulted.
func TestPromoteForNode_GracefulSucceedsWithoutForce(t *testing.T) {
	var forces []bool
	statusCalled := false
	dep := &fakeDeploymentClient{
		drbdPrimaryFunc: func(_ context.Context, host, resource string, force bool) (*deployment.HostResult, error) {
			forces = append(forces, force)
			return &deployment.HostResult{Host: host, Success: true}, nil
		},
		drbdStatusJSONFunc: func(_ context.Context, hosts []string, resource string) (*deployment.ExecResult, error) {
			statusCalled = true
			return successExecResult(hosts, ""), nil
		},
	}
	ctrl := newBasicTestController(dep)

	err := ctrl.resources.PromoteForNode(context.Background(), "data", "node1")
	require.NoError(t, err)
	require.Equal(t, []bool{false}, forces, "graceful promote must be non-forced and not retried")
	require.False(t, statusCalled, "quorum must not be checked when the normal promote succeeds")
}

// TestPromoteForNode_QuorumPresentForces covers the hard-failover case where a
// peer still holds Primary (normal promote fails) but this node holds quorum:
// the guarded logic must escalate to a forced promote.
func TestPromoteForNode_QuorumPresentForces(t *testing.T) {
	var forces []bool
	dep := &fakeDeploymentClient{
		drbdPrimaryFunc: func(_ context.Context, host, resource string, force bool) (*deployment.HostResult, error) {
			forces = append(forces, force)
			if !force {
				// Simulate a peer still Primary / unreachable: plain promote fails.
				return &deployment.HostResult{Host: host, Success: false, Output: "State change failed: (-10) State change was refused by peer node"}, nil
			}
			return &deployment.HostResult{Host: host, Success: true}, nil
		},
		drbdStatusJSONFunc: func(_ context.Context, hosts []string, resource string) (*deployment.ExecResult, error) {
			return successExecResult(hosts, jsonStatusWithQuorum(resource, true)), nil
		},
	}
	ctrl := newBasicTestController(dep)

	err := ctrl.resources.PromoteForNode(context.Background(), "data", "node1")
	require.NoError(t, err)
	require.Equal(t, []bool{false, true}, forces, "must try non-forced first, then force once quorum is confirmed")
}

// TestPromoteForNode_QuorumAbsentRefuses is the split-brain guard: the normal
// promote fails and this node does NOT hold quorum, so the promote must be
// refused with an error and NO forced promote must be issued.
func TestPromoteForNode_QuorumAbsentRefuses(t *testing.T) {
	var forces []bool
	dep := &fakeDeploymentClient{
		drbdPrimaryFunc: func(_ context.Context, host, resource string, force bool) (*deployment.HostResult, error) {
			forces = append(forces, force)
			if !force {
				return &deployment.HostResult{Host: host, Success: false, Output: "State change was refused by peer node"}, nil
			}
			return &deployment.HostResult{Host: host, Success: true}, nil
		},
		drbdStatusJSONFunc: func(_ context.Context, hosts []string, resource string) (*deployment.ExecResult, error) {
			return successExecResult(hosts, jsonStatusWithQuorum(resource, false)), nil
		},
	}
	ctrl := newBasicTestController(dep)

	err := ctrl.resources.PromoteForNode(context.Background(), "data", "node1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "quorum")
	require.Equal(t, []bool{false}, forces, "must NOT force-promote without quorum (split-brain guard)")
}

// TestPromoteForNode_QuorumUnknownRefuses ensures the guard fails closed: if the
// quorum status cannot be read after a failed normal promote, we refuse rather
// than force blindly.
func TestPromoteForNode_QuorumUnknownRefuses(t *testing.T) {
	var forces []bool
	dep := &fakeDeploymentClient{
		drbdPrimaryFunc: func(_ context.Context, host, resource string, force bool) (*deployment.HostResult, error) {
			forces = append(forces, force)
			return &deployment.HostResult{Host: host, Success: false, Output: "refused"}, nil
		},
		drbdStatusJSONFunc: func(_ context.Context, hosts []string, resource string) (*deployment.ExecResult, error) {
			// Empty output -> unparseable -> quorum unknown.
			return successExecResult(hosts, ""), nil
		},
	}
	ctrl := newBasicTestController(dep)

	err := ctrl.resources.PromoteForNode(context.Background(), "data", "node1")
	require.Error(t, err)
	require.Equal(t, []bool{false}, forces, "must NOT force-promote when quorum is undeterminable")
}

func TestLocalNodeHasQuorum(t *testing.T) {
	got, err := localNodeHasQuorum(jsonStatusWithQuorum("data", true))
	require.NoError(t, err)
	assert.True(t, got)

	got, err = localNodeHasQuorum(jsonStatusWithQuorum("data", false))
	require.NoError(t, err)
	assert.False(t, got)

	// Missing quorum field -> fail closed (false, no error).
	got, err = localNodeHasQuorum(`[{"name":"data","role":"Secondary","devices":[{"volume":0,"disk-state":"UpToDate"}],"connections":[]}]`)
	require.NoError(t, err)
	assert.False(t, got)

	// Unparseable -> error.
	_, err = localNodeHasQuorum("")
	require.Error(t, err)
}

// When the forced promote fails as well, the error still carries why the
// normal one failed: that is the reason both did (here, a peer has the device
// open), and the forced attempt's own message does not say it.
func TestPromoteForNode_ForceFailureKeepsFirstReason(t *testing.T) {
	dep := &fakeDeploymentClient{
		drbdPrimaryFunc: func(_ context.Context, host, resource string, force bool) (*deployment.HostResult, error) {
			if !force {
				return &deployment.HostResult{Host: host, Success: false, Output: "Held open by qemu on peer os2"}, nil
			}
			return &deployment.HostResult{Host: host, Success: false, Output: "State change failed: (-10)"}, nil
		},
		drbdStatusJSONFunc: func(_ context.Context, hosts []string, resource string) (*deployment.ExecResult, error) {
			return successExecResult(hosts, jsonStatusWithQuorum(resource, true)), nil
		},
	}
	ctrl := newBasicTestController(dep)

	err := ctrl.resources.PromoteForNode(context.Background(), "data", "node1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Held open by qemu on peer os2")
	assert.Contains(t, err.Error(), "State change failed: (-10)")
}

// The PVE reboot case: the old Primary disconnected on purpose while its VM
// still wrote, so this replica is Outdated, and that node is still booting.
// This one holds quorum with a diskless peer, and forcing it would make the
// stale copy the data.
func TestPromoteForNode_OutdatedCopyRefusesEvenWithQuorum(t *testing.T) {
	for _, state := range []string{"Outdated", "Inconsistent", "Diskless"} {
		var forces []bool
		dep := &fakeDeploymentClient{
			drbdPrimaryFunc: func(_ context.Context, host, resource string, force bool) (*deployment.HostResult, error) {
				forces = append(forces, force)
				if !force {
					return &deployment.HostResult{Host: host, Success: false, Output: "State change failed: (-2) Need access to UpToDate data"}, nil
				}
				return &deployment.HostResult{Host: host, Success: true}, nil
			},
			drbdStatusJSONFunc: func(_ context.Context, hosts []string, resource string) (*deployment.ExecResult, error) {
				return successExecResult(hosts, fmt.Sprintf(
					`[{"name":%q,"role":"Secondary","devices":[{"volume":0,"disk-state":%q,"quorum":true}],"connections":[{"name":"node2","connection-state":"Connecting"},{"name":"node3","connection-state":"Connected","peer_devices":[{"volume":0,"peer-disk-state":"Diskless"}]}]}]`, resource, state)), nil
			},
		}
		err := newBasicTestController(dep).resources.PromoteForNode(context.Background(), "data", "node1")
		require.Error(t, err, state)
		assert.Contains(t, err.Error(), "its copy is "+state, state)
		assert.Equal(t, []bool{false}, forces, "%s: no forced promote", state)
	}
}
