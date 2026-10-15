package csi

import (
	"context"
	"fmt"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const giB = 1 << 30

type controllerServer struct {
	csi.UnimplementedControllerServer
	// VolumeGroupSnapshot is served here too (group_snapshot.go).
	csi.UnimplementedGroupControllerServer
	backend HaifyBackend
	log     *zap.Logger
}

// NewControllerServer returns the CSI Controller service.
func NewControllerServer(b HaifyBackend, log *zap.Logger) csi.ControllerServer {
	return &controllerServer{backend: b, log: log}
}

func (s *controllerServer) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	// Secrets and parameters are not logged: the secrets map holds credentials,
	// and only the count of each says whether the CO sent any.
	s.log.Debug("controllerServer.CreateVolume: start", zap.String("name", req.GetName()),
		zap.Int64("required_bytes", req.GetCapacityRange().GetRequiredBytes()),
		zap.Int64("limit_bytes", req.GetCapacityRange().GetLimitBytes()),
		zap.Int("capabilities", len(req.GetVolumeCapabilities())),
		zap.Int("parameters", len(req.GetParameters())), zap.Int("secrets", len(req.GetSecrets())),
		zap.Bool("has_content_source", req.GetVolumeContentSource() != nil))
	if req.GetName() == "" {
		s.log.Debug("controllerServer.CreateVolume: rejected, no name")
		return nil, status.Error(codes.InvalidArgument, "volume name is required")
	}
	if len(req.GetVolumeCapabilities()) == 0 {
		s.log.Debug("controllerServer.CreateVolume: rejected, no capabilities", zap.String("name", req.GetName()))
		return nil, status.Error(codes.InvalidArgument, "volume capabilities are required")
	}
	// Refuse what cannot work here, where the message reaches the PVC's events,
	// rather than provisioning a volume that kubelet fails to attach later.
	for _, c := range req.GetVolumeCapabilities() {
		if reason := unsupportedCapability(c); reason != "" {
			s.log.Debug("controllerServer.CreateVolume: rejected capability", zap.String("name", req.GetName()),
				zap.String("reason", reason))
			return nil, status.Error(codes.InvalidArgument, reason)
		}
	}
	name := sanitizeResourceName(req.GetName())
	sizeGB := bytesToGiB(req.GetCapacityRange().GetRequiredBytes())
	s.log.Debug("controllerServer.CreateVolume: resolved name and size", zap.String("resource", name),
		zap.Uint32("size_gb", sizeGB))

	// Idempotency: if the resource already exists, check capacity range compatibility.
	start := time.Now()
	existing, getErr := s.backend.GetResource(ctx, name)
	s.log.Debug("controllerServer.CreateVolume: looked up existing resource", zap.String("resource", name),
		zap.Bool("found", getErr == nil && existing != nil), zap.Duration("took", time.Since(start)), zap.Error(getErr))
	if getErr == nil && existing != nil {
		// If the caller specified an exact capacity range (RequiredBytes == LimitBytes),
		// and it differs from what was provisioned, return AlreadyExists per CSI spec.
		cr := req.GetCapacityRange()
		if cr != nil && cr.GetLimitBytes() > 0 && cr.GetRequiredBytes() == cr.GetLimitBytes() {
			existingSizeGB := bytesToGiB(cr.GetRequiredBytes())
			if len(existing.GetVolumes()) > 0 && uint64(existingSizeGB) != existing.GetVolumes()[0].GetSizeGb() {
				s.log.Debug("controllerServer.CreateVolume: exists with different capacity", zap.String("resource", name),
					zap.Uint32("requested_gb", existingSizeGB), zap.Uint64("existing_gb", existing.GetVolumes()[0].GetSizeGb()))
				return nil, status.Errorf(codes.AlreadyExists, "volume %q already exists with different capacity", name)
			}
		}
		vc, err := s.reconcileExisting(ctx, existing, req.GetParameters())
		if err != nil {
			return nil, err
		}
		s.log.Debug("controllerServer.CreateVolume: already exists, returning it (idempotent)", zap.String("resource", name),
			zap.Strings("nodes", existing.GetNodes()))
		return &csi.CreateVolumeResponse{Volume: &csi.Volume{
			VolumeId:           existing.GetName(),
			CapacityBytes:      int64(sizeGB) * giB,
			AccessibleTopology: accessibleTopology(existing.GetNodes()),
			VolumeContext:      vc,
		}}, nil
	}

	// A PVC that names a VolumeAttributesClass from the start brings its
	// parameters here; they override the StorageClass's (modify.go).
	scParams, err := mergeMutableParameters(req.GetParameters(), req.GetMutableParameters())
	if err != nil {
		s.log.Debug("controllerServer.CreateVolume: invalid mutable parameters", zap.String("resource", name), zap.Error(err))
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	params, err := ParseVolumeParams(scParams)
	if err != nil {
		s.log.Debug("controllerServer.CreateVolume: invalid parameters", zap.String("resource", name), zap.Error(err))
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if params.ResourceProfile != "" {
		s.log.Debug("controllerServer.CreateVolume: applying resource profile", zap.String("resource", name),
			zap.String("profile", params.ResourceProfile))
		profileClient, ok := s.backend.(interface {
			GetResourceProfile(context.Context, string) (*haifypb.ResourceProfile, error)
		})
		if !ok {
			s.log.Debug("controllerServer.CreateVolume: backend has no resource profiles", zap.String("resource", name))
			return nil, status.Error(codes.Internal, "Haify backend does not support resource profiles")
		}
		start = time.Now()
		profile, profileErr := profileClient.GetResourceProfile(ctx, params.ResourceProfile)
		s.log.Debug("controllerServer.CreateVolume: fetched resource profile", zap.String("profile", params.ResourceProfile),
			zap.Duration("took", time.Since(start)), zap.Error(profileErr))
		if profileErr != nil {
			return nil, status.Errorf(codes.InvalidArgument, "resource profile %q: %v", params.ResourceProfile, profileErr)
		}
		if _, explicit := scParams["pool"]; !explicit {
			s.log.Debug("controllerServer.CreateVolume: pool from profile", zap.String("pool", profile.Pool))
			params.Pool = profile.Pool
		}
		if _, explicit := scParams["replicas"]; !explicit && profile.Replicas > 0 {
			s.log.Debug("controllerServer.CreateVolume: replicas from profile", zap.Uint32("replicas", profile.Replicas))
			params.Replicas = int(profile.Replicas)
		}
		if _, explicit := scParams["storageType"]; !explicit && profile.StorageType != "" {
			s.log.Debug("controllerServer.CreateVolume: storage type from profile", zap.String("storage_type", profile.StorageType))
			params.StorageType = profile.StorageType
		}
		if params.Pool == "" {
			s.log.Debug("controllerServer.CreateVolume: profile defines no pool", zap.String("profile", params.ResourceProfile))
			return nil, status.Errorf(codes.InvalidArgument, "resource profile %q does not define a pool", params.ResourceProfile)
		}
	}
	s.log.Debug("controllerServer.CreateVolume: parameters", zap.String("resource", name), zap.String("pool", params.Pool),
		zap.Int("replicas", params.Replicas), zap.String("storage_type", params.StorageType),
		zap.Bool("allow_remote", params.AllowRemoteVolumeAccess), zap.String("fault_domain_label", params.FaultDomainLabel),
		zap.Int("labels", len(params.ResourceLabels)), zap.Int("qos", len(params.QoS)), zap.String("pvc", params.PVC),
		zap.Bool("encryption", params.Encryption), zap.Bool("replication_tls", params.ReplicationTLS))

	// A volume restored from a snapshot, or cloned from another volume, must be
	// filled before first use. Resolve where the data comes from first: the copy
	// runs on the node holding the source, so that node has to be among the new
	// volume's replicas.
	var source *volumeSource
	if cs := req.GetVolumeContentSource(); cs != nil {
		source, err = s.resolveVolumeSource(ctx, cs, sizeGB, params.Encryption)
		if err != nil {
			s.log.Debug("controllerServer.CreateVolume: content source unusable", zap.String("resource", name), zap.Error(err))
			return nil, err
		}
		s.log.Debug("controllerServer.CreateVolume: content source resolved", zap.String("resource", name),
			zap.String("source_resource", source.snapshot.GetResource()), zap.String("snapshot", source.snapshot.GetName()),
			zap.String("node", source.node), zap.Bool("temporary_snapshot", source.cleanup != nil))
		if source.cleanup != nil {
			defer source.cleanup()
		}
	}

	start = time.Now()
	nodes, err := s.backend.ListNodes(ctx)
	s.log.Debug("controllerServer.CreateVolume: listed nodes", zap.Int("count", len(nodes)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list nodes: %v", err)
	}
	start = time.Now()
	pools, err := s.backend.ListPools(ctx)
	s.log.Debug("controllerServer.CreateVolume: listed pools", zap.Int("count", len(pools)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list pools: %v", err)
	}
	// Only consider nodes that actually host the requested pool: replicas
	// placed on a node without the backing pool fail at LV-creation time.
	if err := checkEncryptable(params, pools); err != nil {
		s.log.Debug("controllerServer.CreateVolume: cannot be encrypted", zap.String("resource", name), zap.Error(err))
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	candidates := nodesWithPool(nodes, pools, params.Pool, params.FaultDomainLabel)
	pinned := requisiteNodes(req.GetAccessibilityRequirements())
	if source != nil {
		// Put the source's node first so a replica lands there and the copy is
		// local; without this the new volume could be placed entirely elsewhere.
		pinned = append([]string{source.node}, pinned...)
	}
	if params.ReplicationTLS {
		if candidates, err = s.tlsReadyCandidates(ctx, candidates, pinned, params.Replicas); err != nil {
			s.log.Debug("controllerServer.CreateVolume: replication TLS not possible", zap.String("resource", name), zap.Error(err))
			return nil, err
		}
	}
	// The free space of the pool decides the rest, so a PVC lands where
	// `haify resource create` would: on the emptiest nodes. ResourceExhausted
	// is the status the CO acts on — external-provisioner drops the PVC's
	// selected-node annotation on it and reschedules — so every placement
	// failure, capacity or otherwise, has to surface under that code.
	s.log.Debug("controllerServer.CreateVolume: placement inputs", zap.String("resource", name),
		zap.Int("candidates", len(candidates)), zap.Strings("pinned", pinned))
	replicaNodes, err := selectReplicaNodes(candidates, pinned, params.Replicas, uint64(sizeGB)*giB)
	if err != nil {
		s.log.Debug("controllerServer.CreateVolume: placement failed", zap.String("resource", name),
			zap.String("pool", params.Pool), zap.Error(err))
		return nil, status.Errorf(codes.ResourceExhausted, "pool %q: %v", params.Pool, err)
	}
	s.log.Debug("controllerServer.CreateVolume: placement chosen", zap.String("resource", name), zap.Strings("nodes", replicaNodes))
	if source != nil && !containsNode(replicaNodes, source.node) {
		s.log.Debug("controllerServer.CreateVolume: source node not among replicas", zap.String("resource", name),
			zap.String("node", source.node), zap.Strings("nodes", replicaNodes))
		return nil, status.Errorf(codes.ResourceExhausted,
			"node %q holds the source data but cannot host a replica of the new volume (pool %q)", source.node, params.Pool)
	}

	labels := make(map[string]string, len(params.ResourceLabels)+1)
	for key, value := range params.ResourceLabels {
		labels[key] = value
	}
	labels[managedByLabel] = managedByValue
	placementLabels(labels, params.AllowRemoteVolumeAccess, replicaNodes, params.FaultDomainLabel)
	if params.PVC != "" {
		// So the volume is recognisable in Haify by its claim, not only by
		// the generated pvc-<uid> name.
		labels[pvcLabel] = params.PVC
	}
	start = time.Now()
	if backend, ok := s.backend.(interface {
		CreateResourceRequest(context.Context, *haifypb.CreateResourceRequest) error
	}); ok {
		s.log.Debug("controllerServer.CreateVolume: creating resource (full request)", zap.String("resource", name),
			zap.Strings("nodes", replicaNodes), zap.Uint32("size_gb", sizeGB), zap.String("pool", params.Pool),
			zap.Int("labels", len(labels)))
		err = backend.CreateResourceRequest(ctx, &haifypb.CreateResourceRequest{
			Name:        name,
			Nodes:       replicaNodes,
			Protocol:    "C",
			SizeGb:      sizeGB,
			Pool:        params.Pool,
			StorageType: params.StorageType,
			Profile:     params.ResourceProfile,
			Labels:      labels,
			Encrypt:     params.Encryption,
		})
	} else if params.ResourceProfile != "" || len(params.ResourceLabels) > 0 || params.Encryption {
		s.log.Debug("controllerServer.CreateVolume: backend cannot take profile, labels or encryption", zap.String("resource", name))
		return nil, status.Error(codes.Internal, "Haify backend does not support resource profiles, labels or encryption")
	} else {
		s.log.Debug("controllerServer.CreateVolume: creating resource (pool and type)", zap.String("resource", name),
			zap.Strings("nodes", replicaNodes), zap.Uint32("size_gb", sizeGB), zap.String("pool", params.Pool))
		err = s.backend.CreateResourceWithPoolAndType(ctx, name, 0, replicaNodes, "C", sizeGB, params.Pool, params.StorageType, nil)
	}
	s.log.Debug("controllerServer.CreateVolume: create resource returned", zap.String("resource", name),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create resource: %v", err)
	}

	// Encrypt the links before the copy below, so a clone's data never crosses
	// the network in the clear. A volume that asked for TLS and did not get it
	// is removed again rather than handed out replicating in plaintext.
	if params.ReplicationTLS {
		if terr := s.enableReplicationTLS(ctx, name); terr != nil {
			s.log.Debug("controllerServer.CreateVolume: rolling back, TLS switch failed", zap.String("resource", name), zap.Error(terr))
			if derr := s.backend.DeleteResource(context.WithoutCancel(ctx), name); derr != nil {
				s.log.Error("failed to roll back a volume whose replication TLS switch failed; it may need manual cleanup",
					zap.String("volume", name), zap.Error(derr))
			}
			return nil, status.Error(codes.Internal, terr.Error())
		}
	}

	// Fill the new volume from its source. This must happen before the volume is
	// ever handed out, so on failure the half-written resource is destroyed
	// rather than returned: a retry then starts from a clean, empty volume.
	//
	// (If the controller itself dies mid-copy the resource survives empty, and a
	// retry would take the "already exists" path above and return it as ready.
	// That window is not closed here; it needs persisted provisioning state.)
	if source != nil {
		start = time.Now()
		copied, perr := s.backend.PopulateVolumeFromSnapshot(ctx, name, 0, source.snapshot, source.node)
		s.log.Debug("controllerServer.CreateVolume: populated from source", zap.String("resource", name),
			zap.String("node", source.node), zap.Uint64("bytes", copied), zap.Duration("took", time.Since(start)), zap.Error(perr))
		if perr != nil {
			s.log.Debug("controllerServer.CreateVolume: rolling back the half-filled volume", zap.String("resource", name))
			if derr := s.backend.DeleteResource(context.WithoutCancel(ctx), name); derr != nil {
				s.log.Error("failed to roll back a volume whose restore failed; it may need manual cleanup",
					zap.String("volume", name), zap.Error(derr))
			}
			return nil, status.Errorf(codes.Internal, "populate %q from %s: %v", name, describeSource(source), perr)
		}
	}

	s.log.Debug("controllerServer.CreateVolume: done", zap.String("resource", name), zap.Strings("nodes", replicaNodes),
		zap.Uint32("size_gb", sizeGB))
	return &csi.CreateVolumeResponse{Volume: &csi.Volume{
		VolumeId:           name,
		CapacityBytes:      int64(sizeGB) * giB,
		AccessibleTopology: topologyFor(replicaNodes, params.AllowRemoteVolumeAccess),
		VolumeContext:      securityContext(volumeContextFor(params.AllowRemoteVolumeAccess, params.QoS), params.Encryption, params.ReplicationTLS),
		ContentSource:      contentSourceOf(req.GetVolumeContentSource()),
	}}, nil
}

// containsNode reports whether nodes includes want.
func containsNode(nodes []string, want string) bool {
	for _, n := range nodes {
		if n == want {
			return true
		}
	}
	return false
}

// topologyFor decides where the CO may schedule Pods that use the volume. By
// default it pins them to the replica nodes (local I/O). With remote access
// enabled the volume is reachable from every node via a diskless client, so we
// impose no topology constraint and let the scheduler place the Pod anywhere.
func topologyFor(replicaNodes []string, allowRemote bool) []*csi.Topology {
	if allowRemote {
		zap.L().Debug("topologyFor: remote access allowed, no topology constraint")
		return nil
	}
	zap.L().Debug("topologyFor: pinned to replica nodes", zap.Strings("nodes", replicaNodes))
	return accessibleTopology(replicaNodes)
}

// volumeContextFor carries the remote-access flag into the volume's context so
// the node service can tell, at stage time, whether a Pod on a non-replica node
// is allowed to attach the volume diskless — and the I/O limits it applies at
// publish time (qos.go).
func volumeContextFor(allowRemote bool, qos map[string]string) map[string]string {
	zap.L().Debug("volumeContextFor: building volume context", zap.Bool("allow_remote", allowRemote), zap.Int("qos", len(qos)))
	if !allowRemote && len(qos) == 0 {
		return nil
	}
	out := map[string]string{}
	if allowRemote {
		out[paramAllowRemoteVolumeAccess] = "true"
	}
	for k, v := range qos {
		out[k] = v
	}
	return out
}

func (s *controllerServer) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	s.log.Debug("controllerServer.DeleteVolume: start", zap.String("resource", req.GetVolumeId()),
		zap.Int("secrets", len(req.GetSecrets())))
	if req.GetVolumeId() == "" {
		s.log.Debug("controllerServer.DeleteVolume: rejected, no volume id")
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	start := time.Now()
	if _, err := s.backend.GetResource(ctx, req.GetVolumeId()); err != nil {
		// Not found -> already deleted; idempotent success.
		s.log.Debug("controllerServer.DeleteVolume: not found, treating as deleted", zap.String("resource", req.GetVolumeId()),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		return &csi.DeleteVolumeResponse{}, nil
	}
	start = time.Now()
	err := s.backend.DeleteResource(ctx, req.GetVolumeId())
	s.log.Debug("controllerServer.DeleteVolume: delete resource returned", zap.String("resource", req.GetVolumeId()),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "delete resource: %v", err)
	}
	return &csi.DeleteVolumeResponse{}, nil
}

func (s *controllerServer) ControllerGetCapabilities(context.Context, *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	s.log.Debug("controllerServer.ControllerGetCapabilities: reporting capabilities")
	cap := func(t csi.ControllerServiceCapability_RPC_Type) *csi.ControllerServiceCapability {
		return &csi.ControllerServiceCapability{Type: &csi.ControllerServiceCapability_Rpc{
			Rpc: &csi.ControllerServiceCapability_RPC{Type: t}}}
	}
	return &csi.ControllerGetCapabilitiesResponse{Capabilities: []*csi.ControllerServiceCapability{
		cap(csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME),
		cap(csi.ControllerServiceCapability_RPC_EXPAND_VOLUME),
		cap(csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT),
		// Snapshots live on one node each with no cluster-wide index, so
		// ListSnapshots walks the CSI volumes and asks each replica node.
		cap(csi.ControllerServiceCapability_RPC_LIST_SNAPSHOTS),
		cap(csi.ControllerServiceCapability_RPC_CLONE_VOLUME),
		cap(csi.ControllerServiceCapability_RPC_LIST_VOLUMES),
		cap(csi.ControllerServiceCapability_RPC_GET_VOLUME),
		cap(csi.ControllerServiceCapability_RPC_GET_CAPACITY),
		cap(csi.ControllerServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER),
		// Replicas, resource profile and I/O limits of a provisioned volume,
		// through a VolumeAttributesClass (modify.go).
		cap(csi.ControllerServiceCapability_RPC_MODIFY_VOLUME),
	}}, nil
}

func (s *controllerServer) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	s.log.Debug("controllerServer.ValidateVolumeCapabilities: start", zap.String("resource", req.GetVolumeId()),
		zap.Int("capabilities", len(req.GetVolumeCapabilities())))
	if req.GetVolumeId() == "" {
		s.log.Debug("controllerServer.ValidateVolumeCapabilities: rejected, no volume id")
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	if len(req.GetVolumeCapabilities()) == 0 {
		s.log.Debug("controllerServer.ValidateVolumeCapabilities: rejected, no capabilities", zap.String("resource", req.GetVolumeId()))
		return nil, status.Error(codes.InvalidArgument, "volume capabilities are required")
	}
	start := time.Now()
	if _, err := s.backend.GetResource(ctx, req.GetVolumeId()); err != nil {
		s.log.Debug("controllerServer.ValidateVolumeCapabilities: volume not found", zap.String("resource", req.GetVolumeId()),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		return nil, status.Errorf(codes.NotFound, "volume %q not found", req.GetVolumeId())
	}
	for _, c := range req.GetVolumeCapabilities() {
		if reason := unsupportedCapability(c); reason != "" {
			s.log.Debug("controllerServer.ValidateVolumeCapabilities: unsupported", zap.String("resource", req.GetVolumeId()),
				zap.String("reason", reason))
			return &csi.ValidateVolumeCapabilitiesResponse{Message: reason}, nil // unsupported -> empty Confirmed
		}
	}
	s.log.Debug("controllerServer.ValidateVolumeCapabilities: confirmed", zap.String("resource", req.GetVolumeId()))
	return &csi.ValidateVolumeCapabilitiesResponse{Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
		VolumeCapabilities: req.GetVolumeCapabilities(),
	}}, nil
}

// ControllerExpandVolume resizes the backing DRBD+LVM/ZFS volume on all
// replica nodes. The node plugin follows up with NodeExpandVolume to grow
// the filesystem online.
func (s *controllerServer) ControllerExpandVolume(ctx context.Context, req *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	s.log.Debug("controllerServer.ControllerExpandVolume: start", zap.String("resource", req.GetVolumeId()),
		zap.Int64("required_bytes", req.GetCapacityRange().GetRequiredBytes()))
	if req.GetVolumeId() == "" {
		s.log.Debug("controllerServer.ControllerExpandVolume: rejected, no volume id")
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	newSizeGB := bytesToGiB(req.GetCapacityRange().GetRequiredBytes())

	start := time.Now()
	err := s.backend.ResizeVolume(ctx, req.GetVolumeId(), 0, newSizeGB)
	s.log.Debug("controllerServer.ControllerExpandVolume: resize returned", zap.String("resource", req.GetVolumeId()),
		zap.Uint32("size_gb", newSizeGB), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "resize volume: %v", err)
	}

	return &csi.ControllerExpandVolumeResponse{
		CapacityBytes:         int64(newSizeGB) * giB,
		NodeExpansionRequired: true, // filesystem resize needed on the node
	}, nil
}

// bytesToGiB converts a byte count to whole GiB, rounding up; minimum 1.
func bytesToGiB(b int64) uint32 {
	if b <= 0 {
		return 1
	}
	g := (b + giB - 1) / giB
	return uint32(g)
}

// unsupportedCapability explains why a volume capability cannot be honoured, or
// returns "" when it can.
//
// DRBD runs one Primary per resource, and only the Primary can do I/O. So every
// single-node mode works — including ReadWriteOncePod, which only narrows
// single-node further to single-Pod. The one multi-node mode is ReadWriteMany
// on a raw block volume, for the live migration of a virtual machine: the node
// plugin lets two nodes be Primary while both have it staged
// (node_multiwriter.go). A filesystem would be corrupted by two writers, so
// ReadWriteMany stays refused for filesystem volumes, and the read-only and
// single-writer multi-node modes are refused as before. Before this check
// CreateVolume looked at no access mode at all: a ReadWriteMany PVC was
// provisioned, and failed only when a second node tried to promote.
func unsupportedCapability(c *csi.VolumeCapability) string {
	zap.L().Debug("unsupportedCapability: checking access mode", zap.Stringer("access_mode", c.GetAccessMode().GetMode()),
		zap.Bool("block", c.GetBlock() != nil))
	switch c.GetAccessMode().GetMode() {
	case csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER:
	case csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER:
		if c.GetBlock() == nil {
			return "access mode MULTI_NODE_MULTI_WRITER (ReadWriteMany) is supported only with volumeMode: Block, for the live migration of a virtual machine; a filesystem cannot be written from two nodes"
		}
	case csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
		csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER:
		return fmt.Sprintf("access mode %s is not supported: a DRBD resource has one Primary, so a volume is usable from one node at a time; use ReadWriteOnce or ReadWriteOncePod (or ReadWriteMany with volumeMode: Block for live migration)",
			c.GetAccessMode().GetMode())
	default:
		return fmt.Sprintf("access mode %s is not supported", c.GetAccessMode().GetMode())
	}
	return ""
}
