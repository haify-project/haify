package gateway

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"go.uber.org/zap"
)

// ==================== Multi-Export Support ====================
//
// Exports added to and removed from an NFS gateway that already exists: a
// read-modify-write of its promoter config, like the iSCSI LUN edits.

// AddNFSExport adds an additional export to an existing NFS gateway
func (n *NFSManager) AddNFSExport(ctx context.Context, resource, exportPath string, fsid int, clientSpec, options string) error {
	log := n.log()
	log.Debug("NFSManager.AddNFSExport: start",
		zap.String("resource", resource), zap.String("export_path", exportPath), zap.Int("fsid", fsid),
		zap.String("clientspec", clientSpec), zap.String("options", options))
	n.logger.Info("Adding NFS export",
		zap.String("resource", resource),
		zap.String("export_path", exportPath))

	pluginID := fmt.Sprintf("haify-nfs-%s", resource)
	cfg, err := n.readGatewayConfig(ctx, resource, pluginID)
	if err != nil {
		log.Debug("NFSManager.AddNFSExport: reading config failed", zap.String("resource", resource), zap.Error(err))
		return err
	}
	content := cfg.content

	lines, trailingNewline := splitConfigLines(content)
	exportID := nextExportID(lines)
	if fsid <= 0 {
		log.Debug("NFSManager.AddNFSExport: no fsid given, using the export id", zap.Int("fsid", exportID))
		fsid = exportID
	}
	if clientSpec == "" {
		log.Debug("NFSManager.AddNFSExport: no clientspec given, allowing every client")
		clientSpec = "0.0.0.0/0.0.0.0"
	}
	if options == "" {
		log.Debug("NFSManager.AddNFSExport: no options given, using the defaults")
		options = "rw,all_squash,anonuid=0,anongid=0"
	}

	resolvedPath, err := ResolveNFSExportPath(resource, exportPath)
	if err != nil {
		log.Debug("NFSManager.AddNFSExport: invalid export path", zap.String("export_path", exportPath), zap.Error(err))
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
		log.Debug("NFSManager.AddNFSExport: no nfsserver entry in config", zap.String("resource", resource))
		return fmt.Errorf("failed to locate the nfsserver entry in %s", gatewayConfigPath(pluginID))
	}
	log.Debug("NFSManager.AddNFSExport: inserting export line",
		zap.String("resource", resource), zap.Int("export_id", exportID), zap.String("directory", resolvedPath),
		zap.Int("fsid", fsid), zap.Int("after_line", anchor))
	lines = append(lines[:anchor+1], append([]string{newLine}, lines[anchor+1:]...)...)

	return n.persistGatewayConfig(ctx, resource, pluginID, cfg, joinConfigLines(lines, trailingNewline))
}

// RemoveNFSExport removes an export from an existing NFS gateway.
func (n *NFSManager) RemoveNFSExport(ctx context.Context, resource, exportPath string) error {
	log := n.log()
	log.Debug("NFSManager.RemoveNFSExport: start", zap.String("resource", resource), zap.String("export_path", exportPath))
	n.logger.Info("Removing NFS export",
		zap.String("resource", resource),
		zap.String("export_path", exportPath))

	pluginID := fmt.Sprintf("haify-nfs-%s", resource)
	cfg, err := n.readGatewayConfig(ctx, resource, pluginID)
	if err != nil {
		log.Debug("NFSManager.RemoveNFSExport: reading config failed", zap.String("resource", resource), zap.Error(err))
		return err
	}
	content := cfg.content

	normalizedPath, err := ResolveNFSExportPath(resource, exportPath)
	if err != nil {
		log.Debug("NFSManager.RemoveNFSExport: invalid export path", zap.String("export_path", exportPath), zap.Error(err))
		return err
	}
	lines, trailingNewline := splitConfigLines(content)
	lines, removed := removeLine(lines, func(line string) bool {
		params, ok := parseNFSExportLine(line)
		return ok && params["directory"] == normalizedPath
	})
	if !removed {
		log.Debug("NFSManager.RemoveNFSExport: export not in config", zap.String("resource", resource), zap.String("directory", normalizedPath))
		return fmt.Errorf("export not found: %s", normalizedPath)
	}

	return n.persistGatewayConfig(ctx, resource, pluginID, cfg, joinConfigLines(lines, trailingNewline))
}

// ListNFSExports lists all exports for an NFS gateway
func (n *NFSManager) ListNFSExports(ctx context.Context, resource string) ([]map[string]string, error) {
	log := n.log()
	log.Debug("NFSManager.ListNFSExports: start", zap.String("resource", resource))
	pluginID := fmt.Sprintf("haify-nfs-%s", resource)
	cfg, err := n.readGatewayConfig(ctx, resource, pluginID)
	if err != nil {
		log.Debug("NFSManager.ListNFSExports: reading config failed", zap.String("resource", resource), zap.Error(err))
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
		log.Debug("NFSManager.ListNFSExports: export",
			zap.String("resource", resource), zap.String("directory", params["directory"]), zap.String("clientspec", params["clientspec"]))
		exports = append(exports, map[string]string{
			"directory":  params["directory"],
			"fsid":       params["fsid"],
			"clientspec": params["clientspec"],
			"options":    params["options"],
		})
	}

	log.Debug("NFSManager.ListNFSExports: done", zap.String("resource", resource), zap.Int("count", len(exports)))
	return exports, nil
}
