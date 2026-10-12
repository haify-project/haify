package controller

import (
	"context"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/inspect"
	"go.uber.org/zap"
)

// RunInspection inspects the cluster now and returns the report.
func (s *Server) RunInspection(ctx context.Context, req *haifypb.RunInspectionRequest) (*haifypb.RunInspectionResponse, error) {
	log := s.serverLog()
	log.Debug("RunInspection: inspecting now", zap.Strings("areas", req.Areas))
	areas := make([]inspect.Area, 0, len(req.Areas))
	for _, a := range req.Areas {
		areas = append(areas, inspect.Area(a))
	}
	r, err := s.ctrl.inspections.Run(ctx, inspect.TriggerManual, areas)
	if err != nil {
		log.Debug("RunInspection: failed", zap.Int("areas", len(areas)), zap.Error(err))
		return &haifypb.RunInspectionResponse{Success: false, Message: err.Error(), Report: reportToPB(r, true)}, nil
	}
	log.Debug("RunInspection: done", zap.String("report", r.ID), zap.Int("pass", r.Summary.Pass),
		zap.Int("warn", r.Summary.Warn), zap.Int("fail", r.Summary.Fail), zap.Int("error", r.Summary.Error))
	return &haifypb.RunInspectionResponse{Success: true, Message: inspect.Headline(r, headlineItems), Report: reportToPB(r, true)}, nil
}

// ListInspections lists stored reports, newest first, without their checks.
func (s *Server) ListInspections(ctx context.Context, req *haifypb.ListInspectionsRequest) (*haifypb.ListInspectionsResponse, error) {
	log := s.serverLog()
	log.Debug("ListInspections: listing", zap.Int32("limit", req.Limit))
	reports, err := s.ctrl.inspections.List(ctx, int(req.Limit))
	if err != nil {
		log.Debug("ListInspections: failed", zap.Error(err))
		return &haifypb.ListInspectionsResponse{Success: false, Message: err.Error()}, nil
	}
	out := make([]*haifypb.InspectionReport, 0, len(reports))
	for _, r := range reports {
		log.Debug("ListInspections: report", zap.String("report", r.ID), zap.String("trigger", string(r.Trigger)))
		out = append(out, reportToPB(r, false))
	}
	log.Debug("ListInspections: listed", zap.Int("count", len(out)))
	return &haifypb.ListInspectionsResponse{Success: true, Reports: out}, nil
}

// GetInspection returns one stored report, "latest" for the newest.
func (s *Server) GetInspection(ctx context.Context, req *haifypb.GetInspectionRequest) (*haifypb.GetInspectionResponse, error) {
	log := s.serverLog()
	log.Debug("GetInspection: reading", zap.String("report", req.Id))
	r, err := s.ctrl.inspections.Get(ctx, req.Id)
	if err != nil {
		log.Debug("GetInspection: failed", zap.String("report", req.Id), zap.Error(err))
		return &haifypb.GetInspectionResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("GetInspection: found", zap.String("report", r.ID), zap.Int("checks", len(r.Checks)))
	return &haifypb.GetInspectionResponse{Success: true, Report: reportToPB(r, true)}, nil
}

func reportToPB(r *inspect.Report, withChecks bool) *haifypb.InspectionReport {
	if r == nil {
		zap.L().Debug("reportToPB: no report")
		return nil
	}
	out := &haifypb.InspectionReport{
		Id:               r.ID,
		Trigger:          string(r.Trigger),
		StartedAtUnixMs:  r.StartedAt.UnixMilli(),
		FinishedAtUnixMs: r.FinishedAt.UnixMilli(),
		Summary: &haifypb.InspectionSummary{
			Pass: int32(r.Summary.Pass), Warn: int32(r.Summary.Warn),
			Fail: int32(r.Summary.Fail), Error: int32(r.Summary.Error),
		},
	}
	for _, a := range r.Areas {
		out.Areas = append(out.Areas, string(a))
	}
	if !withChecks {
		zap.L().Debug("reportToPB: rendered report without checks", zap.String("report", r.ID))
		return out
	}
	for _, c := range r.Checks {
		out.Checks = append(out.Checks, &haifypb.InspectionCheck{
			Id: c.ID, Area: string(c.Area), Subject: c.Subject, Status: string(c.Status),
			Message: c.Message, Evidence: c.Evidence, Fix: c.Fix, Runbook: c.Runbook,
		})
	}
	zap.L().Debug("reportToPB: rendered report with checks", zap.String("report", r.ID), zap.Int("checks", len(out.Checks)))
	return out
}
