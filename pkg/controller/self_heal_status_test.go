package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/config"
	"github.com/haify-project/haify/pkg/database"
)

// A client in use is listed with how long it has left, and once it is due
// with what auto-diskful decided about it.
func TestSelfHealStatusFollowsAClient(t *testing.T) {
	fx := newFollowFixture(t)
	ctx := context.Background()
	fx.f.tick(ctx)
	fx.now = fx.now.Add(20 * time.Minute)

	st := fx.ctrl.SelfHealStatus(ctx).GetAutoDiskful()
	assert.Equal(t, "on", st.Mode)
	assert.EqualValues(t, 60, st.AfterMinutes)
	assert.EqualValues(t, 24, st.CooldownHours)
	require.Len(t, st.Clients, 1)
	c := st.Clients[0]
	assert.Equal(t, "vol1", c.Resource)
	assert.Equal(t, "c", c.Node)
	assert.Equal(t, "counting", c.State)
	assert.EqualValues(t, 20*60, c.InUseSeconds)
	assert.EqualValues(t, 40*60, c.RemainingSeconds)
	assert.Empty(t, c.Reason)

	// Due, and passed over: the reason the event gave is the reason shown.
	fx.save(func(r *database.Resource) { r.Labels = map[string]string{autoDiskfulLabel: "false"} })
	fx.now = fx.now.Add(45 * time.Minute)
	fx.f.tick(ctx)
	c = fx.ctrl.SelfHealStatus(ctx).GetAutoDiskful().Clients[0]
	assert.Equal(t, "skipped", c.State)
	assert.Equal(t, "optout", c.Skip)
	assert.Contains(t, c.Reason, autoDiskfulLabel+"=false")
	assert.Zero(t, c.RemainingSeconds)

	// The client stops using the volume: it is no longer watched.
	fx.set(func() { fx.roles["c"] = "Secondary" })
	fx.f.tick(ctx)
	assert.Empty(t, fx.ctrl.SelfHealStatus(ctx).GetAutoDiskful().Clients)
}

// A dry run shows what would be done; a real run shows the swap and its
// phase, and afterwards the cooldown.
func TestSelfHealStatusShowsPlanSwapAndCooldown(t *testing.T) {
	fx := newFollowFixture(t)
	ctx := context.Background()
	fx.ctrl.config.SelfHeal.AutoDiskful = "dry-run"
	fx.f.tick(ctx)
	fx.now = fx.now.Add(61 * time.Minute)
	fx.f.tick(ctx)
	st := fx.ctrl.SelfHealStatus(ctx).GetAutoDiskful()
	assert.Equal(t, "dry-run", st.Mode)
	require.Len(t, st.Clients, 1)
	assert.Equal(t, "planned", st.Clients[0].State)
	assert.Contains(t, st.Clients[0].Reason, "would get a replica of its own")
	assert.Empty(t, st.Swaps)

	// A swap on record whose new replica is still syncing.
	fx.f.mu.Lock()
	fx.f.state.Pending["vol1"] = &followPending{Node: "c", Replicas: 2, StartedAt: fx.now}
	fx.f.mu.Unlock()
	fx.save(func(r *database.Resource) { r.Nodes, r.DisklessClients = "a,b,c", "" })
	fx.set(func() { fx.disks["c"] = "Inconsistent" })
	st = fx.ctrl.SelfHealStatus(ctx).GetAutoDiskful()
	require.Len(t, st.Swaps, 1)
	sw := st.Swaps[0]
	assert.Equal(t, "vol1", sw.Resource)
	assert.Equal(t, "c", sw.Node)
	assert.Equal(t, "syncing", sw.Phase)
	assert.EqualValues(t, 3, sw.ReplicasNow)
	assert.EqualValues(t, 2, sw.ReplicasTarget)

	// In sync: an old replica is to go; a step that waits says why.
	fx.set(func() { fx.disks["c"] = "UpToDate" })
	fx.f.noteStep("vol1", "none of the others can be removed now")
	sw = fx.ctrl.SelfHealStatus(ctx).GetAutoDiskful().Swaps[0]
	assert.Equal(t, "removing", sw.Phase)
	assert.Nil(t, sw.SyncPercent)
	assert.Equal(t, "none of the others can be removed now", sw.Detail)

	// Removed, with the third vote still to keep.
	fx.f.mu.Lock()
	fx.f.state.Pending["vol1"].Removed = "a"
	fx.f.mu.Unlock()
	sw = fx.ctrl.SelfHealStatus(ctx).GetAutoDiskful().Swaps[0]
	assert.Equal(t, "tiebreaker", sw.Phase)
	assert.Equal(t, "a", sw.Removed)

	fx.f.finish("vol1")
	st = fx.ctrl.SelfHealStatus(ctx).GetAutoDiskful()
	assert.Empty(t, st.Swaps)
	require.Len(t, st.Cooldowns, 1)
	assert.Equal(t, "vol1", st.Cooldowns[0].Resource)
	assert.Equal(t, fx.now.Add(followCooldown).Unix(), st.Cooldowns[0].Until)
	fx.f.mu.Lock()
	_, noted := fx.f.stepNotes["vol1"]
	fx.f.mu.Unlock()
	assert.False(t, noted, "a finished swap leaves no note behind")
}

// With the loops off the status says so and lists nothing; offline nodes and
// moves are reported from what is on record.
func TestSelfHealStatusEvictAndMoves(t *testing.T) {
	fx := newFollowFixture(t)
	ctx := context.Background()
	fx.ctrl.follower = nil
	fx.ctrl.config.SelfHeal = config.SelfHealConfig{}
	resp, err := NewServer(fx.ctrl).GetSelfHealStatus(ctx, &haifypb.GetSelfHealStatusRequest{})
	require.NoError(t, err)
	require.True(t, resp.Success)
	assert.Equal(t, "off", resp.AutoEvict.Mode)
	assert.Equal(t, "off", resp.AutoDiskful.Mode)
	assert.Empty(t, resp.AutoEvict.Nodes)
	assert.Empty(t, resp.AutoDiskful.Clients)

	fx.ctrl.config.SelfHeal = config.SelfHealConfig{AutoEvict: "on", AfterMinutes: 60, MaxOfflinePercent: 34}
	fx.ctrl.evictor = &autoEvictor{c: fx.ctrl, announced: map[string]bool{}, inflight: "vol9"}
	fx.ctrl.nodes.mu.Lock()
	fx.ctrl.nodes.nodes["10.0.0.2"].OfflineSince = time.Now().Add(-20 * time.Minute)
	fx.ctrl.nodes.nodes["10.0.0.2"].State = NodeStateOffline
	fx.ctrl.nodes.mu.Unlock()
	fx.save(func(r *database.Resource) { r.MoveFrom, r.MoveTo = "a", "c" })

	resp = fx.ctrl.SelfHealStatus(ctx)
	ev := resp.AutoEvict
	assert.Equal(t, "on", ev.Mode)
	assert.Equal(t, "vol9", ev.Syncing)
	require.Len(t, ev.Nodes, 1)
	assert.Equal(t, "b", ev.Nodes[0].Node)
	assert.Equal(t, "waiting", ev.Nodes[0].State)
	assert.InDelta(t, 40*60, ev.Nodes[0].RemainingSeconds, 5)
	assert.EqualValues(t, 1, ev.Nodes[0].Replicas)
	require.Len(t, resp.Moves, 1)
	assert.Equal(t, "vol1", resp.Moves[0].Resource)
	assert.Equal(t, "c", resp.Moves[0].To)
}
