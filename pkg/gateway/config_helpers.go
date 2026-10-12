package gateway

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
)

func gatewayConfigPath(pluginID string) string {
	return filepath.Join(DrbdReactorConfigDir, fmt.Sprintf("%s.toml", pluginID))
}

// persistGatewayConfig writes an edited gateway config back to the nodes that
// may run the gateway (see readGatewayConfig for where it was read from).
//
// A gateway that is not stopped goes through editRunningGateway, which writes
// the config without restarting the gateway and applies the edit where it
// runs (see live_edit.go). A stopped gateway's config goes back to the
// .toml.disabled copy and nothing is reloaded or applied: writing the live
// .toml would start the gateway as a side effect of the edit.
func (m *Manager) persistGatewayConfig(ctx context.Context, resource, pluginID string, cfg *gatewayConfig, content string) error {
	log := m.log()
	log.Debug("persistGatewayConfig: start",
		zap.String("resource", resource), zap.String("plugin", pluginID),
		zap.Bool("disabled", cfg.disabled), zap.Int("size_bytes", len(content)))
	if !cfg.disabled {
		log.Debug("persistGatewayConfig: gateway not stopped, editing it live", zap.String("resource", resource))
		return m.editRunningGateway(ctx, resource, pluginID, cfg.content, content)
	}
	run, _ := m.promoterHosts(ctx, resource)
	path := gatewayConfigPath(pluginID) + disabledSuffix
	start := time.Now()
	if err := m.deployment.DistributeConfig(ctx, run, content, path); err != nil {
		log.Debug("persistGatewayConfig: writing the disabled copy failed",
			zap.Strings("hosts", run), zap.String("path", path), zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("failed to write config: %w", err)
	}
	log.Debug("persistGatewayConfig: disabled copy written",
		zap.Strings("hosts", run), zap.String("path", path), zap.Duration("took", time.Since(start)))
	return nil
}

func splitConfigLines(content string) ([]string, bool) {
	hasTrailingNewline := strings.HasSuffix(content, "\n")
	trimmed := strings.TrimSuffix(content, "\n")
	if trimmed == "" {
		return []string{}, hasTrailingNewline
	}
	return strings.Split(trimmed, "\n"), hasTrailingNewline
}

func joinConfigLines(lines []string, trailingNewline bool) string {
	if len(lines) == 0 {
		if trailingNewline {
			return "\n"
		}
		return ""
	}

	content := strings.Join(lines, "\n")
	if trailingNewline {
		content += "\n"
	}
	return content
}

func findLineIndex(lines []string, match func(string) bool) int {
	for idx, line := range lines {
		if match(line) {
			return idx
		}
	}
	return -1
}

func insertLineBefore(lines []string, newLine string, match func(string) bool) ([]string, error) {
	idx := findLineIndex(lines, match)
	if idx < 0 {
		zap.L().Debug("insertLineBefore: no line matches the insertion point", zap.Int("count", len(lines)))
		return nil, fmt.Errorf("failed to find insertion point")
	}

	lines = append(lines[:idx], append([]string{newLine}, lines[idx:]...)...)
	return lines, nil
}

func removeLine(lines []string, match func(string) bool) ([]string, bool) {
	for idx, line := range lines {
		if match(line) {
			return append(lines[:idx], lines[idx+1:]...), true
		}
	}
	return lines, false
}

func splitKeyValueTokens(content string) []string {
	return strings.Fields(strings.TrimSpace(content))
}

func parseOCFParams(content string, skipTokens int) map[string]string {
	fields := splitKeyValueTokens(content)
	if len(fields) <= skipTokens {
		return map[string]string{}
	}

	params := make(map[string]string)
	currentKey := ""
	for _, field := range fields[skipTokens:] {
		if strings.Contains(field, "=") {
			parts := strings.SplitN(field, "=", 2)
			currentKey = parts[0]
			params[currentKey] = parts[1]
			continue
		}

		if currentKey == "" {
			continue
		}

		if params[currentKey] == "" {
			params[currentKey] = field
		} else {
			params[currentKey] += " " + field
		}
	}

	// A value written quoted (see ocfListValue) comes back with its quotes,
	// escaped for TOML; the value itself is what is between them.
	for k, v := range params {
		params[k] = strings.Trim(strings.ReplaceAll(v, `\"`, `"`), `"`)
	}
	return params
}

// ocfListValue renders a space-separated OCF parameter value. drbd-reactor
// splits an OCF start line into name=value pairs with shell-word rules, so a
// list of two or more entries must be double-quoted to reach the agent as one
// value; unquoted, every entry after the first became a parameter of its own
// (`OCF_RESKEY_iqn.2026-10.test:probe=`) and silently left the allow-list. The
// quotes are escaped because the line sits inside a TOML string.
func ocfListValue(values []string) string {
	joined := strings.Join(values, " ")
	if len(values) < 2 {
		return joined
	}
	return `\"` + joined + `\"`
}

func quotedCommandContent(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	trimmed = strings.TrimSuffix(trimmed, ",")
	trimmed = strings.Trim(trimmed, "\"")
	if trimmed == "" {
		return "", false
	}
	return trimmed, true
}

func parseISCSITargetLine(line string) (map[string]string, bool) {
	content, ok := quotedCommandContent(line)
	if !ok || !strings.Contains(content, "ocf:heartbeat:iSCSITarget ") {
		return nil, false
	}
	return parseOCFParams(content, 2), true
}

func buildISCSITargetLine(iqn, portals, username, password string, allowed []string, implementation string) string {
	return fmt.Sprintf(
		`        "ocf:heartbeat:iSCSITarget target iqn=%s portals=%s incoming_username=%s incoming_password=%s allowed_initiators=%s implementation=%s",`,
		iqn,
		portals,
		username,
		password,
		ocfListValue(allowed),
		implementation,
	)
}

func parseISCSILUNLine(line string) (map[string]string, bool) {
	content, ok := quotedCommandContent(line)
	if !ok || !strings.Contains(content, "ocf:heartbeat:iSCSILogicalUnit ") {
		return nil, false
	}
	return parseOCFParams(content, 2), true
}

// buildISCSILUNLine builds a LUN line. implementation is the target's: the
// iSCSILogicalUnit agent otherwise picks its own, preferring ietadm and tgtadm
// over targetcli, so a node that also has tgt installed would add the LUN to a
// target that does not exist.
func buildISCSILUNLine(lunNumber int, iqn, device, implementation string) string {
	serial := generateSerialFromIQN(iqn, lunNumber)
	line := fmt.Sprintf(
		`ocf:heartbeat:iSCSILogicalUnit lu%d target_iqn=%s lun=%d path=%s product_id=%s scsi_sn=%s`,
		lunNumber,
		iqn,
		lunNumber,
		device,
		serial,
		serial,
	)
	if implementation != "" {
		line += " implementation=" + implementation
	}
	return `        "` + line + `",`
}

func parseNFSExportLine(line string) (map[string]string, bool) {
	content, ok := quotedCommandContent(line)
	if !ok || !strings.Contains(content, "ocf:heartbeat:exportfs ") {
		return nil, false
	}
	return parseOCFParams(content, 2), true
}

func buildNFSExportLine(exportID int, directory, fsid, clientSpec, options string) string {
	return fmt.Sprintf(
		`        "ocf:heartbeat:exportfs export_%d_0 directory=%s fsid=%s clientspec=%s options=%s",`,
		exportID,
		directory,
		fsid,
		clientSpec,
		options,
	)
}

func parseNVMeSubsystemLine(line string) (map[string]string, bool) {
	content, ok := quotedCommandContent(line)
	if !ok || !strings.Contains(content, "ocf:heartbeat:nvmet-subsystem ") {
		return nil, false
	}
	return parseOCFParams(content, 2), true
}

func buildNVMeSubsystemLine(nqn string, allowed []string, serial string) string {
	if len(allowed) == 0 {
		return fmt.Sprintf(`        "ocf:heartbeat:nvmet-subsystem subsys nqn=%s serial=%s",`, nqn, serial)
	}
	return fmt.Sprintf(
		`        "ocf:heartbeat:nvmet-subsystem subsys nqn=%s allowed_initiators=%s serial=%s",`,
		nqn,
		ocfListValue(allowed),
		serial,
	)
}

func parseNVMeNamespaceLine(line string) (map[string]string, bool) {
	content, ok := quotedCommandContent(line)
	if !ok || !strings.Contains(content, "ocf:heartbeat:nvmet-namespace ") {
		return nil, false
	}
	return parseOCFParams(content, 2), true
}

func parseNVMePortLine(line string) (map[string]string, bool) {
	content, ok := quotedCommandContent(line)
	if !ok || !strings.Contains(content, "ocf:heartbeat:nvmet-port ") {
		return nil, false
	}
	return parseOCFParams(content, 2), true
}

func buildNVMeNamespaceLine(namespaceID int, nqn, device string) string {
	uuid := generateUUID()
	return fmt.Sprintf(
		`        "ocf:heartbeat:nvmet-namespace ns_%d nqn=%s namespace_id=%d backing_path=%s uuid=%s nguid=%s",`,
		namespaceID,
		nqn,
		namespaceID,
		device,
		uuid,
		uuid,
	)
}

func uniqueSortedValues(values []string) []string {
	seen := make(map[string]struct{})
	var result []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	slices.Sort(result)
	return result
}

func removeValue(values []string, target string) []string {
	target = strings.TrimSpace(target)
	var result []string
	for _, value := range values {
		if strings.TrimSpace(value) == target {
			continue
		}
		result = append(result, strings.TrimSpace(value))
	}
	return result
}

func parseAllowedList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.EqualFold(raw, "ALL") {
		return nil
	}
	return uniqueSortedValues(strings.Fields(raw))
}

func formatAllowedList(values []string) []string {
	values = uniqueSortedValues(values)
	if len(values) == 0 {
		return nil
	}
	return values
}

func parseIntParam(params map[string]string, key string) (int, error) {
	value, ok := params[key]
	if !ok || strings.TrimSpace(value) == "" {
		zap.L().Debug("parseIntParam: parameter missing", zap.String("key", key))
		return 0, fmt.Errorf("missing %s", key)
	}
	return strconv.Atoi(strings.TrimSpace(value))
}

func nextExportID(lines []string) int {
	maxID := 0
	for _, line := range lines {
		content, ok := quotedCommandContent(line)
		if !ok || !strings.Contains(content, "ocf:heartbeat:exportfs ") {
			continue
		}

		fields := splitKeyValueTokens(content)
		if len(fields) < 2 || !strings.HasPrefix(fields[1], "export_") {
			continue
		}

		parts := strings.Split(fields[1], "_")
		if len(parts) < 2 {
			continue
		}

		id, err := strconv.Atoi(parts[1])
		if err == nil && id > maxID {
			maxID = id
		}
	}

	zap.L().Debug("nextExportID: chosen", zap.Int("export_id", maxID+1))
	return maxID + 1
}

// ResolveNFSExportPath turns the user-supplied export path into the
// directory that is mounted and exported. An absolute path is honored
// verbatim — when an admin asks for /srv/nfs-test, clients mount
// <vip>:/srv/nfs-test, not a path nested under the gateway base directory.
// Relative or empty paths land under DefaultExportBasePath/<resource> so
// quick setups stay grouped and collision-free.
func ResolveNFSExportPath(resource, exportPath string) (string, error) {
	log := zap.L()
	exportPath = strings.TrimSpace(exportPath)
	if exportPath == "" {
		log.Debug("ResolveNFSExportPath: no path given, using the default", zap.String("resource", resource))
		return filepath.Join(DefaultExportBasePath, resource), nil
	}
	if !strings.HasPrefix(exportPath, "/") {
		log.Debug("ResolveNFSExportPath: relative path, placed under the default",
			zap.String("resource", resource), zap.String("export_path", exportPath))
		return filepath.Join(DefaultExportBasePath, resource, exportPath), nil
	}

	cleaned := filepath.Clean(exportPath)
	if cleaned == "/" {
		log.Debug("ResolveNFSExportPath: refusing the filesystem root", zap.String("resource", resource))
		return "", fmt.Errorf("export path must not be the filesystem root")
	}
	// The export directory becomes a mount point owned by the gateway;
	// refuse paths that would shadow system directories or Haify state.
	forbidden := []string{"/bin", "/boot", "/dev", "/etc", "/lib", "/proc", "/root", "/run", "/sbin", "/sys", "/usr", "/var/lib/haify", DefaultClusterPrivateMountPath}
	for _, prefix := range forbidden {
		if cleaned == prefix || strings.HasPrefix(cleaned, prefix+"/") {
			log.Debug("ResolveNFSExportPath: path would shadow a system directory",
				zap.String("export_path", cleaned), zap.String("shadowed", prefix))
			return "", fmt.Errorf("export path %s would shadow %s; choose a dedicated directory (e.g. /srv/...)", cleaned, prefix)
		}
	}
	log.Debug("ResolveNFSExportPath: absolute path accepted", zap.String("resource", resource), zap.String("export_path", cleaned))
	return cleaned, nil
}
