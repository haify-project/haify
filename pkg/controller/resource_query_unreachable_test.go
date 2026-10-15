package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/haify-project/haify/pkg/deployment"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A host that just failed a status query is asked after the others, even when
// it was the last-seen Primary, until the window passes or it answers again.
func TestStatusAskOrderPutsUnreachableHostsLast(t *testing.T) {
	rm := NewResourceManager(newBasicTestController(&fakeDeploymentClient{}))
	hosts := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}
	rm.primaryHosts.Store("r", "10.0.0.1")
	assert.Equal(t, []int{0, 1, 2}, rm.statusAskOrder("r", hosts))

	rm.markHostUnreachable("10.0.0.1")
	assert.Equal(t, []int{1, 2, 0}, rm.statusAskOrder("r", hosts))

	rm.markHostReachable("10.0.0.1")
	assert.Equal(t, []int{0, 1, 2}, rm.statusAskOrder("r", hosts))

	rm.unreachableHosts.Store("10.0.0.2", time.Now().Add(-unreachableFor))
	assert.Equal(t, []int{0, 1, 2}, rm.statusAskOrder("r", hosts), "the window has passed")
}

func TestDrbdStatusFromAnyHostSkipsAHostThatJustFailed(t *testing.T) {
	var asked []string
	dep := &fakeDeploymentClient{}
	dep.drbdStatusFunc = func(_ context.Context, hosts []string, _ string) (*deployment.ExecResult, error) {
		asked = append(asked, hosts[0])
		if hosts[0] == "10.0.0.1" {
			return nil, errors.New("dial tcp 10.0.0.1:22: i/o timeout")
		}
		return successExecResult(hosts, "r role:Secondary"), nil
	}
	rm := newBasicTestController(dep).resources
	hosts := []string{"10.0.0.1", "10.0.0.2"}

	_, idx, err := rm.drbdStatusFromAnyHost(context.Background(), hosts, "r")
	require.NoError(t, err)
	assert.Equal(t, 1, idx)
	assert.Equal(t, []string{"10.0.0.1", "10.0.0.2"}, asked)

	asked = nil
	_, idx, err = rm.drbdStatusFromAnyHost(context.Background(), hosts, "r")
	require.NoError(t, err)
	assert.Equal(t, 1, idx)
	assert.Equal(t, []string{"10.0.0.2"}, asked, "the dead host is not asked first again")
}

// A host that hangs is given up on after statusAskTimeout while another can
// answer; the last host is not cut short.
func TestDrbdStatusFromAnyHostDoesNotWaitOutAHungHost(t *testing.T) {
	dep := &fakeDeploymentClient{}
	dep.drbdStatusFunc = func(ctx context.Context, hosts []string, _ string) (*deployment.ExecResult, error) {
		if hosts[0] == "10.0.0.1" {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		_, hasDeadline := ctx.Deadline()
		assert.False(t, hasDeadline, "the last host gets the caller's context")
		return successExecResult(hosts, "r role:Secondary"), nil
	}
	rm := newBasicTestController(dep).resources
	start := time.Now()
	_, idx, err := rm.drbdStatusFromAnyHost(context.Background(), []string{"10.0.0.1", "10.0.0.2"}, "r")
	require.NoError(t, err)
	assert.Equal(t, 1, idx)
	assert.Less(t, time.Since(start), statusAskTimeout+2*time.Second)
	assert.True(t, rm.recentlyUnreachable("10.0.0.1"))
}
