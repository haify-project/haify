package main

import (
	"bytes"
	"strings"
	"testing"

	haifypb "github.com/haify-project/haify/api/proto/v1"
)

func TestLiveProgressPrinterPrintsEachStepOnce(t *testing.T) {
	var out bytes.Buffer
	print := liveProgressPrinter(&out)
	m := &haifypb.HaMigration{Phase: "releasing", Steps: []string{"moving web1 from k1 to k2"}}
	print(m)
	print(m)
	m.Phase, m.Steps = "migrating", append(m.Steps, "migrating web1 to k2")
	m.ProgressPercent = 3
	print(m)
	m.ProgressPercent = 41
	print(m)
	m.ProgressPercent = 43
	print(m)
	m.Phase, m.Steps = "done", append(m.Steps, "web1 now runs on k2")
	print(m)

	want := "  moving web1 from k1 to k2\n  migrating web1 to k2\n  memory copied: 41%\n  web1 now runs on k2\n"
	if out.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestLiveOutcomeError(t *testing.T) {
	if err := liveOutcomeError(&haifypb.HaMigration{Outcome: "moved", ActiveNode: "k2"}); err != nil {
		t.Fatalf("a guest that moved is a success: %v", err)
	}
	for outcome, want := range map[string]string{
		"stayed":    "still running on k1",
		"restarted": "restarted and runs on k1",
		"attention": "needs attention",
	} {
		err := liveOutcomeError(&haifypb.HaMigration{Outcome: outcome, ActiveNode: "k1"})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", outcome, err)
		}
	}
}

func TestPrintHaMigrationAndDefinition(t *testing.T) {
	var out bytes.Buffer
	printHaMigration(&out, &haifypb.HaMigration{Resource: "web1", Domain: "web1", SourceNode: "k1", TargetNode: "k2",
		Phase: "migrating", ProgressPercent: 62, Steps: []string{"migrating web1 to k2"}})
	for _, want := range []string{"From:     k1", "To:       k2", "Phase:    migrating", "Copied:   62%", "- migrating web1 to k2"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, out.String())
		}
	}
	out.Reset()
	printVMDefinition(&out, &haifypb.VmDefinitionResponse{Resource: "web1", Domain: "web1", Uuid: "6f1b", Nodes: []*haifypb.VmDefinitionNode{
		{Node: "k1", State: "ok", Running: true}, {Node: "k2", State: "missing", Detail: "no definition"},
		{Node: "k3", State: "uuid_mismatch"}}})
	for _, want := range []string{"- k1: defined, guest running here", "- k2: not defined (no definition)", "- k3: another UUID"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, out.String())
		}
	}
}
