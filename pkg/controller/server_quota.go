package controller

import (
	"context"
	"fmt"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// Quota RPCs (nfs_quota.go).

func (s *Server) SetNFSExportQuota(ctx context.Context, req *haifypb.SetNFSExportQuotaRequest) (*haifypb.SetNFSExportQuotaResponse, error) {
	log := s.serverLog()
	log.Debug("SetNFSExportQuota: setting quota", zap.String("resource", req.Resource), zap.String("export_path", req.ExportPath),
		zap.Uint64("size_bytes", req.SizeBytes))
	if err := s.resources.SetNFSExportQuota(ctx, req.Resource, req.ExportPath, req.SizeBytes); err != nil {
		log.Debug("SetNFSExportQuota: failed", zap.String("resource", req.Resource), zap.Error(err))
		return &haifypb.SetNFSExportQuotaResponse{Success: false, Message: err.Error()}, nil
	}
	msg := fmt.Sprintf("quota of %d bytes set on %s", req.SizeBytes, req.ExportPath)
	if req.SizeBytes == 0 {
		log.Debug("SetNFSExportQuota: size zero, quota removed", zap.String("export_path", req.ExportPath))
		msg = "quota removed from " + req.ExportPath
	}
	log.Debug("SetNFSExportQuota: done", zap.String("resource", req.Resource), zap.String("export_path", req.ExportPath))
	return &haifypb.SetNFSExportQuotaResponse{Success: true, Message: msg}, nil
}
