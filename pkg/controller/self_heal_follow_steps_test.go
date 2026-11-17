package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/deployment"
)

// Auto-diskful's steps against the fixture of self_heal_follow_test.go: the
// conversion, the wait for the sync, the removal and what follows it.

const followConfig = `resource vol1 {
    volume 0 {
        device    minor 7;
        disk      /dev/haify_pool0/vol1_data;
        meta-disk internal;
    }
    on a {
        address   10.0.0.1:7001;
        node-id   0;
    }
    on b {
        address   10.0.0.2:7001;
        node-id   1;
    }
    on c {
        address   10.0.0.3:7001;
        node-id   2;
    }
    connection-mesh {
        hosts a b c;
    }
}
`

// converted puts the fixture where a swap stands once the client has its
// disk: three replicas, and the swap on record.
func (fx *followFixture) converted(keepVote bool) {
	fx.save(func(r *database.Resource) { r.Nodes, r.DisklessClients = "a,b,c", "" })
	fx.set(func() { fx.disks["c"] = "Inconsistent"; fx.config = followConfig })
	fx.f.state.Pending["vol1"] = &followPending{Node: "c", Replicas: 2, KeepVote: keepVote, StartedAt: fx.now}
}

// The old replica goes only when the new one is in sync, and the node that
// gave it up stays as the third vote.
func TestAutoDiskfulConvergeRemovesAfterSync(t *testing.T) {
	ctx := context.Background()
	fx := newFollowFixture(t)
	fx.converted(true)

	fx.f.converge(ctx, "vol1", fx.nodes())
	assert.Equal(t, "a,b,c", fx.resource().Nodes, "nothing is removed while the new replica syncs")
	assert.Contains(t, fx.f.busy(ctx, nil), "vol1", "and no other sync starts meanwhile")

	fx.set(func() { fx.disks["c"] = "UpToDate" })
	fx.f.converge(ctx, "vol1", fx.nodes())
	r := fx.resource()
	assert.Equal(t, "b,c", r.Nodes)
	assert.Equal(t, "a", r.DisklessNodes, "the node that gave up its replica is the tiebreaker")
	fx.lastMessage("the replica of vol1 moved from a to c")
	fx.lastMessage("a stays in vol1 as its tiebreaker")
	assert.Empty(t, fx.f.state.Pending)
	assert.Equal(t, fx.now, fx.f.state.Followed["vol1"])

	var forgot bool
	for _, call := range fx.dep.execCalls {
		forgot = forgot || strings.Contains(call.cmd, "forget-peer")
	}
	assert.True(t, forgot, "the removal is the ordinary one, which forgets the peer")

	entries, _, err := fx.ctrl.db.ListAuditEvents(ctx, database.AuditFilter{Target: "vol1", User: followAuditUser})
	require.NoError(t, err)
	var methods []string
	for _, e := range entries {
		methods = append(methods, e.Method)
	}
	assert.ElementsMatch(t, []string{"RemoveReplica", "SetTiebreaker"}, methods)

	// The volume does not follow a workload that hops back within the day.
	fx.save(func(r *database.Resource) { r.DisklessClients = "a"; r.DisklessNodes = "" })
	kind, _ := fx.f.followSkip(ctx, followCandidate{res: fx.resource(), node: "a", since: fx.now}, fx.nodes())
	assert.Equal(t, "cooldown", kind)
}

// A removal that fails leaves one replica more, says so, and is tried again
// later — never answered with another new replica.
func TestAutoDiskfulConvergeRetriesAFailedRemoval(t *testing.T) {
	ctx := context.Background()
	fx := newFollowFixture(t)
	fx.converted(false)
	fx.set(func() { fx.disks["c"] = "UpToDate" })
	inner := fx.dep.execFunc
	fail := true
	fx.dep.execFunc = func(ctx context.Context, hosts []string, cmd string, opts ...deployment.ExecOption) (*deployment.ExecResult, error) {
		if fail && strings.Contains(cmd, "drbdadm down") {
			return failedExecResult(hosts, "device is held open"), nil
		}
		return inner(ctx, hosts, cmd, opts...)
	}

	fx.f.converge(ctx, "vol1", fx.nodes())
	assert.Equal(t, "a,b,c", fx.resource().Nodes)
	msg := fx.lastMessage("failed")
	assert.Contains(t, msg, "vol1 has 3 replicas until that works")
	assert.Contains(t, msg, "haify resource remove-replica vol1 --node a")
	p := fx.f.state.Pending["vol1"]
	require.NotNil(t, p)
	assert.Equal(t, 1, p.Attempts)
	assert.Equal(t, fx.now.Add(followRetryFirst), p.NextTry)

	// Still on record: the resource is not looked at for another replica,
	// and nothing else may start a sync.
	resources, _ := fx.ctrl.db.ListResources(ctx)
	assert.Empty(t, fx.f.observe(ctx, resources))
	assert.True(t, fx.f.following("vol1"))

	// Not before the pause is over.
	fail = false
	fx.f.converge(ctx, "vol1", fx.nodes())
	assert.Equal(t, "a,b,c", fx.resource().Nodes)

	// A new controller picks the swap up from the database.
	require.NoError(t, fx.f.save(ctx))
	next := newDataFollower(fx.ctrl)
	later := fx.now.Add(followRetryFirst + time.Minute)
	next.now = func() time.Time { return later }
	next.load(ctx)
	next.converge(ctx, "vol1", fx.nodes())
	assert.Equal(t, "b,c", fx.resource().Nodes)
	assert.Empty(t, next.state.Pending)
}

// A swap whose first step never happened, or that an operator finished by
// hand, is closed without touching anything.
func TestAutoDiskfulConvergeClosesStaleSwaps(t *testing.T) {
	ctx := context.Background()
	fx := newFollowFixture(t)
	fx.f.state.Pending["vol1"] = &followPending{Node: "c", Replicas: 2, StartedAt: fx.now}
	fx.f.converge(ctx, "vol1", fx.nodes())
	assert.Empty(t, fx.f.state.Pending, "c never got its disk")
	assert.Equal(t, "a,b", fx.resource().Nodes)
	assert.Empty(t, fx.f.state.Followed)

	fx.f.state.Pending["vol1"] = &followPending{Node: "c", Replicas: 2, StartedAt: fx.now}
	fx.save(func(r *database.Resource) { r.Nodes, r.DisklessClients = "b,c", "" })
	fx.f.converge(ctx, "vol1", fx.nodes())
	assert.Empty(t, fx.f.state.Pending)
	assert.Equal(t, "b,c", fx.resource().Nodes)
	assert.NotZero(t, fx.f.state.Followed["vol1"])

	fx.f.state.Pending["gone"] = &followPending{Node: "c", Replicas: 2}
	fx.f.converge(ctx, "gone", fx.nodes())
	assert.Nil(t, fx.f.state.Pending["gone"])
}

// A node that became a replica is not cut out of the resource by the detach
// its former user still sends.
func TestDetachDisklessClientLeavesAReplicaAlone(t *testing.T) {
	fx := newFollowFixture(t)
	fx.save(func(r *database.Resource) { r.Nodes, r.DisklessClients = "a,b,c", "" })
	require.NoError(t, fx.ctrl.resources.DetachDisklessClient(context.Background(), "vol1", "c"))
	assert.Equal(t, "a,b,c", fx.resource().Nodes)
	for _, call := range fx.dep.execCalls {
		assert.NotContains(t, call.cmd, "drbdadm down")
	}
	assert.Empty(t, fx.dep.distributedConfigs)
}

func TestFollowBackoff(t *testing.T) {
	assert.Equal(t, followRetryFirst, followBackoff(1))
	assert.Equal(t, 2*followRetryFirst, followBackoff(2))
	assert.Equal(t, followRetryMax, followBackoff(30))
}

const followClientConfig = `resource vol1 {
    volume 0 {
        device    minor 7;
        disk      /dev/haify_pool0/vol1_data;
        meta-disk internal;
    }
    on a {
        address   10.0.0.1:7001;
        node-id   0;
    }
    on b {
        address   10.0.0.2:7001;
        node-id   1;
    }
    on c {
        address   10.0.0.3:7001;
        node-id   2;
        volume 0 {
            disk      none;
        }
    }
    connection-mesh {
        hosts a b c;
    }
}
`

// The whole first step: after the hour the client gets its disk in place,
// the swap is on record before anything is changed, and the next ticks wait
// for the sync instead of starting again.
func TestAutoDiskfulTickConvertsTheClient(t *testing.T) {
	ctx := context.Background()
	fx := newFollowFixture(t)
	fx.set(func() { fx.config = followClientConfig })
	fx.due()

	fx.f.tick(ctx)
	r := fx.resource()
	assert.Equal(t, "a,b,c", r.Nodes)
	assert.Empty(t, r.DisklessClients)
	p := fx.f.state.Pending["vol1"]
	require.NotNil(t, p)
	assert.Equal(t, followPending{Node: "c", Replicas: 2, KeepVote: true, StartedAt: fx.now}, *p)
	assert.Contains(t, fx.lastMessage("it syncs now"), "c has used vol1 as a diskless client since")
	var createdMD bool
	for _, call := range fx.dep.execCalls {
		createdMD = createdMD || (strings.Contains(call.cmd, "create-md") && call.hosts[0] == "10.0.0.3")
	}
	assert.True(t, createdMD, "the disk is added on the client itself")

	stored := newDataFollower(fx.ctrl)
	stored.now = fx.f.now
	stored.load(ctx)
	assert.NotNil(t, stored.state.Pending["vol1"], "the swap is in the database")

	// While it syncs: no removal, no second conversion.
	fx.set(func() { fx.disks["c"] = "Inconsistent"; fx.config = followConfig })
	calls := len(fx.dep.execCalls)
	fx.now = fx.now.Add(2 * time.Hour)
	fx.f.tick(ctx)
	assert.Equal(t, "a,b,c", fx.resource().Nodes)
	for _, call := range fx.dep.execCalls[calls:] {
		assert.NotContains(t, call.cmd, "create-md")
		assert.NotContains(t, call.cmd, "drbdadm down")
	}

	// In sync: an old replica goes, and its node keeps the third vote.
	fx.set(func() { fx.disks["c"] = "UpToDate" })
	fx.f.tick(ctx)
	r = fx.resource()
	assert.Equal(t, "b,c", r.Nodes)
	assert.Equal(t, "a", r.DisklessNodes)
	assert.Empty(t, fx.f.state.Pending)
}
