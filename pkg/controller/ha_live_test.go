package controller

import (
	"context"
	"testing"
	"time"

	"github.com/haify-project/haify/pkg/database"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fastLive shortens the waits of a live move for the simulated hosts.
func fastLive(t *testing.T) {
	t.Helper()
	poll, settle, adopt, restart, grace := livePollInterval, liveSettlePause, liveAdoptWait, liveRestartWait, liveAbortGrace
	livePollInterval, liveSettlePause = 2*time.Millisecond, 2*time.Millisecond
	liveAdoptWait, liveRestartWait, liveAbortGrace = 200*time.Millisecond, 200*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() {
		livePollInterval, liveSettlePause, liveAdoptWait, liveRestartWait, liveAbortGrace = poll, settle, adopt, restart, grace
	})
}

// waitLive follows a started move to its end.
func waitLive(t *testing.T, c *Controller) *database.HaMigration {
	t.Helper()
	var rec *database.HaMigration
	require.Eventually(t, func() bool {
		var err error
		rec, err = c.resources.GetHaMigration(context.Background(), "web1")
		return err == nil && rec != nil && rec.Done && c.resources.live.get("web1") == nil
	}, 10*time.Second, 5*time.Millisecond)
	return rec
}

// settled asserts the end state every move must reach, with the guest on on.
func (s *liveSim) settled(t *testing.T, on string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	assert.Empty(t, s.violations)
	assert.Equal(t, on, s.name[s.guestOn], "where the guest runs")
	assert.Equal(t, []string{on}, s.primaries(), "one Primary, where the guest runs")
	for _, h := range s.hosts {
		n := s.name[h]
		assert.Equal(t, "enabled", s.promoter[h], "promoter on %s", n)
		assert.False(t, s.twoPrim[h], "dual-primary left on at %s", n)
		assert.False(t, s.guard[h], "restart guard left on %s", n)
		assert.False(t, s.marked[h], "hook marker left on %s", n)
		want := "inactive"
		if n == on {
			want = "active"
		}
		assert.Equal(t, want, s.target[h], "services target on %s", n)
		assert.Equal(t, want, s.agent[h], "agent unit on %s", n)
	}
}

func TestLiveEvictMovesTheGuestWithoutStoppingIt(t *testing.T) {
	fastLive(t)
	s := newLiveSim(t)
	c := s.controller()

	started, err := c.resources.StartLiveEvict(context.Background(), "web1", "", 0)
	require.NoError(t, err)
	assert.Equal(t, "k1", started.SourceNode)
	assert.Equal(t, "k2", started.TargetNode, "the first replica that can take it")
	rec := waitLive(t, c)

	assert.Equal(t, liveOutcomeMoved, rec.Outcome, rec.Message)
	assert.Equal(t, "k2", rec.ActiveNode)
	assert.Equal(t, 100, rec.Progress)
	assert.Contains(t, rec.Message, "was not stopped")
	s.settled(t, "k2")
	assert.Equal(t, -1, s.index("guest-start:"), "the guest was migrated, never started again")

	// The order is the safety argument.
	before := func(a, b string) {
		t.Helper()
		ia, ib := s.index(a), s.index(b)
		require.NotEqual(t, -1, ia, a)
		require.NotEqual(t, -1, ib, b)
		assert.Less(t, ia, ib, "%s must come before %s", a, b)
	}
	before("guard:k1", "disable:k1")
	for _, n := range []string{"k1", "k2", "k3"} {
		before("disable:"+n, "two-primaries-on:")
	}
	before("two-primaries-on:k2", "promote:k2")
	before("promote:k2", "move-start:k1->k2")
	before("guest-arrived:k2", "demote:k1")
	before("demote:k1", "two-primaries-off:")
	before("two-primaries-off:k1", "retire:k1")
	before("retire:k1", "enable:k2")
	before("enable:k2", "enable:k1")
	before("enable:k2", "enable:k3")
	assert.Equal(t, -1, s.index("two-primaries-on:k3"), "the window is between the two nodes only")
}

func TestLiveEvictLeavesTheGuestWhereItWasWhenTheMigrationFails(t *testing.T) {
	fastLive(t)
	s := newLiveSim(t)
	s.moveFails = true
	c := s.controller()

	_, err := c.resources.StartLiveEvict(context.Background(), "web1", "k3", 0)
	require.NoError(t, err)
	rec := waitLive(t, c)

	assert.Equal(t, liveOutcomeStayed, rec.Outcome, rec.Message)
	assert.Equal(t, "k1", rec.ActiveNode)
	assert.Contains(t, rec.Message, "migration out job")
	s.settled(t, "k1")
	assert.Equal(t, 7, s.domID["10.0.0.1"], "the same guest, never restarted")
	assert.NotEqual(t, -1, s.index("demote:k3"), "the target gives the Primary role back")
}

func TestLiveEvictPutsThingsBackWhenTheTargetCannotPromote(t *testing.T) {
	fastLive(t)
	s := newLiveSim(t)
	s.promoteFails = true
	c := s.controller()

	_, err := c.resources.StartLiveEvict(context.Background(), "web1", "", 0)
	require.NoError(t, err)
	rec := waitLive(t, c)

	assert.Equal(t, liveOutcomeStayed, rec.Outcome, rec.Message)
	assert.Contains(t, rec.Message, "could not be made Primary on k2")
	s.settled(t, "k1")
	assert.Equal(t, -1, s.index("move-start:"), "no migration was started")
}

// A controller that restarts in the middle finds the record and settles
// around wherever the guest ended up.
func TestInterruptedLiveMoveIsSettledByTheNextController(t *testing.T) {
	fastLive(t)
	for _, tc := range []struct {
		name    string
		arrange func(s *liveSim)
		on      string
		outcome string
	}{
		{"before the migration ran", func(s *liveSim) {
			s.guard["10.0.0.1"], s.twoPrim["10.0.0.1"], s.twoPrim["10.0.0.2"] = true, true, true
			s.role["10.0.0.2"] = "Primary"
		}, "k1", liveOutcomeStayed},
		{"after the guest arrived", func(s *liveSim) {
			s.guard["10.0.0.1"], s.twoPrim["10.0.0.1"], s.twoPrim["10.0.0.2"] = true, true, true
			s.role["10.0.0.2"] = "Primary"
			s.guestOn, s.domID["10.0.0.2"], s.agent["10.0.0.1"] = "10.0.0.2", 31, "failed"
			s.moveState, s.moveRC = "done", "0"
		}, "k2", liveOutcomeMoved},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newLiveSim(t)
			for _, h := range s.hosts {
				s.promoter[h], s.marked[h] = "disabled", true
			}
			tc.arrange(s)
			c := s.controller()
			require.NoError(t, c.db.SaveHaMigration(context.Background(), &database.HaMigration{Resource: "web1", Domain: "web1",
				SourceNode: "k1", TargetNode: "k2", AgentUnit: "ocf.rs@vm_web1_web1.service", Phase: livePhaseMigrating,
				Deadline: time.Now().Add(time.Hour)}))

			c.resources.resumeLiveMigrations(context.Background())
			rec := waitLive(t, c)
			assert.Equal(t, tc.outcome, rec.Outcome, rec.Message)
			s.settled(t, tc.on)
		})
	}
}

func TestLiveEvictRefusesBeforeChangingAnything(t *testing.T) {
	fastLive(t)
	ctx := context.Background()
	untouched := func(t *testing.T, s *liveSim) {
		t.Helper()
		s.mu.Lock()
		defer s.mu.Unlock()
		assert.Empty(t, s.events, "nothing may change on a node before a refusal")
		assert.Empty(t, s.violations)
	}

	t.Run("target is not UpToDate", func(t *testing.T) {
		s := newLiveSim(t)
		s.disk["10.0.0.3"] = "Inconsistent"
		_, err := s.controller().resources.StartLiveEvict(ctx, "web1", "k3", 0)
		require.ErrorContains(t, err, "k3 cannot take the guest")
		assert.Contains(t, err.Error(), "Inconsistent")
		untouched(t, s)
	})
	t.Run("no replica can take it", func(t *testing.T) {
		s := newLiveSim(t)
		s.disk["10.0.0.2"], s.disk["10.0.0.3"] = "Outdated", "Inconsistent"
		_, err := s.controller().resources.StartLiveEvict(ctx, "web1", "", 0)
		require.ErrorContains(t, err, "no replica can take the guest")
		untouched(t, s)
	})
	t.Run("target is the node the guest runs on", func(t *testing.T) {
		s := newLiveSim(t)
		_, err := s.controller().resources.StartLiveEvict(ctx, "web1", "k1", 0)
		require.ErrorContains(t, err, "already runs on k1")
		untouched(t, s)
	})
	t.Run("a replica does not answer", func(t *testing.T) {
		s := newLiveSim(t)
		s.down["10.0.0.3"] = true
		_, err := s.controller().resources.StartLiveEvict(ctx, "web1", "", 0)
		require.ErrorContains(t, err, "k3 did not answer")
		untouched(t, s)
	})
	t.Run("the target's definition has another UUID", func(t *testing.T) {
		s := newLiveSim(t)
		c := s.controller()
		def, err := c.db.GetVMDefinition(ctx, "web1")
		require.NoError(t, err)
		def.UUID = "11111111-2222-4333-8444-555555555555"
		require.NoError(t, c.db.SaveVMDefinition(ctx, def))
		_, err = c.resources.StartLiveEvict(ctx, "web1", "k2", 0)
		require.ErrorContains(t, err, "haify ha vm-sync web1")
		untouched(t, s)
	})
	t.Run("the promoter is disabled", func(t *testing.T) {
		s := newLiveSim(t)
		s.promoter["10.0.0.2"] = "disabled"
		_, err := s.controller().resources.StartLiveEvict(ctx, "web1", "", 0)
		require.ErrorContains(t, err, "disabled on k2")
		untouched(t, s)
	})
	t.Run("dual-primary is already on", func(t *testing.T) {
		s := newLiveSim(t)
		s.twoPrim["10.0.0.1"] = true
		_, err := s.controller().resources.StartLiveEvict(ctx, "web1", "", 0)
		require.ErrorContains(t, err, "already allows two Primaries")
		untouched(t, s)
	})
	t.Run("a resource that runs no guest", func(t *testing.T) {
		s := newLiveSim(t)
		c := s.controller()
		require.NoError(t, c.db.SaveHaConfig(ctx, &database.HaConfig{Resource: "web1", Services: []string{"nginx.service"}, VIP: "10.0.0.50/24"}))
		_, err := c.resources.StartLiveEvict(ctx, "web1", "", 0)
		require.ErrorContains(t, err, "runs no libvirt guest")
		assert.Contains(t, err.Error(), "without --live")
		untouched(t, s)
	})
	t.Run("the controller's own resource", func(t *testing.T) {
		s := newLiveSim(t)
		_, err := s.controller().resources.StartLiveEvict(ctx, SelfHaResource, "", 0)
		require.ErrorContains(t, err, "runs no libvirt guest")
		untouched(t, s)
	})
}

func TestOtherHaOperationsWaitForALiveMove(t *testing.T) {
	s := newLiveSim(t)
	c := s.controller()
	job := &haLiveJob{rm: c.resources, rec: database.HaMigration{Resource: "web1", Domain: "web1", SourceNode: "k1",
		TargetNode: "k2", Phase: livePhaseMigrating}}
	require.True(t, c.resources.live.reserve("web1", job))
	ctx := context.Background()

	require.ErrorContains(t, c.resources.EvictHa(ctx, "web1"), "live move of web1 from k1 to k2 is in progress")
	require.ErrorContains(t, c.resources.RemoveHa(ctx, "web1"), "in progress")
	_, err := c.resources.SyncHaToml(ctx, "web1", "[[promoter]]\n")
	require.ErrorContains(t, err, "in progress")
	_, err = c.resources.SyncVMDefinition(ctx, "web1", "")
	require.ErrorContains(t, err, "in progress")
	_, err = c.resources.StartLiveEvict(ctx, "web1", "", 0)
	require.ErrorContains(t, err, "in progress")
	assert.Empty(t, s.events)
}

func TestLiveProgress(t *testing.T) {
	assert.Equal(t, 0, liveProgress("", ""))
	assert.Equal(t, 50, liveProgress("1.000 GiB", "2.000 GiB"))
	assert.Equal(t, 25, liveProgress("512.000 MiB", "2.000 GiB"))
	assert.Equal(t, 99, liveProgress("3.000 GiB", "2.000 GiB"), "a guest that dirties memory is copied more than once")
}

func TestLiveMoveURIs(t *testing.T) {
	dest, data := liveMoveURIs("192.168.1.12")
	assert.Equal(t, "qemu+ssh://192.168.1.12/system", dest)
	assert.Equal(t, "tcp://192.168.1.12", data)
	dest, data = liveMoveURIs("fd00::12")
	assert.Equal(t, "qemu+ssh://[fd00::12]/system", dest)
	assert.Equal(t, "tcp://[fd00::12]", data)
}
