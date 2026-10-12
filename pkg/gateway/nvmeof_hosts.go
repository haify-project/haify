package gateway

import (
	"context"
	"fmt"
	"strings"

	"go.uber.org/zap"
)

// The NVMe subsystem's host (initiator) allow-list.
//
// The list is one parameter of the nvmet-subsystem line, so both functions here
// rewrite that whole line from its parsed parameters — dropping the serial or
// the NQN while editing the allow-list would change which subsystem clients
// think they are talking to, not just who may talk to it.
//
// As with the iSCSI ACL, an empty allow-list means "allow every host", not
// "deny all". RemoveHost therefore refuses to run against a subsystem that has
// no explicit list yet: the caller is asking to narrow access, and reporting
// success without having narrowed anything is the failure worth preventing.

// ==================== Host Management ====================

// AddHost adds a host (initiator) to the NVMe subsystem
func (n *NVMeManager) AddHost(ctx context.Context, resource, hostNQN string) error {
	log := n.log()
	log.Debug("NVMeManager.AddHost: start", zap.String("resource", resource), zap.String("host_nqn", hostNQN))
	n.logger.Info("Adding host to NVMe subsystem",
		zap.String("resource", resource),
		zap.String("host_nqn", hostNQN))

	if err := validateHostNQN(hostNQN); err != nil {
		log.Debug("NVMeManager.AddHost: invalid host NQN", zap.String("host_nqn", hostNQN), zap.Error(err))
		return invalidArgument(err)
	}

	pluginID := fmt.Sprintf("haify-nvmeof-%s", resource)
	cfg, err := n.readGatewayConfig(ctx, resource, pluginID)
	if err != nil {
		log.Debug("NVMeManager.AddHost: reading config failed", zap.String("resource", resource), zap.Error(err))
		return err
	}
	content := cfg.content

	lines, trailingNewline := splitConfigLines(content)
	subsystemIdx := findLineIndex(lines, func(line string) bool {
		_, ok := parseNVMeSubsystemLine(line)
		return ok
	})
	if subsystemIdx < 0 {
		log.Debug("NVMeManager.AddHost: no subsystem line in config", zap.String("resource", resource))
		return fmt.Errorf("failed to locate NVMe subsystem definition")
	}

	params, _ := parseNVMeSubsystemLine(lines[subsystemIdx])
	allowed := parseAllowedList(params["allowed_initiators"])
	log.Debug("NVMeManager.AddHost: current allow-list",
		zap.String("resource", resource), zap.Strings("allowed", allowed), zap.Bool("allows_all", len(allowed) == 0))
	allowed = append(allowed, hostNQN)
	lines[subsystemIdx] = buildNVMeSubsystemLine(params["nqn"], formatAllowedList(allowed), params["serial"])

	return n.persistGatewayConfig(ctx, resource, pluginID, cfg, joinConfigLines(lines, trailingNewline))
}

// RemoveHost removes a host from the NVMe subsystem
func (n *NVMeManager) RemoveHost(ctx context.Context, resource, hostNQN string) error {
	log := n.log()
	log.Debug("NVMeManager.RemoveHost: start", zap.String("resource", resource), zap.String("host_nqn", hostNQN))
	n.logger.Info("Removing host from NVMe subsystem",
		zap.String("resource", resource),
		zap.String("host_nqn", hostNQN))

	pluginID := fmt.Sprintf("haify-nvmeof-%s", resource)
	cfg, err := n.readGatewayConfig(ctx, resource, pluginID)
	if err != nil {
		log.Debug("NVMeManager.RemoveHost: reading config failed", zap.String("resource", resource), zap.Error(err))
		return err
	}
	content := cfg.content

	lines, trailingNewline := splitConfigLines(content)
	subsystemIdx := findLineIndex(lines, func(line string) bool {
		_, ok := parseNVMeSubsystemLine(line)
		return ok
	})
	if subsystemIdx < 0 {
		log.Debug("NVMeManager.RemoveHost: no subsystem line in config", zap.String("resource", resource))
		return fmt.Errorf("failed to locate NVMe subsystem definition")
	}

	params, _ := parseNVMeSubsystemLine(lines[subsystemIdx])
	current := parseAllowedList(params["allowed_initiators"])
	if len(current) == 0 {
		log.Debug("NVMeManager.RemoveHost: subsystem allows every host, refusing", zap.String("resource", resource))
		return fmt.Errorf("gateway currently allows all initiators; cannot remove a specific host without first defining an explicit allow-list")
	}

	updated := removeValue(current, hostNQN)
	if len(updated) == len(current) {
		log.Debug("NVMeManager.RemoveHost: host not in allow-list",
			zap.String("resource", resource), zap.String("host_nqn", hostNQN), zap.Strings("allowed", current))
		return fmt.Errorf("host not found: %s", hostNQN)
	}
	log.Debug("NVMeManager.RemoveHost: allow-list updated", zap.String("resource", resource), zap.Strings("allowed", updated))

	lines[subsystemIdx] = buildNVMeSubsystemLine(params["nqn"], formatAllowedList(updated), params["serial"])
	return n.persistGatewayConfig(ctx, resource, pluginID, cfg, joinConfigLines(lines, trailingNewline))
}

// ListHosts lists all hosts for an NVMe subsystem
func (n *NVMeManager) ListHosts(ctx context.Context, resource string) ([]string, error) {
	log := n.log()
	log.Debug("NVMeManager.ListHosts: start", zap.String("resource", resource))
	pluginID := fmt.Sprintf("haify-nvmeof-%s", resource)
	cfg, err := n.readGatewayConfig(ctx, resource, pluginID)
	if err != nil {
		log.Debug("NVMeManager.ListHosts: reading config failed", zap.String("resource", resource), zap.Error(err))
		return nil, err
	}
	content := cfg.content

	for _, line := range strings.Split(content, "\n") {
		if params, ok := parseNVMeSubsystemLine(line); ok {
			allowed := parseAllowedList(params["allowed_initiators"])
			if len(allowed) == 0 {
				log.Debug("NVMeManager.ListHosts: subsystem allows every host", zap.String("resource", resource))
				return []string{"ALL"}, nil
			}
			log.Debug("NVMeManager.ListHosts: allow-list", zap.String("resource", resource), zap.Strings("allowed", allowed))
			return allowed, nil
		}
	}

	log.Debug("NVMeManager.ListHosts: no subsystem line in config", zap.String("resource", resource))
	return nil, fmt.Errorf("failed to locate NVMe subsystem definition")
}
