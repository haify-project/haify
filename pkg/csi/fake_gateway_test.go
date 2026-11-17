package csi

import (
	"context"
	"fmt"
	"net"
	"sync"

	haifypb "github.com/haify-project/haify/api/proto/v1"
)

// The fake's NFS gateways. They behave as the controller's do where the
// driver depends on it: a gateway records its service address in its options,
// reports its export directory as its path, adds the state volume after the
// data volume, and a lookup of one that does not exist fails with a plain
// error, not a gRPC status.

func (f *fakeBackend) CreateNFSGateway(_ context.Context, req *haifypb.CreateNFSGatewayRequest) (*haifypb.CreateNFSGatewayResponse, error) {
	f.gatewayCreates = append(f.gatewayCreates, req)
	if f.gatewayErr != nil {
		return nil, f.gatewayErr
	}
	r, ok := f.resources[req.GetResource()]
	if !ok {
		return nil, fmt.Errorf("resource %q not found", req.GetResource())
	}
	if len(r.Volumes) < 2 {
		r.Volumes = append(r.Volumes, &haifypb.VolumeInfo{VolumeId: 1, Device: "/dev/drbd101", SizeGb: 1,
			BackingVolume: req.GetResource() + "_state1"})
	}
	host, _, err := net.ParseCIDR(req.GetServiceIp())
	if err != nil {
		return nil, fmt.Errorf("invalid service IP: %w", err)
	}
	if f.gateways == nil {
		f.gateways = map[string]*haifypb.GatewayInfo{}
	}
	f.gateways[req.GetResource()] = &haifypb.GatewayInfo{
		Id: req.GetResource(), Name: req.GetResource() + "-nfs", Type: "nfs", Resource: req.GetResource(),
		Path:    "/srv/gateway-exports/" + req.GetResource(),
		Options: map[string]string{"service_ip": req.GetServiceIp(), "service_host": host.String()},
	}
	return &haifypb.CreateNFSGatewayResponse{Success: true}, nil
}

func (f *fakeBackend) GetGateway(_ context.Context, resource string) (*haifypb.GatewayInfo, error) {
	if g, ok := f.gateways[resource]; ok {
		return g, nil
	}
	return nil, fmt.Errorf("gateway not found: %s", resource)
}

func (f *fakeBackend) ListGateways(context.Context) ([]*haifypb.GatewayInfo, error) {
	if f.listGatewaysErr != nil {
		return nil, f.listGatewaysErr
	}
	out := make([]*haifypb.GatewayInfo, 0, len(f.gateways))
	for _, g := range f.gateways {
		out = append(out, g)
	}
	return out, nil
}

func (f *fakeBackend) DeleteGateway(_ context.Context, resource string) error {
	f.gatewayDeletes = append(f.gatewayDeletes, resource)
	delete(f.gateways, resource)
	return nil
}

var _ gatewayBackend = (*fakeBackend)(nil)

// lockedBackend serialises the calls CreateVolume and DeleteVolume make, so
// that a test can run them from several goroutines against one fakeBackend,
// whose maps are not safe for that.
type lockedBackend struct {
	mu sync.Mutex
	*fakeBackend
}

func (l *lockedBackend) GetResource(ctx context.Context, name string) (*haifypb.ResourceInfo, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.fakeBackend.GetResource(ctx, name)
}

func (l *lockedBackend) ListResources(ctx context.Context) ([]*haifypb.ResourceInfo, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.fakeBackend.ListResources(ctx)
}

func (l *lockedBackend) ListNodes(ctx context.Context) ([]*haifypb.NodeInfo, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.fakeBackend.ListNodes(ctx)
}

func (l *lockedBackend) ListPools(ctx context.Context) ([]*haifypb.PoolInfo, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.fakeBackend.ListPools(ctx)
}

func (l *lockedBackend) CreateResourceRequest(ctx context.Context, req *haifypb.CreateResourceRequest) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.fakeBackend.CreateResourceRequest(ctx, req)
}

func (l *lockedBackend) DeleteResource(ctx context.Context, name string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.fakeBackend.DeleteResource(ctx, name)
}

func (l *lockedBackend) CreateNFSGateway(ctx context.Context, req *haifypb.CreateNFSGatewayRequest) (*haifypb.CreateNFSGatewayResponse, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.fakeBackend.CreateNFSGateway(ctx, req)
}

func (l *lockedBackend) GetGateway(ctx context.Context, resource string) (*haifypb.GatewayInfo, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.fakeBackend.GetGateway(ctx, resource)
}

func (l *lockedBackend) ListGateways(ctx context.Context) ([]*haifypb.GatewayInfo, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.fakeBackend.ListGateways(ctx)
}

func (l *lockedBackend) DeleteGateway(ctx context.Context, resource string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.fakeBackend.DeleteGateway(ctx, resource)
}
