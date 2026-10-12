package gateway

import (
	"context"
	"fmt"
	"strings"

	"go.uber.org/zap"
)

// The target and the block devices it exports, edited on a gateway that already
// exists.
//
// Creating an iSCSI gateway writes the promoter config once, from a template.
// Everything here does the opposite: it reads that config back, edits one line,
// and writes it out again — because targetcli state is owned by the OCF agents
// and is rebuilt from the config on every promotion, so the config file is the
// only durable place a LUN can be added or removed. The parse/build helpers
// that make a line editable both ways live in config_helpers.go.
//
// The subtlety these functions exist to contain is placement: a LUN line has to
// be inside the promoter's start array and after the target that owns it, so
// insertion is anchored on a landmark (see AddLUN) rather than appended. An
// appended LUN parses fine, distributes fine, and is simply never started.

// AddLUN adds a LUN to an existing iSCSI gateway
func (i *iSCSIManager) AddLUN(ctx context.Context, resource string, lunNumber int, device string) error {
	log := i.log()
	log.Debug("iSCSIManager.AddLUN: start", zap.String("resource", resource), zap.Int("lun", lunNumber), zap.String("device", device))
	i.logger.Info("Adding LUN to iSCSI gateway",
		zap.String("resource", resource),
		zap.Int("lun", lunNumber),
		zap.String("device", device))

	pluginID := fmt.Sprintf("haify-iscsi-%s", resource)
	cfg, err := i.readGatewayConfig(ctx, resource, pluginID)
	if err != nil {
		log.Debug("iSCSIManager.AddLUN: reading config failed", zap.String("resource", resource), zap.Error(err))
		return err
	}
	content := cfg.content

	lines, trailingNewline := splitConfigLines(content)
	for _, line := range lines {
		if params, ok := parseISCSILUNLine(line); ok && params["lun"] == fmt.Sprintf("%d", lunNumber) {
			log.Debug("iSCSIManager.AddLUN: LUN number taken", zap.String("resource", resource), zap.Int("lun", lunNumber))
			return fmt.Errorf("LUN %d already exists", lunNumber)
		}
	}

	targetIdx := findLineIndex(lines, func(line string) bool {
		_, ok := parseISCSITargetLine(line)
		return ok
	})
	if targetIdx < 0 {
		log.Debug("iSCSIManager.AddLUN: no target line in config", zap.String("resource", resource))
		return fmt.Errorf("failed to locate iSCSI target definition")
	}

	targetParams, _ := parseISCSITargetLine(lines[targetIdx])
	iqn := targetParams["iqn"]
	if iqn == "" {
		log.Debug("iSCSIManager.AddLUN: target line has no IQN", zap.String("resource", resource))
		return fmt.Errorf("failed to parse target IQN from config")
	}

	newLine := buildISCSILUNLine(lunNumber, iqn, device, targetParams["implementation"])
	// After the last LUN (or the target, when there is none): the service IP
	// that follows must stay last in the chain — see the iSCSI template.
	anchor := targetIdx
	for i, line := range lines {
		if _, ok := parseISCSILUNLine(line); ok {
			anchor = i
		}
	}
	log.Debug("iSCSIManager.AddLUN: inserting LUN line",
		zap.String("resource", resource), zap.String("iqn", iqn), zap.Int("target_line", targetIdx),
		zap.Int("after_line", anchor), zap.String("implementation", targetParams["implementation"]))
	lines = append(lines[:anchor+1], append([]string{newLine}, lines[anchor+1:]...)...)

	return i.persistGatewayConfig(ctx, resource, pluginID, cfg, joinConfigLines(lines, trailingNewline))
}

// RemoveLUN removes a LUN from an iSCSI gateway
func (i *iSCSIManager) RemoveLUN(ctx context.Context, resource string, lunNumber int) error {
	log := i.log()
	log.Debug("iSCSIManager.RemoveLUN: start", zap.String("resource", resource), zap.Int("lun", lunNumber))
	i.logger.Info("Removing LUN from iSCSI gateway",
		zap.String("resource", resource),
		zap.Int("lun", lunNumber))

	pluginID := fmt.Sprintf("haify-iscsi-%s", resource)
	cfg, err := i.readGatewayConfig(ctx, resource, pluginID)
	if err != nil {
		log.Debug("iSCSIManager.RemoveLUN: reading config failed", zap.String("resource", resource), zap.Error(err))
		return err
	}
	content := cfg.content

	lines, trailingNewline := splitConfigLines(content)
	lines, removed := removeLine(lines, func(line string) bool {
		params, ok := parseISCSILUNLine(line)
		return ok && params["lun"] == fmt.Sprintf("%d", lunNumber)
	})
	if !removed {
		log.Debug("iSCSIManager.RemoveLUN: LUN not in config", zap.String("resource", resource), zap.Int("lun", lunNumber))
		return fmt.Errorf("LUN %d not found", lunNumber)
	}

	return i.persistGatewayConfig(ctx, resource, pluginID, cfg, joinConfigLines(lines, trailingNewline))
}

// ListLUNs lists all configured LUNs for an iSCSI gateway.
func (i *iSCSIManager) ListLUNs(ctx context.Context, resource string) ([]map[string]string, error) {
	log := i.log()
	log.Debug("iSCSIManager.ListLUNs: start", zap.String("resource", resource))
	pluginID := fmt.Sprintf("haify-iscsi-%s", resource)
	cfg, err := i.readGatewayConfig(ctx, resource, pluginID)
	if err != nil {
		log.Debug("iSCSIManager.ListLUNs: reading config failed", zap.String("resource", resource), zap.Error(err))
		return nil, err
	}
	content := cfg.content

	var luns []map[string]string
	for _, line := range strings.Split(content, "\n") {
		params, ok := parseISCSILUNLine(line)
		if !ok {
			continue
		}
		log.Debug("iSCSIManager.ListLUNs: LUN", zap.String("resource", resource), zap.String("lun", params["lun"]), zap.String("device", params["path"]))
		luns = append(luns, map[string]string{
			"lun":        params["lun"],
			"device":     params["path"],
			"target_iqn": params["target_iqn"],
		})
	}

	log.Debug("iSCSIManager.ListLUNs: done", zap.String("resource", resource), zap.Int("count", len(luns)))
	return luns, nil
}

// ==================== Target Management ====================

// CreateTarget creates an iSCSI target on the gateway
func (i *iSCSIManager) CreateTarget(ctx context.Context, resource string) error {
	i.log().Debug("iSCSIManager.CreateTarget: refused, the OCF agent owns targets", zap.String("resource", resource))
	i.logger.Info("Creating iSCSI target", zap.String("resource", resource))

	// Use targetcli or configuration files to create the target
	// This is typically done by the OCF resource agent during start

	return fmt.Errorf("CreateTarget: managed by OCF resource agent")
}

// DeleteTarget deletes an iSCSI target
func (i *iSCSIManager) DeleteTarget(ctx context.Context, resource string) error {
	i.log().Debug("iSCSIManager.DeleteTarget: refused, gateway deletion removes targets", zap.String("resource", resource))
	i.logger.Info("Deleting iSCSI target", zap.String("resource", resource))

	return fmt.Errorf("DeleteTarget: use gateway deletion instead")
}

// ListTargets lists the IQNs of the running iSCSI gateways' targets, read from
// the managed nodes' /etc/drbd-reactor.d — the controller need not be a
// gateway node. host is unused: a target's config lives on every diskful node
// of its resource, so the union over all nodes is the answer.
func (i *iSCSIManager) ListTargets(ctx context.Context, host string) ([]string, error) {
	log := i.log()
	log.Debug("iSCSIManager.ListTargets: start", zap.String("host", host))
	contents, err := i.readAllNodeConfigs(ctx, "haify-iscsi-*.toml")
	if err != nil {
		log.Debug("iSCSIManager.ListTargets: reading configs failed", zap.Error(err))
		return nil, err
	}

	var targets []string
	for _, content := range contents {
		for _, line := range strings.Split(content, "\n") {
			if params, ok := parseISCSITargetLine(line); ok && params["iqn"] != "" {
				targets = append(targets, params["iqn"])
			}
		}
	}

	targets = uniqueSortedValues(targets)
	log.Debug("iSCSIManager.ListTargets: done", zap.Strings("targets", targets))
	return targets, nil
}
