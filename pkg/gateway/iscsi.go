// Package gateway provides iSCSI gateway functionality
package gateway

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	v1 "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// iSCSIManager handles iSCSI gateway operations
type iSCSIManager struct {
	*Manager
}

// NewISCSIManager creates a new iSCSI gateway manager
func NewISCSIManager(m *Manager) *iSCSIManager {
	return &iSCSIManager{Manager: m}
}

// CreateISCSIGateway creates an iSCSI gateway with drbd-reactor configuration
func (i *iSCSIManager) CreateISCSIGateway(ctx context.Context, req *v1.CreateISCSIGatewayRequest) (*v1.CreateISCSIGatewayResponse, error) {
	log := i.log()
	begin := time.Now()
	// The request carries the CHAP password: only whether it is set is traced.
	log.Debug("iSCSIManager.CreateISCSIGateway: start",
		zap.String("resource", req.Resource), zap.String("iqn", req.Iqn), zap.String("service_ip", req.ServiceIp),
		zap.Strings("allowed_initiators", req.AllowedInitiators), zap.String("implementation", req.Implementation),
		zap.String("username", req.Username), zap.Bool("has_password", req.Password != ""))
	i.logger.Info("Creating iSCSI gateway",
		zap.String("resource", req.Resource),
		zap.String("iqn", req.Iqn),
		zap.String("service_ip", req.ServiceIp))

	// Reject a malformed IQN before anything with a side effect runs — see
	// validate.go for why this cannot wait until targetcli parses it.
	if err := validateIQN(req.Iqn); err != nil {
		log.Debug("iSCSIManager.CreateISCSIGateway: invalid IQN", zap.String("iqn", req.Iqn), zap.Error(err))
		return &v1.CreateISCSIGatewayResponse{
			Success: false,
			Message: err.Error(),
		}, invalidArgument(err)
	}
	for _, initiator := range req.AllowedInitiators {
		if err := validateIQN(initiator); err != nil {
			log.Debug("iSCSIManager.CreateISCSIGateway: invalid allowed initiator", zap.String("iqn", initiator), zap.Error(err))
			err = fmt.Errorf("allowed initiator: %w", err)
			return &v1.CreateISCSIGatewayResponse{
				Success: false,
				Message: err.Error(),
			}, invalidArgument(err)
		}
	}
	if _, err := validateISCSIImplementation(req.Implementation); err != nil {
		log.Debug("iSCSIManager.CreateISCSIGateway: invalid implementation", zap.String("implementation", req.Implementation), zap.Error(err))
		return &v1.CreateISCSIGatewayResponse{
			Success: false,
			Message: err.Error(),
		}, invalidArgument(err)
	}

	// Parse service IP
	serviceIP, err := parseServiceIP(req.ServiceIp)
	if err != nil {
		log.Debug("iSCSIManager.CreateISCSIGateway: invalid service IP", zap.String("service_ip", req.ServiceIp), zap.Error(err))
		return &v1.CreateISCSIGatewayResponse{
			Success: false,
			Message: fmt.Sprintf("invalid service IP: %v", err),
		}, invalidArgument(fmt.Errorf("invalid service IP: %w", err))
	}

	// Fail early with a clear message if the OCF agents/tools an iSCSI gateway
	// needs are not installed on the resource's nodes, rather than writing a
	// promoter config that silently fails to start.
	if res, rerr := i.resources.GetResource(ctx, req.Resource); rerr == nil && res != nil {
		if err := i.checkGatewayPrereqs(ctx, gatewayNodes(res), iscsiPrereqs()); err != nil {
			log.Debug("iSCSIManager.CreateISCSIGateway: prerequisites missing", zap.String("resource", req.Resource), zap.Error(err))
			return &v1.CreateISCSIGatewayResponse{Success: false, Message: err.Error()}, err
		}
	} else {
		log.Debug("iSCSIManager.CreateISCSIGateway: resource lookup failed, prerequisite check skipped",
			zap.String("resource", req.Resource), zap.Error(rerr))
	}

	// Auto-provision the cluster-private state volume when the resource is one
	// short, so a single-volume resource can be exported without a manual
	// add-volume step first.
	start := time.Now()
	if err := i.resources.EnsureGatewayVolumes(ctx, req.Resource, 2); err != nil {
		log.Debug("iSCSIManager.CreateISCSIGateway: state volume provisioning failed",
			zap.String("resource", req.Resource), zap.Duration("took", time.Since(start)), zap.Error(err))
		return &v1.CreateISCSIGatewayResponse{
			Success: false,
			Message: fmt.Sprintf("failed to provision gateway state volume: %v", err),
		}, err
	}
	log.Debug("iSCSIManager.CreateISCSIGateway: gateway volumes ensured",
		zap.String("resource", req.Resource), zap.Duration("took", time.Since(start)))

	// Get volume info from resource - iSCSI requires at least 2 volumes
	// Volume 0: cluster-private, Volume 1+: LUNs exposed to initiators
	resInfo, err := i.resources.GetResource(ctx, req.Resource)
	if err != nil {
		log.Debug("iSCSIManager.CreateISCSIGateway: resource lookup failed", zap.String("resource", req.Resource), zap.Error(err))
		return &v1.CreateISCSIGatewayResponse{
			Success: false,
			Message: fmt.Sprintf("failed to get resource info: %v", err),
		}, err
	}

	if len(resInfo.Volumes) < 2 {
		log.Debug("iSCSIManager.CreateISCSIGateway: too few volumes", zap.String("resource", req.Resource), zap.Int("count", len(resInfo.Volumes)))
		return &v1.CreateISCSIGatewayResponse{
			Success: false,
			Message: fmt.Sprintf("iSCSI gateway requires at least 2 volumes (got %d): volume 0 for cluster-private, volume 1+ for LUNs", len(resInfo.Volumes)),
		}, fmt.Errorf("resource %s has insufficient volumes for iSCSI gateway (need >= 2, got %d)", req.Resource, len(resInfo.Volumes))
	}

	// Get DRBD device for the resource
	drbdDevice, err := i.getDRBDDevice(ctx, req.Resource)
	if err != nil {
		i.logger.Warn("Failed to get DRBD device, using fallback",
			zap.String("resource", req.Resource),
			zap.Error(err))
		drbdDevice = "/dev/drbd0"
	}

	i.logger.Info("Using DRBD device for iSCSI gateway",
		zap.String("resource", req.Resource),
		zap.String("device", drbdDevice),
		zap.Int("volume_count", len(resInfo.Volumes)),
		zap.Int("lun_count", len(resInfo.Volumes)-1))

	// Generate drbd-reactor configuration
	// Promote on one of the resource's own nodes and make sure the
	// cluster-private volume carries a filesystem BEFORE reactor takes
	// over: its Filesystem agent mounts but never formats.
	// Only the cluster-private volume is formatted. The exported volume must
	// stay raw — it is a block device handed to an initiator, which puts its
	// own filesystem on it. Passing volumes[0] here formatted the operator's
	// data volume as gateway scratch; see clusterPrivateAndPayload.
	clusterPrivateDev, _ := clusterPrivateAndPayload(resInfo.Volumes, drbdDevice)
	if err := i.ensureGatewayPrerequisites(ctx, req.Resource, resInfo.Nodes, clusterPrivateDev); err != nil {
		log.Debug("iSCSIManager.CreateISCSIGateway: preparing the resource failed", zap.String("resource", req.Resource), zap.Error(err))
		return &v1.CreateISCSIGatewayResponse{
			Success: false,
			Message: err.Error(),
		}, err
	}

	config, err := i.generateISCSIGatewayConfig(req, serviceIP, drbdDevice, resInfo.Volumes)
	if err != nil {
		log.Debug("iSCSIManager.CreateISCSIGateway: config generation failed", zap.String("resource", req.Resource), zap.Error(err))
		return &v1.CreateISCSIGatewayResponse{
			Success: false,
			Message: fmt.Sprintf("failed to generate config: %v", err),
		}, err
	}

	// Write configuration to all nodes
	pluginID := fmt.Sprintf("haify-iscsi-%s", req.Resource)
	if err := i.writeReactorConfig(ctx, req.Resource, pluginID, config); err != nil {
		log.Debug("iSCSIManager.CreateISCSIGateway: writing config failed", zap.String("resource", req.Resource), zap.Error(err))
		return &v1.CreateISCSIGatewayResponse{
			Success: false,
			Message: fmt.Sprintf("failed to write config: %v", err),
		}, err
	}

	configPath := filepath.Join(DrbdReactorConfigDir, fmt.Sprintf("%s.toml", pluginID))
	log.Debug("iSCSIManager.CreateISCSIGateway: done",
		zap.String("resource", req.Resource), zap.String("path", configPath), zap.Duration("took", time.Since(begin)))

	return &v1.CreateISCSIGatewayResponse{
		Success:    true,
		Message:    "iSCSI gateway configuration created successfully",
		ConfigPath: configPath,
	}, nil
}

// generateISCSIGatewayConfig generates drbd-reactor TOML configuration for iSCSI gateway
func (i *iSCSIManager) generateISCSIGatewayConfig(req *v1.CreateISCSIGatewayRequest, serviceIP *ServiceIP, drbdDevice string, volumes []*ResourceVolumeInfo) (string, error) {
	// Template for iSCSI gateway - matches linstor-gateway pattern
	tmpl := `# Haify iSCSI Gateway Configuration
# Generated by Haify Controller
# Resource: {{ .Resource }}
# IQN: {{ .IQN }}
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
        "ocf:heartbeat:iSCSITarget target iqn={{ .IQN }} portals={{ .Portal }} {{ .CHAPArgs }}allowed_initiators={{ .AllowedInitiators }} implementation={{ .Implementation }}",
{{ range $idx, $lun := .LUNs }}
        "ocf:heartbeat:iSCSILogicalUnit lu{{ $lun.Number }} target_iqn={{ $.IQN }} lun={{ $lun.Number }} path={{ $lun.Device }} product_id={{ $lun.Serial }} scsi_sn={{ $lun.Serial }} implementation={{ $.Implementation }}",
{{ end }}
        "ocf:heartbeat:IPaddr2 service_ip0 ip={{ .IPAddress }} cidr_netmask={{ .Prefix }}",
      ]
`
	// The service IP comes last so that it goes first. drbd-reactor stops the
	// chain in reverse, and with the IP ahead of the target the LUNs were
	// deleted while initiators could still reach the portal: every command in
	// that window came back "LUN not supported", a hard error the initiator
	// passes up, and a client filesystem under write load went read-only on
	// every switchover. With the IP gone first the initiator sees only a lost
	// connection, which it rides out (replacement_timeout) until the target is
	// back on the other node. LIO binds the portal to the service IP before
	// the address is up.
	//
	// portblock/portunblock removed: on failover the unblock step did not
	// reliably clear the block's DROP rule on the new active node, firewalling
	// clients off the iSCSI port (verified on real node failover). The data
	// fencing is provided by DRBD (the demoted node goes Secondary, losing
	// write access) plus the target being stopped and the VIP moving away.

	ipAddr := serviceIP.IP.String()
	prefix := serviceIP.Prefix
	portal := fmt.Sprintf("%s:%d", ipAddr, DefaultISCSIPort)

	// Prepare LUNs - Volume 0 is cluster-private, volumes 1+ are exposed as LUNs.
	// Each LUN needs a unique serial number based on IQN + volume number.
	type LUN struct {
		Number int
		Device string
		Serial string
	}

	// Which volume is the gateway's own scratch and which is the operator's is
	// decided by name, not by position — see clusterPrivateAndPayload. Doing it
	// by position handed the initiator the 1 GiB state volume.
	clusterPrivateDev, payload := clusterPrivateAndPayload(volumes, drbdDevice)

	// LUN numbering stays 1-based and independent of the DRBD volume number:
	// LUN 0 has special meaning to some initiators, and the payload volume is
	// now usually volume 0.
	luns := make([]LUN, 0, len(payload))
	for i, vol := range payload {
		n := i + 1
		zap.L().Debug("iSCSIManager.generateISCSIGatewayConfig: LUN", zap.Int("lun", n), zap.String("device", vol.Device))
		luns = append(luns, LUN{
			Number: n,
			Device: vol.Device,
			Serial: generateSerialFromIQN(req.Iqn, n),
		})
	}

	// Default values
	username := req.Username
	password := req.Password
	implementation, err := validateISCSIImplementation(req.Implementation)
	if err != nil {
		i.log().Debug("iSCSIManager.generateISCSIGatewayConfig: invalid implementation", zap.Error(err))
		return "", err
	}
	allowedInitiators := ocfListValue(req.AllowedInitiators)

	// Only emit CHAP arguments when credentials were actually supplied.
	// Writing literal "username"/"password" placeholders (the old default)
	// forced CHAP auth with bogus credentials on a gateway the user meant to
	// leave open.
	chapArgs := ""
	if username != "" && password != "" {
		chapArgs = fmt.Sprintf("incoming_username=%s incoming_password=%s ", username, password)
	}
	// An empty allowed_initiators list stays empty: the OCF agent treats
	// each token as an initiator WWN, so a made-up "ALL" fails validation
	// ("WWN not valid"). linstor-gateway passes the empty string too.
	_ = allowedInitiators

	clusterPrivatePath := filepath.Join(DefaultClusterPrivateMountPath, req.Resource)
	i.log().Debug("iSCSIManager.generateISCSIGatewayConfig: rendering",
		zap.String("resource", req.Resource), zap.String("portal", portal), zap.Int("prefix", prefix),
		zap.String("cluster_private", clusterPrivateDev), zap.Int("luns", len(luns)),
		zap.String("implementation", implementation), zap.Bool("chap", chapArgs != ""),
		zap.Int("allowed_initiators", len(req.AllowedInitiators)))

	data := struct {
		Resource           string
		IQN                string
		ServiceIP          string
		IPAddress          string
		Prefix             int
		Portal             string
		FSType             string
		ClusterPrivatePath string
		ISCSIPort          int
		CHAPArgs           string
		AllowedInitiators  string
		Implementation     string
		LUNs               []LUN
		DRBDDevice         string
	}{
		Resource:           req.Resource,
		IQN:                req.Iqn,
		ServiceIP:          req.ServiceIp,
		IPAddress:          ipAddr,
		Prefix:             prefix,
		Portal:             portal,
		FSType:             DefaultFSType,
		DRBDDevice:         clusterPrivateDev,
		ClusterPrivatePath: clusterPrivatePath,
		ISCSIPort:          DefaultISCSIPort,
		CHAPArgs:           chapArgs,
		AllowedInitiators:  allowedInitiators,
		Implementation:     implementation,
		LUNs:               luns,
	}

	return executeTemplate(tmpl, data)
}

// GetISCSIGatewayStatus gets the status of an iSCSI gateway
func (i *iSCSIManager) GetISCSIGatewayStatus(ctx context.Context, resource string) (map[string]interface{}, error) {
	log := i.log()
	log.Debug("iSCSIManager.GetISCSIGatewayStatus: start", zap.String("resource", resource))
	status := map[string]interface{}{
		"type":     "iscsi",
		"resource": resource,
		"status":   "unknown",
	}

	// Check if the gateway config exists
	configPath := filepath.Join(DrbdReactorConfigDir, fmt.Sprintf("haify-iscsi-%s.toml", resource))
	if _, err := os.Stat(configPath); err != nil {
		log.Debug("iSCSIManager.GetISCSIGatewayStatus: no local config", zap.String("path", configPath), zap.Error(err))
		status["status"] = "not_configured"
		return status, nil
	}

	status["status"] = "configured"
	status["config_path"] = configPath

	// Check if resource is primary
	if resInfo, err := i.resources.GetResource(ctx, resource); err == nil {
		status["role"] = resInfo.Role
		status["nodes"] = resInfo.Nodes
		status["volumes"] = len(resInfo.Volumes)
		log.Debug("iSCSIManager.GetISCSIGatewayStatus: configured",
			zap.String("resource", resource), zap.String("role", resInfo.Role), zap.Strings("nodes", resInfo.Nodes))
	} else {
		log.Debug("iSCSIManager.GetISCSIGatewayStatus: resource lookup failed", zap.String("resource", resource), zap.Error(err))
	}

	return status, nil
}

// DeleteISCSIGateway deletes an iSCSI gateway
func (i *iSCSIManager) DeleteISCSIGateway(ctx context.Context, resource string) error {
	log := i.log()
	log.Debug("iSCSIManager.DeleteISCSIGateway: start", zap.String("resource", resource), zap.Strings("hosts", i.hosts))
	i.logger.Info("Deleting iSCSI gateway", zap.String("resource", resource))

	configFile := fmt.Sprintf("haify-iscsi-%s.toml", resource)
	configPath := filepath.Join(DrbdReactorConfigDir, configFile)

	// Remove config from all nodes
	for _, host := range i.hosts {
		rmCmd := fmt.Sprintf("sudo rm -f %s", configPath)
		start := time.Now()
		err := i.deployment.Exec(ctx, []string{host}, rmCmd)
		log.Debug("iSCSIManager.DeleteISCSIGateway: removed config",
			zap.String("host", host), zap.String("path", configPath), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			i.logger.Warn("Failed to delete config",
				zap.String("node", host),
				zap.Error(err))
		}
	}

	// Reload drbd-reactor
	if err := i.reloadDrbdReactor(ctx); err != nil {
		log.Debug("iSCSIManager.DeleteISCSIGateway: reload failed", zap.String("resource", resource), zap.Error(err))
		return err
	}

	i.logger.Info("iSCSI gateway deleted", zap.String("resource", resource))
	return nil
}

// ==================== Helper Functions ====================

// generateIQN generates an IQN for a given resource
func generateIQN(resource string) string {
	// Format: iqn.2024-01.com.example:haify.resource-name
	return fmt.Sprintf("iqn.2024-01.com.example:haify.%s", resource)
}

// parsePortal parses an iSCSI portal string (host:port)
func parsePortal(portal string) (string, int, error) {
	host, portStr, err := net.SplitHostPort(portal)
	if err != nil {
		zap.L().Debug("parsePortal: not host:port", zap.String("portal", portal), zap.Error(err))
		return "", 0, err
	}

	port := 3260 // default iSCSI port
	if portStr != "" {
		if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
			zap.L().Debug("parsePortal: invalid port", zap.String("portal", portal), zap.Error(err))
			return "", 0, fmt.Errorf("invalid port number: %s", portStr)
		}
	}

	zap.L().Debug("parsePortal: parsed", zap.String("host", host), zap.Int("port", port))
	return host, port, nil
}
