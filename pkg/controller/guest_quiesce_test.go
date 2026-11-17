package controller

import (
	"context"
	"encoding/base64"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/deployment"
)

// guestNode stands in for the KVM host: it answers the guest scripts and
// records every command in order.
type guestNode struct {
	mu   sync.Mutex
	cmds []string
	// freeze, thaw and resize are what the three scripts print.
	freeze, thaw, resize string
	// freezeErr fails the freeze script's transport instead.
	freezeErr error
	frozen    int // how many freezes are in force
	maxFrozen int
}

// decodeGuestScript returns the script a base64-wrapped command carries.
func decodeGuestScript(cmd string) (string, bool) {
	rest, ok := strings.CutPrefix(cmd, "echo ")
	if !ok || !strings.HasSuffix(cmd, " | base64 -d | sudo /bin/bash") {
		return "", false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSuffix(rest, " | base64 -d | sudo /bin/bash"))
	return string(raw), err == nil
}

func (n *guestNode) exec(_ context.Context, hosts []string, cmd string, _ ...deployment.ExecOption) (*deployment.ExecResult, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	script, wrapped := decodeGuestScript(cmd)
	switch {
	case wrapped && strings.Contains(script, "virsh domfsfreeze"):
		n.cmds = append(n.cmds, hosts[0]+": FREEZE")
		if n.freezeErr != nil {
			return nil, n.freezeErr
		}
		if strings.Contains(n.freeze, "frozen=yes") {
			n.frozen++
			n.maxFrozen = max(n.maxFrozen, n.frozen)
		}
		return successExecResult(hosts, n.freeze), nil
	case wrapped && strings.Contains(script, "thawed="):
		n.cmds = append(n.cmds, hosts[0]+": THAW")
		n.frozen = max(n.frozen-1, 0)
		return successExecResult(hosts, n.thaw), nil
	case wrapped && strings.Contains(script, "virsh blockresize"):
		n.cmds = append(n.cmds, hosts[0]+": RESIZE "+script)
		return successExecResult(hosts, n.resize), nil
	case wrapped:
		n.cmds = append(n.cmds, hosts[0]+": SCRIPT "+script)
	default:
		n.cmds = append(n.cmds, hosts[0]+": "+cmd)
	}
	return successExecResult(hosts, ""), nil
}

func (n *guestNode) joined() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return strings.Join(n.cmds, "\n")
}

// guestFixture is a two-node resource "web1-root" of guest "web1", which
// runs on 10.0.0.1 (the Primary).
func guestFixture(t *testing.T, n *guestNode) *Controller {
	t.Helper()
	dep := &fakeDeploymentClient{execFunc: n.exec}
	dep.lvIsThinFunc = func(context.Context, string, string, string) (bool, error) { return true, nil }
	dep.lvCreateThinSnapshotFunc = func(_ context.Context, hosts []string, vg, lv, name string) (*deployment.ExecResult, error) {
		n.mu.Lock()
		defer n.mu.Unlock()
		n.cmds = append(n.cmds, hosts[0]+": SNAPSHOT "+vg+"/"+name)
		if strings.Contains(name, "fails") {
			return failedExecResult(hosts, "no space"), nil
		}
		return successExecResult(hosts, ""), nil
	}
	dep.drbdStatusFunc = func(_ context.Context, hosts []string, resource string) (*deployment.ExecResult, error) {
		res := &deployment.ExecResult{Hosts: map[string]*deployment.HostResult{}}
		for _, h := range hosts {
			role := "Secondary"
			if h == "10.0.0.1" {
				role = "Primary"
			}
			res.Hosts[h] = &deployment.HostResult{Host: h, Success: true, Output: resource + " role:" + role + "\n  disk:UpToDate\n"}
		}
		return res, nil
	}
	ctrl := newBasicTestController(dep)
	ctrl.db = newTestDB(t)
	ctx := context.Background()
	for _, r := range []string{"web1-root", "web1-data"} {
		require.NoError(t, ctrl.db.SaveResource(ctx, &database.Resource{Name: r, Port: 7000, Nodes: "10.0.0.1,10.0.0.2",
			Labels: map[string]string{libvirtDomainLabel: "web1"}}))
		require.NoError(t, ctrl.db.SaveVolume(ctx, &database.Volume{ResourceName: r, VolumeName: r + "_00000", Pool: "vg0",
			SizeGB: 2, Device: "/dev/vg0/" + r + "_00000"}))
	}
	require.NoError(t, ctrl.db.SaveResource(ctx, &database.Resource{Name: "plain", Port: 7001, Nodes: "10.0.0.1,10.0.0.2"}))
	require.NoError(t, ctrl.db.SaveVolume(ctx, &database.Volume{ResourceName: "plain", VolumeName: "plain_00000", Pool: "vg0",
		SizeGB: 2, Device: "/dev/vg0/plain_00000"}))
	return ctrl
}

func indexAll(t *testing.T, s string, parts ...string) []int {
	t.Helper()
	out := make([]int, len(parts))
	for i, p := range parts {
		out[i] = strings.Index(s, p)
		require.GreaterOrEqual(t, out[i], 0, "missing %q in:\n%s", p, s)
	}
	return out
}

// A guest's resource is snapshotted between freeze and thaw, I/O is suspended
// inside the freeze, and the snapshots are marked filesystem consistent.
func TestGuestFrozenAroundResourceSnapshot(t *testing.T) {
	n := &guestNode{freeze: "frozen=yes\n", thaw: "thawed=yes\ndetail=Thawed 2 filesystem(s)\n"}
	ctrl := guestFixture(t, n)
	note, err := ctrl.resources.CreateResourceSnapshotNoted(context.Background(), "web1-root", "before")
	require.NoError(t, err)
	assert.Equal(t, "filesystem-consistent: guest web1 was frozen on 10.0.0.1 while it was taken", note)

	j := n.joined()
	at := indexAll(t, j, "10.0.0.1: FREEZE", "drbdadm suspend-io web1-root", "SNAPSHOT vg0/web1-root_00000_snap_before",
		"drbdadm resume-io web1-root;", "10.0.0.1: THAW", "lvchange --addtag haify.consistency=filesystem vg0/web1-root_00000_snap_before")
	assert.IsIncreasing(t, at, "freeze, suspend, snapshot, resume, thaw, mark:\n%s", j)
	assert.Equal(t, 2, strings.Count(j, "lvchange --addtag haify.consistency=filesystem"), "the mark is on every replica's snapshot")
	assert.NotContains(t, j, "10.0.0.2: FREEZE", "only the node the guest runs on freezes it")
}

// Without a guest agent the snapshot is taken all the same, and says what it
// is. Nothing was frozen, so nothing is thawed.
func TestGuestWithoutAgentIsCrashConsistent(t *testing.T) {
	n := &guestNode{freeze: "guest=no-agent\ndetail=error: Guest agent is not responding: QEMU guest agent is not connected\n"}
	ctrl := guestFixture(t, n)
	note, err := ctrl.resources.CreateResourceSnapshotNoted(context.Background(), "web1-root", "before")
	require.NoError(t, err)
	assert.Contains(t, note, "crash-consistent: the guest agent of web1 did not freeze its filesystems")
	assert.Contains(t, note, "QEMU guest agent is not connected")
	j := n.joined()
	assert.NotContains(t, j, "THAW")
	assert.Contains(t, j, "lvchange --addtag haify.consistency=crash vg0/web1-root_00000_snap_before")
}

// A freeze that never reported back may have frozen the guest: the thaw runs,
// and the snapshot is not called filesystem consistent.
func TestGuestFreezeOfUnknownOutcomeIsThawed(t *testing.T) {
	n := &guestNode{freezeErr: errors.New("context deadline exceeded"), thaw: "thawed=yes\n"}
	ctrl := guestFixture(t, n)
	note, err := ctrl.resources.CreateResourceSnapshotNoted(context.Background(), "web1-root", "before")
	require.NoError(t, err)
	assert.Contains(t, note, "crash-consistent: ")
	assert.Contains(t, n.joined(), "10.0.0.1: THAW")
}

// A snapshot that outlasts the watchdog was not frozen to its end.
func TestGuestFreezeThatExpiredIsCrashConsistent(t *testing.T) {
	for _, thaw := range []string{"thawed=expired\n", "thawed=not-frozen\n", "thawed=failed\ndetail=agent gone\n", ""} {
		n := &guestNode{freeze: "frozen=yes\n", thaw: thaw}
		ctrl := guestFixture(t, n)
		note, err := ctrl.resources.CreateResourceSnapshotNoted(context.Background(), "web1-root", "before")
		require.NoError(t, err, thaw)
		assert.Contains(t, note, "crash-consistent: ", thaw)
		assert.Contains(t, n.joined(), "haify.consistency=crash", thaw)
	}
}

// A failed snapshot still thaws the guest.
func TestGuestThawedWhenSnapshotFails(t *testing.T) {
	n := &guestNode{freeze: "frozen=yes\n", thaw: "thawed=yes\n"}
	ctrl := guestFixture(t, n)
	_, err := ctrl.resources.CreateResourceSnapshotNoted(context.Background(), "web1-root", "fails")
	require.Error(t, err)
	j := n.joined()
	assert.Contains(t, j, "10.0.0.1: THAW")
	assert.NotContains(t, j, "haify.consistency", "nothing is left to mark")
	assert.Zero(t, n.frozen)
}

// A resource of no guest is snapshotted exactly as before.
func TestResourceWithoutGuestIsNotFrozen(t *testing.T) {
	n := &guestNode{freeze: "frozen=yes\n", thaw: "thawed=yes\n"}
	ctrl := guestFixture(t, n)
	note, err := ctrl.resources.CreateResourceSnapshotNoted(context.Background(), "plain", "before")
	require.NoError(t, err)
	assert.Empty(t, note)
	j := n.joined()
	assert.NotContains(t, j, "FREEZE")
	assert.NotContains(t, j, "haify.consistency")
}

// The one-node snapshot, the schedule and the backup freeze the guest too.
func TestGuestFrozenOnEverySnapshotPath(t *testing.T) {
	ctx := context.Background()
	t.Run("volume snapshot", func(t *testing.T) {
		n := &guestNode{freeze: "frozen=yes\n", thaw: "thawed=yes\n"}
		ctrl := guestFixture(t, n)
		note, err := ctrl.snapshots.CreateSnapshotNoted(ctx, "vg0/web1-data_00000", "s1", "10.0.0.2")
		require.NoError(t, err)
		assert.Contains(t, note, "filesystem-consistent")
		j := n.joined()
		assert.IsIncreasing(t, indexAll(t, j, "10.0.0.1: FREEZE", "10.0.0.2: sudo lvcreate -s", "10.0.0.1: THAW",
			"lvchange --addtag haify.consistency=filesystem vg0/s1"), j)
	})
	t.Run("schedule", func(t *testing.T) {
		n := &guestNode{freeze: "frozen=yes\n", thaw: "thawed=yes\n"}
		ctrl := guestFixture(t, n)
		require.NoError(t, ctrl.db.SaveSnapshotSchedule(ctx, &database.SnapshotSchedule{Name: "web1-root", Resource: "web1-root", Enabled: true}))
		res := &ResourceInfo{Name: "web1-root", Nodes: []string{"10.0.0.1", "10.0.0.2"},
			Volumes: []*ResourceVolumeInfo{{Pool: "vg0", BackingVolume: "web1-root_00000", Device: "/dev/drbd1000", SizeGB: 2}}}
		s, err := ctrl.db.GetSnapshotSchedule(ctx, "web1-root")
		require.NoError(t, err)
		ctrl.schedules.snapshotResource(ctx, res, s, lockNow())
		j := n.joined()
		at := indexAll(t, j, "10.0.0.1: FREEZE", "10.0.0.1: SNAPSHOT vg0/", "10.0.0.2: SNAPSHOT vg0/", "10.0.0.1: THAW", "haify.consistency=filesystem")
		assert.IsIncreasing(t, at, j)
	})
	t.Run("backup", func(t *testing.T) {
		n := &guestNode{freeze: "frozen=yes\n", thaw: "thawed=yes\n"}
		ctrl := guestFixture(t, n)
		info := &ResourceInfo{Name: "web1-root", Nodes: []string{"10.0.0.1", "10.0.0.2"},
			Volumes: []*ResourceVolumeInfo{{Pool: "vg0", BackingVolume: "web1-root_00000", SizeGB: 2}}}
		snaps, consistency, err := ctrl.backups.snapshotVolumes(ctx, "10.0.0.2", info, lockNow())
		require.NoError(t, err)
		require.Len(t, snaps, 1)
		assert.Equal(t, "filesystem", consistency, "the backup records what its snapshots hold")
		j := n.joined()
		assert.IsIncreasing(t, indexAll(t, j, "10.0.0.1: FREEZE", "10.0.0.2: SNAPSHOT vg0/", "10.0.0.1: THAW", "haify.consistency=filesystem"), j)
	})
}

// Snapshots of two resources of one guest take turns: the guest is never
// thawed by one while the other still counts on the freeze.
func TestSnapshotsOfOneGuestTakeTurns(t *testing.T) {
	n := &guestNode{freeze: "frozen=yes\n", thaw: "thawed=yes\n"}
	ctrl := guestFixture(t, n)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		for _, r := range []string{"web1-root", "web1-data"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				q := ctrl.resources.quiesceGuests(context.Background(), r)
				assert.Equal(t, consistencyFilesystem, q.consistency(r))
				n.mu.Lock()
				assert.Equal(t, 1, n.frozen, "exactly this snapshot's freeze is in force")
				n.mu.Unlock()
				q.finish()
				q.finish() // a second call thaws nothing more
			}()
		}
	}
	wg.Wait()
	assert.Equal(t, 1, n.maxFrozen)
	assert.Equal(t, 8, strings.Count(n.joined(), "THAW"))
}

// A group covering two resources of one guest freezes it once.
func TestSnapshotGroupFreezesEachGuestOnce(t *testing.T) {
	n := &guestNode{freeze: "frozen=yes\n", thaw: "thawed=yes\n"}
	ctrl := guestFixture(t, n)
	note, err := ctrl.resources.CreateSnapshotGroupNoted(context.Background(), []SnapshotGroupMember{
		{Resource: "web1-root", Volume: "vg0/web1-root_00000", Name: "g-root", Node: "10.0.0.2"},
		{Resource: "web1-data", Volume: "vg0/web1-data_00000", Name: "g-data", Node: "10.0.0.2"},
	})
	require.NoError(t, err)
	assert.Equal(t, "filesystem-consistent: guest web1 was frozen on 10.0.0.1 while it was taken", note)
	j := n.joined()
	assert.Equal(t, 1, strings.Count(j, "FREEZE"))
	assert.Equal(t, 1, strings.Count(j, "THAW"))
	at := indexAll(t, j, "FREEZE", "drbdadm suspend-io web1-root", "lvcreate -s", "drbdadm resume-io web1-data;", "THAW")
	assert.IsIncreasing(t, at, j)
}

// A label that is not a guest name never reaches a script.
func TestGuestLabelIsValidated(t *testing.T) {
	n := &guestNode{}
	ctrl := guestFixture(t, n)
	ctx := context.Background()
	require.NoError(t, ctrl.db.SaveResource(ctx, &database.Resource{Name: "odd", Port: 7002, Nodes: "10.0.0.1",
		Labels: map[string]string{libvirtDomainLabel: "x'; reboot #"}}))
	assert.Empty(t, ctrl.resources.guestDomain(ctx, "odd"))
	assert.Equal(t, "web1", ctrl.resources.guestDomain(ctx, "web1-root"))
	assert.Empty(t, ctrl.resources.guestDomain(ctx, "plain"))
}

// The node scripts are valid bash, with every placeholder filled in, and the
// watchdog is armed before the freeze.
func TestGuestScriptsRender(t *testing.T) {
	scripts := map[string]string{
		"freeze": renderGuestScript(guestFreezeScript, "web1"),
		"thaw":   renderGuestScript(guestThawScript, "web1"),
		"resize": strings.NewReplacer("@DOM@", "web1", "@SRC@", "/dev/drbd/by-res/web1-root/0", "@WANT@", "1024",
			"@SETTLE@", "15").Replace(guestResizeScript),
	}
	bash, _ := exec.LookPath("bash")
	for name, s := range scripts {
		assert.NotRegexp(t, `@[A-Z]+@`, s, name)
		if bash != "" {
			cmd := exec.Command(bash, "-n")
			cmd.Stdin = strings.NewReader(s)
			out, err := cmd.CombinedOutput()
			assert.NoError(t, err, "%s: %s", name, out)
		}
	}
	f := scripts["freeze"]
	assert.Less(t, strings.Index(f, "systemd-run --unit=haify-guest-thaw-"), strings.Index(f, "virsh domfsfreeze"))
	assert.Contains(t, f, "--on-active=90 ")
	unit, _, _ := guestRuntime("web+1.example")
	assert.Regexp(t, `^haify-guest-thaw-[0-9a-f]{16}$`, unit)
}
