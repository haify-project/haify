package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/haify-project/haify/pkg/database"
	"go.uber.org/zap"
)

// AdoptResult summarizes what AdoptResource recorded, so callers can display it.
type AdoptResult struct {
	Name     string
	Nodes    []string
	Port     uint32
	Protocol string
	Volumes  int
}

// AdoptResource imports an already-existing (foreign) DRBD resource — one
// created outside Haify, e.g. a hand-configured resource — into Haify management by
// writing its metadata into the Haify database. It lets subsequent Haify operations
// (MakeHa, gateways, ...) that require db.GetResource work against it.
//
// It NEVER creates or modifies the DRBD resource, its backing devices, or any
// data: no drbdadm create-md/up/down, no lvcreate/mkfs, no writes to the
// backing device. It only reads the live /etc/drbd.d/<name>.res on a reachable
// node and records what it finds. When nodes/port/protocol are supplied they
// override the auto-discovered values; anything left empty/zero is discovered
// from the .res. Adopting a resource that is already recorded simply refreshes
// the record (idempotent, no error).
func (rm *ResourceManager) AdoptResource(ctx context.Context, name string, nodes []string, port uint32, protocol string) (*AdoptResult, error) {
	log := rm.resourcesLog()
	log.Debug("AdoptResource: start", zap.String("resource", name), zap.Strings("nodes", nodes),
		zap.Uint32("port", port), zap.String("protocol", protocol))
	if rm.deployment == nil {
		log.Debug("AdoptResource: no deployment client", zap.String("resource", name))
		return nil, fmt.Errorf("deployment client not set")
	}
	if rm.controller.db == nil {
		log.Debug("AdoptResource: no database", zap.String("resource", name))
		return nil, fmt.Errorf("database not available")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		log.Debug("AdoptResource: no resource name")
		return nil, fmt.Errorf("resource name is required")
	}

	// Normalize an explicit --nodes list (trim, drop blanks).
	var flagNodes []string
	for _, n := range nodes {
		if n = strings.TrimSpace(n); n != "" {
			flagNodes = append(flagNodes, n)
		}
	}

	// Ordered candidate hosts to read the .res from: an explicit --nodes flag
	// wins, then every registered node, then any statically configured host.
	candidates := rm.adoptCandidateHosts(ctx, flagNodes)
	if len(candidates) == 0 {
		log.Debug("AdoptResource: no candidate host", zap.String("resource", name))
		return nil, fmt.Errorf("no reachable node to read DRBD config for %q: register a node or pass --nodes", name)
	}

	// Read the live config and confirm the resource actually exists. This is a
	// pure read — it never mutates the resource.
	configContent, usedHost, err := rm.readForeignResourceConfig(ctx, candidates, name)
	if err != nil {
		log.Debug("AdoptResource: could not read config", zap.String("resource", name), zap.Error(err))
		return nil, err
	}

	// Auto-discover from the config; flags override.
	discNodes := parseResourceConfigNodes(configContent)
	discPort := parsePortFromConfig(configContent)
	cfgVolumes := parseResourceConfigVolumes(configContent)
	log.Debug("AdoptResource: discovered from config", zap.String("resource", name), zap.String("host", usedHost),
		zap.Strings("nodes", discNodes), zap.Uint32("port", discPort), zap.Int("volumes", len(cfgVolumes)))

	adoptNodes := flagNodes
	if len(adoptNodes) == 0 {
		adoptNodes = discNodes
	}
	if len(adoptNodes) == 0 {
		log.Debug("AdoptResource: no nodes determined", zap.String("resource", name))
		return nil, fmt.Errorf("could not determine nodes for %q from %s:/etc/drbd.d/%s.res; pass --nodes", name, usedHost, name)
	}
	if port == 0 {
		port = discPort
	}
	if protocol == "" {
		protocol = "C"
	}
	log.Debug("AdoptResource: adopting with", zap.String("resource", name), zap.Strings("nodes", adoptNodes),
		zap.Bool("nodes_from_flag", len(flagNodes) > 0), zap.Uint32("port", port), zap.String("protocol", protocol))

	// Persist Haify metadata only — the DRBD resource and its data are left
	// exactly as they are.
	resRecord := &database.Resource{
		Name:     name,
		Nodes:    strings.Join(adoptNodes, ","),
		Port:     int(port),
		Protocol: protocol,
		Replicas: len(adoptNodes),
	}
	if existing, err := rm.controller.db.GetResource(ctx, name); err == nil {
		log.Debug("AdoptResource: refreshing existing record", zap.String("resource", name))
		resRecord.Profile = existing.Profile
		resRecord.Labels = cloneStringMap(existing.Labels)
		resRecord.CreatedAt = existing.CreatedAt
	} else {
		log.Debug("AdoptResource: no existing record", zap.String("resource", name), zap.Error(err))
	}
	err = rm.controller.db.SaveResource(ctx, resRecord)
	log.Debug("AdoptResource: saved resource record", zap.String("resource", name), zap.Error(err))
	if err != nil {
		return nil, fmt.Errorf("record adopted resource %q: %w", name, err)
	}

	savedVolumes := 0
	seenVolID := make(map[int]bool)
	for _, v := range cfgVolumes {
		// Skip diskless volume overrides (`disk none;` in a tiebreaker's `on`
		// section) and any volume with no backing disk — they carry no data to
		// record and would duplicate the data-bearing volume of the same ID.
		if v.DiskPath == "" || v.DiskPath == "none" {
			log.Debug("AdoptResource: skipping diskless volume", zap.String("resource", name), zap.Int("volume_id", v.VolumeID))
			continue
		}
		if seenVolID[v.VolumeID] {
			log.Debug("AdoptResource: skipping duplicate volume", zap.String("resource", name), zap.Int("volume_id", v.VolumeID))
			continue
		}
		seenVolID[v.VolumeID] = true

		volumeName, pool := volumeNameAndPoolFromDiskPath(v.DiskPath)
		if volumeName == "" {
			volumeName = fmt.Sprintf("%s_vol%d", name, v.VolumeID)
		}
		// SizeGB is best-effort and left 0: adoption never probes/opens the
		// backing device to size it.
		volRecord := &database.Volume{
			ResourceName: name,
			VolumeName:   volumeName,
			VolumeID:     v.VolumeID,
			Pool:         pool,
			Device:       v.DiskPath,
		}
		err := rm.controller.db.SaveVolume(ctx, volRecord)
		log.Debug("AdoptResource: saved volume record", zap.String("resource", name), zap.Int("volume_id", v.VolumeID),
			zap.String("volume", volumeName), zap.String("pool", pool), zap.String("device", v.DiskPath), zap.Error(err))
		if err != nil {
			return nil, fmt.Errorf("record adopted volume %d of %q: %w", v.VolumeID, name, err)
		}
		savedVolumes++
	}

	rm.controller.logger.Info("Adopted foreign DRBD resource into Haify management",
		zap.String("resource", name),
		zap.Strings("nodes", adoptNodes),
		zap.Uint32("port", port),
		zap.String("protocol", protocol),
		zap.Int("volumes", savedVolumes),
		zap.String("read_from", usedHost))

	return &AdoptResult{
		Name:     name,
		Nodes:    adoptNodes,
		Port:     port,
		Protocol: protocol,
		Volumes:  savedVolumes,
	}, nil
}

// adoptCandidateHosts builds the ordered, de-duplicated list of node addresses
// to try when reading a foreign resource's .res: explicit flag nodes first,
// then every registered node, then any statically configured host.
func (rm *ResourceManager) adoptCandidateHosts(ctx context.Context, flagNodes []string) []string {
	log := rm.resourcesLog()
	var hosts []string
	seen := make(map[string]bool)
	add := func(h string) {
		if h = strings.TrimSpace(h); h == "" || seen[h] {
			return
		}
		seen[h] = true
		hosts = append(hosts, h)
	}
	for _, n := range flagNodes {
		add(rm.controller.ResolveHost(n))
	}
	if rm.controller.nodes != nil {
		list, err := rm.controller.nodes.ListNodes(ctx)
		log.Debug("adoptCandidateHosts: listed registered nodes", zap.Int("count", len(list)), zap.Error(err))
		if err == nil {
			for _, n := range list {
				add(n.Address)
			}
		}
	}
	for _, h := range rm.GetHosts() {
		add(h)
	}
	log.Debug("adoptCandidateHosts: candidates", zap.Strings("flag_nodes", flagNodes), zap.Strings("hosts", hosts))
	return hosts
}

// readForeignResourceConfig reads /etc/drbd.d/<name>.res from the first
// candidate host that actually has it. It validates that the resource exists
// (a present, non-empty .res that declares the resource) so adoption never
// fabricates a record for a resource that is not really there. Returns the
// config content and the host it was read from.
func (rm *ResourceManager) readForeignResourceConfig(ctx context.Context, hosts []string, name string) (string, string, error) {
	log := rm.resourcesLog()
	cmd := fmt.Sprintf("cat /etc/drbd.d/%s.res 2>/dev/null || true", name)
	var lastErr error
	for _, host := range hosts {
		start := time.Now()
		result, err := rm.deployment.Exec(ctx, []string{host}, cmd)
		log.Debug("readForeignResourceConfig: read from host", zap.String("resource", name), zap.String("host", host),
			zap.Array("failed_hosts", resourcesFailedHosts{result}), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			lastErr = err
			continue
		}
		var content string
		for _, hr := range result.Hosts {
			if hr != nil && hr.Success {
				content = hr.Output
				break
			}
		}
		if strings.Contains(content, "resource "+name) ||
			(strings.Contains(content, "resource ") && strings.Contains(content, "on ")) {
			log.Debug("readForeignResourceConfig: found config", zap.String("resource", name), zap.String("host", host),
				zap.Int("bytes", len(content)))
			return content, host, nil
		}
		log.Debug("readForeignResourceConfig: no config for resource there", zap.String("resource", name), zap.String("host", host))
	}
	if lastErr != nil {
		log.Debug("readForeignResourceConfig: read failed", zap.String("resource", name), zap.Error(lastErr))
		return "", "", fmt.Errorf("read DRBD config for %q: %w", name, lastErr)
	}
	log.Debug("readForeignResourceConfig: resource not found on any host", zap.String("resource", name), zap.Int("hosts", len(hosts)))
	return "", "", fmt.Errorf("DRBD resource %q not found: no /etc/drbd.d/%s.res on any reachable node — refusing to adopt a resource that does not exist", name, name)
}
