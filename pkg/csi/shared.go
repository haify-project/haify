package csi

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Shared volumes: ReadWriteMany filesystem volumes.
//
// A DRBD resource has one Primary, so a filesystem on it can be mounted on
// one node. A StorageClass with sharedFilesystem: nfs asks for a volume many
// nodes mount at once, and gets one by putting a Haify NFS gateway in front
// of the resource: the gateway mounts the filesystem on whichever replica
// node is Primary and exports it on a floating address, and every Pod's node
// mounts that export. When the node serving it fails, drbd-reactor moves the
// gateway, address included, to another replica and the clients carry on.
//
// So for a shared volume the node plugin never touches DRBD: it mounts NFS
// (node_shared.go). Everything that reasons about a volume by which node is
// Primary, or by which nodes hold a replica, has to leave these alone, since
// neither says anything about where the Pods are.

// StorageClass parameters of a shared volume.
const (
	// paramSharedFilesystem turns a class into one for shared volumes. Its
	// value is how the volume is shared; "nfs" is the only way. It is also the
	// key in the volume context that tells the node plugin to mount NFS.
	paramSharedFilesystem = "sharedFilesystem"
	sharedNFS             = "nfs"
	// paramNFSServiceIPs is the pool of floating addresses (shared_pool.go).
	paramNFSServiceIPs = "nfsServiceIPs"
	// paramNFSAllowedClients limits who may mount the exports, as a
	// comma-separated list of networks. Empty: every client that reaches the
	// address.
	paramNFSAllowedClients = "nfsAllowedClients"
)

// Labels on the Haify resource of a shared volume.
const (
	// sharedLabel marks the resource as a shared volume, with how it is
	// shared as its value.
	sharedLabel = "haify.csi/shared"
	// nfsServiceIPLabel is the volume's floating address, as address/prefix.
	// It is written when the resource is created, before the gateway exists,
	// so the address is taken from that moment on (shared_pool.go).
	nfsServiceIPLabel = "haify.csi/nfs-service-ip"
)

// Volume context of a shared volume: all the node plugin needs to mount it,
// so that mounting does not depend on the Haify controller being reachable.
const (
	ctxNFSServer = "nfsServer" // the floating address, without a prefix length
	ctxNFSExport = "nfsExport" // the exported directory
)

// sharedExportBase is where a gateway created without an export path puts
// its export, as <base>/<resource>. It mirrors gateway.DefaultExportBasePath
// and is only the fallback for a controller that does not report the path.
const sharedExportBase = "/srv/gateway-exports"

// sharedFSType is the filesystem a gateway puts on the volume. The gateway
// formats it; the fsType of a PVC has no say.
const sharedFSType = "ext4"

// SharedParams is the shared-volume part of a StorageClass.
type SharedParams struct {
	// Enabled is true for sharedFilesystem: nfs.
	Enabled bool
	// ServiceIPs is the parsed nfsServiceIPs.
	ServiceIPs []ipRange
	// AllowedClients is the parsed nfsAllowedClients.
	AllowedClients []string
}

// sharedRequested reports whether the parameters ask for a shared volume,
// before they are validated: the capability check comes first and needs to
// know which rules apply.
func sharedRequested(p map[string]string) bool {
	return strings.TrimSpace(p[paramSharedFilesystem]) != ""
}

// parseSharedParams validates the shared-volume parameters.
func parseSharedParams(p map[string]string) (SharedParams, error) {
	log := zap.L()
	mode := strings.TrimSpace(p[paramSharedFilesystem])
	pool := strings.TrimSpace(p[paramNFSServiceIPs])
	clients := strings.TrimSpace(p[paramNFSAllowedClients])
	log.Debug("parseSharedParams: start", zap.String("shared_filesystem", mode), zap.String("service_ips", pool),
		zap.String("allowed_clients", clients))
	var out SharedParams
	switch mode {
	case "":
		if pool != "" || clients != "" {
			log.Debug("parseSharedParams: NFS settings without sharedFilesystem")
			return out, fmt.Errorf("%s and %s need %s: \"%s\"", paramNFSServiceIPs, paramNFSAllowedClients, paramSharedFilesystem, sharedNFS)
		}
		return out, nil
	case sharedNFS:
	default:
		log.Debug("parseSharedParams: unknown mode", zap.String("shared_filesystem", mode))
		return out, fmt.Errorf("invalid %s %q (want %q)", paramSharedFilesystem, mode, sharedNFS)
	}
	out.Enabled = true
	if pool == "" {
		log.Debug("parseSharedParams: no service address pool")
		return out, fmt.Errorf("%s needs %s: the addresses the volumes' NFS services may use, as in 192.168.1.200-192.168.1.220/24",
			paramSharedFilesystem, paramNFSServiceIPs)
	}
	ranges, err := parseServiceIPPool(pool)
	if err != nil {
		return out, err
	}
	out.ServiceIPs = ranges
	for _, c := range strings.Split(clients, ",") {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if _, perr := netip.ParsePrefix(c); perr != nil {
			if _, aerr := netip.ParseAddr(c); aerr != nil {
				log.Debug("parseSharedParams: invalid allowed client", zap.String("client", c))
				return out, fmt.Errorf("%s entry %q is not an address or a network such as 10.0.0.0/16", paramNFSAllowedClients, c)
			}
		}
		out.AllowedClients = append(out.AllowedClients, c)
	}
	log.Debug("parseSharedParams: parsed", zap.Int("ranges", len(out.ServiceIPs)), zap.Int("allowed_clients", len(out.AllowedClients)))
	return out, nil
}

// sharedParamConflict names the first StorageClass parameter that cannot be
// combined with a shared volume, or returns nil.
func sharedParamConflict(p VolumeParams) error {
	if !p.Shared.Enabled {
		return nil
	}
	if len(p.QoS) > 0 {
		// The limits are written against the DRBD device in the Pod's cgroup.
		// A Pod on a shared volume does its I/O through an NFS mount, which
		// that rule does not match: the limit would be accepted and limit
		// nothing.
		zap.L().Debug("sharedParamConflict: I/O limits on a shared volume")
		return fmt.Errorf("I/O limits (readIOPS, writeIOPS, readBytesPerSecond, writeBytesPerSecond) cannot be combined with %s: "+
			"they apply to a volume's block device on the Pod's node, and a shared volume is reached over NFS", paramSharedFilesystem)
	}
	return nil
}

// isSharedVolume reports whether a resource is a shared volume.
func isSharedVolume(r *haifypb.ResourceInfo) bool {
	return r.GetLabels()[sharedLabel] == sharedNFS
}

// isSharedContext reports whether a volume context is that of a shared
// volume.
func isSharedContext(vc map[string]string) bool {
	return vc[paramSharedFilesystem] == sharedNFS
}

// capabilityProblem explains why a capability cannot be honoured, for a
// shared volume or an ordinary one, or returns "".
//
// A shared volume is a filesystem reached over NFS, so every access mode
// works, the multi-node ones included, and the one thing it cannot be is a
// raw block device. The rules for every other volume are unsupportedCapability's
// and are not touched.
func capabilityProblem(c *csi.VolumeCapability, shared bool) string {
	if !shared {
		return unsupportedCapability(c)
	}
	zap.L().Debug("capabilityProblem: checking a shared volume", zap.Stringer("access_mode", c.GetAccessMode().GetMode()),
		zap.Bool("block", c.GetBlock() != nil), zap.String("fs_type", c.GetMount().GetFsType()))
	if c.GetBlock() != nil {
		return fmt.Sprintf("a StorageClass with %s provides filesystem volumes over NFS; volumeMode: Block is not available from it",
			paramSharedFilesystem)
	}
	if fs := c.GetMount().GetFsType(); fs != "" && fs != sharedFSType && fs != sharedNFS {
		return fmt.Sprintf("fsType %q cannot be chosen for a StorageClass with %s: the volume is formatted %s on the Haify side and mounted over NFS",
			fs, paramSharedFilesystem, sharedFSType)
	}
	switch c.GetAccessMode().GetMode() {
	case csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER,
		csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
		csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER,
		csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER:
		return ""
	default:
		return fmt.Sprintf("access mode %s is not supported", c.GetAccessMode().GetMode())
	}
}

// sharedVolumeContext is the volume context of a shared volume: where to
// mount it from, plus whatever flags the volume carries anyway. The
// remote-access flag is dropped: it tells the node plugin it may attach a
// diskless DRBD client, which a shared volume never does.
func sharedVolumeContext(base map[string]string, server, export string) map[string]string {
	out := map[string]string{paramSharedFilesystem: sharedNFS, ctxNFSServer: server, ctxNFSExport: export}
	for k, v := range base {
		if k != paramAllowRemoteVolumeAccess {
			out[k] = v
		}
	}
	return out
}

// sharedState is what the controller service keeps for shared volumes.
type sharedState struct {
	ips serviceIPAllocator

	mu sync.Mutex
	// busy holds the volumes a call is creating or removing a gateway for.
	busy map[string]bool
}

// begin claims a volume for one call, or reports that another call has it.
//
// Creating a gateway promotes the resource and may format it, and takes long
// enough for the CO to time out and retry while the first call is still at
// it. Two calls doing that to one resource at once is how a volume gets
// formatted under a mount, so the second is turned away until the first is
// done.
func (st *sharedState) begin(volume string) (func(), bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.busy[volume] {
		return nil, false
	}
	if st.busy == nil {
		st.busy = map[string]bool{}
	}
	st.busy[volume] = true
	return func() {
		st.mu.Lock()
		delete(st.busy, volume)
		st.mu.Unlock()
	}, true
}

// beginShared claims the volume for a CreateVolume that asks for a shared
// volume. For any other request it does nothing.
func (s *controllerServer) beginShared(p map[string]string, volume string) (func(), error) {
	if !sharedRequested(p) {
		return func() {}, nil
	}
	done, ok := s.shared.begin(volume)
	if !ok {
		s.log.Debug("controllerServer.beginShared: another call is working on the volume", zap.String("resource", volume))
		return nil, status.Errorf(codes.Aborted, "another operation on shared volume %q is in progress", volume)
	}
	s.log.Debug("controllerServer.beginShared: claimed", zap.String("resource", volume))
	return done, nil
}

// checkSharedRequest refuses, before anything is created, a shared volume
// that could not be completed or would come out wrong. It does nothing for
// any other request.
func (s *controllerServer) checkSharedRequest(ctx context.Context, req *csi.CreateVolumeRequest, params VolumeParams, dr drParams, sizeGB uint32) error {
	if !params.Shared.Enabled {
		return nil
	}
	s.log.Debug("controllerServer.checkSharedRequest: start", zap.String("name", req.GetName()), zap.Uint32("size_gb", sizeGB),
		zap.Bool("has_content_source", req.GetVolumeContentSource() != nil), zap.String("dr_node", dr.node))
	if _, ok := s.backend.(gatewayBackend); !ok {
		s.log.Debug("controllerServer.checkSharedRequest: backend has no gateways")
		return status.Error(codes.Internal, "this Haify backend cannot create NFS gateways, which shared volumes need")
	}
	if _, ok := s.backend.(interface {
		CreateResourceRequest(context.Context, *haifypb.CreateResourceRequest) error
	}); !ok {
		s.log.Debug("controllerServer.checkSharedRequest: backend cannot label a resource at creation")
		return status.Error(codes.Internal, "this Haify backend cannot record a shared volume's service address")
	}
	if dr.node != "" {
		// The NFS service runs where the gateway's promoter runs, which is the
		// local replicas. A failover to the DR site brings the data up there
		// and not the service the Pods mount, so the class would promise a
		// recovery it does not deliver.
		s.log.Debug("controllerServer.checkSharedRequest: DR replica asked for", zap.String("dr_node", dr.node))
		return status.Errorf(codes.InvalidArgument,
			"%s cannot be combined with %s: the NFS service of a shared volume does not follow the volume to the DR site",
			paramDRNode, paramSharedFilesystem)
	}
	return s.checkSharedSource(ctx, req.GetVolumeContentSource(), sizeGB)
}

// checkSharedSource decides whether a new shared volume may start from the
// given snapshot or volume.
//
// The copy is block for block, and the gateway then mounts what it finds
// rather than formatting. That is right only when what it finds is what a
// gateway would have made: the filesystem of another shared volume, filling
// the device. A source of another kind may hold a filesystem the gateway
// cannot mount, and a smaller source leaves a filesystem smaller than the
// volume: the PVC would report a size its Pods do not see until something
// grew the filesystem, which creating a volume does not do. Both are refused
// here instead; a larger volume is had by expanding the new one afterwards.
func (s *controllerServer) checkSharedSource(ctx context.Context, cs *csi.VolumeContentSource, sizeGB uint32) error {
	if cs == nil {
		return nil
	}
	var source, what string
	switch src := cs.GetType().(type) {
	case *csi.VolumeContentSource_Snapshot:
		ref, err := parseSnapshotRef(src.Snapshot.GetSnapshotId())
		if err != nil {
			s.log.Debug("controllerServer.checkSharedSource: malformed snapshot id", zap.Error(err))
			return status.Errorf(codes.NotFound, "snapshot %q: %v", src.Snapshot.GetSnapshotId(), err)
		}
		if ref.backedUp {
			s.log.Debug("controllerServer.checkSharedSource: snapshot is on a backup target", zap.String("resource", ref.resource))
			return status.Errorf(codes.InvalidArgument,
				"a shared volume cannot be restored from a snapshot kept on a backup target: what filesystem the backup holds is not known "+
					"until it is restored; restore it into a ReadWriteOnce volume and copy the files")
		}
		source, what = ref.resource, "snapshot"
	case *csi.VolumeContentSource_Volume:
		source, what = src.Volume.GetVolumeId(), "volume"
	default:
		s.log.Debug("controllerServer.checkSharedSource: unknown content source")
		return status.Error(codes.InvalidArgument, "unsupported volume content source")
	}
	start := time.Now()
	r, err := s.backend.GetResource(ctx, source)
	s.log.Debug("controllerServer.checkSharedSource: looked up source", zap.String("resource", source), zap.String("kind", what),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil || r == nil {
		return status.Errorf(codes.NotFound, "source volume %q of the %s not found", source, what)
	}
	if !isSharedVolume(r) {
		s.log.Debug("controllerServer.checkSharedSource: source is not a shared volume", zap.String("resource", source))
		return status.Errorf(codes.InvalidArgument,
			"a shared volume can only be created from a %s of another shared volume: %q is not one, and its filesystem may not be one the NFS service can mount",
			what, source)
	}
	if vols := r.GetVolumes(); len(vols) == 0 || vols[0].GetSizeGb() != uint64(sizeGB) {
		var have uint64
		if len(vols) > 0 {
			have = vols[0].GetSizeGb()
		}
		s.log.Debug("controllerServer.checkSharedSource: size differs from the source", zap.String("resource", source),
			zap.Uint64("source_gb", have), zap.Uint32("want_gb", sizeGB))
		return status.Errorf(codes.InvalidArgument,
			"a shared volume created from a %s must have the size of its source (%dGi, asked for %dGi): create it at that size and expand it afterwards",
			what, have, sizeGB)
	}
	s.log.Debug("controllerServer.checkSharedSource: source accepted", zap.String("resource", source))
	return nil
}

// sharedModifyProblem rejects the changes a VolumeAttributesClass cannot
// make to a shared volume. Replicas and resource profile work as for any
// volume; I/O limits do not (sharedParamConflict).
func sharedModifyProblem(r *haifypb.ResourceInfo, change mutableChange) error {
	if !isSharedVolume(r) || len(change.qos) == 0 {
		return nil
	}
	zap.L().Debug("sharedModifyProblem: I/O limits on a shared volume", zap.String("resource", r.GetName()))
	return status.Errorf(codes.InvalidArgument,
		"I/O limits cannot be set on shared volume %q: they apply to a volume's block device on the Pod's node, and a shared volume is reached over NFS",
		r.GetName())
}
