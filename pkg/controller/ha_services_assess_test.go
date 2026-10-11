package controller

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/deployment"
)

// standbyOpts varies a standby's probe of the openclaw chain.
type standbyOpts struct {
	reactord     string // systemctl is-active drbd-reactor; "" is active
	disk         string // local disk; "" is UpToDate, "-" is not up
	missingUnit  string // a unit systemd does not know there
	maskedUnit   string
	disabledConf bool
}

func openclawStandbyProbe(o standbyOpts) string {
	m := haProbeMarker
	j := openclawReactorJSON
	if o.disabledConf {
		j = strings.Replace(j, "haify-ha-openclaw.toml", "haify-ha-openclaw.toml.disabled", 1)
	}
	reactord := o.reactord
	if reactord == "" {
		reactord = "active"
	}
	var b strings.Builder
	b.WriteString(reactorLine(j, "inactive"))
	b.WriteString(m + " reactord " + reactord + "\n")
	b.WriteString(m + " drbd-begin\n")
	switch o.disk {
	case "-":
	case "":
		b.WriteString("openclaw role:Secondary\n  disk:UpToDate open:no\n")
	default:
		b.WriteString("openclaw role:Secondary\n  disk:" + o.disk + " open:no\n")
	}
	b.WriteString(m + " drbd-end\n" + m + " units-begin\n")
	for _, u := range []string{"cortexdb-grpc.service", "drbd-promote@openclaw.service", "drbd-services@openclaw.target",
		"openclaw.service", "service-ip@192.168.123.252-24.service", "var-lib-openclaw.mount"} {
		load, fileState := "loaded", "disabled"
		switch u {
		case o.missingUnit:
			load, fileState = "not-found", ""
		case o.maskedUnit:
			load, fileState = "masked", "masked"
		}
		b.WriteString(showBlock(u, "drbd-reactor controlled "+u, load, fileState, "inactive", "dead", "", "", 0) + "\n")
	}
	b.WriteString(m + " units-end\n")
	return b.String()
}

func mustFacts(t *testing.T, out string) *haNodeFacts {
	t.Helper()
	f, err := parseHaNodeProbe(out)
	require.NoError(t, err)
	return f
}

func TestHaStandbyReasons(t *testing.T) {
	cases := []struct {
		name    string
		opts    standbyOpts
		reasons []string
		disk    string
	}{
		{"ready", standbyOpts{}, nil, "UpToDate"},
		{"reactor down", standbyOpts{reactord: "inactive"}, []string{"drbd-reactor not running"}, "UpToDate"},
		{"reactor failed", standbyOpts{reactord: "failed"}, []string{"drbd-reactor not running (failed)"}, "UpToDate"},
		{"service missing", standbyOpts{missingUnit: "openclaw.service"}, []string{"openclaw.service not installed"}, "UpToDate"},
		{"template missing", standbyOpts{missingUnit: "service-ip@192.168.123.252-24.service"},
			[]string{"service-ip@.service not installed"}, "UpToDate"},
		{"masked", standbyOpts{maskedUnit: "cortexdb-grpc.service"}, []string{"cortexdb-grpc.service masked"}, "UpToDate"},
		{"generated mount not checked", standbyOpts{missingUnit: "var-lib-openclaw.mount"}, nil, "UpToDate"},
		{"outdated", standbyOpts{disk: "Outdated"}, []string{"disk Outdated"}, "Outdated"},
		{"not up", standbyOpts{disk: "-"}, []string{"resource not up"}, ""},
		{"several", standbyOpts{reactord: "inactive", missingUnit: "openclaw.service", disk: "Inconsistent"},
			[]string{"drbd-reactor not running", "openclaw.service not installed", "disk Inconsistent"}, "Inconsistent"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := mustFacts(t, openclawStandbyProbe(c.opts))
			reasons, disk := haStandbyReasons("openclaw", &f.Reactor.Promoter[0], f)
			assert.Equal(t, c.reasons, reasons)
			assert.Equal(t, c.disk, disk)
		})
	}
}

func TestHaStandbyReasonsOCFAgent(t *testing.T) {
	agent := "/usr/lib/ocf/resource.d/heartbeat/nfsserver"
	f := &haNodeFacts{
		ReactorState: "active",
		Drbd:         map[string]haDrbdLocal{"data": {Role: "Secondary", Disk: "UpToDate"}},
		Units: map[string]*haUnitFacts{
			"ocf.rs@nfsserver_data.service": {LoadState: "loaded", Env: map[string]string{"AGENT": agent}},
		},
		Agents: map[string]bool{agent: false},
	}
	p := &deployment.ReactorPromoterStatus{DRBDResource: "data",
		Dependencies: []deployment.ReactorServiceStatus{{Name: "ocf.rs@nfsserver_data.service"}}}
	reasons, _ := haStandbyReasons("data", p, f)
	assert.Equal(t, []string{"OCF agent heartbeat/nfsserver not installed"}, reasons)

	f.Agents[agent] = true
	reasons, _ = haStandbyReasons("data", p, f)
	assert.Empty(t, reasons)
}

func TestHaOutsideUsers(t *testing.T) {
	f := mustFacts(t, openclawActiveProbe())
	chain := map[string]bool{"cortexdb-grpc.service": true, "openclaw.service": true}

	out, sessions, sessionProcs := haOutsideUsers(chain, []string{"/var/lib/openclaw"}, f)
	assert.Equal(t, []HaOutsideService{{Unit: "backup-agent.service", Description: "Nightly backup agent",
		Processes: 2, MountPoints: []string{"/var/lib/openclaw"}}}, out)
	assert.Equal(t, 2, sessions)
	assert.Equal(t, 4, sessionProcs)

	// For the haify-meta chain, openclaw's units are outside if they used its
	// mount; here only the backup agent does, and the controller is its own.
	out, sessions, _ = haOutsideUsers(map[string]bool{"haify-controller.service": true}, []string{"/var/lib/haify"}, f)
	assert.Equal(t, []HaOutsideService{{Unit: "backup-agent.service", Description: "Nightly backup agent",
		Processes: 3, MountPoints: []string{"/var/lib/haify"}}}, out)
	assert.Zero(t, sessions)

	out, _, _ = haOutsideUsers(chain, nil, f)
	assert.Empty(t, out, "no mount, nothing to hold up")
	out, _, _ = haOutsideUsers(chain, []string{"/var/lib/openclaw"}, nil)
	assert.Empty(t, out)
}

func TestHaChainMountsNeverRoot(t *testing.T) {
	got := haChainMounts([]HaServiceStatus{
		{Kind: haKindMount, Label: "/var/lib/openclaw/"},
		{Kind: haKindMount, Label: "/"},
		{Kind: haKindMount, Label: "Filesystem"},
		{Kind: haKindService, Label: "/x"},
	})
	assert.Equal(t, []string{"/var/lib/openclaw"}, got)
}

func TestBuildHaPromoterStatus(t *testing.T) {
	probes := map[string]haProbeResult{
		"n1": {facts: mustFacts(t, openclawStandbyProbe(standbyOpts{missingUnit: "openclaw.service"}))},
		"n2": {facts: mustFacts(t, openclawActiveProbe())},
		"n3": {facts: mustFacts(t, openclawStandbyProbe(standbyOpts{disabledConf: true}))},
		"n4": {err: errors.New("ssh: connect timeout")},
		"n5": {facts: &haNodeFacts{ReactorState: "active", Drbd: map[string]haDrbdLocal{"openclaw": {Disk: "UpToDate"}}}},
	}
	hosts := []string{"n1", "n2", "n3", "n4", "n5"}
	failover := map[string]bool{"n1": true, "n2": true, "n3": true, "n4": true, "n5": true}
	label := func(h string) string { return "node-" + h }

	ps := buildHaPromoterStatus("openclaw", hosts, failover, probes, label)
	require.NotNil(t, ps)
	assert.Equal(t, "active", ps.Status)
	assert.Equal(t, "sds-e", ps.PrimaryOn)
	assert.Equal(t, "node-n2", ps.ActiveNode)
	assert.Equal(t, haKindTarget, ps.Target.Kind)

	var kinds, labels []string
	for _, d := range ps.Deps {
		kinds = append(kinds, d.Kind)
		labels = append(labels, d.Label)
	}
	assert.Equal(t, []string{haKindPromote, haKindMount, haKindServiceIP, haKindService, haKindService}, kinds)
	assert.Equal(t, []string{"promote openclaw", "/var/lib/openclaw", "192.168.123.252/24",
		"CortexDB shared brain (gRPC)", "OpenClaw Gateway"}, labels)
	assert.Equal(t, "2026-10-08T08:06:41Z", ps.Deps[4].Since)
	assert.Equal(t, 2965837, ps.Deps[4].MainPID)
	assert.Equal(t, "High Availability Service IP 192.168.123.252-24", ps.Deps[2].Description)

	assert.Equal(t, []HaStandby{
		{Node: "node-n1", Reasons: []string{"openclaw.service not installed"}, DiskState: "UpToDate"},
		{Node: "node-n4", Reasons: []string{"node did not answer"}},
		{Node: "node-n5", Reasons: []string{"drbd-reactor status unavailable"}, DiskState: "UpToDate"},
	}, ps.Standbys, "n3 holds only the disabled config")

	require.Len(t, ps.Outside, 1)
	assert.Equal(t, "backup-agent.service", ps.Outside[0].Unit)
	assert.Equal(t, 2, ps.OutsideSessions)
	assert.Equal(t, 4, ps.OutsideSessionProcesses)

	assert.Nil(t, buildHaPromoterStatus("other", hosts, failover, probes, label))
}

func TestBuildHaPromoterStatusNothingRunning(t *testing.T) {
	probes := map[string]haProbeResult{
		"n1": {facts: mustFacts(t, openclawStandbyProbe(standbyOpts{}))},
		"n2": {facts: mustFacts(t, openclawStandbyProbe(standbyOpts{disk: "Outdated"}))},
	}
	ps := buildHaPromoterStatus("openclaw", []string{"n1", "n2"}, map[string]bool{"n1": true, "n2": true},
		probes, func(h string) string { return h })
	require.NotNil(t, ps)
	assert.Empty(t, ps.ActiveNode)
	assert.Equal(t, "inactive", ps.Status)
	assert.Empty(t, ps.Outside)
	assert.Equal(t, []HaStandby{
		{Node: "n1", Ready: true, DiskState: "UpToDate"},
		{Node: "n2", Reasons: []string{"disk Outdated"}, DiskState: "Outdated"},
	}, ps.Standbys)
}

func TestGetHaStatusProbesOncePerNodeAndCaches(t *testing.T) {
	var calls atomic.Int32
	dep := &fakeDeploymentClient{}
	dep.execFunc = func(_ context.Context, hosts []string, cmd string, _ ...deployment.ExecOption) (*deployment.ExecResult, error) {
		require.Len(t, hosts, 1)
		require.Equal(t, haProbeCommand(), cmd)
		calls.Add(1)
		switch hosts[0] {
		case "n1":
			return successExecResult(hosts, openclawActiveProbe()), nil
		case "n2":
			return successExecResult(hosts, openclawStandbyProbe(standbyOpts{})), nil
		}
		return &deployment.ExecResult{Hosts: map[string]*deployment.HostResult{
			hosts[0]: {Host: hosts[0], Output: "ssh: no route to host"}}}, nil
	}
	ctrl := newBasicTestController(dep)
	ctrl.db = newTestDB(t)
	ctx := context.Background()
	require.NoError(t, ctrl.db.SaveHaConfig(ctx, &database.HaConfig{Resource: "openclaw"}))
	require.NoError(t, ctrl.db.SaveResource(ctx, &database.Resource{Name: "openclaw", Nodes: "n1,n2,n3"}))

	statuses, err := ctrl.resources.GetHaStatus(ctx, "openclaw")
	require.NoError(t, err)
	require.Len(t, statuses, 1)
	assert.Equal(t, int32(3), calls.Load(), "one probe per node")
	st := statuses[0]
	assert.Equal(t, "n1", st.ActiveNode)
	assert.Equal(t, []HaStandby{
		{Node: "n2", Ready: true, DiskState: "UpToDate"},
		{Node: "n3", Reasons: []string{"node did not answer"}},
	}, st.Standbys)

	_, err = ctrl.resources.GetHaStatus(ctx, "")
	require.NoError(t, err)
	assert.Equal(t, int32(3), calls.Load(), "a second poll within the TTL is answered from the cache")

	_, err = ctrl.resources.GetHaStatus(ctx, "missing")
	assert.ErrorContains(t, err, "ha config not found")
}
