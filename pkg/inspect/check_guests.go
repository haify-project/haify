package inspect

import (
	"fmt"
	"strings"

	"go.uber.org/zap"
)

// GuestDefinition is a libvirt guest that `ha create --vm` handed to
// drbd-reactor, and its definition on each replica that may run it.
type GuestDefinition struct {
	Resource string
	Domain   string
	// UUID is the identity Haify keeps for the guest; Kept is false when no
	// definition has been taken yet.
	UUID string
	Kept bool
	// Nodes is each replica's copy.
	Nodes []GuestNode
	// Unsettled is set when a live move of the guest was interrupted and
	// has not been put back yet; it says what state that left.
	Unsettled string
}

// GuestNode is one replica's copy of a guest's definition. State is ok,
// missing, changed, uuid_mismatch or unreachable.
type GuestNode struct {
	Node    string
	State   string
	UUID    string
	Running bool
	Detail  string
}

// checkGuests judges whether every replica of an HA guest could start it:
// the takeover and the live move both need the guest defined there, under
// the UUID the running guest has. Nothing reports that until the day the
// guest has to move.
func checkGuests(in *Input) []Check {
	log := zap.L()
	log.Debug("checkGuests: start", zap.Int("guests", len(in.Guests)))
	var out []Check
	for _, g := range in.Guests {
		var missing, mismatch, changed, ev []string
		for _, n := range g.Nodes {
			line := fmt.Sprintf("%s: %s", n.Node, n.State)
			if n.UUID != "" {
				line += ", UUID " + n.UUID
			}
			if n.Running {
				line += ", running"
			}
			if n.Detail != "" {
				line += " (" + n.Detail + ")"
			}
			ev = append(ev, line)
			switch n.State {
			case "missing":
				missing = append(missing, n.Node)
			case "uuid_mismatch":
				mismatch = append(mismatch, n.Node)
			case "changed":
				changed = append(changed, n.Node)
			}
		}
		log.Debug("checkGuests: guest", zap.String("resource", g.Resource), zap.String("domain", g.Domain), zap.Bool("kept", g.Kept),
			zap.Strings("missing", missing), zap.Strings("uuid_mismatch", mismatch), zap.Strings("changed", changed),
			zap.Bool("unsettled", g.Unsettled != ""))
		fix := "haify ha vm-sync " + g.Resource
		before := len(out)
		if g.Unsettled != "" {
			out = append(out, Check{ID: "guest.move_unsettled", Area: AreaResources, Subject: g.Resource, Status: StatusFail,
				Message: fmt.Sprintf("a live move of %s was interrupted and has not been put back: %s", g.Domain, g.Unsettled),
				Fix:     "haify ha migration " + g.Resource})
		}
		if !g.Kept {
			out = append(out, Check{ID: "guest.definition_not_kept", Area: AreaResources, Subject: g.Resource, Status: StatusWarn,
				Message: fmt.Sprintf("no definition of %s is kept, so a replica added later would not get one and the replicas' copies are not compared", g.Domain),
				Fix:     fix})
			continue
		}
		if len(missing) > 0 {
			out = append(out, Check{ID: "guest.definition_missing", Area: AreaResources, Subject: g.Resource, Status: StatusFail,
				Message:  fmt.Sprintf("%s is not defined on %s; a takeover there starts nothing", g.Domain, strings.Join(missing, ", ")),
				Evidence: ev, Fix: fix})
		}
		if len(mismatch) > 0 {
			out = append(out, Check{ID: "guest.definition_uuid", Area: AreaResources, Subject: g.Resource, Status: StatusFail,
				Message: fmt.Sprintf("%s is defined under another UUID on %s (expected %s); a live move there is refused and a takeover starts a different guest",
					g.Domain, strings.Join(mismatch, ", "), g.UUID),
				Evidence: ev, Fix: fix + " --from <node>"})
		}
		if len(changed) > 0 {
			out = append(out, Check{ID: "guest.definition_changed", Area: AreaResources, Subject: g.Resource, Status: StatusWarn,
				Message: fmt.Sprintf("the definition of %s on %s differs from the last sync; the guest would start with another configuration on another replica",
					g.Domain, strings.Join(changed, ", ")),
				Evidence: ev, Fix: fix})
		}
		if len(out) == before {
			out = append(out, Check{ID: "guest.definitions", Area: AreaResources, Subject: g.Resource, Status: StatusPass,
				Message: fmt.Sprintf("%s is defined with UUID %s on every replica that answered", g.Domain, g.UUID)})
		}
	}
	return out
}
