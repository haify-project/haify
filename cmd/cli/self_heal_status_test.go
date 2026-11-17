package main

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	haifypb "github.com/haify-project/haify/api/proto/v1"
)

func TestPrintSelfHealStatus(t *testing.T) {
	now := time.Unix(1794650000, 0)
	pct := 37.25
	r := &haifypb.GetSelfHealStatusResponse{
		AutoEvict: &haifypb.AutoEvictStatus{Mode: "on", AfterMinutes: 60, MaxOfflinePercent: 34,
			Nodes: []*haifypb.AutoEvictNode{{Node: "n3", OfflineSince: now.Add(-20 * time.Minute).Unix(), RemainingSeconds: 2400,
				State: "waiting", Replicas: 4}}},
		Moves: []*haifypb.ReplicaMoveStatus{{Resource: "db", From: "n1", To: "n4", State: "Inconsistent", SyncPercent: &pct}},
		AutoDiskful: &haifypb.AutoDiskfulStatus{Mode: "on", AfterMinutes: 60, CooldownHours: 24,
			Clients: []*haifypb.AutoDiskfulClient{
				{Resource: "vol1", Node: "c", InUseSeconds: 1200, RemainingSeconds: 2400, State: "counting"},
				{Resource: "vol2", Node: "d", InUseSeconds: 7500, State: "skipped", Skip: "pinned", Reason: "its PersistentVolume is pinned"},
			},
			Swaps: []*haifypb.AutoDiskfulSwap{{Resource: "vol3", Node: "e", Phase: "syncing", SyncPercent: &pct, ReplicasNow: 3,
				ReplicasTarget: 2, StartedAt: now.Add(-90 * time.Minute).Unix()}},
			Cooldowns: []*haifypb.AutoDiskfulCooldown{{Resource: "vol4", MovedAt: now.Add(-time.Hour).Unix(), Until: now.Add(23 * time.Hour).Unix()}},
		},
	}
	var out bytes.Buffer
	printSelfHealStatus(&out, r, now)
	s := out.String()
	for _, want := range []string{
		"Auto-evict: on (after 60 min offline, at most 34% of the nodes offline)",
		"n3", "waiting", "20m", "40m",
		"db", "Inconsistent (37.2%)",
		"Auto-diskful: on (after 60 min of uninterrupted use, then 24 h before the same resource moves again)",
		"vol1", "counting", "vol2", "2h05m", "skipped", "its PersistentVolume is pinned",
		"vol3: replica on e, syncing (37.2%); 3 of 2 replicas; started 1h30m ago",
		"vol4: moved 1h00m ago",
	} {
		assert.Contains(t, s, want)
	}

	out.Reset()
	printSelfHealStatus(&out, &haifypb.GetSelfHealStatusResponse{AutoEvict: &haifypb.AutoEvictStatus{Mode: "off"},
		AutoDiskful: &haifypb.AutoDiskfulStatus{Mode: "off"}}, now)
	assert.Equal(t, "Auto-evict: off\n\nReplica moves: none\n\nAuto-diskful: off\n", out.String())

	assert.Equal(t, "3d4h", shortDuration(76*time.Hour))
	assert.Equal(t, "now", remaining(true, 0))
	assert.Equal(t, "-", remaining(false, 99))
}
