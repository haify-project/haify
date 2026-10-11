package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	haifypb "github.com/haify-project/haify/api/proto/v1"
)

func TestHaStatusViews(t *testing.T) {
	p := &haifypb.HaPromoterStatus{
		DrbdResource: "openclaw", Status: "active", ActiveNode: "node-e",
		Target: &haifypb.HaServiceStatus{Name: "drbd-services@openclaw.target", Status: "active",
			Kind: "target", Label: "services of openclaw", SubState: "active"},
		Deps: []*haifypb.HaServiceStatus{
			{Name: "var-lib-openclaw.mount", Status: "active", SubState: "mounted", Kind: "mount",
				Label: "/var/lib/openclaw", Since: "2026-10-08T08:06:41Z"},
			{Name: "openclaw.service", Status: "failed", Kind: "service", Label: "OpenClaw Gateway", Restarts: 3},
		},
		Standbys: []*haifypb.HaStandby{
			{Node: "node-b", Ready: true, DiskState: "UpToDate"},
			{Node: "node-a", Reasons: []string{"openclaw.service not installed", "disk Outdated"}, DiskState: "Outdated"},
		},
		Outside: []*haifypb.HaOutsideService{
			{Unit: "backup-agent.service", Description: "Nightly backup agent", Processes: 2,
				MountPoints: []string{"/var/lib/openclaw"}},
		},
		OutsideSessions: 1, OutsideSessionProcesses: 3,
	}
	var b bytes.Buffer
	printHaChain(&b, p)
	printHaStandbys(&b, p)
	printHaOutside(&b, p)
	out := b.String()

	assert.Contains(t, out, "Service chain (on node-e):")
	assert.Regexp(t, `mount\s+/var/lib/openclaw\s+● active \(mounted\)\s+2026-10-0\d \d\d:\d\d\s+0\s+var-lib-openclaw.mount`, out)
	assert.Regexp(t, `service\s+OpenClaw Gateway\s+✗ failed\s+-\s+3\s+openclaw.service`, out)
	assert.Less(t, strings.Index(out, "openclaw.service"), strings.Index(out, "drbd-services@openclaw.target"),
		"the target comes last, as it starts last")
	assert.Regexp(t, `node-b\s+● yes\s+UpToDate\s+-`, out)
	assert.Regexp(t, `node-a\s+✗ no\s+Outdated\s+openclaw.service not installed; disk Outdated`, out)
	assert.Regexp(t, `backup-agent.service\s+2\s+/var/lib/openclaw\s+Nightly backup agent`, out)
	assert.Contains(t, out, "1 login session (3 processes)")

	var none bytes.Buffer
	idle := &haifypb.HaPromoterStatus{Status: "inactive"}
	printHaChain(&none, idle)
	printHaStandbys(&none, idle)
	printHaOutside(&none, idle)
	assert.Contains(t, none.String(), "no node is running it")
	assert.Contains(t, none.String(), "nothing can take over")
	assert.NotContains(t, none.String(), "Using the chain's mounts")
}
