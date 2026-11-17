package inspect

import (
	"strings"
	"testing"
)

func guestChecks(t *testing.T, g GuestDefinition) map[string]Check {
	t.Helper()
	in := cluster()
	in.Guests = []GuestDefinition{g}
	out := map[string]Check{}
	for _, c := range checkGuests(in) {
		if c.Area != AreaResources || c.Subject != g.Resource {
			t.Fatalf("check %s: area %q subject %q", c.ID, c.Area, c.Subject)
		}
		out[c.ID] = c
	}
	return out
}

func TestGuestDefinedOnEveryReplicaPasses(t *testing.T) {
	got := guestChecks(t, GuestDefinition{Resource: "web1", Domain: "web1", UUID: "6f1b", Kept: true, Nodes: []GuestNode{
		{Node: "n1", State: "ok", UUID: "6f1b", Running: true}, {Node: "n2", State: "ok", UUID: "6f1b"},
		// A node that did not answer is the nodes area's finding, not this one's.
		{Node: "n3", State: "unreachable"}}})
	if len(got) != 1 || got["guest.definitions"].Status != StatusPass {
		t.Fatalf("want one passing check, got %+v", got)
	}
}

func TestGuestMissingOnAReplicaFails(t *testing.T) {
	got := guestChecks(t, GuestDefinition{Resource: "web1", Domain: "web1", UUID: "6f1b", Kept: true, Nodes: []GuestNode{
		{Node: "n1", State: "ok", UUID: "6f1b", Running: true}, {Node: "n2", State: "missing", Detail: "no definition"},
		{Node: "n3", State: "uuid_mismatch", UUID: "9999"}}})
	missing, ok := got["guest.definition_missing"]
	if !ok || missing.Status != StatusFail || !strings.Contains(missing.Message, "not defined on n2") {
		t.Fatalf("missing definition not reported: %+v", got)
	}
	if missing.Fix != "haify ha vm-sync web1" {
		t.Errorf("fix = %q", missing.Fix)
	}
	if len(missing.Evidence) != 3 || !strings.Contains(missing.Evidence[0], "running") {
		t.Errorf("evidence = %v", missing.Evidence)
	}
	uuid, ok := got["guest.definition_uuid"]
	if !ok || uuid.Status != StatusFail || !strings.Contains(uuid.Message, "another UUID on n3") || !strings.Contains(uuid.Message, "6f1b") {
		t.Fatalf("UUID mismatch not reported: %+v", got)
	}
	if _, pass := got["guest.definitions"]; pass {
		t.Error("a failing guest must not also pass")
	}
}

func TestGuestDefinitionEditedOnOneNodeWarns(t *testing.T) {
	got := guestChecks(t, GuestDefinition{Resource: "web1", Domain: "web1", UUID: "6f1b", Kept: true, Nodes: []GuestNode{
		{Node: "n1", State: "changed", UUID: "6f1b", Detail: "edited on this node since the last sync"}, {Node: "n2", State: "ok", UUID: "6f1b"}}})
	c, ok := got["guest.definition_changed"]
	if !ok || c.Status != StatusWarn || len(got) != 1 {
		t.Fatalf("want one warning, got %+v", got)
	}
}

func TestGuestWithoutAKeptDefinitionWarns(t *testing.T) {
	got := guestChecks(t, GuestDefinition{Resource: "web1", Domain: "web1"})
	if c, ok := got["guest.definition_not_kept"]; !ok || c.Status != StatusWarn || c.Fix != "haify ha vm-sync web1" {
		t.Fatalf("got %+v", got)
	}
}

func TestInterruptedLiveMoveFails(t *testing.T) {
	got := guestChecks(t, GuestDefinition{Resource: "web1", Domain: "web1", UUID: "6f1b", Kept: true,
		Unsettled: `it stopped in phase "migrating" moving from n1 to n2`,
		Nodes:     []GuestNode{{Node: "n1", State: "ok"}, {Node: "n2", State: "ok"}}})
	c, ok := got["guest.move_unsettled"]
	if !ok || c.Status != StatusFail || c.Fix != "haify ha migration web1" {
		t.Fatalf("got %+v", got)
	}
	if _, pass := got["guest.definitions"]; pass {
		t.Error("an unsettled move must not read as a pass")
	}
}

func TestNoGuestsNoChecks(t *testing.T) {
	if got := checkGuests(cluster()); len(got) != 0 {
		t.Fatalf("a cluster without HA guests gets no guest checks, got %+v", got)
	}
}
