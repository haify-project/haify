package csi

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"

	"github.com/container-storage-interface/spec/lib/go/csi"
	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

const (
	// DriverName is the CSI driver name (reverse-DNS, used in CSIDriver object).
	DriverName = "haify.csi.liliang-cn.com"
	// DriverVersion is reported via Identity.GetPluginInfo.
	DriverVersion = "0.1.0"
	// TopologyKeyNode segments a volume to the node(s) holding a replica.
	TopologyKeyNode = DriverName + "/node"
)

// HaifyBackend is the subset of the haify-controller gRPC client the CSI driver
// uses. *client.HaifyClient satisfies it directly; tests use a fake.
type HaifyBackend interface {
	CreateResourceWithPoolAndType(ctx context.Context, name string, port uint32, nodes []string, protocol string, sizeGB uint32, pool, storageType string, drbdOptions map[string]string) error
	GetResource(ctx context.Context, name string) (*haifypb.ResourceInfo, error)
	// ListResources returns every resource the controller manages. ListVolumes
	// and ListSnapshots filter it down to the ones this driver created.
	ListResources(ctx context.Context) ([]*haifypb.ResourceInfo, error)
	DeleteResource(ctx context.Context, name string) error
	ListNodes(ctx context.Context) ([]*haifypb.NodeInfo, error)
	// ListPools returns every storage pool known to the controller (one entry
	// per node hosting the pool). CreateVolume uses it to keep replica
	// placement pool-aware: a volume only lands on nodes that actually have the
	// requested backing pool.
	ListPools(ctx context.Context) ([]*haifypb.PoolInfo, error)
	RegisterNode(ctx context.Context, name, address string) (*haifypb.NodeInfo, error)
	SetPrimary(ctx context.Context, resource, node string, force bool) error
	// PromoteForNode performs a quorum-guarded promote for hard-failover: the
	// controller tries a normal promote and only force-promotes if this node
	// holds DRBD quorum, refusing otherwise to avoid split-brain.
	PromoteForNode(ctx context.Context, resource, node string) error
	SetSecondary(ctx context.Context, resource, node string) error
	// AttachDisklessClient adds node to resource as a diskless data client so a
	// Pod on a node with no local replica can still mount the volume (I/O over
	// the DRBD network). Idempotent.
	AttachDisklessClient(ctx context.Context, resource, node string) error
	// DetachDisklessClient removes a diskless client added via
	// AttachDisklessClient. Idempotent.
	DetachDisklessClient(ctx context.Context, resource, node string) error
	// CreateSnapshot takes a point-in-time snapshot of the backing volume
	// (`<pool>/<lv>`) on one node that holds a diskful replica.
	CreateSnapshot(ctx context.Context, volume, snapshotName, node string) error
	// DeleteSnapshot removes a snapshot created by CreateSnapshot. Idempotent.
	DeleteSnapshot(ctx context.Context, volume, snapshotName, node string) error
	// ListSnapshots returns the snapshots of a backing volume on one node. The
	// CSI driver uses it to make CreateSnapshot idempotent and, walking every
	// volume's replica nodes, to answer the ListSnapshots RPC.
	ListSnapshots(ctx context.Context, volume, node string) ([]*haifypb.SnapshotInfo, error)
	// PopulateVolumeFromSnapshot copies a snapshot into an already-created,
	// still-empty resource, on a node that holds the snapshot and one of the
	// resource's replicas. It backs restore-from-snapshot and volume cloning;
	// the controller finds the snapshot's device on LVM and ZFS pools alike.
	PopulateVolumeFromSnapshot(ctx context.Context, resource string, volumeID uint32, src *haifypb.PopulateSnapshotSource, node string) (uint64, error)
	// ResizeVolume expands the backing DRBD+LVM/ZFS volume to newSizeGB on
	// all replica nodes. Called by ControllerExpandVolume.
	ResizeVolume(ctx context.Context, resource string, volumeID uint32, sizeGB uint32) error
}

// Driver wires the CSI services onto a gRPC server over a unix socket.
type Driver struct {
	endpoint string
	srv      *grpc.Server
	log      *zap.Logger

	identity   csi.IdentityServer
	controller csi.ControllerServer
	node       csi.NodeServer
}

// NewDriver builds a Driver. Pass nil for services that this binary doesn't
// serve (the controller binary leaves node nil and vice-versa).
func NewDriver(endpoint string, log *zap.Logger, id csi.IdentityServer, ctrl csi.ControllerServer, node csi.NodeServer) *Driver {
	if log == nil {
		log = zap.NewNop()
	}
	return &Driver{endpoint: endpoint, log: log, identity: id, controller: ctrl, node: node}
}

// Run listens on the unix socket and serves until the context is cancelled.
func (d *Driver) Run(ctx context.Context) error {
	proto, addr, err := parseEndpoint(d.endpoint)
	if err != nil {
		return err
	}
	if proto == "unix" {
		_ = os.Remove(addr) // clear a stale socket
	}
	lis, err := net.Listen(proto, addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", d.endpoint, err)
	}
	d.srv = grpc.NewServer()
	if d.identity != nil {
		csi.RegisterIdentityServer(d.srv, d.identity)
	}
	if d.controller != nil {
		csi.RegisterControllerServer(d.srv, d.controller)
	}
	if d.node != nil {
		csi.RegisterNodeServer(d.srv, d.node)
	}
	stopped := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			d.srv.GracefulStop()
		case <-stopped:
		}
	}()
	d.log.Info("CSI driver serving", zap.String("endpoint", d.endpoint))
	err = d.srv.Serve(lis)
	close(stopped)
	if proto == "unix" {
		_ = os.Remove(addr)
	}
	return err
}

// parseEndpoint splits "unix:///path" or "tcp://host:port" into proto + addr.
func parseEndpoint(ep string) (string, string, error) {
	u, err := url.Parse(ep)
	if err != nil {
		return "", "", fmt.Errorf("invalid endpoint %q: %w", ep, err)
	}
	switch u.Scheme {
	case "unix":
		if u.Host != "" {
			return "", "", fmt.Errorf("invalid unix endpoint %q (host must be empty)", ep)
		}
		return "unix", u.Path, nil
	case "tcp":
		return "tcp", u.Host, nil
	default:
		return "", "", fmt.Errorf("invalid endpoint %q (want unix:// or tcp://)", ep)
	}
}
