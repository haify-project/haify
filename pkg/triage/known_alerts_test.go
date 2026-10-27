package triage

import (
	"strings"
	"testing"
	"time"
)

// The two alerts that name a service failing on every node, as triage reads
// them: only while the condition is still raised, and with the steps that go
// with it rather than the ones that go with moving the Primary again.

func loopEvent(status string, at time.Time) Event {
	return Event{
		Type: "resource.failover_loop", Severity: "critical", Status: status,
		Resource: "openclaw", Message: "resource openclaw failed over 3 times in the last 15m",
		Details: map[string]string{"failovers": "3", "nodes": "node-b,node-e"}, At: at,
	}
}

func serviceEvent(status string, at time.Time) Event {
	return Event{
		Type: "ha.service_failing", Severity: "critical", Status: status,
		Resource: "openclaw", Node: "openclaw.service",
		Message: "service openclaw.service of resource openclaw restarted 5 times in the last 10m",
		Details: map[string]string{"unit": "openclaw.service", "node": "node-e", "state": "running", "restarts": "5"},
		At:      at,
	}
}

func TestAFailoverLoopStillRaisedIsReportedWithItsCause(t *testing.T) {
	at := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	in := Input{Window: time.Hour, Now: at.Add(time.Minute), Events: []Event{loopEvent("firing", at)}}

	f, ok := findingByID(Analyze(in), "failover-loop")
	if !ok {
		t.Fatal("a raised failover loop was not reported")
	}
	if !f.Known || f.Resource != "openclaw" || f.Severity != SeverityCritical {
		t.Errorf("known=%v resource=%q severity=%q, want true, openclaw, critical", f.Known, f.Resource, f.Severity)
	}
	if !strings.Contains(f.Cause, "every node") {
		t.Errorf("the cause must say the fault is not any one node's: %q", f.Cause)
	}
	steps := strings.Join(f.Advice, "\n")
	if !strings.Contains(steps, "ha status openclaw") {
		t.Errorf("advice does not start from the chain's status: %v", f.Advice)
	}
	if f.Caution == "" || !strings.Contains(strings.ToLower(f.Caution), "evict") {
		t.Errorf("moving the role again is the wrong answer and the caution must say so: %q", f.Caution)
	}
}

func TestAFailoverLoopThatResolvedIsNotReported(t *testing.T) {
	at := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	in := Input{Window: time.Hour, Now: at.Add(time.Hour), Events: []Event{
		loopEvent("firing", at), loopEvent("resolved", at.Add(20*time.Minute)),
	}}
	if _, ok := findingByID(Analyze(in), "failover-loop"); ok {
		t.Fatal("a loop that ended was reported as still going")
	}
}

// A condition that cleared and came back is raised: what matters is the last
// word on it, not that it ever resolved.
func TestAFailoverLoopRaisedAgainAfterResolvingIsReported(t *testing.T) {
	at := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	in := Input{Window: 2 * time.Hour, Now: at.Add(time.Hour), Events: []Event{
		loopEvent("firing", at),
		loopEvent("resolved", at.Add(20*time.Minute)),
		loopEvent("firing", at.Add(40*time.Minute)),
	}}
	if _, ok := findingByID(Analyze(in), "failover-loop"); !ok {
		t.Fatal("a loop that came back was not reported")
	}
}

func TestAFailingServiceNamesTheUnitAndTheNodeToReadItsJournalOn(t *testing.T) {
	at := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	in := Input{Window: time.Hour, Now: at.Add(time.Minute), Events: []Event{serviceEvent("firing", at)}}

	f, ok := findingByID(Analyze(in), "ha-service-failing")
	if !ok {
		t.Fatal("a raised service failure was not reported")
	}
	if !f.Known || f.Resource != "openclaw" || f.Severity != SeverityCritical {
		t.Errorf("known=%v resource=%q severity=%q, want true, openclaw, critical", f.Known, f.Resource, f.Severity)
	}
	if len(f.Nodes) != 1 || f.Nodes[0] != "node-e" {
		t.Errorf("the finding is about the host running the unit, not the unit: nodes=%v", f.Nodes)
	}
	if !strings.Contains(f.Title, "openclaw.service") {
		t.Errorf("the title does not name the unit: %q", f.Title)
	}
	steps := strings.Join(f.Advice, "\n")
	if !strings.Contains(steps, "journalctl -u openclaw.service") {
		t.Errorf("advice does not send the operator to the unit's own journal: %v", f.Advice)
	}
	if !strings.Contains(strings.ToLower(f.Cause), "drbd") {
		t.Errorf("the cause must say why the resource checks showed nothing: %q", f.Cause)
	}
}

func TestAFailingServiceThatRecoveredIsNotReported(t *testing.T) {
	at := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	in := Input{Window: time.Hour, Now: at.Add(time.Hour), Events: []Event{
		serviceEvent("firing", at), serviceEvent("resolved", at.Add(10*time.Minute)),
	}}
	if _, ok := findingByID(Analyze(in), "ha-service-failing"); ok {
		t.Fatal("a service that recovered was reported as still failing")
	}
}

// Two units of two resources are two conditions: one recovering says nothing
// about the other.
func TestOneServiceRecoveringDoesNotHideAnother(t *testing.T) {
	at := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	other := serviceEvent("firing", at)
	other.Resource = "cortexdb"
	other.Node = "cortexdb-grpc.service"
	other.Details = map[string]string{"unit": "cortexdb-grpc.service", "node": "node-b", "state": "dead"}

	in := Input{Window: time.Hour, Now: at.Add(time.Hour), Events: []Event{
		serviceEvent("firing", at), serviceEvent("resolved", at.Add(5*time.Minute)), other,
	}}
	var failing []Finding
	for _, f := range Analyze(in).Findings {
		if f.ID == "ha-service-failing" {
			failing = append(failing, f)
		}
	}
	if len(failing) != 1 || failing[0].Resource != "cortexdb" {
		t.Fatalf("want exactly the cortexdb finding, got %d: %+v", len(failing), failing)
	}
}

// A failover loop carries no node, so what tells two loops apart is the
// resource alone. One of them ending must not hide the other.
func TestOneFailoverLoopEndingDoesNotHideAnother(t *testing.T) {
	at := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	other := loopEvent("firing", at.Add(2*time.Minute))
	other.Resource = "cortexdb"

	in := Input{Window: time.Hour, Now: at.Add(time.Hour), Events: []Event{
		loopEvent("firing", at), loopEvent("resolved", at.Add(5*time.Minute)), other,
	}}
	var loops []Finding
	for _, f := range Analyze(in).Findings {
		if f.ID == "failover-loop" {
			loops = append(loops, f)
		}
	}
	if len(loops) != 1 || loops[0].Resource != "cortexdb" {
		t.Fatalf("want exactly the cortexdb loop, got %d: %+v", len(loops), loops)
	}
}
