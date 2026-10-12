// Package triage turns what a cluster recorded into a short list of problems.
//
// The controller keeps three separate records — operational events, the audit
// trail, its own log — and the nodes keep a fourth in their journals and
// kernel ring buffers. Each is readable on its own and none of them answers
// "what is wrong": the events say a resource degraded, the node journal says
// why the promoter could not start it, and only reading both in the same
// minute connects them.
//
// This package does the connecting, and it does it deterministically. It is
// not a model and it does not guess: it normalises lines into signatures so
// four hundred occurrences of one problem are one finding, and it matches a
// small table of failure modes Haify actually knows the cause of. Everything
// else comes back as evidence, grouped and ranked, for something that can
// reason to read. What it must never do is state a cause it cannot show the
// line for — a confident wrong diagnosis of a storage cluster is worse than
// no diagnosis, because it is acted on.
package triage

import (
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Severity orders findings. Three levels, because a fourth would be a
// distinction nothing downstream acts on differently.
type Severity string

const (
	// SeverityCritical is data or availability at risk right now.
	SeverityCritical Severity = "critical"
	// SeverityError is something that failed and has not recovered.
	SeverityError Severity = "error"
	// SeverityWarning is a condition heading somewhere bad.
	SeverityWarning Severity = "warning"
)

func severityRank(s Severity) int {
	switch s {
	case SeverityCritical:
		return 0
	case SeverityError:
		return 1
	default:
		return 2
	}
}

// ── input ───────────────────────────────────────────────────────────────
//
// The input types are plain structs rather than the protobuf messages the
// caller happens to hold. That is what lets a test state a situation in six
// lines instead of building four nested messages, and this package is mostly
// judgement about situations — if stating one is laborious, the situations
// that get tested are the easy ones.

// Event is one operational notification the controller published.
type Event struct {
	ID       uint64
	Type     string
	Severity string
	Status   string
	Resource string
	Node     string
	Message  string
	Details  map[string]string
	At       time.Time
}

// Audit is one recorded RPC.
type Audit struct {
	At      time.Time
	Method  string
	User    string
	Target  string
	Result  string
	Granted bool
	Node    string
	Error   string
}

// LogEntry is one line from the controller's own ring buffer.
type LogEntry struct {
	At      time.Time
	Level   string
	Logger  string
	Message string
	Fields  map[string]string
}

// CollectorOutput is one collector's reading on one node.
type CollectorOutput struct {
	Collector string
	Command   string
	Lines     []string
	Ok        bool
	Truncated bool
	Error     string
}

// NodeReport is everything collected from one node.
type NodeReport struct {
	Node       string
	Address    string
	Reachable  bool
	Error      string
	Collectors []CollectorOutput
}

// Collector returns the named collector's output, and whether it ran.
func (n NodeReport) Collector(name string) (CollectorOutput, bool) {
	for _, c := range n.Collectors {
		if c.Collector == name {
			return c, true
		}
	}
	return CollectorOutput{}, false
}

// Input is everything gathered for one analysis.
type Input struct {
	Now    time.Time
	Window time.Duration
	Events []Event
	Audit  []Audit
	Logs   []LogEntry
	Nodes  []NodeReport
}

// ── output ──────────────────────────────────────────────────────────────

// Evidence is one line that supports a finding, with enough to find it again.
type Evidence struct {
	// Source is where the line came from: "event", "audit", "controller-log",
	// or "<node>/<collector>". A reader has to be able to go look.
	Source string   `json:"source"`
	Node   string   `json:"node,omitempty"`
	At     string   `json:"at,omitempty"`
	Line   string   `json:"line"`
	Repeat int      `json:"repeat,omitempty"`
	Around []string `json:"around,omitempty"`
}

// Finding is one problem.
type Finding struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	Severity Severity `json:"severity"`
	// Count is how many lines collapsed into this one finding. Four hundred is
	// a different problem from two, and it is the only number here that says so.
	Count int      `json:"count"`
	Nodes []string `json:"nodes,omitempty"`
	// Resource, when the finding is about one.
	Resource string `json:"resource,omitempty"`
	// Signature is the normalised line, present on grouped findings so a
	// caller can see what was merged.
	Signature string `json:"signature,omitempty"`
	// Known says a failure mode in the table matched. Only a known finding
	// carries Cause and Advice; everything else carries evidence and stops.
	Known bool `json:"known"`
	// Cause is why this happens, stated only where Haify knows.
	Cause string `json:"cause,omitempty"`
	// Advice is the exact steps, in order. Commands are literal — a step
	// somebody has to adapt is a step they will adapt wrongly at 3am.
	Advice []string `json:"advice,omitempty"`
	// Caution is what to know before running Advice. Present when a step is
	// disruptive; absent otherwise, so its presence means something.
	Caution  string     `json:"caution,omitempty"`
	Evidence []Evidence `json:"evidence,omitempty"`
}

// Scanned is what the analysis actually read, so a caller can tell an empty
// report that means "nothing is wrong" from one that means "nothing was read".
type Scanned struct {
	Events         int      `json:"events"`
	AuditEntries   int      `json:"audit_entries"`
	ControllerLogs int      `json:"controller_logs"`
	NodeLines      int      `json:"node_lines"`
	Nodes          []string `json:"nodes"`
	Unreachable    []string `json:"unreachable,omitempty"`
	// FailedCollectors names collectors that ran nowhere. A collector that
	// failed on every node is a gap in the evidence, and a report built over a
	// gap has to say so rather than read as complete.
	FailedCollectors []string `json:"failed_collectors,omitempty"`
}

// Report is the result.
type Report struct {
	WindowMinutes int       `json:"window_minutes"`
	Findings      []Finding `json:"findings"`
	Scanned       Scanned   `json:"scanned"`
	// Healthy is true when nothing was found AND something was read. It exists
	// because those are the two ways to get an empty Findings list and they
	// mean opposite things.
	Healthy bool `json:"healthy"`
}

const (
	maxFindings        = 20
	maxEvidencePerFind = 4
	maxEvidenceLine    = 400
)

// Analyze reads everything and returns the ranked problems.
func Analyze(in Input) Report {
	log := zap.L()
	start := time.Now()
	log.Debug("Analyze: analyzing cluster records", zap.Duration("window", in.Window),
		zap.Int("events", len(in.Events)), zap.Int("audit_entries", len(in.Audit)),
		zap.Int("controller_logs", len(in.Logs)), zap.Int("nodes", len(in.Nodes)))
	if in.Now.IsZero() {
		log.Debug("Analyze: no reference time given, using now")
		in.Now = time.Now()
	}
	rep := Report{
		WindowMinutes: int(in.Window / time.Minute),
		Scanned:       scan(in),
	}

	found := knownFindings(in)
	// A known failure mode owns its lines. Grouping them again would report
	// the same problem twice, once with a cause and once without, and the
	// second copy reads as a separate fault.
	claimed := map[string]bool{}
	for _, f := range found {
		for _, e := range f.Evidence {
			claimed[e.Line] = true
		}
	}
	log.Debug("Analyze: known failure modes matched", zap.Int("count", len(found)), zap.Int("claimed_lines", len(claimed)))
	found = append(found, groupedFindings(in, claimed)...)

	sort.SliceStable(found, func(i, j int) bool {
		a, b := found[i], found[j]
		if ra, rb := severityRank(a.Severity), severityRank(b.Severity); ra != rb {
			return ra < rb
		}
		// A known cause outranks an equally severe pile of lines: it is the one
		// a reader can act on.
		if a.Known != b.Known {
			return a.Known
		}
		return a.Count > b.Count
	})
	if len(found) > maxFindings {
		log.Debug("Analyze: truncating findings", zap.Int("count", len(found)), zap.Int("max", maxFindings))
		found = found[:maxFindings]
	}
	for i := range found {
		if len(found[i].Evidence) > maxEvidencePerFind {
			found[i].Evidence = found[i].Evidence[:maxEvidencePerFind]
		}
		for j := range found[i].Evidence {
			found[i].Evidence[j].Line = clip(found[i].Evidence[j].Line, maxEvidenceLine)
		}
	}

	rep.Findings = found
	read := rep.Scanned.Events + rep.Scanned.AuditEntries +
		rep.Scanned.ControllerLogs + rep.Scanned.NodeLines
	rep.Healthy = len(found) == 0 && read > 0
	if ce := log.Check(zap.DebugLevel, "Analyze: findings"); ce != nil {
		for _, f := range found {
			log.Debug("Analyze: finding", zap.String("id", f.ID), zap.String("severity", string(f.Severity)),
				zap.Bool("known", f.Known), zap.Int("count", f.Count), zap.Strings("nodes", f.Nodes),
				zap.String("resource", f.Resource))
		}
		ce.Write(zap.Int("count", len(found)))
	}
	log.Debug("Analyze: done", zap.Int("findings", len(found)), zap.Int("lines_read", read),
		zap.Bool("healthy", rep.Healthy), zap.Duration("took", time.Since(start)))
	return rep
}

func scan(in Input) Scanned {
	log := zap.L()
	s := Scanned{
		Events:         len(in.Events),
		AuditEntries:   len(in.Audit),
		ControllerLogs: len(in.Logs),
	}
	// A collector counts as failed only when it ran nowhere; one node missing
	// drbd-reactor is normal on a DR replica and is not a gap in the evidence.
	ran, seen := map[string]bool{}, map[string]bool{}
	for _, n := range in.Nodes {
		s.Nodes = append(s.Nodes, n.Node)
		log.Debug("scan: node", zap.String("node", n.Node), zap.Bool("reachable", n.Reachable),
			zap.Int("collectors", len(n.Collectors)))
		if !n.Reachable {
			s.Unreachable = append(s.Unreachable, n.Node)
		}
		for _, c := range n.Collectors {
			seen[c.Collector] = true
			s.NodeLines += len(c.Lines)
			log.Debug("scan: collector", zap.String("node", n.Node), zap.String("collector", c.Collector),
				zap.Bool("ok", c.Ok), zap.Int("lines", len(c.Lines)), zap.Bool("truncated", c.Truncated))
			if c.Ok {
				ran[c.Collector] = true
			}
		}
	}
	for c := range seen {
		if !ran[c] {
			log.Debug("scan: collector ran on no node", zap.String("collector", c))
			s.FailedCollectors = append(s.FailedCollectors, c)
		}
	}
	sort.Strings(s.FailedCollectors)
	log.Debug("scan: done", zap.Int("events", s.Events), zap.Int("audit_entries", s.AuditEntries),
		zap.Int("controller_logs", s.ControllerLogs), zap.Int("node_lines", s.NodeLines),
		zap.Strings("unreachable", s.Unreachable), zap.Strings("failed_collectors", s.FailedCollectors))
	return s
}

// ── generic grouping ────────────────────────────────────────────────────

type group struct {
	sig      string
	sev      Severity
	count    int
	nodes    map[string]bool
	evidence []Evidence
}

// groupedFindings collapses everything not already claimed by a known mode.
func groupedFindings(in Input, claimed map[string]bool) []Finding {
	groups := map[string]*group{}

	add := func(sev Severity, source, node, at, line string) {
		line = strings.TrimSpace(line)
		if line == "" || claimed[line] {
			return
		}
		sig := Signature(line)
		if sig == "" {
			return
		}
		g := groups[sig]
		if g == nil {
			g = &group{sig: sig, sev: sev, nodes: map[string]bool{}}
			groups[sig] = g
		}
		if severityRank(sev) < severityRank(g.sev) {
			g.sev = sev
		}
		g.count++
		if node != "" {
			g.nodes[node] = true
		}
		if len(g.evidence) < maxEvidencePerFind {
			g.evidence = append(g.evidence, Evidence{Source: source, Node: node, At: at, Line: line})
		}
	}

	log := zap.L()
	log.Debug("groupedFindings: grouping unclaimed lines", zap.Int("claimed_lines", len(claimed)))
	resolved, eventsAdded := 0, 0
	for _, e := range in.Events {
		if resolvedLater(e, in.Events) {
			resolved++
			continue
		}
		if sev, ok := eventSeverity(e); ok {
			eventsAdded++
			add(sev, "event", e.Node, stamp(e.At), e.Type+": "+e.Message)
		}
	}
	log.Debug("groupedFindings: events grouped", zap.Int("events", len(in.Events)),
		zap.Int("resolved_later", resolved), zap.Int("added", eventsAdded))
	auditAdded := 0
	for _, a := range in.Audit {
		if a.Result == "" || strings.EqualFold(a.Result, "OK") {
			continue
		}
		msg := a.Method + " " + a.Target + " -> " + a.Result
		if a.Error != "" {
			msg += ": " + a.Error
		}
		auditAdded++
		add(SeverityWarning, "audit", a.Node, stamp(a.At), msg)
	}
	log.Debug("groupedFindings: audit grouped", zap.Int("audit_entries", len(in.Audit)), zap.Int("failed", auditAdded))
	logsAdded := 0
	for _, l := range in.Logs {
		switch strings.ToLower(l.Level) {
		case "error", "fatal", "panic", "dpanic":
			logsAdded++
			add(SeverityError, "controller-log", "", stamp(l.At), l.Message)
		case "warn", "warning":
			logsAdded++
			add(SeverityWarning, "controller-log", "", stamp(l.At), l.Message)
		}
	}
	log.Debug("groupedFindings: controller log grouped", zap.Int("controller_logs", len(in.Logs)),
		zap.Int("warn_or_worse", logsAdded))
	for _, n := range in.Nodes {
		for _, c := range n.Collectors {
			flagged := 0
			for _, line := range c.Lines {
				if sev, ok := lineSeverity(line); ok {
					flagged++
					add(sev, n.Node+"/"+c.Collector, n.Node, "", line)
				}
			}
			log.Debug("groupedFindings: node collector grouped", zap.String("node", n.Node),
				zap.String("collector", c.Collector), zap.Int("lines", len(c.Lines)), zap.Int("flagged", flagged))
		}
	}

	out := make([]Finding, 0, len(groups))
	for _, g := range groups {
		out = append(out, Finding{
			ID:        signatureID(g.sig),
			Title:     title(g.evidence, g.sig),
			Severity:  g.sev,
			Count:     g.count,
			Nodes:     sortedKeys(g.nodes),
			Signature: g.sig,
			Evidence:  withRepeat(g.evidence, g.count),
		})
	}
	// Map order is not an order. Sort by count then signature so two runs over
	// the same input produce the same report — a diagnosis that reshuffles
	// itself cannot be compared with the one from ten minutes ago.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Signature < out[j].Signature
	})
	log.Debug("groupedFindings: done", zap.Int("groups", len(out)))
	return out
}

func withRepeat(ev []Evidence, count int) []Evidence {
	if len(ev) > 0 && count > len(ev) {
		ev[0].Repeat = count
	}
	return ev
}

// resolvedLater reports whether a later event closed this one: the same type
// on the same resource, resolved, on the same node or on none. The event log
// keeps every firing record; without this, a fault that came and went five
// times read as five current faults, and a report about the one still open
// named the ones already over as its cause.
func resolvedLater(e Event, all []Event) bool {
	if strings.EqualFold(e.Status, "resolved") {
		return false
	}
	for _, r := range all {
		if r.At.After(e.At) && strings.EqualFold(r.Status, "resolved") &&
			r.Type == e.Type && r.Resource == e.Resource && (r.Node == "" || r.Node == e.Node) {
			return true
		}
	}
	return false
}

func eventSeverity(e Event) (Severity, bool) {
	// A resolved event is the record of something that ended. Reporting it as
	// a current problem is how a report about a cluster that recovered an hour
	// ago reads exactly like one about a cluster that is still down.
	if strings.EqualFold(e.Status, "resolved") {
		return "", false
	}
	switch strings.ToLower(e.Severity) {
	case "critical":
		return SeverityCritical, true
	case "warning":
		return SeverityWarning, true
	}
	return "", false
}

// lineSeverity classifies a node log line. It is deliberately narrower than a
// generic log scanner: these lines come from journals that are mostly healthy
// chatter, and a matcher that fires on the word "failed" anywhere turns a
// normal boot into forty findings.
func lineSeverity(line string) (Severity, bool) {
	l := strings.ToLower(line)
	// Most lines naming a split brain are not one: the reactor's policy line,
	// one DRBD resolved, the handler that runs on every detection. The known
	// matcher decides which are; the rest are not worth a finding.
	if containsAny(l, "split-brain", "split brain") {
		if isSplitBrainReport(line) {
			return SeverityCritical, true
		}
		return "", false
	}
	switch {
	case containsAny(l, "kernel bug", "general protection fault", "segfault",
		"out of memory", "oom-killer", "i/o error",
		"panic:", "emergency", "aborting journal", "remounting filesystem read-only"):
		return SeverityCritical, true
	case containsAny(l, "error", "failed with result", "failed to", "cannot ",
		"unable to", "refused", "no space left", "timed out", "timeout"):
		return SeverityError, true
	case containsAny(l, "warning:", "warn ", " degraded", "retrying"):
		return SeverityWarning, true
	}
	return "", false
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// title is the first evidence line, shortened. The signature is the identity;
// the title is what a person reads first, and a real line reads better than a
// normalised one with `<n>` in it.
func title(ev []Evidence, sig string) string {
	s := sig
	if len(ev) > 0 {
		s = ev[0].Line
	}
	return clip(strings.TrimSpace(s), 160)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
