package csi

import (
	"fmt"
	"net/netip"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mustPool(t *testing.T, raw string) []ipRange {
	t.Helper()
	pool, err := parseServiceIPPool(raw)
	require.NoError(t, err)
	return pool
}

func nothingInUse() (map[netip.Addr]string, error) { return map[netip.Addr]string{}, nil }

func TestParseServiceIPPool(t *testing.T) {
	pool := mustPool(t, "192.168.1.200-192.168.1.202/24, 10.0.0.9/16 ,fd00::10-fd00::12/64")
	require.Len(t, pool, 3)
	assert.Equal(t, "192.168.1.200", pool[0].first.String())
	assert.Equal(t, "192.168.1.202", pool[0].last.String())
	assert.Equal(t, 24, pool[0].bits)
	assert.Equal(t, pool[1].first, pool[1].last, "one address is a range of one")
	assert.Equal(t, 16, pool[1].bits)
	assert.Equal(t, 64, pool[2].bits)

	for _, bad := range []string{
		"",
		" , ",
		"192.168.1.200",                  // no prefix length
		"192.168.1.200-192.168.1.220",    // no prefix length
		"192.168.1.220-192.168.1.200/24", // backwards
		"192.168.1.200-192.168.2.5/24",   // leaves the network
		"192.168.1.200-fd00::1/24",       // mixed families
		"192.168.1.200/33",
		"192.168.1.200/0",
		"nfs.example.com/24",
		"192.168.1.200-oops/24",
	} {
		_, err := parseServiceIPPool(bad)
		assert.Errorf(t, err, "%q should be rejected", bad)
	}
}

func TestAllocateSkipsWhatIsInUse(t *testing.T) {
	var a serviceIPAllocator
	pool := mustPool(t, "192.168.1.200-192.168.1.203/24")
	used := map[netip.Addr]string{
		netip.MustParseAddr("192.168.1.200"): "volume a",
		netip.MustParseAddr("192.168.1.201"): "gateway b",
	}
	got, err := a.allocate("pvc_new", pool, func() (map[netip.Addr]string, error) { return used, nil })
	require.NoError(t, err)
	assert.Equal(t, "192.168.1.202/24", got.String(), "the address carries the network's prefix length")
}

func TestAllocateNeverGivesNetworkOrBroadcastAddress(t *testing.T) {
	var a serviceIPAllocator
	pool := mustPool(t, "192.168.1.0-192.168.1.1/24,192.168.1.255/24")
	got, err := a.allocate("v1", pool, nothingInUse)
	require.NoError(t, err)
	assert.Equal(t, "192.168.1.1/24", got.String())
	_, err = a.allocate("v2", pool, nothingInUse)
	assert.ErrorIs(t, err, errServiceIPsExhausted, ".0 and .255 of a /24 are not host addresses")
}

func TestAllocateExhaustedAndReleased(t *testing.T) {
	var a serviceIPAllocator
	pool := mustPool(t, "10.0.0.10-10.0.0.11/24")
	first, err := a.allocate("v1", pool, nothingInUse)
	require.NoError(t, err)
	second, err := a.allocate("v2", pool, nothingInUse)
	require.NoError(t, err)
	assert.NotEqual(t, first, second, "an address pending for one volume is not given to another")

	_, err = a.allocate("v3", pool, nothingInUse)
	assert.ErrorIs(t, err, errServiceIPsExhausted)

	again, err := a.allocate("v1", pool, nothingInUse)
	require.NoError(t, err)
	assert.Equal(t, first, again, "the same volume asking again keeps its address")

	// v1's creation failed: nothing recorded it, so the address is free again.
	a.release("v1")
	third, err := a.allocate("v3", pool, nothingInUse)
	require.NoError(t, err)
	assert.Equal(t, first, third)
}

func TestAllocateFailsWhenWhatIsInUseCannotBeRead(t *testing.T) {
	var a serviceIPAllocator
	_, err := a.allocate("v1", mustPool(t, "10.0.0.10/24"), func() (map[netip.Addr]string, error) {
		return nil, fmt.Errorf("controller unreachable")
	})
	require.Error(t, err)
	assert.Empty(t, a.pending, "nothing is reserved from half the picture")
}

// Many volumes are created at once, each recording its address durably and
// then dropping the in-memory reservation, as CreateVolume does. No two may
// end up with the same address, and none may be left unused.
func TestAllocateConcurrentlyGivesEveryVolumeItsOwnAddress(t *testing.T) {
	const volumes = 200
	var a serviceIPAllocator
	pool := mustPool(t, "10.1.0.1-10.1.0.200/16")

	var mu sync.Mutex
	recorded := map[netip.Addr]string{} // what a listing of the resources shows
	inUse := func() (map[netip.Addr]string, error) {
		mu.Lock()
		defer mu.Unlock()
		out := make(map[netip.Addr]string, len(recorded))
		for k, v := range recorded {
			out[k] = v
		}
		return out, nil
	}

	var wg sync.WaitGroup
	errs := make(chan error, volumes)
	for i := 0; i < volumes; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("pvc_%d", i)
			got, err := a.allocate(name, pool, inUse)
			if err != nil {
				errs <- err
				return
			}
			mu.Lock()
			if holder, dup := recorded[got.Addr()]; dup {
				errs <- fmt.Errorf("%s given to %s and %s", got, holder, name)
			}
			recorded[got.Addr()] = name
			mu.Unlock()
			a.release(name)
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	assert.Len(t, recorded, volumes)
	assert.Empty(t, a.pending)

	_, err := a.allocate("one_more", pool, inUse)
	assert.ErrorIs(t, err, errServiceIPsExhausted)
}

func TestServiceAddr(t *testing.T) {
	for raw, want := range map[string]string{
		"192.168.1.200/24":   "192.168.1.200",
		" 192.168.1.200 ":    "192.168.1.200",
		"fd00::10/64":        "fd00::10",
		"::ffff:10.0.0.1/24": "10.0.0.1",
	} {
		got, ok := serviceAddr(raw)
		require.Truef(t, ok, "%q", raw)
		assert.Equal(t, want, got.String())
	}
	for _, bad := range []string{"", "node-a", "10.0.0.1/99"} {
		_, ok := serviceAddr(bad)
		assert.Falsef(t, ok, "%q", bad)
	}
}
