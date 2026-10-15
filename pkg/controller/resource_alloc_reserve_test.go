package controller

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/haify-project/haify/pkg/deployment"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Two creates that scan the same .res files before either has written its own
// must still get different ports and minors.
func TestReservationsKeepConcurrentCreatesApart(t *testing.T) {
	dep := &fakeDeploymentClient{execFunc: func(_ context.Context, hosts []string, cmd string, _ ...deployment.ExecOption) (*deployment.ExecResult, error) {
		if strings.Contains(cmd, "cat /etc/drbd.d/*.res") {
			return successExecResult(hosts, "address 10.0.0.1:7000;\ndevice minor 0;"), nil
		}
		return successExecResult(hosts, ""), nil
	}}
	rm := newBasicTestController(dep).resources
	ctx := context.Background()

	var wg sync.WaitGroup
	ports := make([]uint32, 4)
	minors := make([]int, 4)
	for i := range ports {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := []string{"a", "b", "c", "d"}[i]
			p, err := rm.reservePort(ctx, "n1", name)
			require.NoError(t, err)
			m, err := rm.reserveMinors(ctx, []string{"n1"}, 2, name)
			require.NoError(t, err)
			ports[i], minors[i] = p, m
		}(i)
	}
	wg.Wait()
	seenPort := map[uint32]bool{}
	seenMinor := map[int]bool{}
	for i := range ports {
		assert.False(t, seenPort[ports[i]], "port %d handed out twice", ports[i])
		seenPort[ports[i]] = true
		for _, m := range []int{minors[i], minors[i] + 1} {
			assert.False(t, seenMinor[m], "minor %d handed out twice", m)
			seenMinor[m] = true
		}
		assert.GreaterOrEqual(t, minors[i], 1)
	}

	// Released, the lowest free port is offered again.
	for _, n := range []string{"a", "b", "c", "d"} {
		rm.releaseAlloc(n)
	}
	p, err := rm.reservePort(ctx, "n1", "e")
	require.NoError(t, err)
	assert.Equal(t, uint32(7001), p)
}
