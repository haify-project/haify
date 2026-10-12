// Package gateway provides NFS gateway functionality
package gateway

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	v1 "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// NFSManager handles NFS gateway operations
type NFSManager struct {
	*Manager
	// projectQuota is set when the export filesystem has ext4's project
	// quota feature: it is then mounted with prjquota, which directory
	// quotas need (pkg/controller/nfs_quota.go). An older filesystem without
	// the feature must not be mounted with it, or the mount fails.
	projectQuota bool
}

// NewNFSManager creates a new NFS gateway manager
func NewNFSManager(m *Manager) *NFSManager {
	return &NFSManager{Manager: m}
}

// CreateNFSGateway creates an NFS gateway with drbd-reactor configuration
func (n *NFSManager) CreateNFSGateway(ctx context.Context, req *v1.CreateNFSGatewayRequest) (*v1.CreateNFSGatewayResponse, error) {
	n.logger.Info("Creating NFS gateway",
		zap.String("resource", req.Resource),
		zap.String("service_ip", req.ServiceIp),
		zap.String("export_path", req.ExportPath))

	// Parse service IP
	serviceIP, err := parseServiceIP(req.ServiceIp)
	if err != nil {
		return &v1.CreateNFSGatewayResponse{
			Success: false,
			Message: fmt.Sprintf("invalid service IP: %v", err),
		}, invalidArgument(fmt.Errorf("invalid service IP: %w", err))
	}

	// Fail early with a clear message if the OCF agents or the NFS server an
	// NFS gateway needs are not installed on the resource's diskful nodes,
	// instead of writing a promoter config that silently fails to start.
	if res, rerr := n.resources.GetResource(ctx, req.Resource); rerr == nil && res != nil {
		if err := n.checkGatewayPrereqs(ctx, gatewayNodes(res), nfsPrereqs()); err != nil {
			return &v1.CreateNFSGatewayResponse{Success: false, Message: err.Error()}, err
		}
	}

	// Auto-provision the cluster-private state volume when the resource is one
	// short, so a single-volume resource can be exported without a manual
	// add-volume step first.
	if err := n.resources.EnsureGatewayVolumes(ctx, req.Resource, 2); err != nil {
		return &v1.CreateNFSGatewayResponse{
			Success: false,
			Message: fmt.Sprintf("failed to provision gateway state volume: %v", err),
		}, err
	}

	// Get volume info from resource - NFS requires at least 2 volumes
	// Volume 0: cluster-private (NFS state), Volume 1+: exported data
	resInfo, err := n.resources.GetResource(ctx, req.Resource)
	if err != nil {
		return &v1.CreateNFSGatewayResponse{
			Success: false,
			Message: fmt.Sprintf("failed to get resource info: %v", err),
		}, err
	}

	if len(resInfo.Volumes) < 2 {
		return &v1.CreateNFSGatewayResponse{
			Success: false,
			Message: fmt.Sprintf("NFS gateway requires at least 2 volumes (got %d): volume 0 for cluster-private, volume 1+ for exports", len(resInfo.Volumes)),
		}, fmt.Errorf("resource %s has insufficient volumes for NFS gateway (need >= 2, got %d)", req.Resource, len(resInfo.Volumes))
	}

	// Get DRBD device for the resource
	drbdDevice, err := n.getDRBDDevice(ctx, req.Resource)
	if err != nil {
		n.logger.Warn("Failed to get DRBD device, using fallback",
			zap.String("resource", req.Resource),
			zap.Error(err))
		drbdDevice = "/dev/drbd0"
	}

	n.logger.Info("Using DRBD device for NFS gateway",
		zap.String("resource", req.Resource),
		zap.String("device", drbdDevice),
		zap.Int("volume_count", len(resInfo.Volumes)))

	// Generate drbd-reactor configuration
	// Promote on one of the resource's own nodes and make sure the
	// cluster-private volume carries a filesystem BEFORE reactor takes
	// over: its Filesystem agent mounts but never formats.
	// NFS mounts TWO filesystems: the cluster-private volume and the
	// export volume, so both need formatting before reactor takes over.
	clusterPrivateDev, payload := clusterPrivateAndPayload(resInfo.Volumes, drbdDevice)
	if err := n.ensureGatewayPrerequisites(ctx, req.Resource, resInfo.Nodes,
		clusterPrivateDev, payloadDevice(payload, drbdDevice)); err != nil {
		return &v1.CreateNFSGatewayResponse{
			Success: false,
			Message: err.Error(),
		}, err
	}

	if len(resInfo.Nodes) > 0 {
		n.projectQuota = n.deployment.Exec(ctx, resInfo.Nodes[:1],
			"sudo tune2fs -l "+payloadDevice(payload, drbdDevice)+" 2>/dev/null | grep -qw project") == nil
	}
	config, err := n.generateNFSGatewayConfig(req, serviceIP, drbdDevice, resInfo.Volumes)
	if err != nil {
		return &v1.CreateNFSGatewayResponse{
			Success: false,
			Message: fmt.Sprintf("failed to generate config: %v", err),
		}, err
	}

	// Write configuration to all nodes
	pluginID := fmt.Sprintf("haify-nfs-%s", req.Resource)
	if err := n.writeReactorConfig(ctx, req.Resource, pluginID, config); err != nil {
		return &v1.CreateNFSGatewayResponse{
			Success: false,
			Message: fmt.Sprintf("failed to write config: %v", err),
		}, err
	}

	configPath := filepath.Join(DrbdReactorConfigDir, fmt.Sprintf("%s.toml", pluginID))

	return &v1.CreateNFSGatewayResponse{
		Success:    true,
		Message:    "NFS gateway configuration created successfully",
		ConfigPath: configPath,
	}, nil
}

// prepareNFSNode readies a node to run an NFS gateway's chain.
//
// fsidd and nfsdcld (nfs-utils 2.6+) keep files open under /var/lib/nfs, which
// the nfsserver agent bind-mounts from the cluster-private volume. The agent
// stops nfs-server and then unmounts, but nothing stops those two, so the
// unmount fails with "target is busy". The failed stop leaves the old node with
// the cluster-private volume still mounted and open, and it cannot be promoted
// again: after one switchover the gateway can move away from a node but never
// back, and after two it has nowhere left to run. PartOf carries nfs-server's
// stop to them; nfs-server's own Requires/Wants start them again with it.
//
// The distribution's own nfs-server is switched off at boot and, on a node
// where no gateway is serving NFS, stopped along with fsidd and nfsdcld: the
// gateway starts nfs-server where it runs. A stray nfsdcld left from the
// package install keeps the root disk's /var/lib/nfs, cannot record clients
// on the gateway's state volume, and the next node to take over then holds
// every client in a full 90-second grace period instead of ending it as soon
// as they reclaim.
//
// ip_nonlocal_bind lets nfsserver's sm-notify bind to the service IP, which
// the chain now brings up after nfsserver (see the NFS template). Without it
// NFSv3 clients are never told to reclaim their locks after a switchover.
//
// Best-effort: on a node without the units the drop-in is inert. A non-empty
// onlyIfNFS skips hosts that hold no NFS promoter for that gateway, so starting
// an iSCSI or NVMe-oF gateway leaves the NFS settings alone.
func (m *Manager) prepareNFSNode(ctx context.Context, hosts []string, onlyIfNFS string) {
	if len(hosts) == 0 {
		return
	}
	guard := ""
	if onlyIfNFS != "" {
		guard = fmt.Sprintf("ls /etc/drbd-reactor.d/haify-nfs-%s.toml* >/dev/null 2>&1 || exit 0\n", onlyIfNFS)
	}
	script := guard + `changed=
for u in fsidd nfsdcld; do
  d=/etc/systemd/system/$u.service.d
  f=$d/50-haify-nfs-gateway.conf
  want='[Unit]
PartOf=nfs-server.service'
  [ "$(cat "$f" 2>/dev/null)" = "$want" ] && continue
  mkdir -p "$d" && printf '%s\n' "$want" > "$f" && changed=1
done
[ -z "$changed" ] || systemctl daemon-reload
systemctl disable -q nfs-server 2>/dev/null
if [ -z "$(systemctl list-units --no-legend --all --state=active,activating,deactivating,reloading 'ocf.rs@nfsserver_*' 2>/dev/null)" ]; then
  systemctl stop nfs-server fsidd nfsdcld 2>/dev/null
fi
f=/etc/sysctl.d/90-haify-nfs-gateway.conf
want='# Haify NFS gateway: sm-notify binds to the service IP before it is up.
net.ipv4.ip_nonlocal_bind = 1'
[ "$(cat "$f" 2>/dev/null)" = "$want" ] || printf '%s\n' "$want" > "$f"
sysctl -q -w net.ipv4.ip_nonlocal_bind=1
true`
	if err := m.runScript(ctx, hosts, script); err != nil {
		m.logger.Warn("Failed to prepare node for the NFS gateway", zap.Error(err))
	}
}

// generateNFSGatewayConfig generates drbd-reactor TOML configuration for NFS gateway
func (n *NFSManager) generateNFSGatewayConfig(req *v1.CreateNFSGatewayRequest, serviceIP *ServiceIP, drbdDevice string, volumes []*ResourceVolumeInfo) (string, error) {
	clusterPrivateDev, payload := clusterPrivateAndPayload(volumes, drbdDevice)
	// Template for NFS gateway - matches linstor-gateway pattern
	tmpl := `# Haify NFS Gateway Configuration
# Generated by Haify Controller
# Resource: {{ .Resource }}
# Service IP: {{ .ServiceIP }}
# Export Path: {{ .ExportPath }}

[[promoter]]

  [promoter.metadata]
    linstor-gateway-schema-version = 1

  [promoter.resources]

    [promoter.resources.{{ .Resource }}]
      on-drbd-demote-failure = "reboot-immediate"
      runner = "systemd"
      stop-services-on-exit = true
      target-as = "BindsTo"

      start = [
        "ocf:heartbeat:Filesystem fs_cluster_private device={{ .DRBDDevice }} directory={{ .ClusterPrivatePath }} fstype={{ .FSType }} run_fsck=no force_unmount=false",
        "ocf:heartbeat:Filesystem fs_export device={{ .ExportDevice }} directory={{ .ExportPath }} fstype={{ .FSType }}{{ if .ProjectQuota }} options=prjquota{{ end }} run_fsck=no force_unmount=false",
        "ocf:heartbeat:nfsserver nfsserver nfs_ip={{ .IPAddress }} nfs_shared_infodir={{ .NFSInfoDir }} nfs_server_scope={{ .IPAddress }}",
{{ range $idx, $client := .AllowedClients }}
        "ocf:heartbeat:exportfs export_{{ $idx }} directory={{ $.ExportPath }} fsid={{ $.FSID }} clientspec={{ $client }} options={{ $.Options }}",
{{ end }}
        "ocf:heartbeat:IPaddr2 service_ip ip={{ .IPAddress }} cidr_netmask={{ .Prefix }}",
      ]
`
	// The service IP comes last so that it goes first: drbd-reactor stops the
	// chain in reverse, and with the IP ahead of the exports every request in
	// the second between unexport and the IP leaving was refused — writes on a
	// hard mount failed outright across a switchover. With the IP gone first
	// the client sees only a server that stopped answering, and retries.
	// nfsserver then starts before the IP exists; its sm-notify binds to the
	// service IP to send NFSv3 lock-recovery notices, which is why NFS nodes
	// get ip_nonlocal_bind (see prepareNFSNode).
	//
	// Taking the IP down first does what the portblock/portunblock OCF pair was
	// for, without its failure: that pair could strand a DROP rule on the new
	// active node after a failover, firewalling clients off 2049.

	ipAddr := serviceIP.IP.String()
	prefix := serviceIP.Prefix
	fsType := req.FsType
	if fsType == "" {
		fsType = DefaultFSType
	}

	// Prepare export path (the directory clients mount)
	exportsPath, err := ResolveNFSExportPath(req.Resource, req.ExportPath)
	if err != nil {
		return "", err
	}

	// Generate UUID-based FSID (matches linstor-gateway)
	// FSID is derived from resource UUID + volume UUID for uniqueness
	resourceUUID := generateUUID()
	volumeUUID := generateUUID()
	fsid := generateFSID(resourceUUID, volumeUUID)

	// Client specs - format as nfs CIDR notation (a.b.c.d/0.0.0.0 for /0)
	var clientSpecs []string
	if len(req.AllowedIps) > 0 {
		for _, ip := range req.AllowedIps {
			clientSpecs = append(clientSpecs, nfsFormatCIDR(ip))
		}
	} else {
		// Default: allow all
		clientSpecs = append(clientSpecs, "0.0.0.0/0.0.0.0")
	}

	options := "rw,all_squash,anonuid=0,anongid=0"

	// NFS info directory uses cluster private mount path
	nfsInfoDir := filepath.Join(DefaultClusterPrivateMountPath, req.Resource, "nfs")
	clusterPrivatePath := filepath.Join(DefaultClusterPrivateMountPath, req.Resource)

	data := struct {
		Resource           string
		ServiceIP          string
		IPAddress          string
		Prefix             int
		FSType             string
		ExportPath         string
		ExportDevice       string
		ClusterPrivatePath string
		NFSPort            int
		NFSInfoDir         string
		DRBDDevice         string
		FSID               string
		AllowedClients     []string
		Options            string
		ProjectQuota       bool
	}{
		Resource:  req.Resource,
		ServiceIP: req.ServiceIp,
		IPAddress: ipAddr,
		Prefix:    prefix,
		FSType:    fsType,
		// The cluster-private volume is the one the gateway provisioned for
		// itself, and the export is the operator's. Deciding this by position
		// exported the 1 GiB scratch volume and formatted their data volume as
		// gateway state — see clusterPrivateAndPayload.
		DRBDDevice:         clusterPrivateDev,
		ExportDevice:       payloadDevice(payload, drbdDevice),
		ExportPath:         exportsPath,
		ClusterPrivatePath: clusterPrivatePath,
		NFSPort:            DefaultNFSPort,
		NFSInfoDir:         nfsInfoDir,
		FSID:               fsid,
		AllowedClients:     clientSpecs,
		Options:            options,
		ProjectQuota:       n.projectQuota,
	}

	return executeTemplate(tmpl, data)
}

// nfsFormatCIDR formats a CIDR string for NFS exportfs clientspec
// NFS requires a.b.c.d/0.0.0.0 instead of a.b.c.d/0 for IPv4
func nfsFormatCIDR(cidr string) string {
	ip, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return cidr
	}

	prefix, _ := ipNet.Mask.Size()

	// For IPv4 with /0, use 0.0.0.0 mask
	if ip.To4() != nil && prefix == 0 {
		return fmt.Sprintf("%s/0.0.0.0", ip.String())
	}

	// For IPv6, wrap in brackets
	if ip.To4() == nil {
		return fmt.Sprintf("[%s]/%d", ip.String(), prefix)
	}

	return cidr
}

// GetNFSGatewayStatus gets the status of an NFS gateway
func (n *NFSManager) GetNFSGatewayStatus(ctx context.Context, resource string) (map[string]interface{}, error) {
	status := map[string]interface{}{
		"type":     "nfs",
		"resource": resource,
		"status":   "unknown",
	}

	// Check if the gateway config exists
	configPath := filepath.Join(DrbdReactorConfigDir, fmt.Sprintf("haify-nfs-%s.toml", resource))
	if _, err := os.Stat(configPath); err != nil {
		status["status"] = "not_configured"
		return status, nil
	}

	status["status"] = "configured"
	status["config_path"] = configPath

	// Check if resource is primary
	if resInfo, err := n.resources.GetResource(ctx, resource); err == nil {
		status["role"] = resInfo.Role
		status["nodes"] = resInfo.Nodes
	}

	return status, nil
}

// DeleteNFSGateway deletes an NFS gateway
func (n *NFSManager) DeleteNFSGateway(ctx context.Context, resource string) error {
	n.logger.Info("Deleting NFS gateway", zap.String("resource", resource))

	configFile := fmt.Sprintf("haify-nfs-%s.toml", resource)
	configPath := filepath.Join(DrbdReactorConfigDir, configFile)

	// Remove config from all nodes
	for _, host := range n.hosts {
		rmCmd := fmt.Sprintf("sudo rm -f %s", configPath)
		if err := n.deployment.Exec(ctx, []string{host}, rmCmd); err != nil {
			n.logger.Warn("Failed to delete config",
				zap.String("node", host),
				zap.Error(err))
		}
	}

	// Reload drbd-reactor
	if err := n.reloadDrbdReactor(ctx); err != nil {
		return err
	}

	n.logger.Info("NFS gateway deleted", zap.String("resource", resource))
	return nil
}

// ==================== Multi-Export Support ====================

// AddNFSExport adds an additional export to an existing NFS gateway
func (n *NFSManager) AddNFSExport(ctx context.Context, resource, exportPath string, fsid int, clientSpec, options string) error {
	n.logger.Info("Adding NFS export",
		zap.String("resource", resource),
		zap.String("export_path", exportPath))

	pluginID := fmt.Sprintf("haify-nfs-%s", resource)
	cfg, err := n.readGatewayConfig(ctx, resource, pluginID)
	if err != nil {
		return err
	}
	content := cfg.content

	lines, trailingNewline := splitConfigLines(content)
	exportID := nextExportID(lines)
	if fsid <= 0 {
		fsid = exportID
	}
	if clientSpec == "" {
		clientSpec = "0.0.0.0/0.0.0.0"
	}
	if options == "" {
		options = "rw,all_squash,anonuid=0,anongid=0"
	}

	resolvedPath, err := ResolveNFSExportPath(resource, exportPath)
	if err != nil {
		return err
	}
	newLine := buildNFSExportLine(exportID, resolvedPath, strconv.Itoa(fsid), clientSpec, options)
	// After the last export (or nfsserver): the service IP that follows must
	// stay last in the chain — see the NFS template.
	anchor := findLineIndex(lines, func(line string) bool {
		return strings.Contains(line, "ocf:heartbeat:nfsserver ")
	})
	for i, line := range lines {
		if _, ok := parseNFSExportLine(line); ok {
			anchor = i
		}
	}
	if anchor < 0 {
		return fmt.Errorf("failed to locate the nfsserver entry in %s", gatewayConfigPath(pluginID))
	}
	lines = append(lines[:anchor+1], append([]string{newLine}, lines[anchor+1:]...)...)

	return n.persistGatewayConfig(ctx, resource, pluginID, cfg, joinConfigLines(lines, trailingNewline))
}

// RemoveNFSExport removes an export from an existing NFS gateway.
func (n *NFSManager) RemoveNFSExport(ctx context.Context, resource, exportPath string) error {
	n.logger.Info("Removing NFS export",
		zap.String("resource", resource),
		zap.String("export_path", exportPath))

	pluginID := fmt.Sprintf("haify-nfs-%s", resource)
	cfg, err := n.readGatewayConfig(ctx, resource, pluginID)
	if err != nil {
		return err
	}
	content := cfg.content

	normalizedPath, err := ResolveNFSExportPath(resource, exportPath)
	if err != nil {
		return err
	}
	lines, trailingNewline := splitConfigLines(content)
	lines, removed := removeLine(lines, func(line string) bool {
		params, ok := parseNFSExportLine(line)
		return ok && params["directory"] == normalizedPath
	})
	if !removed {
		return fmt.Errorf("export not found: %s", normalizedPath)
	}

	return n.persistGatewayConfig(ctx, resource, pluginID, cfg, joinConfigLines(lines, trailingNewline))
}

// ListNFSExports lists all exports for an NFS gateway
func (n *NFSManager) ListNFSExports(ctx context.Context, resource string) ([]map[string]string, error) {
	pluginID := fmt.Sprintf("haify-nfs-%s", resource)
	cfg, err := n.readGatewayConfig(ctx, resource, pluginID)
	if err != nil {
		return nil, err
	}
	content := cfg.content

	var exports []map[string]string
	lines := strings.Split(content, "\n")

	for _, line := range lines {
		params, ok := parseNFSExportLine(line)
		if !ok {
			continue
		}
		exports = append(exports, map[string]string{
			"directory":  params["directory"],
			"fsid":       params["fsid"],
			"clientspec": params["clientspec"],
			"options":    params["options"],
		})
	}

	return exports, nil
}

// ==================== Helper Functions ====================

// parseCIDR parses a CIDR string and returns IP and prefix
func parseCIDR(cidr string) (string, int, error) {
	ip, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", 0, err
	}
	prefix, _ := ipNet.Mask.Size()
	return ip.String(), prefix, nil
}
