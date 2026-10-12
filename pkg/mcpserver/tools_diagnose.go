package mcpserver

import (
	"context"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/triage"
)

// The diagnosis tool: read everything, then say what is wrong.
//
// The three observability tools next door each return one record, and an
// assistant asked "why did the cluster stop" has to call all three, guess a
// time window, notice that the failover event and the journal line four
// seconds later are the same story, and do it again for the node-side logs it
// cannot reach at all. In practice it does not: it calls one, finds a hundred
// lines, and answers from whichever of them was most recent.
//
// This tool does the reading and the correlating, and returns problems instead
// of lines. What it will not do is decide what to say — a finding it cannot
// show the line for is not returned, and only the failure modes Haify actually
// knows the cause of carry a cause and steps. The rest arrives as grouped
// evidence for the model to reason over, which is the honest division: the
// deterministic part is deterministic and the judgement is visibly judgement.

type diagnoseIn struct {
	Nodes []string `json:"nodes,omitempty" jsonschema:"nodes to read; empty for every registered node, which is usually what you want — a symptom on one node is often caused on another"`
	// The window bounds the journals. Events, audit and the controller log are
	// bounded by what the controller still holds, which is its own ring buffer
	// and can be shorter than this.
	WindowMinutes int      `json:"window_minutes,omitempty" jsonschema:"how far back to read the node journals; 0 means 60. Widen this when investigating something that started hours ago"`
	Collectors    []string `json:"collectors,omitempty" jsonschema:"which node collectors to run; empty for all. Names are returned in scanned and in available_collectors"`
	// SkipNodes exists for the case where the fan-out is the problem: a node
	// that is down makes every collector wait for its SSH timeout.
	SkipNodes bool `json:"skip_nodes,omitempty" jsonschema:"read only what the controller already holds (events, audit, its own log) and do not reach out to nodes. Faster, and misses every DRBD and kernel error"`
}

type diagnoseFinding struct {
	ID       string             `json:"id" jsonschema:"stable handle for this finding; a known failure mode has a name like phantom-peer, a grouped one has sig-xxxxxxxx"`
	Title    string             `json:"title"`
	Severity string             `json:"severity" jsonschema:"critical, error, or warning"`
	Count    int                `json:"count" jsonschema:"how many lines collapsed into this finding; a large number means a loop, not a blip"`
	Nodes    []string           `json:"nodes,omitempty"`
	Resource string             `json:"resource,omitempty"`
	Known    bool               `json:"known" jsonschema:"true when this matched a failure mode Haify knows the cause of; only then are cause and advice present, and they are then reliable enough to repeat verbatim"`
	Cause    string             `json:"cause,omitempty"`
	Advice   []string           `json:"advice,omitempty" jsonschema:"exact steps in order; commands are literal and can be given to an operator as written"`
	Caution  string             `json:"caution,omitempty" jsonschema:"present only when a step is destructive or irreversible; when present it must be repeated to the operator, not summarised away"`
	Evidence []diagnoseEvidence `json:"evidence,omitempty" jsonschema:"the lines this rests on; cite them rather than paraphrasing, so the operator can go and look"`
}

type diagnoseEvidence struct {
	Source string `json:"source" jsonschema:"event, audit, controller-log, or <node>/<collector>"`
	Node   string `json:"node,omitempty"`
	At     string `json:"at,omitempty"`
	Line   string `json:"line"`
	Repeat int    `json:"repeat,omitempty" jsonschema:"how many times this line's shape occurred, when more than the evidence shown"`
}

type diagnoseOut struct {
	WindowMinutes int               `json:"window_minutes"`
	Healthy       bool              `json:"healthy" jsonschema:"true only when something was read AND nothing was found; false with an empty findings list means nothing could be read, which is not the same answer"`
	Findings      []diagnoseFinding `json:"findings" jsonschema:"most severe first, and a known cause ahead of an equally severe pile of lines"`
	Scanned       diagnoseScanned   `json:"scanned"`
	// Notes carries what went wrong with the collection itself, so a thin
	// report is not mistaken for a quiet cluster.
	Notes []string `json:"notes,omitempty"`
}

type diagnoseScanned struct {
	Events           int      `json:"events"`
	AuditEntries     int      `json:"audit_entries"`
	ControllerLogs   int      `json:"controller_logs"`
	NodeLines        int      `json:"node_lines"`
	Nodes            []string `json:"nodes,omitempty"`
	Unreachable      []string `json:"unreachable,omitempty"`
	FailedCollectors []string `json:"failed_collectors,omitempty" jsonschema:"collectors that ran on no node at all; the report is built over a gap where these would have been"`
}

// diagnoseTimeout bounds the whole tool. It is the collector timeout times the
// table, plus room for the three controller reads.
const diagnoseTimeout = 4 * time.Minute

// How much of each record to pull. These are caps on what is read, not on what
// is reported: the analysis collapses repeats, so a wide read costs a request
// and not a context window.
const (
	diagEventLimit = 200
	diagAuditLimit = 200
	diagLogLimit   = 500
)

func (s *Server) registerDiagnoseTools(srv *mcp.Server) {
	addReadWithin(s, srv, readOnlyTool("haify_diagnose", "Diagnose the cluster",
		"Read every record the cluster keeps — operational events, the audit trail, the controller's own log, and "+
			"the nodes' journals, kernel messages, DRBD status and LVM usage — correlate them, and return the "+
			"problems rather than the lines. This is the tool to reach for when asked what is wrong, why something "+
			"broke, or why a resource will not start; the individual haify_event_list / haify_audit_list / haify_log_list "+
			"tools are for when you already know what you are looking for.\n\n"+
			"Findings marked known:true carry a cause and exact steps that Haify is sure of — repeat those verbatim, "+
			"including any caution, rather than rewording them. Findings without it are grouped evidence and nothing "+
			"more: reason over them, cite the lines, and say plainly when the evidence does not settle the question. "+
			"Do not invent a cause for a finding that has none.\n\n"+
			"healthy:false with an empty findings list means nothing could be read, not that the cluster is fine. "+
			"Check scanned.unreachable and scanned.failed_collectors before reporting a clean bill of health.",
	), diagnoseTimeout, s.handleDiagnose)
}

func (s *Server) handleDiagnose(ctx context.Context, _ *mcp.CallToolRequest, in diagnoseIn) (*mcp.CallToolResult, diagnoseOut, error) {
	s.logger.Debug("haify_diagnose: diagnosing", zap.Strings("nodes", in.Nodes), zap.Int("window_minutes", in.WindowMinutes),
		zap.Strings("collectors", in.Collectors), zap.Bool("skip_nodes", in.SkipNodes))
	window := in.WindowMinutes
	if window <= 0 {
		s.logger.Debug("haify_diagnose: no window given, using 60 minutes", zap.Int("window_minutes", in.WindowMinutes))
		window = 60
	}
	input := triage.Input{Now: time.Now(), Window: time.Duration(window) * time.Minute}
	var notes []string

	// Each source is optional. A controller with alerting off has no events and
	// a node that is down has no journal, and neither is a reason to refuse to
	// diagnose what is left — a diagnosis from partial evidence is useful as
	// long as it says which part is missing.
	start := time.Now()
	if resp, err := s.client.ListEvents(ctx, &haifypb.ListEventsRequest{Limit: diagEventLimit}); err != nil {
		s.logger.Debug("haify_diagnose: events unreadable, noting the gap", zap.Duration("took", time.Since(start)), zap.Error(err))
		notes = append(notes, "could not read cluster events: "+err.Error())
	} else {
		s.logger.Debug("haify_diagnose: read events", zap.Int("count", len(resp.Events)), zap.Duration("took", time.Since(start)))
		for _, e := range resp.Events {
			input.Events = append(input.Events, triage.Event{
				ID: e.Id, Type: e.Type, Severity: e.Severity, Status: e.Status,
				Resource: e.Resource, Node: e.Node, Message: e.Message, Details: e.Details,
				At: msTime(e.TimestampUnixMs),
			})
		}
	}

	start = time.Now()
	if resp, err := s.client.ListAuditEvents(ctx, &haifypb.ListAuditEventsRequest{
		Limit: diagAuditLimit, FailuresOnly: true,
	}); err != nil {
		s.logger.Debug("haify_diagnose: audit unreadable, noting the gap", zap.Duration("took", time.Since(start)), zap.Error(err))
		notes = append(notes, "could not read the audit trail: "+err.Error())
	} else {
		s.logger.Debug("haify_diagnose: read audit failures", zap.Int("count", len(resp.Events)), zap.Duration("took", time.Since(start)))
		for _, a := range resp.Events {
			input.Audit = append(input.Audit, triage.Audit{
				At: msTime(a.TimestampUnixMs), Method: a.Method, User: a.User,
				Target: a.Target, Result: a.Result, Granted: a.Granted,
				Node: a.Node, Error: a.Error,
			})
		}
	}

	start = time.Now()
	if resp, err := s.client.ListControllerLogs(ctx, &haifypb.ListControllerLogsRequest{
		Limit: diagLogLimit, Level: "warn",
	}); err != nil {
		s.logger.Debug("haify_diagnose: controller log unreadable, noting the gap", zap.Duration("took", time.Since(start)), zap.Error(err))
		notes = append(notes, "could not read the controller log: "+err.Error())
	} else {
		s.logger.Debug("haify_diagnose: read controller log", zap.Int("count", len(resp.Entries)), zap.Duration("took", time.Since(start)))
		for _, l := range resp.Entries {
			input.Logs = append(input.Logs, triage.LogEntry{
				At: msTime(l.TimestampUnixMs), Level: l.Level,
				Logger: l.Logger, Message: l.Message, Fields: l.Fields,
			})
		}
	}

	if in.SkipNodes {
		s.logger.Debug("haify_diagnose: skipping node collection as asked")
		notes = append(notes, "node collection was skipped, so no DRBD, kernel, promoter or storage evidence was read")
	} else {
		s.logger.Debug("haify_diagnose: collecting from nodes", zap.Strings("nodes", in.Nodes),
			zap.Strings("collectors", in.Collectors), zap.Int("since_minutes", window))
		start = time.Now()
		resp, err := s.client.CollectNodeDiagnostics(ctx, &haifypb.CollectNodeDiagnosticsRequest{
			Nodes: in.Nodes, Collectors: in.Collectors,
			SinceMinutes: int32(window), MaxLines: 200,
		})
		switch {
		case err != nil:
			s.logger.Debug("haify_diagnose: node collection failed, reporting controller records only",
				zap.Duration("took", time.Since(start)), zap.Error(err))
			notes = append(notes, "could not collect from nodes: "+err.Error()+
				" — this report covers only what the controller itself holds")
		default:
			s.logger.Debug("haify_diagnose: collected from nodes", zap.Int("nodes", len(resp.Nodes)),
				zap.Duration("took", time.Since(start)))
			for _, n := range resp.Nodes {
				s.logger.Debug("haify_diagnose: node report", zap.String("node", n.Node), zap.String("address", n.Address),
					zap.Bool("reachable", n.Reachable), zap.String("error", n.Error), zap.Int("collectors", len(n.Collectors)))
				nr := triage.NodeReport{
					Node: n.Node, Address: n.Address, Reachable: n.Reachable, Error: n.Error,
				}
				for _, c := range n.Collectors {
					s.logger.Debug("haify_diagnose: collector output", zap.String("node", n.Node),
						zap.String("collector", c.Collector), zap.Bool("ok", c.Ok), zap.Int("lines", len(c.Lines)),
						zap.Bool("truncated", c.Truncated), zap.String("error", c.Error))
					nr.Collectors = append(nr.Collectors, triage.CollectorOutput{
						Collector: c.Collector, Command: c.Command, Lines: c.Lines,
						Ok: c.Ok, Truncated: c.Truncated, Error: c.Error,
					})
				}
				input.Nodes = append(input.Nodes, nr)
			}
			for _, u := range resp.UnknownCollectors {
				s.logger.Debug("haify_diagnose: unknown collector requested", zap.String("collector", u))
				notes = append(notes, "no collector named "+u+"; the ones that exist are "+
					joinComma(resp.AvailableCollectors))
			}
		}
	}

	s.logger.Debug("haify_diagnose: analyzing", zap.Int("events", len(input.Events)), zap.Int("audit", len(input.Audit)),
		zap.Int("logs", len(input.Logs)), zap.Int("nodes", len(input.Nodes)), zap.Int("notes", len(notes)))
	rep := triage.Analyze(input)
	s.logger.Debug("haify_diagnose: analyzed", zap.Bool("healthy", rep.Healthy), zap.Int("findings", len(rep.Findings)),
		zap.Int("node_lines", rep.Scanned.NodeLines), zap.Strings("unreachable", rep.Scanned.Unreachable),
		zap.Strings("failed_collectors", rep.Scanned.FailedCollectors))
	return nil, diagnoseOut{
		WindowMinutes: rep.WindowMinutes,
		Healthy:       rep.Healthy,
		Findings:      toDiagnoseFindings(rep.Findings),
		Scanned: diagnoseScanned{
			Events:           rep.Scanned.Events,
			AuditEntries:     rep.Scanned.AuditEntries,
			ControllerLogs:   rep.Scanned.ControllerLogs,
			NodeLines:        rep.Scanned.NodeLines,
			Nodes:            rep.Scanned.Nodes,
			Unreachable:      rep.Scanned.Unreachable,
			FailedCollectors: rep.Scanned.FailedCollectors,
		},
		Notes: notes,
	}, nil
}

func toDiagnoseFindings(in []triage.Finding) []diagnoseFinding {
	out := make([]diagnoseFinding, 0, len(in))
	log := zap.L()
	for _, f := range in {
		log.Debug("toDiagnoseFindings: finding", zap.String("id", f.ID), zap.String("severity", string(f.Severity)),
			zap.Int("count", f.Count), zap.Bool("known", f.Known), zap.String("resource", f.Resource),
			zap.Int("evidence", len(f.Evidence)))
		df := diagnoseFinding{
			ID: f.ID, Title: f.Title, Severity: string(f.Severity), Count: f.Count,
			Nodes: f.Nodes, Resource: f.Resource, Known: f.Known,
			Cause: f.Cause, Advice: f.Advice, Caution: f.Caution,
		}
		for _, e := range f.Evidence {
			df.Evidence = append(df.Evidence, diagnoseEvidence{
				Source: e.Source, Node: e.Node, At: e.At, Line: e.Line, Repeat: e.Repeat,
			})
		}
		out = append(out, df)
	}
	return out
}

func msTime(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

func joinComma(s []string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += ", "
		}
		out += v
	}
	return out
}
