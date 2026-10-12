package controller

import (
	"context"
	"os"
	"sync"
	"time"

	pb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/logbuf"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Two log surfaces, answering two different questions.
//
// The audit trail answers "who changed what, and did it work" — it is
// persisted, because the interesting cases (a failover, a deletion) are
// precisely the ones you go looking for afterwards.
//
// The controller log answers "what is this process doing right now" — it is a
// memory ring, because once the controller has relocated its old node's output
// describes a process that no longer exists.

// localNode is the hostname the controller reports as its own, resolved once.
var localNode = sync.OnceValue(func() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
})

// auditSink returns the persistence hook for the audit interceptor, or nil when
// there is no database to write to (the controller runs without persistence
// rather than refusing to start, so this is a real case).
func (c *Controller) auditSink() auditSink {
	log := c.traceLogger()
	if c.db == nil {
		log.Debug("auditSink: no database, audit events are not persisted")
		return nil
	}
	node := localNode()
	log.Debug("auditSink: persisting audit events", zap.String("node", node))
	return func(ev *database.AuditEvent) {
		ev.Node = node
		// The trail must never be able to fail a request. A write error is
		// worth knowing about, but the call it describes already succeeded.
		start := time.Now()
		err := c.db.AppendAuditEvent(context.Background(), ev)
		log.Debug("auditSink: appended audit event", zap.String("method", ev.Method),
			zap.String("result", ev.Result), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			c.logger.Warn("Failed to persist audit event",
				zap.String("method", ev.Method), zap.Error(err))
		}
	}
}

// ListAuditEvents returns the persisted audit trail, newest first.
func (s *Server) ListAuditEvents(ctx context.Context, req *pb.ListAuditEventsRequest) (*pb.ListAuditEventsResponse, error) {
	log := s.ctrl.traceLogger()
	log.Debug("ListAuditEvents: listing", zap.Int32("limit", req.GetLimit()), zap.String("method", req.GetMethod()),
		zap.String("target", req.GetTarget()), zap.String("user", req.GetUser()),
		zap.Bool("failures_only", req.GetFailuresOnly()), zap.Int64("since_unix_ms", req.GetSinceUnixMs()))
	if s.ctrl.db == nil {
		log.Debug("ListAuditEvents: no database")
		return &pb.ListAuditEventsResponse{
			Success: false,
			Message: "audit trail unavailable: controller is running without persistence",
		}, nil
	}
	if !s.ctrl.config.Audit.Enabled {
		log.Debug("ListAuditEvents: audit log disabled")
		// An empty list would read as "nothing has happened", which is a very
		// different statement from "nothing is being recorded".
		return &pb.ListAuditEventsResponse{
			Success: false,
			Message: "audit log is disabled; enable [audit] in controller.toml",
		}, nil
	}

	filter := database.AuditFilter{
		Limit:        int(req.GetLimit()),
		Method:       req.GetMethod(),
		Target:       req.GetTarget(),
		User:         req.GetUser(),
		FailuresOnly: req.GetFailuresOnly(),
	}
	if ms := req.GetSinceUnixMs(); ms > 0 {
		filter.Since = time.UnixMilli(ms)
	}

	start := time.Now()
	events, total, err := s.ctrl.db.ListAuditEvents(ctx, filter)
	log.Debug("ListAuditEvents: read audit events", zap.Int("count", len(events)), zap.Int("total", total),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return &pb.ListAuditEventsResponse{Success: false, Message: err.Error()}, nil
	}

	out := make([]*pb.AuditEvent, 0, len(events))
	for _, ev := range events {
		out = append(out, &pb.AuditEvent{
			TimestampUnixMs: ev.Timestamp.UnixMilli(),
			Method:          ev.Method,
			Client:          ev.Client,
			User:            ev.User,
			Target:          ev.Target,
			Result:          ev.Result,
			Granted:         ev.Granted,
			LatencyMs:       ev.Latency.Milliseconds(),
			Error:           ev.Error,
			Node:            ev.Node,
		})
	}
	return &pb.ListAuditEventsResponse{
		Success: true,
		Message: "OK",
		Events:  out,
		Total:   int64(total),
	}, nil
}

// ListControllerLogs returns recent lines from the controller's own log.
func (s *Server) ListControllerLogs(ctx context.Context, req *pb.ListControllerLogsRequest) (*pb.ListControllerLogsResponse, error) {
	// One trace per request, written before the ring is read: a trace per
	// served line would grow the ring while it is being read.
	log := s.ctrl.traceLogger()
	log.Debug("ListControllerLogs: listing", zap.Int32("limit", req.GetLimit()),
		zap.String("level", req.GetLevel()), zap.Bool("has_contains", req.GetContains() != ""),
		zap.Bool("has_ring", s.ctrl.logRing != nil))
	if s.ctrl.logRing == nil {
		return &pb.ListControllerLogsResponse{
			Success: false,
			Message: "controller log buffer is not enabled",
		}, nil
	}

	filter := logbuf.Filter{
		Limit:    int(req.GetLimit()),
		Contains: req.GetContains(),
	}
	if lvl := req.GetLevel(); lvl != "" {
		var parsed zapcore.Level
		if err := parsed.UnmarshalText([]byte(lvl)); err != nil {
			log.Debug("ListControllerLogs: unknown level", zap.String("level", lvl), zap.Error(err))
			return &pb.ListControllerLogsResponse{
				Success: false,
				Message: "unknown level " + lvl + " (want debug, info, warn or error)",
			}, nil
		}
		filter.MinLevel, filter.HasLevel = parsed, true
	}

	entries, truncated := s.ctrl.logRing.List(filter)
	out := make([]*pb.ControllerLogEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, &pb.ControllerLogEntry{
			TimestampUnixMs: e.Time.UnixMilli(),
			Level:           e.Level,
			Logger:          e.Logger,
			Caller:          e.Caller,
			Message:         e.Message,
			Fields:          e.Fields,
		})
	}
	return &pb.ListControllerLogsResponse{
		Success:   true,
		Message:   "OK",
		Entries:   out,
		Node:      localNode(),
		Truncated: truncated,
	}, nil
}
