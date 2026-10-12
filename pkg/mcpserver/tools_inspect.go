package mcpserver

import (
	"context"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"

	haifypb "github.com/haify-project/haify/api/proto/v1"
)

// inspectRunTimeout covers one SSH probe round to every node (bounded at 90s
// on the controller) plus the database reads around it.
const inspectRunTimeout = 4 * time.Minute

type inspectCheckOut struct {
	ID       string   `json:"id" jsonschema:"stable slug, e.g. resource.no_primary, alerts.channel_failing, nodes.clock"`
	Area     string   `json:"area" jsonschema:"resources, gateways, nodes, pools, backups, alerts, selfha, tls or hygiene"`
	Subject  string   `json:"subject,omitempty" jsonschema:"the resource, node or channel concerned"`
	Status   string   `json:"status" jsonschema:"fail, error (the check could not run), warn, or pass"`
	Message  string   `json:"message"`
	Evidence []string `json:"evidence,omitempty" jsonschema:"what was seen, one fact per line"`
	Fix      string   `json:"fix,omitempty" jsonschema:"the command that fixes it, as written; confirm with the user before running anything that changes data"`
	Runbook  string   `json:"runbook,omitempty" jsonschema:"name of the haify_runbook entry covering the repair"`
}

type inspectReportOut struct {
	ID         string            `json:"id"`
	Trigger    string            `json:"trigger" jsonschema:"schedule or manual"`
	StartedAt  string            `json:"started_at" jsonschema:"RFC3339"`
	FinishedAt string            `json:"finished_at" jsonschema:"RFC3339"`
	Areas      []string          `json:"areas,omitempty" jsonschema:"areas inspected; empty means all"`
	Fail       int32             `json:"fail"`
	Warn       int32             `json:"warn"`
	Error      int32             `json:"error"`
	Pass       int32             `json:"pass"`
	Checks     []inspectCheckOut `json:"checks" jsonschema:"sorted by area, most serious first"`
}

type inspectReportIn struct {
	ID string `json:"id,omitempty" jsonschema:"report id; empty or latest for the newest"`
}

type inspectRunIn struct {
	Areas []string `json:"areas,omitempty" jsonschema:"only these areas: resources, gateways, nodes, pools, backups, alerts, selfha, tls, hygiene; empty for all"`
}

func reportOut(r *haifypb.InspectionReport) inspectReportOut {
	s := r.GetSummary()
	out := inspectReportOut{
		ID: r.Id, Trigger: r.Trigger, StartedAt: rfc3339(r.StartedAtUnixMs), FinishedAt: rfc3339(r.FinishedAtUnixMs),
		Areas: r.Areas, Fail: s.GetFail(), Warn: s.GetWarn(), Error: s.GetError(), Pass: s.GetPass(),
		Checks: make([]inspectCheckOut, 0, len(r.Checks)),
	}
	log := zap.L()
	for _, c := range r.Checks {
		log.Debug("reportOut: check", zap.String("id", c.Id), zap.String("area", c.Area), zap.String("subject", c.Subject),
			zap.String("status", c.Status))
		out.Checks = append(out.Checks, inspectCheckOut{ID: c.Id, Area: c.Area, Subject: c.Subject, Status: c.Status,
			Message: c.Message, Evidence: c.Evidence, Fix: c.Fix, Runbook: c.Runbook})
	}
	return out
}

// registerInspectTools adds the cluster inspection tools.
func (s *Server) registerInspectTools(srv *mcp.Server) {
	addRead(s, srv, readOnlyTool("haify_inspect_report", "Read an inspection report",
		"Read a stored cluster inspection report (the newest by default). The controller runs an inspection on a "+
			"schedule (daily by default): deterministic checks of what the alert detector does not cover — a "+
			"serving resource or gateway with no Primary, a replica stuck mid-handshake (WFBitMapS on one side), "+
			"notification channels whose deliveries fail and alerts that reached no one, node clocks, root disks, "+
			"registered addresses, DRBD and controller versions, thin pool growth, late or failing backups, "+
			"Self-HA readiness, certificate expiry, and leftovers of deleted resources. Each check has a status, "+
			"evidence and the exact fix command. Start here for 'is the cluster healthy?' before reading individual "+
			"resources; it is cheap, it only reads the stored report."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in inspectReportIn) (*mcp.CallToolResult, inspectReportOut, error) {
			s.logger.Debug("haify_inspect_report: reading report", zap.String("id", in.ID))
			id := in.ID
			if id == "" {
				s.logger.Debug("haify_inspect_report: no id given, reading the newest")
				id = "latest"
			}
			r, err := s.client.GetInspection(ctx, id)
			if err != nil {
				s.logger.Debug("haify_inspect_report: read failed", zap.String("id", id), zap.Error(err))
				return nil, inspectReportOut{}, err
			}
			out := reportOut(r)
			s.logger.Debug("haify_inspect_report: read", zap.String("id", out.ID), zap.Int("checks", len(out.Checks)),
				zap.Int32("fail", out.Fail), zap.Int32("warn", out.Warn), zap.Int32("error", out.Error))
			return nil, out, nil
		})

	addReadWithin(s, srv, readOnlyTool("haify_inspect_run", "Inspect the cluster now",
		"Run a cluster inspection now and return the report (see haify_inspect_report for what it checks). It changes "+
			"nothing on the cluster: one read-only probe per node over SSH plus database reads, then the report is "+
			"stored. Takes from seconds to a minute or two on a cluster with an unreachable node. Use it after a fix, "+
			"to confirm the finding cleared, or when the stored report is old. Fails if an inspection is already "+
			"running; read that one with haify_inspect_report afterwards."),
		inspectRunTimeout,
		func(ctx context.Context, _ *mcp.CallToolRequest, in inspectRunIn) (*mcp.CallToolResult, inspectReportOut, error) {
			s.logger.Debug("haify_inspect_run: running inspection", zap.Strings("areas", in.Areas))
			start := time.Now()
			r, err := s.client.RunInspection(ctx, in.Areas)
			if err != nil {
				s.logger.Debug("haify_inspect_run: run failed", zap.Strings("areas", in.Areas),
					zap.Duration("took", time.Since(start)), zap.Error(err))
				return nil, inspectReportOut{}, err
			}
			out := reportOut(r)
			s.logger.Debug("haify_inspect_run: done", zap.String("id", out.ID), zap.Int("checks", len(out.Checks)),
				zap.Int32("fail", out.Fail), zap.Int32("warn", out.Warn), zap.Int32("error", out.Error),
				zap.Duration("took", time.Since(start)))
			return nil, out, nil
		})
}
