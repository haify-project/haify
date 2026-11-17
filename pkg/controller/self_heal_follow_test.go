package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haify-project/haify/pkg/config"
	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/deployment"
	"github.com/haify-project/haify/pkg/event"
)

// followFixture is a three-node cluster with one resource, vol1: replicas on
// a and b, and c using it as a diskless client. The DRBD state the nodes
// report is whatever the test sets.
type followFixture struct {
	t    *testing.T
	ctrl *Controller
	dep  *fakeDeploymentClient
	f    *dataFollower
	now  time.Time

	mu       sync.Mutex
	roles    map[string]string // node -> role as DRBD reports it
	disks    map[string]string // node -> disk state
	statusOK bool
	free     map[string]uint64 // host -> free bytes of haify_pool0; absent = no such pool
	twoPrim  bool              // allow-two-primaries is set
	config   string            // what `cat /etc/drbd.d/vol1.res` returns
}

var followAddrs = map[string]string{"a": "10.0.0.1", "b": "10.0.0.2", "c": "10.0.0.3"}

func newFollowFixture(t *testing.T) *followFixture {
	t.Helper()
	fx := &followFixture{t: t, now: time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC), statusOK: true,
		roles: map[string]string{"a": "Secondary", "b": "Secondary", "c": "Primary"},
		disks: map[string]string{"a": "UpToDate", "b": "UpToDate", "c": "Diskless"},
		free:  map[string]uint64{"10.0.0.1": 50 << 30, "10.0.0.2": 50 << 30, "10.0.0.3": 50 << 30}}
	fx.dep = &fakeDeploymentClient{}
	fx.dep.drbdStatusFunc = func(_ context.Context, hosts []string, _ string) (*deployment.ExecResult, error) {
		fx.mu.Lock()
		defer fx.mu.Unlock()
		if !fx.statusOK {
			return nil, fmt.Errorf("unreachable")
		}
		// The text status carries the answering node's own role, which is
		// what a removal checks before it takes a replica down.
		role := "Secondary"
		for name, addr := range followAddrs {
			if len(hosts) == 1 && hosts[0] == addr {
				role = fx.roles[name]
			}
		}
		return successExecResult(hosts, "vol1 role:"+role+"\n  disk:UpToDate\n"), nil
	}
	fx.dep.drbdStatusJSONFunc = func(_ context.Context, hosts []string, _ string) (*deployment.ExecResult, error) {
		// The node that answers describes itself as the local one.
		local := "a"
		for name, addr := range followAddrs {
			if len(hosts) == 1 && hosts[0] == addr {
				local = name
			}
		}
		return successExecResult(hosts, fx.statusJSON(local)), nil
	}
	fx.dep.execFunc = func(_ context.Context, hosts []string, cmd string, _ ...deployment.ExecOption) (*deployment.ExecResult, error) {
		fx.mu.Lock()
		defer fx.mu.Unlock()
		switch {
		case strings.Contains(cmd, "vgs --noheadings"):
			res := &deployment.ExecResult{Hosts: map[string]*deployment.HostResult{}}
			for _, h := range hosts {
				out := ""
				if free, ok := fx.free[h]; ok {
					out = fmt.Sprintf("  haify_pool0|%d|%d|/dev/vdb\n", uint64(100<<30), free)
				}
				res.Hosts[h] = &deployment.HostResult{Host: h, Success: true, Output: out}
			}
			return res, nil
		case strings.Contains(cmd, "drbdsetup show") && fx.twoPrim:
			return successExecResult(hosts, "net {\n    allow-two-primaries yes;\n}\n"), nil
		case strings.Contains(cmd, "blockdev --getsize64"):
			return successExecResult(hosts, fmt.Sprintf("%d\n", uint64(10<<30))), nil
		case strings.HasPrefix(cmd, "cat /etc/drbd.d/"):
			return successExecResult(hosts, fx.config), nil
		}
		return successExecResult(hosts, ""), nil
	}
	fx.ctrl = newBasicTestController(fx.dep)
	fx.ctrl.db = newTestDB(t)
	fx.ctrl.config = &config.Config{SelfHeal: config.SelfHealConfig{AutoDiskful: "on", AutoDiskfulAfterMinutes: 60}}
	fx.ctrl.events = event.NewBus(64)
	registerNodes(fx.ctrl, followAddrs)
	fx.ctrl.hosts = []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}
	ctx := context.Background()
	require.NoError(t, fx.ctrl.db.SaveResource(ctx, &database.Resource{Name: "vol1", Port: 7001, Nodes: "a,b",
		DisklessNodes: "", DisklessClients: "c", Protocol: "C", Replicas: 2}))
	require.NoError(t, fx.ctrl.db.SaveVolume(ctx, &database.Volume{ResourceName: "vol1", VolumeName: "vol1_data",
		VolumeID: 0, Pool: "haify_pool0", SizeGB: 10, Device: "/dev/haify_pool0/vol1_data"}))
	fx.f = newDataFollower(fx.ctrl)
	fx.f.now = func() time.Time { return fx.now }
	fx.f.load(ctx)
	fx.ctrl.follower = fx.f
	return fx
}

// statusJSON is `drbdsetup status --json` as local reports it.
func (fx *followFixture) statusJSON(local string) string {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	st := drbdsetupStatus{Name: "vol1", Role: fx.roles[local], Devices: []drbdDevice{{DiskState: fx.disks[local]}}}
	for _, n := range []string{"a", "b", "c"} {
		if n == local || fx.roles[n] == "" {
			continue
		}
		st.Connections = append(st.Connections, drbdConnection{Name: n, ConnectionState: "Connected", PeerRole: fx.roles[n],
			PeerDevices: []drbdPeerDevice{{PeerDiskState: fx.disks[n], ReplicationState: "Established"}}})
	}
	raw, err := json.Marshal([]drbdsetupStatus{st})
	require.NoError(fx.t, err)
	return string(raw)
}

func (fx *followFixture) set(mut func()) {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	mut()
}

func (fx *followFixture) resource() *database.Resource {
	r, err := fx.ctrl.db.GetResource(context.Background(), "vol1")
	require.NoError(fx.t, err)
	return r
}

func (fx *followFixture) save(mut func(r *database.Resource)) {
	r := fx.resource()
	mut(r)
	require.NoError(fx.t, fx.ctrl.db.SaveResource(context.Background(), r))
}

// messages are the events published so far.
func (fx *followFixture) messages() []string {
	var out []string
	for _, ev := range fx.ctrl.events.Recent(event.Filter{}, 0, 0) {
		out = append(out, ev.Message)
	}
	return out
}

// lastMessage is the published event whose message contains part.
func (fx *followFixture) lastMessage(part string) string {
	fx.t.Helper()
	for _, m := range fx.messages() {
		if strings.Contains(m, part) {
			return m
		}
	}
	require.Failf(fx.t, "no such event", "no event containing %q in %q", part, fx.messages())
	return ""
}

// due is the candidate for c once it has been in use for the hour.
func (fx *followFixture) due() followCandidate {
	ctx := context.Background()
	resources, err := fx.ctrl.db.ListResources(ctx)
	require.NoError(fx.t, err)
	require.Empty(fx.t, fx.f.observe(ctx, resources), "the hour has not passed")
	fx.now = fx.now.Add(61 * time.Minute)
	due := fx.f.observe(ctx, resources)
	require.Len(fx.t, due, 1)
	return due[0]
}

func (fx *followFixture) nodes() map[string]*NodeInfo {
	out := map[string]*NodeInfo{}
	for _, n := range fx.ctrl.nodes.nodes {
		out[n.Name] = n
	}
	return out
}

// Use counts only while the client is the one Primary, without a break.
func TestAutoDiskfulTimesUninterruptedUse(t *testing.T) {
	fx := newFollowFixture(t)
	ctx := context.Background()
	resources, _ := fx.ctrl.db.ListResources(ctx)

	assert.Empty(t, fx.f.observe(ctx, resources))
	fx.now = fx.now.Add(59 * time.Minute)
	assert.Empty(t, fx.f.observe(ctx, resources), "not yet an hour")
	fx.now = fx.now.Add(2 * time.Minute)
	due := fx.f.observe(ctx, resources)
	require.Len(t, due, 1)
	assert.Equal(t, "c", due[0].node)

	// A pod restart: Secondary for one reading, and the hour starts again.
	fx.set(func() { fx.roles["c"] = "Secondary" })
	assert.Empty(t, fx.f.observe(ctx, resources))
	fx.set(func() { fx.roles["c"] = "Primary" })
	assert.Empty(t, fx.f.observe(ctx, resources), "the earlier use does not count")

	// A live migration passing through: two Primaries end the timer.
	fx.now = fx.now.Add(61 * time.Minute)
	fx.set(func() { fx.roles["a"] = "Primary" })
	assert.Empty(t, fx.f.observe(ctx, resources))
	fx.set(func() { fx.roles["a"] = "Secondary" })
	assert.Empty(t, fx.f.observe(ctx, resources))

	// A status that cannot be read proves nothing.
	fx.now = fx.now.Add(61 * time.Minute)
	fx.set(func() { fx.statusOK = false })
	assert.Empty(t, fx.f.observe(ctx, resources))
	fx.set(func() { fx.statusOK = true })
	assert.Empty(t, fx.f.observe(ctx, resources))
}

// A restart or failover continues the timers; a longer gap starts them again,
// and never makes a client due at once. Swaps on record survive either.
func TestAutoDiskfulStateAcrossRestart(t *testing.T) {
	fx := newFollowFixture(t)
	ctx := context.Background()
	resources, _ := fx.ctrl.db.ListResources(ctx)
	fx.f.observe(ctx, resources)
	fx.f.state.Pending["other"] = &followPending{Node: "x", Replicas: 2}
	require.NoError(t, fx.f.save(ctx))

	restart := func(after time.Duration) *dataFollower {
		f := newDataFollower(fx.ctrl)
		at := fx.now.Add(after)
		f.now = func() time.Time { return at }
		f.load(ctx)
		return f
	}
	soon := restart(2 * time.Minute)
	assert.Equal(t, fx.now, soon.state.InUse["vol1/c"], "a failover keeps the timer")
	assert.NotNil(t, soon.state.Pending["other"])

	late := restart(3 * time.Hour)
	assert.Empty(t, late.state.InUse, "nobody watched for three hours")
	assert.NotNil(t, late.state.Pending["other"], "a swap is finished whoever started it")
	assert.Empty(t, late.observe(ctx, resources), "use is counted from the restart, so nothing is due at once")
}

func TestAutoDiskfulSkips(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		setup func(fx *followFixture)
		kind  string
		says  string
	}{
		{"wan", func(fx *followFixture) { fx.save(func(r *database.Resource) { r.WANMode = true }) }, "wan", "another site"},
		{"opted out", func(fx *followFixture) {
			fx.save(func(r *database.Resource) { r.Labels = map[string]string{autoDiskfulLabel: "false"} })
		}, "optout", "auto-diskful=false"},
		{"moving", func(fx *followFixture) { fx.save(func(r *database.Resource) { r.MoveFrom, r.MoveTo = "a", "d" }) }, "moving", "already moving"},
		{"drained node", func(fx *followFixture) { fx.ctrl.nodes.nodes["10.0.0.3"].State = NodeStateMaintenance }, "node", "maintenance"},
		{"node opted out", func(fx *followFixture) {
			fx.ctrl.nodes.nodes["10.0.0.3"].Labels = map[string]string{autoDiskfulLabel: "false"}
		}, "optout", "c is labelled"},
		{"local agent", func(fx *followFixture) { fx.ctrl.nodes.nodes["10.0.0.3"].LocalAgent = true }, "node", "local-agent"},
		{"single replica", func(fx *followFixture) { fx.save(func(r *database.Resource) { r.Nodes = "a" }) }, "single", "one replica"},
		{"pinned CSI volume", func(fx *followFixture) {
			fx.save(func(r *database.Resource) {
				r.Labels = map[string]string{csiManagedByLabel: "csi", csiTopologyLabel: "a+b"}
			})
		}, "pinned", "pinned"},
		{"ha", func(fx *followFixture) {
			require.NoError(fx.t, fx.ctrl.db.SaveHaConfig(ctx, &database.HaConfig{Resource: "vol1"}))
		}, "promoter", "promoter"},
		{"degraded", func(fx *followFixture) { fx.set(func() { fx.disks["b"] = "Inconsistent" }) }, "degraded", "replica on b"},
		{"live migration", func(fx *followFixture) { fx.set(func() { fx.twoPrim = true }) }, "migration", "live migration"},
		{"no pool", func(fx *followFixture) { fx.set(func() { delete(fx.free, "10.0.0.3") }) }, "pool", "c has no pool pool0"},
		{"pool too small", func(fx *followFixture) { fx.set(func() { fx.free["10.0.0.3"] = 4 << 30 }) }, "pool", "4.0 GiB free"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFollowFixture(t)
			tc.setup(fx)
			cand := fx.due()
			cand.res = fx.resource()
			kind, why := fx.f.followSkip(ctx, cand, fx.nodes())
			assert.Equal(t, tc.kind, kind)
			assert.Contains(t, why, tc.says)

			// The reason reaches the operator, once, and nothing changes.
			resources, _ := fx.ctrl.db.ListResources(ctx)
			assert.False(t, fx.f.consider(ctx, cand, resources, fx.nodes()))
			assert.False(t, fx.f.consider(ctx, cand, resources, fx.nodes()))
			msg := fx.lastMessage("gets no replica of its own")
			assert.Contains(t, msg, tc.says)
			n := 0
			for _, m := range fx.messages() {
				if strings.Contains(m, "gets no replica of its own") {
					n++
				}
			}
			assert.Equal(t, 1, n, "said once")
			assert.Equal(t, "c", fx.resource().DisklessClients)
			assert.Empty(t, fx.f.state.Pending)
		})
	}
}

// A remote-access CSI volume and a plain resource pass every check.
func TestAutoDiskfulEligible(t *testing.T) {
	fx := newFollowFixture(t)
	fx.save(func(r *database.Resource) {
		r.Labels = map[string]string{csiManagedByLabel: "csi", csiTopologyLabel: csiTopologyAnywhere}
	})
	cand := fx.due()
	cand.res = fx.resource()
	kind, why := fx.f.followSkip(context.Background(), cand, fx.nodes())
	assert.Empty(t, kind, why)
}

// One self-healing sync at a time, and a dry run changes nothing.
func TestAutoDiskfulWaitsAndDryRun(t *testing.T) {
	ctx := context.Background()
	fx := newFollowFixture(t)
	cand := fx.due()
	require.NoError(t, fx.ctrl.db.SaveResource(ctx, &database.Resource{Name: "vol2", Nodes: "a,b,c", MoveFrom: "a", MoveTo: "c"}))
	resources, _ := fx.ctrl.db.ListResources(ctx)
	assert.False(t, fx.f.consider(ctx, cand, resources, fx.nodes()))
	assert.Contains(t, fx.lastMessage("one sync at a time"), "vol2 is moving from a to c")
	assert.Empty(t, fx.f.state.Pending)

	fx = newFollowFixture(t)
	fx.ctrl.config.SelfHeal.AutoDiskful = "dry-run"
	cand = fx.due()
	resources, _ = fx.ctrl.db.ListResources(ctx)
	assert.False(t, fx.f.consider(ctx, cand, resources, fx.nodes()))
	assert.Contains(t, fx.lastMessage("dry run:"), "it gets a replica of its own")
	assert.Equal(t, "c", fx.resource().DisklessClients)
	assert.Empty(t, fx.f.state.Pending)
	for _, call := range fx.dep.execCalls {
		assert.NotContains(t, call.cmd, "create-md")
	}
}

func TestPickFollowVictim(t *testing.T) {
	good := func(node string) followReplica {
		return followReplica{Node: node, Known: true, UpToDate: true, Reachable: true, Fill: -1}
	}
	primary := func(node string) followReplica {
		r := good(node)
		r.Primary = true
		return r
	}
	labels := map[string]map[string]string{}
	pick := func(replicas []followReplica, tiebreakers []string, keepVote bool) (string, string) {
		return pickFollowVictim("c", replicas, tiebreakers, keepVote, labels, "host")
	}

	v, why := pick([]followReplica{good("a"), good("b"), primary("c")}, nil, true)
	assert.Equal(t, "a", v, why)

	// The new replica never goes, and neither does a Primary.
	v, _ = pick([]followReplica{primary("a"), good("b"), good("c")}, nil, false)
	assert.Equal(t, "b", v)

	// A replica that is behind goes before a good one, as long as two good
	// copies remain.
	behind := good("b")
	behind.UpToDate = false
	v, _ = pick([]followReplica{good("a"), behind, primary("c")}, nil, false)
	assert.Equal(t, "b", v)

	// With one old replica behind, removing the good one would leave the new
	// replica as the only UpToDate copy.
	other := good("a")
	other.Reachable = false
	v, why = pick([]followReplica{other, behind, primary("c")}, nil, false)
	assert.Equal(t, "b", v, why)
	v, why = pick([]followReplica{good("a"), followReplica{Node: "b", Fill: -1}, primary("c")}, nil, false)
	assert.Empty(t, v)
	assert.Contains(t, why, "fewer than two UpToDate copies")
	assert.Contains(t, why, "the state of the one on b cannot be read")

	unreachable := good("a")
	unreachable.Reachable = false
	v, why = pick([]followReplica{unreachable, primary("b"), good("c")}, nil, false)
	assert.Empty(t, v)
	assert.Contains(t, why, "a does not answer")
	assert.Contains(t, why, "b is Primary")

	// Fault domains: a and c are VMs on one machine, so a is the one to give
	// up; keeping it would leave two of three replicas on that machine.
	labels = map[string]map[string]string{"a": {"host": "m1"}, "b": {"host": "m2"}, "c": {"host": "m1"}, "d": {"host": "m3"}}
	v, _ = pick([]followReplica{good("b"), good("a"), good("d"), primary("c")}, nil, false)
	assert.Equal(t, "a", v)

	// Giving up b instead would leave both copies on m1, which is worse than
	// a tiebreaker there: a keeps a copy on each machine.
	v, _ = pick([]followReplica{good("a"), good("b"), primary("c")}, nil, true)
	assert.Equal(t, "a", v)

	// Otherwise the fuller pool gives up its replica.
	labels = map[string]map[string]string{}
	full := good("b")
	full.Fill = 0.9
	roomy := good("a")
	roomy.Fill = 0.2
	v, _ = pick([]followReplica{roomy, full, primary("c")}, nil, false)
	assert.Equal(t, "b", v)
}
