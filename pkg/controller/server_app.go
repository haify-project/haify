package controller

import (
	"context"
	"errors"
	"fmt"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/apptemplate"
)

// Database application RPCs (app*.go). Like the other operation RPCs they
// report failure in the response; only a request that fails validation
// before anything is done is a gRPC error (InvalidArgument).

func (s *Server) CreateApp(ctx context.Context, req *haifypb.CreateAppRequest) (*haifypb.CreateAppResponse, error) {
	log := s.serverLog()
	log.Debug("CreateApp: creating app", zap.String("app", req.Name), zap.String("engine", req.Engine),
		zap.String("resource", req.Resource), zap.String("service_ip", req.ServiceIp), zap.Uint32("port", req.Port),
		zap.Bool("vector", req.Vector))
	spec := apptemplate.Spec{Name: req.Name, Engine: apptemplate.Engine(req.Engine), Resource: req.Resource,
		ServiceIP: req.ServiceIp, Port: int(req.Port), Vector: req.Vector}
	created, err := s.ctrl.appManager().Create(ctx, spec)
	if err != nil {
		if errors.Is(err, apptemplate.ErrInvalid) {
			log.Debug("CreateApp: invalid spec", zap.String("app", req.Name), zap.Error(err))
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		log.Debug("CreateApp: failed", zap.String("app", req.Name), zap.Error(err))
		return &haifypb.CreateAppResponse{Success: false, Message: err.Error()}, nil
	}
	info := appInfo(created.App)
	msg := fmt.Sprintf("app %s (%s) created on resource %s; drbd-reactor starts it on a replica and raises %s; "+
		"connect with: %s", created.App.Name, created.App.Engine, created.App.Resource, created.App.ServiceIP, info.Connection)
	if created.Reused {
		log.Debug("CreateApp: resource already held data, kept it", zap.String("app", req.Name),
			zap.String("resource", created.App.Resource))
		msg = fmt.Sprintf("app %s (%s) created on resource %s, which already held its data: the data and the "+
			"credentials in %s (root, on the Primary) are kept", created.App.Name, created.App.Engine,
			created.App.Resource, info.CredentialsFile)
	}
	log.Debug("CreateApp: created", zap.String("app", created.App.Name), zap.String("primary", created.Primary),
		zap.Bool("reused", created.Reused), zap.Bool("has_password", created.Password != ""))
	return &haifypb.CreateAppResponse{
		Success:     true,
		Message:     msg,
		App:         info,
		Password:    created.Password,
		PrimaryNode: created.Primary,
		DataReused:  created.Reused,
	}, nil
}

func (s *Server) ListApps(ctx context.Context, _ *haifypb.ListAppsRequest) (*haifypb.ListAppsResponse, error) {
	log := s.serverLog()
	log.Debug("ListApps: listing apps")
	apps, err := s.ctrl.appManager().List(ctx)
	if err != nil {
		log.Debug("ListApps: failed", zap.Error(err))
		return &haifypb.ListAppsResponse{Success: false, Message: err.Error()}, nil
	}
	resp := &haifypb.ListAppsResponse{Success: true, Apps: make([]*haifypb.AppInfo, 0, len(apps))}
	for _, a := range apps {
		log.Debug("ListApps: app", zap.String("app", a.Name), zap.String("engine", string(a.Engine)),
			zap.String("resource", a.Resource))
		resp.Apps = append(resp.Apps, appInfo(a))
	}
	log.Debug("ListApps: listed", zap.Int("count", len(resp.Apps)))
	return resp, nil
}

func (s *Server) GetAppStatus(ctx context.Context, req *haifypb.GetAppStatusRequest) (*haifypb.GetAppStatusResponse, error) {
	log := s.serverLog()
	log.Debug("GetAppStatus: reading status", zap.String("app", req.Name))
	st, err := s.ctrl.appManager().Status(ctx, req.Name)
	if err != nil {
		log.Debug("GetAppStatus: failed", zap.String("app", req.Name), zap.Error(err))
		return &haifypb.GetAppStatusResponse{Success: false, Message: err.Error()}, nil
	}
	msg := fmt.Sprintf("app %s is %s", req.Name, st.State)
	if st.Primary != "" {
		msg += " on " + st.Primary
	}
	log.Debug("GetAppStatus: status", zap.String("app", req.Name), zap.String("state", st.State),
		zap.String("primary", st.Primary), zap.String("service_state", st.ServiceState), zap.Bool("healthy", st.Healthy))
	return &haifypb.GetAppStatusResponse{
		Success:      true,
		Message:      msg,
		App:          appInfo(st.App),
		PrimaryNode:  st.Primary,
		ServiceState: st.ServiceState,
		Healthy:      st.Healthy,
		State:        st.State,
		Nodes:        st.Nodes,
	}, nil
}

func (s *Server) DeleteApp(ctx context.Context, req *haifypb.DeleteAppRequest) (*haifypb.DeleteAppResponse, error) {
	log := s.serverLog()
	log.Debug("DeleteApp: deleting app", zap.String("app", req.Name), zap.Bool("delete_data", req.DeleteData))
	msg, err := s.ctrl.appManager().Delete(ctx, req.Name, req.DeleteData)
	if err != nil {
		log.Debug("DeleteApp: failed", zap.String("app", req.Name), zap.Error(err))
		return &haifypb.DeleteAppResponse{Success: false, Message: err.Error()}, nil
	}
	log.Debug("DeleteApp: deleted", zap.String("app", req.Name))
	return &haifypb.DeleteAppResponse{Success: true, Message: msg}, nil
}

func (s *Server) FailoverApp(ctx context.Context, req *haifypb.FailoverAppRequest) (*haifypb.FailoverAppResponse, error) {
	log := s.serverLog()
	log.Debug("FailoverApp: moving app", zap.String("app", req.Name))
	from, to, err := s.ctrl.appManager().Failover(ctx, req.Name)
	if err != nil {
		log.Debug("FailoverApp: failed", zap.String("app", req.Name), zap.String("from", from), zap.Error(err))
		return &haifypb.FailoverAppResponse{Success: false, Message: err.Error(), FromNode: from}, nil
	}
	msg := fmt.Sprintf("app %s moved off %s", req.Name, from)
	if to != "" {
		msg = fmt.Sprintf("app %s moved from %s to %s", req.Name, from, to)
	}
	log.Debug("FailoverApp: moved", zap.String("app", req.Name), zap.String("from", from), zap.String("to", to))
	return &haifypb.FailoverAppResponse{Success: true, Message: msg, FromNode: from, ToNode: to}, nil
}

func (s *Server) SnapshotApp(ctx context.Context, req *haifypb.SnapshotAppRequest) (*haifypb.SnapshotAppResponse, error) {
	log := s.serverLog()
	log.Debug("SnapshotApp: snapshotting app", zap.String("app", req.Name), zap.String("snapshot", req.Snapshot))
	frozen, err := s.ctrl.appManager().Snapshot(ctx, req.Name, req.Snapshot)
	if err != nil {
		log.Debug("SnapshotApp: failed", zap.String("app", req.Name), zap.Bool("frozen", frozen), zap.Error(err))
		return &haifypb.SnapshotAppResponse{Success: false, Message: err.Error(), Frozen: frozen}, nil
	}
	msg := fmt.Sprintf("snapshot %s of app %s taken on every replica with the database frozen", req.Snapshot, req.Name)
	if !frozen {
		log.Debug("SnapshotApp: app was not running, nothing frozen", zap.String("app", req.Name))
		msg = fmt.Sprintf("snapshot %s of app %s taken on every replica; the app was not running, so there was "+
			"nothing to freeze", req.Snapshot, req.Name)
	}
	log.Debug("SnapshotApp: taken", zap.String("app", req.Name), zap.String("snapshot", req.Snapshot), zap.Bool("frozen", frozen))
	return &haifypb.SnapshotAppResponse{Success: true, Message: msg, Frozen: frozen}, nil
}
