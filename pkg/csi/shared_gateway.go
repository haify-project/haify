package csi

import (
	"context"
	"errors"
	"net/netip"
	"path"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The NFS gateway of a shared volume: picking its address, creating it,
// finding it again on a retry, and removing it.

// gatewayBackend is what shared volumes need beyond HaifyBackend.
// *client.HaifyClient has all of it; a backend without it can still provision
// every other kind of volume.
type gatewayBackend interface {
	CreateNFSGateway(ctx context.Context, req *haifypb.CreateNFSGatewayRequest) (*haifypb.CreateNFSGatewayResponse, error)
	// GetGateway returns the gateway of a resource. A gateway that does not
	// exist is an error that is not a gRPC status.
	GetGateway(ctx context.Context, resource string) (*haifypb.GatewayInfo, error)
	ListGateways(ctx context.Context) ([]*haifypb.GatewayInfo, error)
	// DeleteGateway removes a resource's gateway from every node.
	DeleteGateway(ctx context.Context, resource string) error
}

// unreachable reports whether err says the controller could not be asked, so
// that what was asked of it may or may not have happened.
func unreachable(err error) bool {
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
		return true
	}
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// noSuchGateway reports whether a failed GetGateway means the controller
// answered and has no gateway for the resource. The controller says so in the
// body of a successful call, which the client turns into a plain error; an
// error that carries a gRPC status is the call itself failing (unreachable,
// not permitted) and says nothing about the gateway.
func noSuchGateway(err error) bool {
	return err != nil && status.Code(err) == codes.Unknown && !unreachable(err)
}

// serviceIPsInUse collects every address a new shared volume must not get:
// the ones recorded on shared volumes' resources, the service addresses of
// every gateway Haify runs, whoever created it, and the nodes' own addresses,
// in case the pool was written across one. Any of the three failing fails the
// allocation: an address picked from half the picture can be one in use.
func (s *controllerServer) serviceIPsInUse(ctx context.Context, gw gatewayBackend) (map[netip.Addr]string, error) {
	used := map[netip.Addr]string{}
	start := time.Now()
	resources, err := s.backend.ListResources(ctx)
	s.log.Debug("controllerServer.serviceIPsInUse: listed resources", zap.Int("count", len(resources)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "list resources to find the service addresses in use: %v", err)
	}
	for _, r := range resources {
		if addr, ok := serviceAddr(r.GetLabels()[nfsServiceIPLabel]); ok {
			used[addr] = "volume " + r.GetName()
		}
	}
	start = time.Now()
	gateways, err := gw.ListGateways(ctx)
	s.log.Debug("controllerServer.serviceIPsInUse: listed gateways", zap.Int("count", len(gateways)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "list gateways to find the service addresses in use: %v", err)
	}
	for _, g := range gateways {
		for _, key := range []string{"service_host", "service_ip"} {
			if addr, ok := serviceAddr(g.GetOptions()[key]); ok {
				used[addr] = "gateway " + g.GetResource()
			}
		}
	}
	start = time.Now()
	nodes, err := s.backend.ListNodes(ctx)
	s.log.Debug("controllerServer.serviceIPsInUse: listed nodes", zap.Int("count", len(nodes)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "list nodes to find the addresses in use: %v", err)
	}
	for _, n := range nodes {
		if addr, ok := serviceAddr(n.GetAddress()); ok {
			used[addr] = "node " + n.GetName()
		}
	}
	s.log.Debug("controllerServer.serviceIPsInUse: collected", zap.Int("in_use", len(used)))
	return used, nil
}

// reserveShared picks the service address of a new shared volume and puts it,
// with the shared marker, among the labels the resource is about to be
// created with. The returned function ends the reservation in memory and must
// run once the resource exists or its creation failed; from then on the label
// holds the address, or nothing does. For any other volume it does nothing.
func (s *controllerServer) reserveShared(ctx context.Context, volume string, shared SharedParams, labels map[string]string) (func(), error) {
	if !shared.Enabled {
		return func() {}, nil
	}
	gw, ok := s.backend.(gatewayBackend)
	if !ok {
		s.log.Debug("controllerServer.reserveShared: backend has no gateways", zap.String("resource", volume))
		return nil, status.Error(codes.Internal, "this Haify backend cannot create NFS gateways, which shared volumes need")
	}
	prefix, err := s.shared.ips.allocate(volume, shared.ServiceIPs, func() (map[netip.Addr]string, error) {
		return s.serviceIPsInUse(ctx, gw)
	})
	if errors.Is(err, errServiceIPsExhausted) {
		s.log.Debug("controllerServer.reserveShared: pool exhausted", zap.String("resource", volume))
		return nil, status.Errorf(codes.ResourceExhausted,
			"every address of %s is in use: each shared volume needs one; add addresses to the StorageClass or delete shared volumes that are no longer needed",
			paramNFSServiceIPs)
	}
	if err != nil {
		return nil, err
	}
	labels[sharedLabel] = sharedNFS
	labels[nfsServiceIPLabel] = prefix.String()
	s.log.Debug("controllerServer.reserveShared: reserved", zap.String("resource", volume), zap.Stringer("service_ip", prefix))
	return func() { s.shared.ips.release(volume) }, nil
}

// ensureGateway makes sure the volume's NFS gateway exists and returns where
// clients mount it from. It creates the gateway when there is none, which is
// both the normal case of a new volume and the case of a retry after a call
// that created the resource and got no further.
func (s *controllerServer) ensureGateway(ctx context.Context, gw gatewayBackend, volume, serviceIP string, shared SharedParams) (server, export string, err error) {
	addr, ok := serviceAddr(serviceIP)
	if !ok {
		s.log.Debug("controllerServer.ensureGateway: volume records no usable service address", zap.String("resource", volume),
			zap.String("service_ip", serviceIP))
		return "", "", status.Errorf(codes.Internal, "shared volume %q records no service address (label %s)", volume, nfsServiceIPLabel)
	}
	start := time.Now()
	info, gerr := gw.GetGateway(ctx, volume)
	s.log.Debug("controllerServer.ensureGateway: looked up gateway", zap.String("resource", volume), zap.Bool("found", gerr == nil),
		zap.Duration("took", time.Since(start)), zap.Error(gerr))
	switch {
	case gerr != nil && !noSuchGateway(gerr):
		return "", "", status.Errorf(codes.Unavailable, "look up the NFS gateway of %q: %v", volume, gerr)
	case gerr == nil && info.GetType() != "" && info.GetType() != sharedNFS:
		s.log.Debug("controllerServer.ensureGateway: resource has a gateway of another kind", zap.String("resource", volume),
			zap.String("type", info.GetType()))
		return "", "", status.Errorf(codes.FailedPrecondition, "volume %q already has a %s gateway, not an NFS one", volume, info.GetType())
	case gerr != nil:
		start = time.Now()
		// No export path: the controller then puts the export under its own
		// base directory, named after the resource, where no two volumes meet.
		_, cerr := gw.CreateNFSGateway(ctx, &haifypb.CreateNFSGatewayRequest{
			Resource:   volume,
			ServiceIp:  serviceIP,
			AllowedIps: shared.AllowedClients,
			FsType:     sharedFSType,
		})
		s.log.Debug("controllerServer.ensureGateway: create gateway returned", zap.String("resource", volume),
			zap.String("service_ip", serviceIP), zap.Int("allowed_clients", len(shared.AllowedClients)),
			zap.Duration("took", time.Since(start)), zap.Error(cerr))
		if cerr != nil {
			return "", "", cerr
		}
		// Read it back for the export directory the controller chose. Failing
		// to is not failing to create it: the directory is then the default.
		info, gerr = gw.GetGateway(ctx, volume)
		s.log.Debug("controllerServer.ensureGateway: read back gateway", zap.String("resource", volume), zap.Error(gerr))
	}

	server = addr.String()
	// A gateway that was already there serves on the address it was created
	// with, and that is the one clients have to be sent to.
	if actual, ok := serviceAddr(info.GetOptions()["service_host"]); ok && actual != addr {
		s.log.Warn("shared volume's NFS gateway serves on another address than the volume records; using the gateway's",
			zap.String("volume", volume), zap.Stringer("recorded", addr), zap.Stringer("gateway", actual))
		server = actual.String()
	}
	export = info.GetPath()
	if export == "" {
		export = path.Join(sharedExportBase, volume)
		s.log.Debug("controllerServer.ensureGateway: controller reports no export directory, using the default",
			zap.String("resource", volume), zap.String("export", export))
	}
	s.log.Debug("controllerServer.ensureGateway: done", zap.String("resource", volume), zap.String("server", server),
		zap.String("export", export))
	return server, export, nil
}

// exportShared finishes a new shared volume once its resource exists: it
// creates the gateway and returns the volume context Pods' nodes mount from.
// For any other volume it returns vc as it is.
//
// A gateway that definitely could not be created takes the resource with it,
// like every other step of CreateVolume that fails: the CO may never call
// DeleteVolume for a volume it was never given, and the resource would keep
// its space and its address. When the controller merely stopped answering,
// the gateway may yet appear, so the resource stays and the CO's retry
// finishes the job (reconcileShared).
func (s *controllerServer) exportShared(ctx context.Context, volume string, params VolumeParams, labels, vc map[string]string) (map[string]string, error) {
	if !params.Shared.Enabled {
		return vc, nil
	}
	gw, ok := s.backend.(gatewayBackend)
	if !ok {
		s.log.Debug("controllerServer.exportShared: backend has no gateways", zap.String("resource", volume))
		return nil, status.Error(codes.Internal, "this Haify backend cannot create NFS gateways, which shared volumes need")
	}
	server, export, err := s.ensureGateway(ctx, gw, volume, labels[nfsServiceIPLabel], params.Shared)
	if err == nil {
		return sharedVolumeContext(vc, server, export), nil
	}
	if unreachable(err) || ctx.Err() != nil {
		s.log.Debug("controllerServer.exportShared: gateway creation interrupted, leaving the volume for the retry",
			zap.String("resource", volume), zap.Error(err))
		return nil, status.Errorf(codes.Unavailable, "create the NFS gateway of %q: %v", volume, err)
	}
	s.log.Debug("controllerServer.exportShared: rolling back, gateway creation failed", zap.String("resource", volume), zap.Error(err))
	cleanup := context.WithoutCancel(ctx)
	if derr := gw.DeleteGateway(cleanup, volume); derr != nil {
		s.log.Debug("controllerServer.exportShared: removing the partial gateway failed", zap.String("resource", volume), zap.Error(derr))
	}
	if derr := s.backend.DeleteResource(cleanup, volume); derr != nil {
		s.log.Error("failed to roll back a shared volume whose NFS gateway could not be created; it may need manual cleanup",
			zap.String("volume", volume), zap.Error(derr))
	}
	if status.Code(err) == codes.FailedPrecondition || status.Code(err) == codes.InvalidArgument {
		return nil, status.Errorf(status.Code(err), "create the NFS gateway of %q: %v", volume, status.Convert(err).Message())
	}
	return nil, status.Errorf(codes.Internal, "create the NFS gateway of %q: %v", volume, err)
}

// reconcileShared answers a CreateVolume whose resource already exists. A
// shared volume gets its gateway created if the earlier call did not get that
// far, and its volume context; a request and a volume that disagree on being
// shared are refused, since handing a ReadWriteOnce volume to Pods that
// expect to share it, or the reverse, cannot work. Every other volume keeps
// vc.
func (s *controllerServer) reconcileShared(ctx context.Context, existing *haifypb.ResourceInfo, req *csi.CreateVolumeRequest, vc map[string]string) (map[string]string, error) {
	want, have := sharedRequested(req.GetParameters()), isSharedVolume(existing)
	s.log.Debug("controllerServer.reconcileShared: start", zap.String("resource", existing.GetName()),
		zap.Bool("requested_shared", want), zap.Bool("is_shared", have))
	switch {
	case !want && !have:
		return vc, nil
	case want && !have:
		return nil, status.Errorf(codes.AlreadyExists,
			"volume %q already exists and is not a shared volume, and the StorageClass asks for %s", existing.GetName(), paramSharedFilesystem)
	case !want && have:
		return nil, status.Errorf(codes.AlreadyExists,
			"volume %q already exists as a shared volume, and the StorageClass does not ask for %s", existing.GetName(), paramSharedFilesystem)
	}
	shared, err := parseSharedParams(req.GetParameters())
	if err != nil {
		s.log.Debug("controllerServer.reconcileShared: invalid parameters", zap.String("resource", existing.GetName()), zap.Error(err))
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	gw, ok := s.backend.(gatewayBackend)
	if !ok {
		s.log.Debug("controllerServer.reconcileShared: backend has no gateways", zap.String("resource", existing.GetName()))
		return nil, status.Error(codes.Internal, "this Haify backend cannot create NFS gateways, which shared volumes need")
	}
	server, export, err := s.ensureGateway(ctx, gw, existing.GetName(), existing.GetLabels()[nfsServiceIPLabel], shared)
	if err != nil {
		if status.Code(err) != codes.Unknown {
			return nil, err
		}
		return nil, status.Errorf(codes.Internal, "create the NFS gateway of %q: %v", existing.GetName(), err)
	}
	return sharedVolumeContext(vc, server, export), nil
}

// deleteSharedGateway removes a shared volume's gateway ahead of its
// resource. The gateway holds the resource Primary with its filesystems
// mounted, and a resource in that state cannot be taken down: the service has
// to stop first. A gateway that is already gone is success, so a DeleteVolume
// retried after it got this far and no further goes straight on to the
// resource. For any other volume it does nothing.
func (s *controllerServer) deleteSharedGateway(ctx context.Context, r *haifypb.ResourceInfo) error {
	if !isSharedVolume(r) {
		return nil
	}
	volume := r.GetName()
	gw, ok := s.backend.(gatewayBackend)
	if !ok {
		s.log.Debug("controllerServer.deleteSharedGateway: backend has no gateways", zap.String("resource", volume))
		return status.Error(codes.Internal, "this Haify backend cannot remove NFS gateways, which deleting a shared volume needs")
	}
	done, free := s.shared.begin(volume)
	if !free {
		s.log.Debug("controllerServer.deleteSharedGateway: another call is working on the volume", zap.String("resource", volume))
		return status.Errorf(codes.Aborted, "another operation on shared volume %q is in progress", volume)
	}
	defer done()
	start := time.Now()
	_, gerr := gw.GetGateway(ctx, volume)
	s.log.Debug("controllerServer.deleteSharedGateway: looked up gateway", zap.String("resource", volume),
		zap.Bool("found", gerr == nil), zap.Duration("took", time.Since(start)), zap.Error(gerr))
	if gerr != nil {
		if !noSuchGateway(gerr) {
			return status.Errorf(codes.Unavailable, "look up the NFS gateway of %q: %v", volume, gerr)
		}
		s.log.Debug("controllerServer.deleteSharedGateway: no gateway, nothing to remove", zap.String("resource", volume))
		return nil
	}
	start = time.Now()
	err := gw.DeleteGateway(ctx, volume)
	s.log.Debug("controllerServer.deleteSharedGateway: delete gateway returned", zap.String("resource", volume),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return status.Errorf(codes.Internal, "delete the NFS gateway of %q: %v", volume, err)
	}
	return nil
}
