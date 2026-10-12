package csi

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// managedByLabel marks a resource this driver created. CreateVolume sets it;
// ListVolumes and ListSnapshots use it to leave alone every resource the
// cluster runs for other reasons, such as its own control plane.
const (
	managedByLabel = "haify.csi/managed-by"
	managedByValue = "csi"
	// pvcLabel names the claim the volume was provisioned for, as
	// namespace/name.
	pvcLabel = "haify.csi/pvc"
)

func isCSIVolume(r *haifypb.ResourceInfo) bool {
	return r.GetLabels()[managedByLabel] == managedByValue
}

// GetCapacity reports how much room a pool has left on one node, which
// external-provisioner publishes as a CSIStorageCapacity object and the
// scheduler reads before it places a Pod.
//
// Without this the scheduler is blind to pool space. A PVC whose node's pool is
// full was placed there anyway and then failed with ResourceExhausted, and under
// WaitForFirstConsumer the Pod sat Pending while the provisioner retried. With
// it, the scheduler does not pick the node in the first place.
func (s *controllerServer) GetCapacity(ctx context.Context, req *csi.GetCapacityRequest) (*csi.GetCapacityResponse, error) {
	topoNode := req.GetAccessibleTopology().GetSegments()[TopologyKeyNode]
	s.log.Debug("controllerServer.GetCapacity: start", zap.String("pool", req.GetParameters()["pool"]),
		zap.String("node", topoNode), zap.Int("capabilities", len(req.GetVolumeCapabilities())))
	for _, c := range req.GetVolumeCapabilities() {
		if unsupportedCapability(c) != "" {
			s.log.Debug("controllerServer.GetCapacity: unsupported capability, reporting 0")
			return &csi.GetCapacityResponse{AvailableCapacity: 0}, nil
		}
	}
	// Parameters are optional in GetCapacity. Without a pool the question is how
	// much this driver could provision anywhere, which is every pool it knows.
	if req.GetParameters()["pool"] == "" {
		s.log.Debug("controllerServer.GetCapacity: no pool given, summing every pool")
		return s.capacityOfAllPools(ctx)
	}
	params, err := ParseVolumeParams(req.GetParameters())
	if err != nil {
		s.log.Debug("controllerServer.GetCapacity: invalid parameters", zap.Error(err))
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	start := time.Now()
	nodes, err := s.backend.ListNodes(ctx)
	s.log.Debug("controllerServer.GetCapacity: listed nodes", zap.Int("count", len(nodes)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list nodes: %v", err)
	}
	start = time.Now()
	pools, err := s.backend.ListPools(ctx)
	s.log.Debug("controllerServer.GetCapacity: listed pools", zap.Int("count", len(pools)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list pools: %v", err)
	}

	want := managedPoolName(params.Pool)
	capacity := map[string]uint64{} // node name -> free bytes, for nodes that report it
	var unknown []string
	for _, n := range nodes {
		for _, p := range pools {
			if p.GetName() != want || (p.GetNode() != n.GetAddress() && p.GetNode() != n.GetName()) {
				continue
			}
			free, known := poolCapacity(p)
			s.log.Debug("controllerServer.GetCapacity: node hosts pool", zap.String("node", n.GetName()),
				zap.String("pool", want), zap.Uint64("free_bytes", free), zap.Bool("known", known))
			if known {
				capacity[n.GetName()] = free
			} else {
				unknown = append(unknown, n.GetName())
			}
			break
		}
	}

	// One node, the case external-provisioner asks about: one CSIStorageCapacity
	// object per node, because the topology key is per node.
	if node := topoNode; node != "" {
		free, ok := capacity[node]
		if !ok {
			for _, u := range unknown {
				if u == node {
					s.log.Debug("controllerServer.GetCapacity: node reports no capacity figure", zap.String("node", node),
						zap.String("pool", params.Pool))
					// Publishing 0 would read as "full" and stop the scheduler
					// placing anything here, over a figure nobody measured.
					// Unavailable makes the provisioner retry instead.
					return nil, status.Errorf(codes.Unavailable,
						"pool %q on node %q reports no capacity figure", params.Pool, node)
				}
			}
			if params.AllowRemoteVolumeAccess {
				// A Pod here reaches its volume as a diskless client, so what
				// limits it is the room on the nodes that would hold the
				// replicas. Reporting 0 would keep the scheduler away from
				// exactly the nodes this class lets Pods run on.
				s.log.Debug("controllerServer.GetCapacity: node lacks pool, remote access allowed, reporting pool-wide",
					zap.String("node", node), zap.String("pool", params.Pool))
				return poolWide(capacity, params.Replicas), nil
			}
			// The node does not host the pool: it can take none of this class.
			s.log.Debug("controllerServer.GetCapacity: node does not host the pool, reporting 0", zap.String("node", node),
				zap.String("pool", params.Pool))
			return &csi.GetCapacityResponse{AvailableCapacity: 0, MaximumVolumeSize: wrapperspb.Int64(0)}, nil
		}
		s.log.Debug("controllerServer.GetCapacity: node capacity", zap.String("node", node), zap.String("pool", params.Pool),
			zap.Uint64("free_bytes", free))
		return &csi.GetCapacityResponse{
			AvailableCapacity: int64(free),
			MaximumVolumeSize: wrapperspb.Int64(int64(free)),
		}, nil
	}

	s.log.Debug("controllerServer.GetCapacity: no node in topology, reporting pool-wide", zap.String("pool", params.Pool),
		zap.Int("nodes_known", len(capacity)), zap.Int("nodes_unknown", len(unknown)))
	return poolWide(capacity, params.Replicas), nil
}

// poolWide is the room in a pool as a whole, given each node's free bytes, and
// the biggest volume that fits on the nodes it would need. A volume takes
// `replicas` nodes, so the largest one that fits is bounded by the
// replicas-th roomiest.
func poolWide(capacity map[string]uint64, replicas int) *csi.GetCapacityResponse {
	var total uint64
	frees := make([]uint64, 0, len(capacity))
	for _, f := range capacity {
		total += f
		frees = append(frees, f)
	}
	sort.Slice(frees, func(i, j int) bool { return frees[i] > frees[j] })
	var maxVol uint64
	if replicas > 0 && len(frees) >= replicas {
		maxVol = frees[replicas-1]
	}
	zap.L().Debug("poolWide: computed", zap.Int("nodes", len(frees)), zap.Int("replicas", replicas),
		zap.Uint64("total_bytes", total), zap.Uint64("max_volume_bytes", maxVol))
	return &csi.GetCapacityResponse{
		AvailableCapacity: int64(total),
		MaximumVolumeSize: wrapperspb.Int64(int64(maxVol)),
	}
}

// csiVolume renders a resource as the CSI Volume a list or get returns.
func csiVolume(r *haifypb.ResourceInfo) *csi.Volume {
	var capacity int64
	if vols := r.GetVolumes(); len(vols) > 0 {
		capacity = int64(vols[0].GetSizeGb()) * giB
	}
	return &csi.Volume{
		VolumeId:           r.GetName(),
		CapacityBytes:      capacity,
		AccessibleTopology: accessibleTopology(r.GetNodes()),
	}
}

// ListVolumes returns the volumes this driver created, in a stable order so the
// CO can page through them with the token it is handed back.
func (s *controllerServer) ListVolumes(ctx context.Context, req *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {
	s.log.Debug("controllerServer.ListVolumes: start", zap.String("starting_token", req.GetStartingToken()),
		zap.Int32("max_entries", req.GetMaxEntries()))
	t0 := time.Now()
	all, err := s.backend.ListResources(ctx)
	s.log.Debug("controllerServer.ListVolumes: listed resources", zap.Int("count", len(all)),
		zap.Duration("took", time.Since(t0)), zap.Error(err))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list resources: %v", err)
	}
	var vols []*haifypb.ResourceInfo
	for _, r := range all {
		if isCSIVolume(r) {
			vols = append(vols, r)
		}
	}
	sort.Slice(vols, func(i, j int) bool { return vols[i].GetName() < vols[j].GetName() })

	start, end, next, err := page(len(vols), req.GetStartingToken(), req.GetMaxEntries())
	if err != nil {
		s.log.Debug("controllerServer.ListVolumes: bad paging", zap.Error(err))
		return nil, err
	}
	s.log.Debug("controllerServer.ListVolumes: page", zap.Int("csi_volumes", len(vols)), zap.Int("start", start),
		zap.Int("end", end), zap.String("next_token", next))
	resp := &csi.ListVolumesResponse{NextToken: next}
	for _, r := range vols[start:end] {
		resp.Entries = append(resp.Entries, &csi.ListVolumesResponse_Entry{Volume: csiVolume(r)})
	}
	return resp, nil
}

// ControllerGetVolume returns one volume.
func (s *controllerServer) ControllerGetVolume(ctx context.Context, req *csi.ControllerGetVolumeRequest) (*csi.ControllerGetVolumeResponse, error) {
	s.log.Debug("controllerServer.ControllerGetVolume: start", zap.String("resource", req.GetVolumeId()))
	if req.GetVolumeId() == "" {
		s.log.Debug("controllerServer.ControllerGetVolume: rejected, no volume id")
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	start := time.Now()
	r, err := s.backend.GetResource(ctx, req.GetVolumeId())
	s.log.Debug("controllerServer.ControllerGetVolume: got resource", zap.String("resource", req.GetVolumeId()),
		zap.Bool("found", err == nil && r != nil), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil || r == nil {
		return nil, status.Errorf(codes.NotFound, "volume %q not found", req.GetVolumeId())
	}
	return &csi.ControllerGetVolumeResponse{
		Volume: csiVolume(r),
		// No PublishedNodeIds: the driver has no ControllerPublish step, so the
		// field has nothing true to say.
		Status: &csi.ControllerGetVolumeResponse_VolumeStatus{},
	}, nil
}

// ListSnapshots returns the snapshots this driver created.
//
// There is no snapshot index to read. A CSI snapshot is an LVM/ZFS snapshot of
// one node's backing volume, so listing them means asking each replica node of
// each volume. The two filters narrow that walk — to one volume, or to one
// snapshot, whose ID already names the node that holds it.
func (s *controllerServer) ListSnapshots(ctx context.Context, req *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {
	s.log.Debug("controllerServer.ListSnapshots: start", zap.String("snapshot_id", req.GetSnapshotId()),
		zap.String("source_resource", req.GetSourceVolumeId()), zap.String("starting_token", req.GetStartingToken()),
		zap.Int32("max_entries", req.GetMaxEntries()), zap.Int("secrets", len(req.GetSecrets())))
	var entries []*csi.Snapshot

	switch {
	case req.GetSnapshotId() != "":
		s.log.Debug("controllerServer.ListSnapshots: filtering by snapshot id", zap.String("snapshot_id", req.GetSnapshotId()))
		resource, node, name, err := parseSnapshotID(req.GetSnapshotId())
		if err != nil {
			s.log.Debug("controllerServer.ListSnapshots: malformed snapshot id, matches nothing", zap.Error(err))
			return &csi.ListSnapshotsResponse{}, nil // an ID nobody could have issued matches nothing
		}
		start := time.Now()
		r, err := s.backend.GetResource(ctx, resource)
		s.log.Debug("controllerServer.ListSnapshots: got source resource", zap.String("resource", resource),
			zap.Bool("found", err == nil && r != nil), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil || r == nil {
			return &csi.ListSnapshotsResponse{}, nil
		}
		for _, snap := range s.snapshotsOn(ctx, r, node) {
			if strings.HasSuffix(snap.GetSnapshotId(), snapshotIDSep+name) {
				entries = append(entries, snap)
			}
		}

	case req.GetSourceVolumeId() != "":
		s.log.Debug("controllerServer.ListSnapshots: filtering by source volume", zap.String("resource", req.GetSourceVolumeId()))
		start := time.Now()
		r, err := s.backend.GetResource(ctx, req.GetSourceVolumeId())
		s.log.Debug("controllerServer.ListSnapshots: got source resource", zap.String("resource", req.GetSourceVolumeId()),
			zap.Bool("found", err == nil && r != nil), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil || r == nil {
			return &csi.ListSnapshotsResponse{}, nil
		}
		entries = s.snapshotsOf(ctx, r)

	default:
		s.log.Debug("controllerServer.ListSnapshots: no filter, walking every CSI volume")
		start := time.Now()
		all, err := s.backend.ListResources(ctx)
		s.log.Debug("controllerServer.ListSnapshots: listed resources", zap.Int("count", len(all)),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			return nil, status.Errorf(codes.Internal, "list resources: %v", err)
		}
		for _, r := range all {
			if isCSIVolume(r) {
				entries = append(entries, s.snapshotsOf(ctx, r)...)
			}
		}
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].GetSnapshotId() < entries[j].GetSnapshotId() })
	start, end, next, err := page(len(entries), req.GetStartingToken(), req.GetMaxEntries())
	if err != nil {
		s.log.Debug("controllerServer.ListSnapshots: bad paging", zap.Error(err))
		return nil, err
	}
	s.log.Debug("controllerServer.ListSnapshots: page", zap.Int("snapshots", len(entries)), zap.Int("start", start),
		zap.Int("end", end), zap.String("next_token", next))
	resp := &csi.ListSnapshotsResponse{NextToken: next}
	for _, snap := range entries[start:end] {
		resp.Entries = append(resp.Entries, &csi.ListSnapshotsResponse_Entry{Snapshot: snap})
	}
	return resp, nil
}

// snapshotsOf lists a volume's CSI snapshots across all of its replica nodes.
func (s *controllerServer) snapshotsOf(ctx context.Context, r *haifypb.ResourceInfo) []*csi.Snapshot {
	var out []*csi.Snapshot
	for _, node := range r.GetNodes() {
		s.log.Debug("controllerServer.snapshotsOf: asking replica node", zap.String("resource", r.GetName()), zap.String("node", node))
		out = append(out, s.snapshotsOn(ctx, r, node)...)
	}
	s.log.Debug("controllerServer.snapshotsOf: done", zap.String("resource", r.GetName()), zap.Int("count", len(out)))
	return out
}

// snapshotsOn lists the CSI snapshots of a volume held on one node. A node that
// cannot be asked is skipped rather than failing the whole listing: one
// unreachable replica should not hide every other snapshot in the cluster.
//
// Only names carrying snapshotNamePrefix are the driver's own. The same LV also
// carries the controller's scheduled snapshots, and reporting those would hand
// the CO objects it never created and must never delete.
func (s *controllerServer) snapshotsOn(ctx context.Context, r *haifypb.ResourceInfo, node string) []*csi.Snapshot {
	volumePath, _, err := snapshotSource(r)
	if err != nil {
		s.log.Debug("controllerServer.snapshotsOn: volume has no snapshot source, skipping", zap.String("resource", r.GetName()),
			zap.Error(err))
		return nil
	}
	start := time.Now()
	snaps, err := s.backend.ListSnapshots(ctx, volumePath, node)
	s.log.Debug("controllerServer.snapshotsOn: listed snapshots", zap.String("volume", volumePath), zap.String("node", node),
		zap.Int("count", len(snaps)), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil
	}
	var out []*csi.Snapshot
	for _, snap := range snaps {
		if !strings.HasPrefix(snap.GetName(), snapshotNamePrefix) {
			s.log.Debug("controllerServer.snapshotsOn: not a CSI snapshot, skipping", zap.String("snapshot", snap.GetName()))
			continue
		}
		s.log.Debug("controllerServer.snapshotsOn: CSI snapshot", zap.String("snapshot", snap.GetName()), zap.String("node", node))
		out = append(out, &csi.Snapshot{
			SnapshotId:     makeSnapshotID(r.GetName(), node, snap.GetName()),
			SourceVolumeId: r.GetName(),
			SizeBytes:      snapshotSizeBytes(r),
			CreationTime:   snapshotCreationTime(snap.GetCreatedAt()),
			ReadyToUse:     true,
		})
	}
	return out
}

// page turns a CSI starting token and max entries into slice bounds over n
// sorted items, and the token for the page after. The token is the index of the
// next item; one the driver did not hand out is Aborted, as the spec asks.
func page(n int, token string, maxEntries int32) (start, end int, next string, err error) {
	if token != "" {
		start, err = strconv.Atoi(token)
		if err != nil || start < 0 || start > n {
			zap.L().Debug("page: invalid starting token", zap.String("token", token), zap.Int("count", n))
			return 0, 0, "", status.Errorf(codes.Aborted, "invalid starting token %q", token)
		}
	}
	if maxEntries < 0 {
		zap.L().Debug("page: negative max entries", zap.Int32("max_entries", maxEntries))
		return 0, 0, "", status.Error(codes.InvalidArgument, "max_entries must not be negative")
	}
	end = n
	if maxEntries > 0 && start+int(maxEntries) < n {
		end = start + int(maxEntries)
		next = strconv.Itoa(end)
	}
	return start, end, next, nil
}

// capacityOfAllPools answers a GetCapacity that names no pool: the free space
// across every pool that reports a figure, and the largest single pool as the
// biggest volume that could fit on one node.
func (s *controllerServer) capacityOfAllPools(ctx context.Context) (*csi.GetCapacityResponse, error) {
	start := time.Now()
	pools, err := s.backend.ListPools(ctx)
	s.log.Debug("controllerServer.capacityOfAllPools: listed pools", zap.Int("count", len(pools)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list pools: %v", err)
	}
	var total, largest uint64
	for _, p := range pools {
		free, known := poolCapacity(p)
		s.log.Debug("controllerServer.capacityOfAllPools: pool", zap.String("pool", p.GetName()), zap.String("node", p.GetNode()),
			zap.Uint64("free_bytes", free), zap.Bool("known", known))
		if known {
			total += free
			if free > largest {
				largest = free
			}
		}
	}
	s.log.Debug("controllerServer.capacityOfAllPools: done", zap.Uint64("total_bytes", total), zap.Uint64("largest_bytes", largest))
	return &csi.GetCapacityResponse{
		AvailableCapacity: int64(total),
		MaximumVolumeSize: wrapperspb.Int64(int64(largest)),
	}, nil
}
