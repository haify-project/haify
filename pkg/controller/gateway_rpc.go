package controller

import (
	"context"
	"strconv"
	"time"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/gateway"
	"go.uber.org/zap"
)

// These handlers report a failure inside the response rather than as an RPC
// error, so the trace interceptor sees them succeed: each traces the call it
// makes into the gateway package, with its outcome.

func (s *Server) AddNFSExport(ctx context.Context, req *haifypb.AddNFSExportRequest) (*haifypb.AddNFSExportResponse, error) {
	log := s.ctrl.traceLogger()
	log.Debug("Server.AddNFSExport: adding export", zap.String("resource", req.Resource),
		zap.String("export_path", req.ExportPath), zap.Int32("fsid", req.Fsid), zap.String("client_spec", req.ClientSpec))
	nfsMgr := gateway.NewNFSManager(s.gateway)
	start := time.Now()
	err := nfsMgr.AddNFSExport(ctx, req.Resource, req.ExportPath, int(req.Fsid), req.ClientSpec, req.Options)
	log.Debug("Server.AddNFSExport: gateway call returned", zap.String("resource", req.Resource),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return &haifypb.AddNFSExportResponse{Success: false, Message: err.Error()}, nil
	}
	return &haifypb.AddNFSExportResponse{Success: true, Message: "NFS export added successfully"}, nil
}

func (s *Server) RemoveNFSExport(ctx context.Context, req *haifypb.RemoveNFSExportRequest) (*haifypb.RemoveNFSExportResponse, error) {
	log := s.ctrl.traceLogger()
	log.Debug("Server.RemoveNFSExport: removing export", zap.String("resource", req.Resource),
		zap.String("export_path", req.ExportPath))
	nfsMgr := gateway.NewNFSManager(s.gateway)
	start := time.Now()
	err := nfsMgr.RemoveNFSExport(ctx, req.Resource, req.ExportPath)
	log.Debug("Server.RemoveNFSExport: gateway call returned", zap.String("resource", req.Resource),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return &haifypb.RemoveNFSExportResponse{Success: false, Message: err.Error()}, nil
	}
	return &haifypb.RemoveNFSExportResponse{Success: true, Message: "NFS export removed successfully"}, nil
}

func (s *Server) ListNFSExports(ctx context.Context, req *haifypb.ListNFSExportsRequest) (*haifypb.ListNFSExportsResponse, error) {
	log := s.ctrl.traceLogger()
	log.Debug("Server.ListNFSExports: listing exports", zap.String("resource", req.Resource))
	nfsMgr := gateway.NewNFSManager(s.gateway)
	start := time.Now()
	exports, err := nfsMgr.ListNFSExports(ctx, req.Resource)
	log.Debug("Server.ListNFSExports: gateway call returned", zap.String("resource", req.Resource),
		zap.Int("count", len(exports)), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return &haifypb.ListNFSExportsResponse{Success: false, Message: err.Error()}, nil
	}

	items := make([]*haifypb.NFSExportInfo, 0, len(exports))
	for _, export := range exports {
		log.Debug("Server.ListNFSExports: export", zap.String("resource", req.Resource),
			zap.String("directory", export["directory"]), zap.String("fsid", export["fsid"]))
		items = append(items, &haifypb.NFSExportInfo{
			Directory:  export["directory"],
			Fsid:       export["fsid"],
			Clientspec: export["clientspec"],
			Options:    export["options"],
		})
	}

	return &haifypb.ListNFSExportsResponse{
		Success: true,
		Message: "NFS exports listed successfully",
		Exports: items,
	}, nil
}

func (s *Server) AddISCSILUN(ctx context.Context, req *haifypb.AddISCSILUNRequest) (*haifypb.AddISCSILUNResponse, error) {
	log := s.ctrl.traceLogger()
	log.Debug("Server.AddISCSILUN: adding LUN", zap.String("resource", req.Resource),
		zap.Int32("lun", req.Lun), zap.String("device", req.Device))
	iscsiMgr := gateway.NewISCSIManager(s.gateway)
	start := time.Now()
	err := iscsiMgr.AddLUN(ctx, req.Resource, int(req.Lun), req.Device)
	log.Debug("Server.AddISCSILUN: gateway call returned", zap.String("resource", req.Resource),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return &haifypb.AddISCSILUNResponse{Success: false, Message: err.Error()}, nil
	}
	return &haifypb.AddISCSILUNResponse{Success: true, Message: "iSCSI LUN added successfully"}, nil
}

func (s *Server) RemoveISCSILUN(ctx context.Context, req *haifypb.RemoveISCSILUNRequest) (*haifypb.RemoveISCSILUNResponse, error) {
	log := s.ctrl.traceLogger()
	log.Debug("Server.RemoveISCSILUN: removing LUN", zap.String("resource", req.Resource), zap.Int32("lun", req.Lun))
	iscsiMgr := gateway.NewISCSIManager(s.gateway)
	start := time.Now()
	err := iscsiMgr.RemoveLUN(ctx, req.Resource, int(req.Lun))
	log.Debug("Server.RemoveISCSILUN: gateway call returned", zap.String("resource", req.Resource),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return &haifypb.RemoveISCSILUNResponse{Success: false, Message: err.Error()}, nil
	}
	return &haifypb.RemoveISCSILUNResponse{Success: true, Message: "iSCSI LUN removed successfully"}, nil
}

func (s *Server) ListISCSILUNs(ctx context.Context, req *haifypb.ListISCSILUNsRequest) (*haifypb.ListISCSILUNsResponse, error) {
	log := s.ctrl.traceLogger()
	log.Debug("Server.ListISCSILUNs: listing LUNs", zap.String("resource", req.Resource))
	iscsiMgr := gateway.NewISCSIManager(s.gateway)
	start := time.Now()
	luns, err := iscsiMgr.ListLUNs(ctx, req.Resource)
	log.Debug("Server.ListISCSILUNs: gateway call returned", zap.String("resource", req.Resource),
		zap.Int("count", len(luns)), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return &haifypb.ListISCSILUNsResponse{Success: false, Message: err.Error()}, nil
	}

	items := make([]*haifypb.ISCSILUNInfo, 0, len(luns))
	for _, lun := range luns {
		lunNumber, _ := strconv.Atoi(lun["lun"])
		log.Debug("Server.ListISCSILUNs: LUN", zap.String("resource", req.Resource),
			zap.Int("lun", lunNumber), zap.String("device", lun["device"]))
		items = append(items, &haifypb.ISCSILUNInfo{
			Lun:       int32(lunNumber),
			Device:    lun["device"],
			TargetIqn: lun["target_iqn"],
		})
	}

	return &haifypb.ListISCSILUNsResponse{
		Success: true,
		Message: "iSCSI LUNs listed successfully",
		Luns:    items,
	}, nil
}

func (s *Server) AddISCSIInitiator(ctx context.Context, req *haifypb.AddISCSIInitiatorRequest) (*haifypb.AddISCSIInitiatorResponse, error) {
	log := s.ctrl.traceLogger()
	log.Debug("Server.AddISCSIInitiator: adding initiator", zap.String("resource", req.Resource),
		zap.String("initiator", req.Initiator))
	iscsiMgr := gateway.NewISCSIManager(s.gateway)
	start := time.Now()
	err := iscsiMgr.AddInitiator(ctx, req.Resource, req.Initiator)
	log.Debug("Server.AddISCSIInitiator: gateway call returned", zap.String("resource", req.Resource),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return &haifypb.AddISCSIInitiatorResponse{Success: false, Message: err.Error()}, nil
	}
	return &haifypb.AddISCSIInitiatorResponse{Success: true, Message: "iSCSI initiator added successfully"}, nil
}

func (s *Server) RemoveISCSIInitiator(ctx context.Context, req *haifypb.RemoveISCSIInitiatorRequest) (*haifypb.RemoveISCSIInitiatorResponse, error) {
	log := s.ctrl.traceLogger()
	log.Debug("Server.RemoveISCSIInitiator: removing initiator", zap.String("resource", req.Resource),
		zap.String("initiator", req.Initiator))
	iscsiMgr := gateway.NewISCSIManager(s.gateway)
	start := time.Now()
	err := iscsiMgr.RemoveInitiator(ctx, req.Resource, req.Initiator)
	log.Debug("Server.RemoveISCSIInitiator: gateway call returned", zap.String("resource", req.Resource),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return &haifypb.RemoveISCSIInitiatorResponse{Success: false, Message: err.Error()}, nil
	}
	return &haifypb.RemoveISCSIInitiatorResponse{Success: true, Message: "iSCSI initiator removed successfully"}, nil
}

func (s *Server) ListISCSIInitiators(ctx context.Context, req *haifypb.ListISCSIInitiatorsRequest) (*haifypb.ListISCSIInitiatorsResponse, error) {
	log := s.ctrl.traceLogger()
	log.Debug("Server.ListISCSIInitiators: listing initiators", zap.String("resource", req.Resource))
	iscsiMgr := gateway.NewISCSIManager(s.gateway)
	start := time.Now()
	initiators, err := iscsiMgr.ListInitiators(ctx, req.Resource)
	log.Debug("Server.ListISCSIInitiators: gateway call returned", zap.String("resource", req.Resource),
		zap.Strings("initiators", initiators), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return &haifypb.ListISCSIInitiatorsResponse{Success: false, Message: err.Error()}, nil
	}
	return &haifypb.ListISCSIInitiatorsResponse{
		Success:    true,
		Message:    "iSCSI initiators listed successfully",
		Initiators: initiators,
	}, nil
}

// SetISCSIChap and GetISCSIChap carry the CHAP password: only whether one is
// set is traced, never its value.
func (s *Server) SetISCSIChap(ctx context.Context, req *haifypb.SetISCSIChapRequest) (*haifypb.SetISCSIChapResponse, error) {
	log := s.ctrl.traceLogger()
	log.Debug("Server.SetISCSIChap: setting CHAP", zap.String("resource", req.Resource),
		zap.Bool("has_username", req.Username != ""), zap.Bool("has_password", req.Password != ""),
		zap.Bool("mutual", req.Mutual))
	iscsiMgr := gateway.NewISCSIManager(s.gateway)
	start := time.Now()
	err := iscsiMgr.SetCHAP(ctx, req.Resource, req.Username, req.Password, req.Mutual)
	log.Debug("Server.SetISCSIChap: gateway call returned", zap.String("resource", req.Resource),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return &haifypb.SetISCSIChapResponse{Success: false, Message: err.Error()}, nil
	}
	return &haifypb.SetISCSIChapResponse{Success: true, Message: "iSCSI CHAP updated successfully"}, nil
}

func (s *Server) GetISCSIChap(ctx context.Context, req *haifypb.GetISCSIChapRequest) (*haifypb.GetISCSIChapResponse, error) {
	log := s.ctrl.traceLogger()
	log.Debug("Server.GetISCSIChap: reading CHAP", zap.String("resource", req.Resource))
	iscsiMgr := gateway.NewISCSIManager(s.gateway)
	start := time.Now()
	username, password, mutual, err := iscsiMgr.GetCHAP(ctx, req.Resource)
	log.Debug("Server.GetISCSIChap: gateway call returned", zap.String("resource", req.Resource),
		zap.Bool("has_username", username != ""), zap.Bool("has_password", password != ""),
		zap.Bool("mutual", mutual), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return &haifypb.GetISCSIChapResponse{Success: false, Message: err.Error()}, nil
	}
	return &haifypb.GetISCSIChapResponse{
		Success:  true,
		Message:  "iSCSI CHAP retrieved successfully",
		Username: username,
		Password: password,
		Mutual:   mutual,
	}, nil
}

func (s *Server) AddNVMeNamespace(ctx context.Context, req *haifypb.AddNVMeNamespaceRequest) (*haifypb.AddNVMeNamespaceResponse, error) {
	log := s.ctrl.traceLogger()
	log.Debug("Server.AddNVMeNamespace: adding namespace", zap.String("resource", req.Resource),
		zap.String("device", req.Device))
	nvmeMgr := gateway.NewNVMeManager(s.gateway)
	start := time.Now()
	err := nvmeMgr.AddNamespace(ctx, req.Resource, req.Device)
	log.Debug("Server.AddNVMeNamespace: gateway call returned", zap.String("resource", req.Resource),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return &haifypb.AddNVMeNamespaceResponse{Success: false, Message: err.Error()}, nil
	}
	return &haifypb.AddNVMeNamespaceResponse{Success: true, Message: "NVMe namespace added successfully"}, nil
}

func (s *Server) RemoveNVMeNamespace(ctx context.Context, req *haifypb.RemoveNVMeNamespaceRequest) (*haifypb.RemoveNVMeNamespaceResponse, error) {
	log := s.ctrl.traceLogger()
	log.Debug("Server.RemoveNVMeNamespace: removing namespace", zap.String("resource", req.Resource),
		zap.Int32("namespace_id", req.NamespaceId))
	nvmeMgr := gateway.NewNVMeManager(s.gateway)
	start := time.Now()
	err := nvmeMgr.RemoveNamespace(ctx, req.Resource, int(req.NamespaceId))
	log.Debug("Server.RemoveNVMeNamespace: gateway call returned", zap.String("resource", req.Resource),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return &haifypb.RemoveNVMeNamespaceResponse{Success: false, Message: err.Error()}, nil
	}
	return &haifypb.RemoveNVMeNamespaceResponse{Success: true, Message: "NVMe namespace removed successfully"}, nil
}

func (s *Server) ListNVMeNamespaces(ctx context.Context, req *haifypb.ListNVMeNamespacesRequest) (*haifypb.ListNVMeNamespacesResponse, error) {
	log := s.ctrl.traceLogger()
	log.Debug("Server.ListNVMeNamespaces: listing namespaces", zap.String("resource", req.Resource))
	nvmeMgr := gateway.NewNVMeManager(s.gateway)
	start := time.Now()
	namespaces, err := nvmeMgr.ListNamespaces(ctx, req.Resource)
	log.Debug("Server.ListNVMeNamespaces: gateway call returned", zap.String("resource", req.Resource),
		zap.Int("count", len(namespaces)), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return &haifypb.ListNVMeNamespacesResponse{Success: false, Message: err.Error()}, nil
	}

	items := make([]*haifypb.NVMeNamespaceInfo, 0, len(namespaces))
	for _, namespace := range namespaces {
		namespaceID, _ := strconv.Atoi(namespace["namespace_id"])
		log.Debug("Server.ListNVMeNamespaces: namespace", zap.String("resource", req.Resource),
			zap.Int("namespace_id", namespaceID), zap.String("backing_path", namespace["backing_path"]))
		items = append(items, &haifypb.NVMeNamespaceInfo{
			NamespaceId: int32(namespaceID),
			BackingPath: namespace["backing_path"],
			Uuid:        namespace["uuid"],
			Nguid:       namespace["nguid"],
			Nqn:         namespace["nqn"],
		})
	}

	return &haifypb.ListNVMeNamespacesResponse{
		Success:    true,
		Message:    "NVMe namespaces listed successfully",
		Namespaces: items,
	}, nil
}

func (s *Server) AddNVMeHost(ctx context.Context, req *haifypb.AddNVMeHostRequest) (*haifypb.AddNVMeHostResponse, error) {
	log := s.ctrl.traceLogger()
	log.Debug("Server.AddNVMeHost: adding host", zap.String("resource", req.Resource), zap.String("host_nqn", req.HostNqn))
	nvmeMgr := gateway.NewNVMeManager(s.gateway)
	start := time.Now()
	err := nvmeMgr.AddHost(ctx, req.Resource, req.HostNqn)
	log.Debug("Server.AddNVMeHost: gateway call returned", zap.String("resource", req.Resource),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return &haifypb.AddNVMeHostResponse{Success: false, Message: err.Error()}, nil
	}
	return &haifypb.AddNVMeHostResponse{Success: true, Message: "NVMe host added successfully"}, nil
}

func (s *Server) RemoveNVMeHost(ctx context.Context, req *haifypb.RemoveNVMeHostRequest) (*haifypb.RemoveNVMeHostResponse, error) {
	log := s.ctrl.traceLogger()
	log.Debug("Server.RemoveNVMeHost: removing host", zap.String("resource", req.Resource), zap.String("host_nqn", req.HostNqn))
	nvmeMgr := gateway.NewNVMeManager(s.gateway)
	start := time.Now()
	err := nvmeMgr.RemoveHost(ctx, req.Resource, req.HostNqn)
	log.Debug("Server.RemoveNVMeHost: gateway call returned", zap.String("resource", req.Resource),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return &haifypb.RemoveNVMeHostResponse{Success: false, Message: err.Error()}, nil
	}
	return &haifypb.RemoveNVMeHostResponse{Success: true, Message: "NVMe host removed successfully"}, nil
}

func (s *Server) ListNVMeHosts(ctx context.Context, req *haifypb.ListNVMeHostsRequest) (*haifypb.ListNVMeHostsResponse, error) {
	log := s.ctrl.traceLogger()
	log.Debug("Server.ListNVMeHosts: listing hosts", zap.String("resource", req.Resource))
	nvmeMgr := gateway.NewNVMeManager(s.gateway)
	start := time.Now()
	hosts, err := nvmeMgr.ListHosts(ctx, req.Resource)
	log.Debug("Server.ListNVMeHosts: gateway call returned", zap.String("resource", req.Resource),
		zap.Strings("hosts", hosts), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return &haifypb.ListNVMeHostsResponse{Success: false, Message: err.Error()}, nil
	}
	return &haifypb.ListNVMeHostsResponse{
		Success: true,
		Message: "NVMe hosts listed successfully",
		Hosts:   hosts,
	}, nil
}
