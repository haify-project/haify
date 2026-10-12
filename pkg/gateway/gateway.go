// Package gateway provides DRBD-based storage gateway functionality
// using drbd-reactor for HA/failover with NFS, iSCSI, and NVMe-oF protocols.
package gateway

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"text/template"
	"time"

	v1 "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

const (
	// DrbdReactorConfigDir is the directory for drbd-reactor configuration snippets
	DrbdReactorConfigDir = "/etc/drbd-reactor.d"

	// Default ports
	DefaultISCSIPort = 3260
	DefaultNFSPort   = 2049
	DefaultNVMePort  = 4420

	// Default filesystem types
	DefaultFSType = "ext4"

	// Default export base path
	DefaultExportBasePath = "/srv/gateway-exports"

	// DefaultClusterPrivateMountPath is where the active node mounts a
	// gateway's own state volume, one directory per gateway. It must not lie
	// under the controller's Self-HA mount point (/var/lib/haify): the two
	// promoters move independently, so whichever mounts second covers or is
	// covered by the other — a covered mount can no longer be found by path,
	// its promoter cannot stop, and the resource cannot be demoted.
	DefaultClusterPrivateMountPath = "/var/lib/haify-gateway"

	// legacyClusterPrivateMountPath is where gateways created before that was
	// understood keep their state volume; StartGateway moves them.
	legacyClusterPrivateMountPath = "/var/lib/haify"
)

// ResourceVolumeInfo represents a DRBD volume
type ResourceVolumeInfo struct {
	VolumeID uint32
	Device   string
	SizeGB   uint64
	// BackingVolume is the logical volume name inside the pool, e.g.
	// "<resource>_data" or "<resource>_state1". It is how a gateway tells its
	// own scratch volume apart from the one the operator asked to export —
	// see clusterPrivateAndPayload.
	BackingVolume string
}

// ResourceInfo represents DRBD resource information
type ResourceInfo struct {
	Name     string
	Port     uint32
	Protocol string
	Nodes    []string
	// Hosts are the managed hosts of the diskful replicas: the only machines
	// a gateway's promoter may run on. Empty when the controller cannot say,
	// in which case Nodes is used.
	Hosts      []string
	Role       string
	Volumes    []*ResourceVolumeInfo
	NodeStates map[string]*ResourceNodeState
}

// ResourceNodeState represents node state for a resource
type ResourceNodeState struct {
	Role        string
	DiskState   string
	Replication string
}

// ResourceManager provides access to DRBD resource operations
type ResourceManager interface {
	GetResource(ctx context.Context, name string) (*ResourceInfo, error)
	SetPrimary(ctx context.Context, resource, node string, force bool) error
	// EnsureGatewayVolumes makes sure a resource has at least minVolumes
	// volumes, auto-provisioning the small cluster-private state volume(s) a
	// gateway needs. A no-op when the resource already qualifies or when
	// auto-provisioning is disabled (in which case the caller's own volume
	// check still reports a clear error).
	EnsureGatewayVolumes(ctx context.Context, resource string, minVolumes int) error
}

// DeploymentClient provides deployment operations
type DeploymentClient interface {
	DistributeConfig(ctx context.Context, hosts []string, content, remotePath string) error
	Exec(ctx context.Context, hosts []string, cmd string) error
}

// Manager handles gateway operations
type Manager struct {
	resources  ResourceManager
	deployment DeploymentClient
	logger     *zap.Logger
	hosts      []string
}

// New creates a new gateway manager
func New(resources ResourceManager, deployment DeploymentClient, logger *zap.Logger, hosts []string) *Manager {
	return &Manager{
		resources:  resources,
		deployment: deployment,
		logger:     logger,
		hosts:      append([]string(nil), hosts...),
	}
}

// log returns the manager's logger, or the process-wide one for a manager
// built without one.
func (m *Manager) log() *zap.Logger {
	if m != nil && m.logger != nil {
		return m.logger
	}
	return zap.L()
}

func (m *Manager) SetHosts(hosts []string) {
	m.log().Debug("Manager.SetHosts: managed hosts replaced", zap.Strings("hosts", hosts))
	m.hosts = append([]string(nil), hosts...)
}

func (m *Manager) Hosts() []string {
	return append([]string(nil), m.hosts...)
}

// GatewayInfo represents gateway information
type GatewayInfo struct {
	ID       string
	Name     string
	Type     string
	Resource string
	// State is "stopped" when the reactor config is disabled; empty means
	// reactor-managed (live status comes from drbd-reactorctl).
	State string
}

// ServiceIP represents a service IP with CIDR notation
type ServiceIP struct {
	IP     net.IP
	Prefix int
}

// CreateGatewayRequest is a common interface for all gateway creation requests
type CreateGatewayRequest interface {
	GetResource() string
	GetServiceIP() string
}

// ==================== Common Operations ====================

// HostOutputReader is implemented by deployment clients that can return what a
// command printed on each host. It is kept out of DeploymentClient because
// only reads of the nodes' own state need it.
type HostOutputReader interface {
	// ExecOutput runs cmd on hosts and returns the output of every host on
	// which it succeeded. Hosts that failed or did not answer are left out;
	// an error means the command could not be run at all.
	ExecOutput(ctx context.Context, hosts []string, cmd string) (map[string]string, error)
}

// GatewayTypes are the storage gateway types, as the <type> in a promoter
// config's name, haify-<type>-<resource>.toml. HA and service promoters share the
// directory and the haify- prefix but are not gateways.
//
// Every path that acts on "the gateway of a resource" without knowing its type
// — stop, start, delete, retire — iterates over this list, so a new type is
// added here once rather than in each of their shell loops.
var GatewayTypes = []string{"nfs", "iscsi", "nvmeof", "smb"}

var storageGatewayTypes = func() map[string]bool {
	m := map[string]bool{}
	for _, t := range GatewayTypes {
		m[t] = true
	}
	return m
}()

// promoterConfigPaths lists, space-separated for a shell loop, the promoter
// config path every gateway type would use for resource, each with suffix.
func promoterConfigPaths(resource, suffix string) string {
	paths := make([]string, 0, len(GatewayTypes))
	for _, t := range GatewayTypes {
		paths = append(paths, fmt.Sprintf("%s/haify-%s-%s.toml%s", DrbdReactorConfigDir, t, resource, suffix))
	}
	return strings.Join(paths, " ")
}

// ListGateways lists the storage gateways (NFS, iSCSI, NVMe-oF) configured on
// the managed nodes, read from their /etc/drbd-reactor.d.
//
// The controller's own filesystem is not consulted: a gateway's config lives
// only on its resource's diskful nodes, and the controller need not be one of
// them. A gateway is reactor-managed when any node holds its live .toml, and
// stopped when the nodes hold only the .toml.disabled copy.
func (m *Manager) ListGateways(ctx context.Context) ([]*GatewayInfo, error) {
	log := m.log()
	log.Debug("ListGateways: listing gateway configs on nodes", zap.Strings("hosts", m.hosts))
	if len(m.hosts) == 0 {
		log.Debug("ListGateways: no managed hosts")
		return nil, fmt.Errorf("no managed nodes to read gateway configs from")
	}
	reader, ok := m.deployment.(HostOutputReader)
	if !ok {
		log.Debug("ListGateways: deployment client cannot return output")
		return nil, fmt.Errorf("deployment client cannot read node output")
	}
	start := time.Now()
	outputs, err := reader.ExecOutput(ctx, m.hosts, fmt.Sprintf("ls -1 %s 2>/dev/null; true", DrbdReactorConfigDir))
	if err != nil {
		log.Debug("ListGateways: listing failed", zap.Strings("hosts", m.hosts), zap.Duration("took", time.Since(start)), zap.Error(err))
		return nil, fmt.Errorf("failed to list %s on nodes: %w", DrbdReactorConfigDir, err)
	}
	log.Debug("ListGateways: nodes answered", zap.Int("count", len(outputs)), zap.Duration("took", time.Since(start)))
	if len(outputs) == 0 {
		log.Debug("ListGateways: no node answered")
		return nil, fmt.Errorf("no managed node answered when listing %s", DrbdReactorConfigDir)
	}

	byKey := map[string]*GatewayInfo{}
	for host, out := range outputs {
		for _, name := range strings.Fields(out) {
			gw, live := parseGatewayConfigName(name)
			if gw == nil {
				continue
			}
			log.Debug("ListGateways: gateway config found",
				zap.String("host", host), zap.String("file", name), zap.String("type", gw.Type),
				zap.String("resource", gw.Resource), zap.Bool("live", live))
			key := gw.Type + "/" + gw.Resource
			if prev, seen := byKey[key]; seen {
				if live {
					prev.State = ""
				}
				continue
			}
			byKey[key] = gw
		}
	}

	gateways := make([]*GatewayInfo, 0, len(byKey))
	for _, gw := range byKey {
		gateways = append(gateways, gw)
	}
	sort.Slice(gateways, func(i, j int) bool {
		if gateways[i].Resource != gateways[j].Resource {
			return gateways[i].Resource < gateways[j].Resource
		}
		return gateways[i].Type < gateways[j].Type
	})
	log.Debug("ListGateways: done", zap.Int("count", len(gateways)))
	return gateways, nil
}

// parseGatewayConfigName returns the gateway a file in /etc/drbd-reactor.d
// configures, or nil when it is not a storage gateway config. live is false
// for a stopped gateway's .toml.disabled copy.
// Format: haify-<type>-<resource>.toml[.disabled]
func parseGatewayConfigName(name string) (gw *GatewayInfo, live bool) {
	live = true
	if strings.HasSuffix(name, ".toml.disabled") {
		live = false
		name = strings.TrimSuffix(name, ".disabled")
	}
	if !strings.HasPrefix(name, "haify-") || !strings.HasSuffix(name, ".toml") {
		return nil, false
	}
	parts := strings.SplitN(strings.TrimSuffix(strings.TrimPrefix(name, "haify-"), ".toml"), "-", 2)
	if len(parts) != 2 || parts[1] == "" || !storageGatewayTypes[parts[0]] {
		return nil, false
	}
	state := ""
	if !live {
		state = "stopped"
	}
	return &GatewayInfo{
		ID:       parts[1],
		Name:     parts[1],
		Type:     parts[0],
		Resource: parts[1],
		State:    state,
	}, live
}

// GetGateway retrieves gateway information
func (m *Manager) GetGateway(ctx context.Context, id string) (*GatewayInfo, error) {
	log := m.log()
	log.Debug("GetGateway: looking up gateway", zap.String("id", id))
	gateways, err := m.ListGateways(ctx)
	if err != nil {
		log.Debug("GetGateway: listing failed", zap.String("id", id), zap.Error(err))
		return nil, err
	}

	for _, gw := range gateways {
		if gw.ID == id {
			log.Debug("GetGateway: found", zap.String("id", id), zap.String("type", gw.Type), zap.String("state", gw.State))
			return gw, nil
		}
	}

	log.Debug("GetGateway: not found", zap.String("id", id), zap.Int("count", len(gateways)))
	return nil, fmt.Errorf("gateway not found: %s", id)
}

// ==================== Helpers ====================

// parseServiceIP parses a service IP with CIDR notation
func parseServiceIP(serviceIP string) (*ServiceIP, error) {
	ip, ipNet, err := net.ParseCIDR(serviceIP)
	if err != nil {
		zap.L().Debug("parseServiceIP: not CIDR notation", zap.String("service_ip", serviceIP), zap.Error(err))
		return nil, fmt.Errorf("failed to parse service IP %s: %w", serviceIP, err)
	}

	prefix, _ := ipNet.Mask.Size()
	zap.L().Debug("parseServiceIP: parsed", zap.String("ip", ip.String()), zap.Int("prefix", prefix))

	return &ServiceIP{
		IP:     ip,
		Prefix: prefix,
	}, nil
}

// extractNodeName extracts node name from endpoint (e.g., "node1:50051" -> "node1")
func extractNodeName(endpoint string) string {
	parts := strings.Split(endpoint, ":")
	if len(parts) > 0 {
		return parts[0]
	}
	return endpoint
}

// executeTemplate executes a template with the given data
func executeTemplate(tmplStr string, data interface{}) (string, error) {
	t, err := template.New("gateway").Parse(tmplStr)
	if err != nil {
		zap.L().Debug("executeTemplate: parse failed", zap.Error(err))
		return "", err
	}

	var result strings.Builder
	if err := t.Execute(&result, data); err != nil {
		zap.L().Debug("executeTemplate: execute failed", zap.Error(err))
		return "", err
	}
	zap.L().Debug("executeTemplate: rendered", zap.Int("size_bytes", result.Len()))

	return result.String(), nil
}

// ==================== gRPC Method Implementations ====================

// CreateNFSGateway creates an NFS gateway
func (m *Manager) CreateNFSGateway(ctx context.Context, req *v1.CreateNFSGatewayRequest) (*v1.CreateNFSGatewayResponse, error) {
	m.log().Debug("Manager.CreateNFSGateway: refused, NFSManager creates NFS gateways", zap.String("resource", req.GetResource()))
	return &v1.CreateNFSGatewayResponse{
		Success: false,
		Message: "Use gateway/nfs package directly",
	}, nil
}

// CreateISCSIGateway creates an iSCSI gateway
func (m *Manager) CreateISCSIGateway(ctx context.Context, req *v1.CreateISCSIGatewayRequest) (*v1.CreateISCSIGatewayResponse, error) {
	m.log().Debug("Manager.CreateISCSIGateway: refused, iSCSIManager creates iSCSI gateways", zap.String("resource", req.GetResource()))
	return &v1.CreateISCSIGatewayResponse{
		Success: false,
		Message: "Use gateway/iscsi package directly",
	}, nil
}

// CreateNVMeGateway creates an NVMe-oF gateway
func (m *Manager) CreateNVMeGateway(ctx context.Context, req *v1.CreateNVMeGatewayRequest) (*v1.CreateNVMeGatewayResponse, error) {
	m.log().Debug("Manager.CreateNVMeGateway: refused, NVMeManager creates NVMe-oF gateways", zap.String("resource", req.GetResource()))
	return &v1.CreateNVMeGatewayResponse{
		Success: false,
		Message: "Use gateway/nvmeof package directly",
	}, nil
}
