package gateway

import (
	"context"
	"fmt"
	"strings"

	"go.uber.org/zap"
)

// Who is allowed to attach to an iSCSI target: the initiator allow-list and the
// CHAP credentials.
//
// These are separated from the LUNs they guard because they are one line in the
// config — the iSCSITarget line — and every function here rewrites that whole
// line from its parsed parameters. Editing it in place instead would be worse
// than verbose: an allow-list update that dropped the portals, or a CHAP update
// that dropped the allow-list, would silently widen access on the next
// promotion. Rebuilding the line from a full parameter set is what makes an
// omission a compile-time argument rather than a security regression.
//
// An empty allow-list is deliberately not "deny all": the iSCSITarget agent
// reads it as "allow every initiator". That inversion is why RemoveInitiator
// refuses to operate on a target that has no ACL yet — the request reads as
// "narrow this down", and silently succeeding against an allow-all target would
// report a tightened ACL that does not exist.
//
// The CHAP password is in the target line; traces here say whether one is
// set, never what it is.

// ==================== ACL Management ====================

// AddInitiator adds an allowed initiator to the iSCSI gateway
func (i *iSCSIManager) AddInitiator(ctx context.Context, resource, initiatorIQN string) error {
	log := i.log()
	log.Debug("iSCSIManager.AddInitiator: start", zap.String("resource", resource), zap.String("iqn", initiatorIQN))
	i.logger.Info("Adding initiator ACL",
		zap.String("resource", resource),
		zap.String("iqn", initiatorIQN))

	// LIO refuses a malformed name only when the target next starts, which
	// leaves the gateway down; see validateIQN.
	if err := validateIQN(initiatorIQN); err != nil {
		log.Debug("iSCSIManager.AddInitiator: invalid IQN", zap.String("iqn", initiatorIQN), zap.Error(err))
		return invalidArgument(err)
	}

	pluginID := fmt.Sprintf("haify-iscsi-%s", resource)
	cfg, err := i.readGatewayConfig(ctx, resource, pluginID)
	if err != nil {
		log.Debug("iSCSIManager.AddInitiator: reading config failed", zap.String("resource", resource), zap.Error(err))
		return err
	}
	content := cfg.content

	lines, trailingNewline := splitConfigLines(content)
	targetIdx := findLineIndex(lines, func(line string) bool {
		_, ok := parseISCSITargetLine(line)
		return ok
	})
	if targetIdx < 0 {
		log.Debug("iSCSIManager.AddInitiator: no target line in config", zap.String("resource", resource))
		return fmt.Errorf("failed to locate iSCSI target definition")
	}

	params, _ := parseISCSITargetLine(lines[targetIdx])
	allowed := parseAllowedList(params["allowed_initiators"])
	log.Debug("iSCSIManager.AddInitiator: current allow-list",
		zap.String("resource", resource), zap.Strings("allowed", allowed), zap.Bool("allows_all", len(allowed) == 0))
	allowed = append(allowed, initiatorIQN)
	lines[targetIdx] = buildISCSITargetLine(
		params["iqn"],
		params["portals"],
		params["incoming_username"],
		params["incoming_password"],
		formatAllowedList(allowed),
		params["implementation"],
	)

	return i.persistGatewayConfig(ctx, resource, pluginID, cfg, joinConfigLines(lines, trailingNewline))
}

// RemoveInitiator removes an initiator from the iSCSI gateway
func (i *iSCSIManager) RemoveInitiator(ctx context.Context, resource, initiatorIQN string) error {
	log := i.log()
	log.Debug("iSCSIManager.RemoveInitiator: start", zap.String("resource", resource), zap.String("iqn", initiatorIQN))
	i.logger.Info("Removing initiator ACL",
		zap.String("resource", resource),
		zap.String("iqn", initiatorIQN))

	pluginID := fmt.Sprintf("haify-iscsi-%s", resource)
	cfg, err := i.readGatewayConfig(ctx, resource, pluginID)
	if err != nil {
		log.Debug("iSCSIManager.RemoveInitiator: reading config failed", zap.String("resource", resource), zap.Error(err))
		return err
	}
	content := cfg.content

	lines, trailingNewline := splitConfigLines(content)
	targetIdx := findLineIndex(lines, func(line string) bool {
		_, ok := parseISCSITargetLine(line)
		return ok
	})
	if targetIdx < 0 {
		log.Debug("iSCSIManager.RemoveInitiator: no target line in config", zap.String("resource", resource))
		return fmt.Errorf("failed to locate iSCSI target definition")
	}

	params, _ := parseISCSITargetLine(lines[targetIdx])
	current := parseAllowedList(params["allowed_initiators"])
	if len(current) == 0 {
		log.Debug("iSCSIManager.RemoveInitiator: target allows every initiator, refusing", zap.String("resource", resource))
		return fmt.Errorf("gateway currently allows all initiators; cannot remove a specific initiator without first defining an explicit ACL")
	}

	updated := removeValue(current, initiatorIQN)
	if len(updated) == len(current) {
		log.Debug("iSCSIManager.RemoveInitiator: initiator not in allow-list",
			zap.String("resource", resource), zap.String("iqn", initiatorIQN), zap.Strings("allowed", current))
		return fmt.Errorf("initiator not found: %s", initiatorIQN)
	}
	log.Debug("iSCSIManager.RemoveInitiator: allow-list updated",
		zap.String("resource", resource), zap.Strings("allowed", updated))

	lines[targetIdx] = buildISCSITargetLine(
		params["iqn"],
		params["portals"],
		params["incoming_username"],
		params["incoming_password"],
		formatAllowedList(updated),
		params["implementation"],
	)

	return i.persistGatewayConfig(ctx, resource, pluginID, cfg, joinConfigLines(lines, trailingNewline))
}

// ListInitiators lists all initiators for an iSCSI gateway
func (i *iSCSIManager) ListInitiators(ctx context.Context, resource string) ([]string, error) {
	log := i.log()
	log.Debug("iSCSIManager.ListInitiators: start", zap.String("resource", resource))
	pluginID := fmt.Sprintf("haify-iscsi-%s", resource)
	cfg, err := i.readGatewayConfig(ctx, resource, pluginID)
	if err != nil {
		log.Debug("iSCSIManager.ListInitiators: reading config failed", zap.String("resource", resource), zap.Error(err))
		return nil, err
	}
	content := cfg.content

	for _, line := range strings.Split(content, "\n") {
		if params, ok := parseISCSITargetLine(line); ok {
			allowed := parseAllowedList(params["allowed_initiators"])
			if len(allowed) == 0 {
				log.Debug("iSCSIManager.ListInitiators: target allows every initiator", zap.String("resource", resource))
				return []string{"ALL"}, nil
			}
			log.Debug("iSCSIManager.ListInitiators: allow-list", zap.String("resource", resource), zap.Strings("allowed", allowed))
			return allowed, nil
		}
	}

	log.Debug("iSCSIManager.ListInitiators: no target line in config", zap.String("resource", resource))
	return nil, fmt.Errorf("failed to locate iSCSI target definition")
}

// ==================== CHAP Authentication ====================

// SetCHAP sets CHAP authentication for the iSCSI gateway
func (i *iSCSIManager) SetCHAP(ctx context.Context, resource, username, password string, mutual bool) error {
	log := i.log()
	log.Debug("iSCSIManager.SetCHAP: start",
		zap.String("resource", resource), zap.String("username", username),
		zap.Bool("has_password", password != ""), zap.Bool("mutual", mutual))
	i.logger.Info("Setting CHAP authentication",
		zap.String("resource", resource),
		zap.String("username", username),
		zap.Bool("mutual", mutual))

	if mutual {
		log.Debug("iSCSIManager.SetCHAP: mutual CHAP refused", zap.String("resource", resource))
		return fmt.Errorf("mutual CHAP is not supported by the current iSCSI gateway config writer")
	}

	pluginID := fmt.Sprintf("haify-iscsi-%s", resource)
	cfg, err := i.readGatewayConfig(ctx, resource, pluginID)
	if err != nil {
		log.Debug("iSCSIManager.SetCHAP: reading config failed", zap.String("resource", resource), zap.Error(err))
		return err
	}
	content := cfg.content

	lines, trailingNewline := splitConfigLines(content)
	targetIdx := findLineIndex(lines, func(line string) bool {
		_, ok := parseISCSITargetLine(line)
		return ok
	})
	if targetIdx < 0 {
		log.Debug("iSCSIManager.SetCHAP: no target line in config", zap.String("resource", resource))
		return fmt.Errorf("failed to locate iSCSI target definition")
	}

	params, _ := parseISCSITargetLine(lines[targetIdx])
	log.Debug("iSCSIManager.SetCHAP: replacing credentials",
		zap.String("resource", resource), zap.Bool("had_username", params["incoming_username"] != ""),
		zap.Bool("had_password", params["incoming_password"] != ""))
	lines[targetIdx] = buildISCSITargetLine(
		params["iqn"],
		params["portals"],
		username,
		password,
		formatAllowedList(parseAllowedList(params["allowed_initiators"])),
		params["implementation"],
	)

	return i.persistGatewayConfig(ctx, resource, pluginID, cfg, joinConfigLines(lines, trailingNewline))
}

// GetCHAP gets CHAP authentication settings
func (i *iSCSIManager) GetCHAP(ctx context.Context, resource string) (username, password string, mutual bool, err error) {
	log := i.log()
	log.Debug("iSCSIManager.GetCHAP: start", zap.String("resource", resource))
	pluginID := fmt.Sprintf("haify-iscsi-%s", resource)
	cfg, err := i.readGatewayConfig(ctx, resource, pluginID)
	if err != nil {
		log.Debug("iSCSIManager.GetCHAP: reading config failed", zap.String("resource", resource), zap.Error(err))
		return "", "", false, err
	}
	content := cfg.content

	for _, line := range strings.Split(content, "\n") {
		if params, ok := parseISCSITargetLine(line); ok {
			log.Debug("iSCSIManager.GetCHAP: found",
				zap.String("resource", resource), zap.String("username", params["incoming_username"]),
				zap.Bool("has_password", params["incoming_password"] != ""))
			return params["incoming_username"], params["incoming_password"], false, nil
		}
	}

	log.Debug("iSCSIManager.GetCHAP: no target line in config", zap.String("resource", resource))
	return "", "", false, fmt.Errorf("failed to locate iSCSI target definition")
}
