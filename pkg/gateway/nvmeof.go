// Package gateway provides NVMe-oF gateway functionality
package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	v1 "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// NVMeManager handles NVMe-oF gateway operations
type NVMeManager struct {
	*Manager
}

// NewNVMeManager creates a new NVMe-oF gateway manager
func NewNVMeManager(m *Manager) *NVMeManager {
	return &NVMeManager{Manager: m}
}

// CreateNVMeGateway creates an NVMe-oF gateway with drbd-reactor configuration
func (n *NVMeManager) CreateNVMeGateway(ctx context.Context, req *v1.CreateNVMeGatewayRequest) (*v1.CreateNVMeGatewayResponse, error) {
	log := n.log()
	begin := time.Now()
	log.Debug("NVMeManager.CreateNVMeGateway: start",
		zap.String("resource", req.Resource), zap.String("nqn", req.Nqn),
		zap.String("service_ip", req.ServiceIp), zap.String("transport", req.TransportType))
	n.logger.Info("Creating NVMe-oF gateway",
		zap.String("resource", req.Resource),
		zap.String("nqn", req.Nqn),
		zap.String("service_ip", req.ServiceIp))

	// Reject a malformed NQN or transport type before anything with a side
	// effect runs — see validate.go for why this cannot wait until the OCF
	// agents parse them.
	if err := validateNQN(req.Nqn); err != nil {
		log.Debug("NVMeManager.CreateNVMeGateway: invalid NQN", zap.String("nqn", req.Nqn), zap.Error(err))
		return &v1.CreateNVMeGatewayResponse{
			Success: false,
			Message: err.Error(),
		}, invalidArgument(err)
	}
	// An unset transport is not a caller mistake: generateNVMeGatewayConfig
	// defaults it to tcp. Only a value the caller actually chose is checked.
	if req.TransportType != "" {
		if err := parseTransportType(req.TransportType); err != nil {
			log.Debug("NVMeManager.CreateNVMeGateway: invalid transport", zap.String("transport", req.TransportType), zap.Error(err))
			return &v1.CreateNVMeGatewayResponse{
				Success: false,
				Message: err.Error(),
			}, invalidArgument(err)
		}
	}

	// Parse service IP
	serviceIP, err := parseServiceIP(req.ServiceIp)
	if err != nil {
		log.Debug("NVMeManager.CreateNVMeGateway: invalid service IP", zap.String("service_ip", req.ServiceIp), zap.Error(err))
		return &v1.CreateNVMeGatewayResponse{
			Success: false,
			Message: fmt.Sprintf("invalid service IP: %v", err),
		}, invalidArgument(fmt.Errorf("invalid service IP: %w", err))
	}

	// Fail early with a clear message if the OCF agents an NVMe-oF gateway
	// needs are not installed on the resource's nodes, instead of writing a
	// promoter config that silently fails to start. Their real runtime
	// dependency is the kernel modules for the transport, which
	// ensureNVMeModules loads (and persists) below.
	if res, rerr := n.resources.GetResource(ctx, req.Resource); rerr == nil && res != nil {
		if err := n.checkGatewayPrereqs(ctx, gatewayNodes(res), nvmePrereqs()); err != nil {
			log.Debug("NVMeManager.CreateNVMeGateway: prerequisites missing", zap.String("resource", req.Resource), zap.Error(err))
			return &v1.CreateNVMeGatewayResponse{Success: false, Message: err.Error()}, err
		}
		if err := n.ensureNVMeModules(ctx, gatewayNodes(res), req.TransportType); err != nil {
			log.Debug("NVMeManager.CreateNVMeGateway: kernel modules unavailable", zap.String("resource", req.Resource), zap.Error(err))
			return &v1.CreateNVMeGatewayResponse{Success: false, Message: err.Error()}, err
		}
	} else {
		log.Debug("NVMeManager.CreateNVMeGateway: resource lookup failed, prerequisite and module checks skipped",
			zap.String("resource", req.Resource), zap.Error(rerr))
	}

	// Auto-provision the cluster-private state volume when the resource is one
	// short, so a single-volume resource can be exported without a manual
	// add-volume step first.
	start := time.Now()
	if err := n.resources.EnsureGatewayVolumes(ctx, req.Resource, 2); err != nil {
		log.Debug("NVMeManager.CreateNVMeGateway: state volume provisioning failed",
			zap.String("resource", req.Resource), zap.Duration("took", time.Since(start)), zap.Error(err))
		return &v1.CreateNVMeGatewayResponse{
			Success: false,
			Message: fmt.Sprintf("failed to provision gateway state volume: %v", err),
		}, err
	}
	log.Debug("NVMeManager.CreateNVMeGateway: gateway volumes ensured",
		zap.String("resource", req.Resource), zap.Duration("took", time.Since(start)))

	// Get volume info from resource - NVMe-oF requires at least 2 volumes
	// Volume 0: cluster-private, Volume 1+: namespaces exposed to initiators
	resInfo, err := n.resources.GetResource(ctx, req.Resource)
	if err != nil {
		log.Debug("NVMeManager.CreateNVMeGateway: resource lookup failed", zap.String("resource", req.Resource), zap.Error(err))
		return &v1.CreateNVMeGatewayResponse{
			Success: false,
			Message: fmt.Sprintf("failed to get resource info: %v", err),
		}, err
	}

	if len(resInfo.Volumes) < 2 {
		log.Debug("NVMeManager.CreateNVMeGateway: too few volumes", zap.String("resource", req.Resource), zap.Int("count", len(resInfo.Volumes)))
		return &v1.CreateNVMeGatewayResponse{
			Success: false,
			Message: fmt.Sprintf("NVMe-oF gateway requires at least 2 volumes (got %d): volume 0 for cluster-private, volume 1+ for namespaces", len(resInfo.Volumes)),
		}, fmt.Errorf("resource %s has insufficient volumes for NVMe-oF gateway (need >= 2, got %d)", req.Resource, len(resInfo.Volumes))
	}

	// Get DRBD device for the resource
	drbdDevice, err := n.getDRBDDevice(ctx, req.Resource)
	if err != nil {
		n.logger.Warn("Failed to get DRBD device, using fallback",
			zap.String("resource", req.Resource),
			zap.Error(err))
		drbdDevice = "/dev/drbd0"
	}

	n.logger.Info("Using DRBD device for NVMe-oF gateway",
		zap.String("resource", req.Resource),
		zap.String("device", drbdDevice),
		zap.Int("volume_count", len(resInfo.Volumes)),
		zap.Int("namespace_count", len(resInfo.Volumes)-1))

	// Generate drbd-reactor configuration
	// Promote on one of the resource's own nodes and make sure the
	// cluster-private volume carries a filesystem BEFORE reactor takes
	// over: its Filesystem agent mounts but never formats.
	// Only the cluster-private volume is formatted. The exported volume must
	// stay raw — it is a block device handed to an initiator, which puts its
	// own filesystem on it. Passing volumes[0] here formatted the operator's
	// data volume as gateway scratch; see clusterPrivateAndPayload.
	clusterPrivateDev, _ := clusterPrivateAndPayload(resInfo.Volumes, drbdDevice)
	if err := n.ensureGatewayPrerequisites(ctx, req.Resource, resInfo.Nodes, clusterPrivateDev); err != nil {
		log.Debug("NVMeManager.CreateNVMeGateway: preparing the resource failed", zap.String("resource", req.Resource), zap.Error(err))
		return &v1.CreateNVMeGatewayResponse{
			Success: false,
			Message: err.Error(),
		}, err
	}

	config, err := n.generateNVMeGatewayConfig(req, serviceIP, drbdDevice, resInfo.Volumes)
	if err != nil {
		log.Debug("NVMeManager.CreateNVMeGateway: config generation failed", zap.String("resource", req.Resource), zap.Error(err))
		return &v1.CreateNVMeGatewayResponse{
			Success: false,
			Message: fmt.Sprintf("failed to generate config: %v", err),
		}, err
	}

	// Write configuration to all nodes
	pluginID := fmt.Sprintf("haify-nvmeof-%s", req.Resource)
	if err := n.writeReactorConfig(ctx, req.Resource, pluginID, config); err != nil {
		log.Debug("NVMeManager.CreateNVMeGateway: writing config failed", zap.String("resource", req.Resource), zap.Error(err))
		return &v1.CreateNVMeGatewayResponse{
			Success: false,
			Message: fmt.Sprintf("failed to write config: %v", err),
		}, err
	}

	configPath := filepath.Join(DrbdReactorConfigDir, fmt.Sprintf("%s.toml", pluginID))
	log.Debug("NVMeManager.CreateNVMeGateway: done",
		zap.String("resource", req.Resource), zap.String("path", configPath), zap.Duration("took", time.Since(begin)))

	return &v1.CreateNVMeGatewayResponse{
		Success:    true,
		Message:    "NVMe-oF gateway configuration created successfully",
		ConfigPath: configPath,
	}, nil
}

// generateNVMeGatewayConfig generates drbd-reactor TOML configuration for NVMe-oF gateway
func (n *NVMeManager) generateNVMeGatewayConfig(req *v1.CreateNVMeGatewayRequest, serviceIP *ServiceIP, drbdDevice string, volumes []*ResourceVolumeInfo) (string, error) {
	log := n.log()
	// Template for NVMe-oF gateway - matches linstor-gateway pattern
	tmpl := `# Haify NVMe-oF Gateway Configuration
# Generated by Haify Controller
# Resource: {{ .Resource }}
# NQN: {{ .NQN }}
# Service IP: {{ .ServiceIP }}

[[promoter]]

  [promoter.metadata]
    linstor-gateway-schema-version = 1

  [promoter.resources]

    [promoter.resources.{{ .Resource }}]
      on-drbd-demote-failure = "reboot-immediate"
      runner = "systemd"
      stop-services-on-exit = true
      target-as = "Requires"

      start = [
        "ocf:heartbeat:Filesystem fs_cluster_private device={{ .DRBDDevice }} directory={{ .ClusterPrivatePath }} fstype={{ .FSType }} run_fsck=no force_unmount=false",
        "ocf:heartbeat:IPaddr2 service_ip ip={{ .IPAddress }} cidr_netmask={{ .Prefix }}",
        "ocf:heartbeat:nvmet-subsystem subsys nqn={{ .NQN }} serial={{ .Serial }}",
{{ range $idx, $ns := .Namespaces }}
        "ocf:heartbeat:nvmet-namespace ns_{{ $ns.Number }} nqn={{ $.NQN }} namespace_id={{ $ns.Number }} backing_path={{ $ns.Device }} uuid={{ $ns.UUID }} nguid={{ $ns.NGUID }}",
{{ end }}
        "ocf:heartbeat:nvmet-port port nqns={{ .NQN }} addr={{ .IPAddress }} type={{ .TransportType }}",
      ]
`
	// portblock/portunblock removed (same failover bug as iSCSI/NFS: the unblock
	// step did not reliably clear the block's DROP on the new active node).
	// DRBD demotion + subsystem teardown + VIP move provide the fencing.

	ipAddr := serviceIP.IP.String()
	prefix := serviceIP.Prefix

	transportType := req.TransportType
	if transportType == "" {
		log.Debug("NVMeManager.generateNVMeGatewayConfig: no transport given, using tcp")
		transportType = "tcp"
	}

	// Generate serial from NQN using SHA256 (matches linstor-gateway)
	digest := sha256.Sum256([]byte(req.Nqn))
	serial := hex.EncodeToString(digest[:8])

	// Prepare subsystem ID (extract subsystem name from NQN)
	// Format: nqn.2024-01.com.example:subsystem.name -> subsystem.name
	subsystemID := req.Nqn
	if parts := strings.SplitN(req.Nqn, ":", 2); len(parts) == 2 {
		subsystemID = parts[1]
	}

	// Prepare namespace data - Volume 0 is cluster-private, volumes 1+ are exposed.
	// NVMe namespaces start at 1.
	type Namespace struct {
		Number int
		Device string
		UUID   string
		NGUID  string
	}

	// See clusterPrivateAndPayload: the gateway's scratch volume is identified
	// by name, so the namespace is the operator's volume rather than whichever
	// one happened to be numbered 1.
	clusterPrivateDev, payload := clusterPrivateAndPayload(volumes, drbdDevice)

	// NSIDs are 1-based: zero is not a valid namespace identifier, so they
	// cannot simply mirror the DRBD volume number now that the payload is
	// usually volume 0.
	namespaces := make([]Namespace, 0, len(payload))
	for i, vol := range payload {
		log.Debug("NVMeManager.generateNVMeGatewayConfig: namespace", zap.Int("nsid", i+1), zap.String("device", vol.Device))
		namespaces = append(namespaces, Namespace{
			Number: i + 1,
			Device: vol.Device,
			UUID:   generateUUID(),
			NGUID:  generateUUID(),
		})
	}

	clusterPrivatePath := filepath.Join(DefaultClusterPrivateMountPath, req.Resource)
	log.Debug("NVMeManager.generateNVMeGatewayConfig: rendering",
		zap.String("resource", req.Resource), zap.String("nqn", req.Nqn), zap.String("ip", ipAddr),
		zap.Int("prefix", prefix), zap.String("transport", transportType), zap.String("serial", serial),
		zap.String("cluster_private", clusterPrivateDev), zap.Int("namespaces", len(namespaces)))

	data := struct {
		Resource           string
		NQN                string
		SubsystemID        string
		ServiceIP          string
		IPAddress          string
		Prefix             int
		FSType             string
		ClusterPrivatePath string
		NVMePort           int
		Serial             string
		TransportType      string
		Namespaces         []Namespace
		DRBDDevice         string
	}{
		Resource:           req.Resource,
		NQN:                req.Nqn,
		SubsystemID:        subsystemID,
		ServiceIP:          req.ServiceIp,
		IPAddress:          ipAddr,
		Prefix:             prefix,
		FSType:             DefaultFSType,
		DRBDDevice:         clusterPrivateDev,
		ClusterPrivatePath: clusterPrivatePath,
		NVMePort:           DefaultNVMePort,
		Serial:             serial,
		TransportType:      transportType,
		Namespaces:         namespaces,
	}

	return executeTemplate(tmpl, data)
}

// GetNVMeGatewayStatus gets the status of an NVMe-oF gateway
func (n *NVMeManager) GetNVMeGatewayStatus(ctx context.Context, resource string) (map[string]interface{}, error) {
	log := n.log()
	log.Debug("NVMeManager.GetNVMeGatewayStatus: start", zap.String("resource", resource))
	status := map[string]interface{}{
		"type":     "nvmeof",
		"resource": resource,
		"status":   "unknown",
	}

	// Check if the gateway config exists
	configPath := filepath.Join(DrbdReactorConfigDir, fmt.Sprintf("haify-nvmeof-%s.toml", resource))
	if _, err := os.Stat(configPath); err != nil {
		log.Debug("NVMeManager.GetNVMeGatewayStatus: no local config", zap.String("path", configPath), zap.Error(err))
		status["status"] = "not_configured"
		return status, nil
	}

	status["status"] = "configured"
	status["config_path"] = configPath

	// Check if resource is primary
	if resInfo, err := n.resources.GetResource(ctx, resource); err == nil {
		status["role"] = resInfo.Role
		status["nodes"] = resInfo.Nodes
		status["volumes"] = len(resInfo.Volumes)
		log.Debug("NVMeManager.GetNVMeGatewayStatus: configured",
			zap.String("resource", resource), zap.String("role", resInfo.Role), zap.Strings("nodes", resInfo.Nodes))
	} else {
		log.Debug("NVMeManager.GetNVMeGatewayStatus: resource lookup failed", zap.String("resource", resource), zap.Error(err))
	}

	return status, nil
}

// DeleteNVMeGateway deletes an NVMe-oF gateway
func (n *NVMeManager) DeleteNVMeGateway(ctx context.Context, resource string) error {
	log := n.log()
	log.Debug("NVMeManager.DeleteNVMeGateway: start", zap.String("resource", resource), zap.Strings("hosts", n.hosts))
	n.logger.Info("Deleting NVMe-oF gateway", zap.String("resource", resource))

	configFile := fmt.Sprintf("haify-nvmeof-%s.toml", resource)
	configPath := filepath.Join(DrbdReactorConfigDir, configFile)

	// Remove config from all nodes
	for _, host := range n.hosts {
		rmCmd := fmt.Sprintf("sudo rm -f %s", configPath)
		start := time.Now()
		err := n.deployment.Exec(ctx, []string{host}, rmCmd)
		log.Debug("NVMeManager.DeleteNVMeGateway: removed config",
			zap.String("host", host), zap.String("path", configPath), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			n.logger.Warn("Failed to delete config",
				zap.String("node", host),
				zap.Error(err))
		}
	}

	// Reload drbd-reactor
	if err := n.reloadDrbdReactor(ctx); err != nil {
		log.Debug("NVMeManager.DeleteNVMeGateway: reload failed", zap.String("resource", resource), zap.Error(err))
		return err
	}

	n.logger.Info("NVMe-oF gateway deleted", zap.String("resource", resource))
	return nil
}

// ==================== Helper Functions ====================

// nvmeTransportModule is the kernel module that implements an NVMe-oF
// transport on the target side. The nvmet-port agent only writes the port's
// trtype into configfs; the kernel rejects it unless this module is loaded.
func nvmeTransportModule(transport string) (string, error) {
	switch transport {
	case "", "tcp":
		return "nvmet-tcp", nil
	case "rdma":
		return "nvmet-rdma", nil
	}
	zap.L().Debug("nvmeTransportModule: unknown transport", zap.String("transport", transport))
	return "", parseTransportType(transport)
}

// ensureNVMeModules loads nvmet and the module for the gateway's transport on
// the given nodes and persists both in /etc/modules-load.d/nvmet.conf so they
// survive a reboot, then verifies the nvmet configfs tree exists. The nvmet-*
// OCF agents operate exclusively through /sys/kernel/config/nvmet; without the
// modules a promoted gateway silently fails to export its namespace. Modules
// are added to the file, never replace it, so a node serving a TCP and an RDMA
// gateway keeps both. RDMA additionally needs an RDMA-capable device (or
// soft-RoCE), without which the port cannot be enabled.
func (n *NVMeManager) ensureNVMeModules(ctx context.Context, nodes []string, transport string) error {
	log := n.log()
	if len(nodes) == 0 {
		log.Debug("NVMeManager.ensureNVMeModules: no nodes")
		return nil
	}
	module, err := nvmeTransportModule(transport)
	if err != nil {
		log.Debug("NVMeManager.ensureNVMeModules: no module for transport", zap.String("transport", transport), zap.Error(err))
		return err
	}
	rdmaCheck := ""
	if module == "nvmet-rdma" {
		log.Debug("NVMeManager.ensureNVMeModules: RDMA transport, checking for an RDMA device")
		rdmaCheck = `[ -n "$(ls /sys/class/infiniband 2>/dev/null)" ] || { echo "no RDMA device under /sys/class/infiniband"; exit 1; }` + "\n"
	}
	log.Debug("NVMeManager.ensureNVMeModules: loading modules", zap.Strings("nodes", nodes), zap.String("module", module))
	script := fmt.Sprintf(`set -e
%[2]smodprobe nvmet
modprobe %[1]s
conf=/etc/modules-load.d/nvmet.conf
touch "$conf"
for m in nvmet %[1]s; do grep -qx "$m" "$conf" || echo "$m" >> "$conf"; done
test -d /sys/kernel/config/nvmet`, module, rdmaCheck)
	if err := n.runScript(ctx, nodes, script); err != nil {
		log.Debug("NVMeManager.ensureNVMeModules: loading failed", zap.Strings("nodes", nodes), zap.Error(err))
		return fmt.Errorf("failed to load nvmet kernel modules (nvmet, %s) on gateway nodes; "+
			"ensure the nvme-target kernel modules are available: %w", module, err)
	}
	return nil
}

// generateNQN generates an NVMe qualified name for a given resource
func generateNQN(resource string) string {
	// Format: nqn.2024-01.com.example:haify.resource-name
	return fmt.Sprintf("nqn.2024-01.com.example:haify.%s", resource)
}
