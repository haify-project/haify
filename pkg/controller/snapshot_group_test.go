package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/deployment"
)

// snapshotGroupFixture is a controller with two resources, "db" and "wal",
// each with one volume on two nodes. failOn makes the lvcreate of that
// snapshot name fail.
func snapshotGroupFixture(t *testing.T, failOn string) (*Controller, *[]string) {
	t.Helper()
	var cmds []string
	dep := &fakeDeploymentClient{}
	dep.execFunc = func(_ context.Context, hosts []string, cmd string, _ ...deployment.ExecOption) (*deployment.ExecResult, error) {
		cmds = append(cmds, hosts[0]+": "+cmd)
		if failOn != "" && strings.Contains(cmd, "lvcreate") && strings.Contains(cmd, " -n "+failOn+" ") {
			return failedExecResult(hosts, "no space"), nil
		}
		return successExecResult(hosts, ""), nil
	}
	ctrl := newBasicTestController(dep)
	ctrl.db = newTestDB(t)
	ctx := context.Background()
	for _, r := range []string{"db", "wal"} {
		require.NoError(t, ctrl.db.SaveResource(ctx, &database.Resource{Name: r, Port: 7000, Nodes: "10.0.0.1,10.0.0.2"}))
		require.NoError(t, ctrl.db.SaveVolume(ctx, &database.Volume{ResourceName: r, VolumeName: r + "_data", Pool: "vg0",
			SizeGB: 2, Device: "/dev/vg0/" + r + "_data"}))
	}
	return ctrl, &cmds
}

func groupMembers() []SnapshotGroupMember {
	return []SnapshotGroupMember{
		{Resource: "db", Volume: "vg0/db_data", Name: "g1_db", Node: "10.0.0.1"},
		{Resource: "wal", Volume: "vg0/wal_data", Name: "g1_wal", Node: "10.0.0.2"},
	}
}

// Every resource is suspended before the first snapshot and resumed only after
// the last: that is what puts all of them at one instant.
func TestSnapshotGroupSuspendsAllBeforeSnapshotting(t *testing.T) {
	ctrl, cmds := snapshotGroupFixture(t, "")
	require.NoError(t, ctrl.resources.CreateSnapshotGroup(context.Background(), groupMembers()))

	firstSnap, lastSnap, lastSuspend, firstResume := -1, -1, -1, -1
	for i, c := range *cmds {
		switch {
		case strings.Contains(c, "drbdadm suspend-io"):
			lastSuspend = i
		case strings.Contains(c, "lvcreate -s"):
			if firstSnap < 0 {
				firstSnap = i
			}
			lastSnap = i
		case strings.Contains(c, "sudo drbdadm resume-io") && firstResume < 0:
			firstResume = i
		}
	}
	joined := strings.Join(*cmds, "\n")
	require.True(t, lastSuspend >= 0 && firstSnap > lastSuspend, joined)
	require.True(t, firstResume > lastSnap, joined)
	for _, r := range []string{"db", "wal"} {
		assert.Contains(t, joined, "--on-active=60 drbdadm resume-io "+r+" && sudo drbdadm suspend-io "+r, "watchdog armed for "+r)
		assert.Contains(t, joined, "sudo drbdadm resume-io "+r+";", r+" resumed")
	}
	assert.Contains(t, joined, "10.0.0.1: sudo lvcreate -s -l 20%ORIGIN -n g1_db /dev/vg0/db_data")
	assert.Contains(t, joined, "10.0.0.2: sudo lvcreate -s -l 20%ORIGIN -n g1_wal /dev/vg0/wal_data")
	// The resource taken second resumes first.
	assert.Less(t, strings.Index(joined, "resume-io wal;"), strings.Index(joined, "resume-io db;"))
}

// A member that fails removes the ones already taken, and I/O still resumes.
func TestSnapshotGroupAllOrNothing(t *testing.T) {
	ctrl, cmds := snapshotGroupFixture(t, "g1_wal")
	err := ctrl.resources.CreateSnapshotGroup(context.Background(), groupMembers())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "g1_wal")
	joined := strings.Join(*cmds, "\n")
	assert.Contains(t, joined, "10.0.0.1: sudo lvremove -f /dev/vg0/g1_db", "the member already taken is removed")
	assert.Contains(t, joined, "sudo drbdadm resume-io db;")
	assert.Contains(t, joined, "sudo drbdadm resume-io wal;")
}

// A malformed group is refused before anything is suspended.
func TestSnapshotGroupValidation(t *testing.T) {
	cases := map[string][]SnapshotGroupMember{
		"empty":            nil,
		"unknown resource": {{Resource: "nope", Volume: "vg0/nope_data", Name: "g", Node: "10.0.0.1"}},
		"another's volume": {{Resource: "db", Volume: "vg0/wal_data", Name: "g", Node: "10.0.0.1"}},
		"shell in name":    {{Resource: "db", Volume: "vg0/db_data", Name: "g;reboot", Node: "10.0.0.1"}},
		"no node":          {{Resource: "db", Volume: "vg0/db_data", Name: "g"}},
		"named twice": {
			{Resource: "db", Volume: "vg0/db_data", Name: "a", Node: "10.0.0.1"},
			{Resource: "db", Volume: "vg0/db_data", Name: "b", Node: "10.0.0.1"},
		},
	}
	for name, members := range cases {
		t.Run(name, func(t *testing.T) {
			ctrl, cmds := snapshotGroupFixture(t, "")
			require.Error(t, ctrl.resources.CreateSnapshotGroup(context.Background(), members))
			assert.Empty(t, *cmds, "nothing runs on a node")
		})
	}
}
