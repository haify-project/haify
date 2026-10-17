package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/haify-project/haify/pkg/alert"
	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/deployment"
	"github.com/haify-project/haify/pkg/wanproxy"
	"go.uber.org/zap"
)

// GetResource gets resource information from database with live status
func (rm *ResourceManager) GetResource(ctx context.Context, name string) (*ResourceInfo, error) {
	log := rm.resourcesLog()
	log.Debug("GetResource: start", zap.String("resource", name))
	if rm.controller.db == nil {
		log.Debug("GetResource: no database", zap.String("resource", name))
		return nil, fmt.Errorf("database not available")
	}

	hosts, err := rm.resourceHosts(ctx, name)
	if err != nil {
		log.Debug("GetResource: no hosts", zap.String("resource", name), zap.Error(err))
		return nil, err
	}

	// Get resource info from database
	dbRes, err := rm.controller.db.GetResource(ctx, name)
	if err != nil {
		log.Debug("GetResource: not in database", zap.String("resource", name), zap.Error(err))
		return nil, fmt.Errorf("resource not found: %s", name)
	}

	dbVolumes, err := rm.controller.db.ListVolumes(ctx, name)
	log.Debug("GetResource: listed volume records", zap.String("resource", name), zap.Int("count", len(dbVolumes)), zap.Error(err))
	if err != nil {
		rm.controller.logger.Warn("Failed to list resource volumes from database",
			zap.String("resource", name),
			zap.Error(err))
		dbVolumes = nil
	}
	dbVolumeByID := make(map[int]*database.Volume, len(dbVolumes))
	for _, volume := range dbVolumes {
		dbVolumeByID[volume.VolumeID] = volume
	}

	// Parse nodeAddresses from comma-separated string
	var nodeAddresses []string
	if dbRes.Nodes != "" {
		nodeAddresses = strings.Split(dbRes.Nodes, ",")
	}

	var disklessNodes []string
	if dbRes.DisklessNodes != "" {
		disklessNodes = strings.Split(dbRes.DisklessNodes, ",")
	}

	rm.controller.logger.Debug("GetResource",
		zap.String("name", name),
		zap.String("dbRes.Nodes", dbRes.Nodes),
		zap.Strings("parsed_nodeAddresses", nodeAddresses))

	// Ask hosts in turn until one answers. Asking only hosts[0] made a resource
	// entirely unobservable whenever its first node was down — role Unknown, no
	// node states, every peer greyed out — while healthy peers sat there able to
	// answer. Any live replica reports the whole resource, so which one replies
	// does not matter; that it is reachable does.
	// answeredAt indexes both hosts and nodeAddresses: resourceHosts builds them
	// in the same order, so the node that answered can be named. Both parsers
	// need that name — the status output describes the answering node as
	// "local", and filing it under the wrong node reports a machine that is
	// down as healthy.
	result, answeredAt, err := rm.drbdStatusFromAnyHost(ctx, hosts, name)
	log.Debug("GetResource: status read", zap.String("resource", name), zap.Int("answered_at", answeredAt), zap.Error(err))

	var volumes []*ResourceVolumeInfo
	nodeStates := make(map[string]*ResourceNodeState)
	localRole := "Unknown"

	if err == nil {
		for _, r := range result.Hosts {
			if r.Success {
				rm.controller.logger.Debug("DRBD status output",
					zap.String("output", r.Output))

				// Parse local node role
				localRole = parseRoleFromStatus(r.Output)

				// Parse volumes
				volInfo := parseVolumesFromStatus(r.Output)
				for _, v := range volInfo {
					sizeGB := v.sizeGB
					pool := ""
					backingVolume := ""
					encrypted := false
					var sizeBytes uint64
					if dbVol, ok := dbVolumeByID[v.id]; ok {
						if sizeGB == 0 {
							sizeGB = uint64(max(dbVol.SizeGB, 0))
						}
						pool = dbVol.Pool
						backingVolume = dbVol.VolumeName
						sizeBytes = uint64(max(dbVol.SizeBytes, 0))
						// v.device is the DRBD device (/dev/drbdN); the crypt
						// layer only shows in the recorded BACKING device. The
						// resource flag is required as well, so an adopted
						// foreign resource that merely happens to live under
						// /dev/mapper is not reported as one of ours.
						encrypted = dbRes.Encrypted && luksIsMapperPath(dbVol.Device)
					}
					volumes = append(volumes, &ResourceVolumeInfo{
						VolumeID:      uint32(v.id),
						Device:        v.device,
						SizeGB:        sizeGB,
						SizeBytes:     sizeBytes,
						Pool:          pool,
						BackingVolume: backingVolume,
						Encrypted:     encrypted,
					})
				}

				// Parse node states from status output
				nodeStates = parseNodeStatesFromStatus(r.Output, nodesLocalFirst(nodeAddresses, answeredAt))

				rm.controller.logger.Debug("Parsed node states",
					zap.Int("count", len(nodeStates)))

				break
			}
		}
	}

	// Prefer structured `drbdsetup status --json`: it is keyed by node name and
	// exposes per-peer replication state and resync completion (percent) that
	// the plain-text parse above cannot surface. It must query the same host the
	// text status came from: the JSON "local" node is whichever node answered,
	// not whichever is configured first.
	// On any failure (older drbd without --json, non-zero exit, parse error) we
	// keep the text-parsed states above and degrade gracefully.
	if len(nodeAddresses) > 0 && answeredAt >= 0 && answeredAt < len(nodeAddresses) {
		localNode := nodeAddresses[answeredAt]
		start := time.Now()
		jsonResult, jerr := rm.deployment.DRBDStatusJSON(ctx, []string{hosts[answeredAt]}, name)
		log.Debug("GetResource: read JSON status", zap.String("resource", name), zap.String("host", hosts[answeredAt]),
			zap.Array("failed_hosts", resourcesFailedHosts{jsonResult}), zap.Duration("took", time.Since(start)), zap.Error(jerr))
		if jerr == nil {
			for _, r := range jsonResult.Hosts {
				if !r.Success {
					continue
				}
				parsed, perr := parseNodeStatesFromJSON(r.Output, localNode)
				if perr != nil {
					rm.controller.logger.Debug("drbdsetup status --json parse failed; keeping text-parsed states",
						zap.String("resource", name), zap.Error(perr))
					break
				}
				if len(parsed) > 0 {
					nodeStates = parsed
					if local, ok := parsed[localNode]; ok && local.Role != "" {
						localRole = local.Role
					}
				}
				log.Debug("GetResource: JSON states", zap.String("resource", name), zap.Int("count", len(parsed)),
					zap.String("role", localRole))
				break
			}
		} else {
			rm.controller.logger.Debug("drbdsetup status --json unavailable; keeping text-parsed states",
				zap.String("resource", name), zap.Error(jerr))
		}
	}

	rm.rememberPrimaryHost(name, hosts, nodeAddresses, answeredAt, localRole, nodeStates)

	info := &ResourceInfo{
		Name:            dbRes.Name,
		Port:            uint32(dbRes.Port),
		Protocol:        dbRes.Protocol,
		Nodes:           nodeAddresses,
		Role:            localRole, // Local node's role
		Volumes:         volumes,
		NodeStates:      nodeStates,
		DisklessNodes:   disklessNodes,
		DisklessClients: splitCSV(dbRes.DisklessClients),
		// Two diskful nodes with no third vote means a single failure drops
		// below quorum majority and suspends I/O. A diskless client votes as
		// a tiebreaker does.
		QuorumRisk: len(nodeAddresses) == 2 && len(disklessNodes) == 0 && dbRes.DisklessClients == "",
		Labels:     cloneStringMap(dbRes.Labels),
		Profile:    dbRes.Profile,
		WANMode:    dbRes.WANMode,
		DRNode:     dbRes.DRNode,
		Encrypted:  dbRes.Encrypted,
	}
	info.FaultDomainRisk = faultDomainRisk(nodeAddresses, disklessNodes, rm.labelsByNode(ctx), rm.faultDomainKey())

	if len(info.Volumes) == 0 && len(dbVolumes) > 0 {
		log.Debug("GetResource: no live volumes; using database records", zap.String("resource", name), zap.Int("count", len(dbVolumes)))
		for _, volume := range dbVolumes {
			info.Volumes = append(info.Volumes, volumeInfoFromRecord(dbRes, volume))
		}
	}

	log.Debug("GetResource: done", zap.String("resource", name), zap.String("role", info.Role),
		zap.Int("volumes", len(info.Volumes)), zap.Int("node_states", len(info.NodeStates)),
		zap.Bool("quorum_risk", info.QuorumRisk), zap.String("fault_domain_risk", info.FaultDomainRisk))
	return info, nil
}

// volumeInfoFromRecord is a volume as the API reports it, from its database
// record. Getting one resource and listing them all both use it: the list
// once dropped the exact size, and Proxmox, which rescans by listing, then
// recorded every exactly-sized disk as its whole GiB.
func volumeInfoFromRecord(res *database.Resource, volume *database.Volume) *ResourceVolumeInfo {
	return &ResourceVolumeInfo{
		VolumeID:      uint32(volume.VolumeID),
		Device:        fmt.Sprintf("/dev/drbd/by-res/%s/%d", res.Name, volume.VolumeID),
		SizeGB:        uint64(max(volume.SizeGB, 0)),
		SizeBytes:     uint64(max(volume.SizeBytes, 0)),
		Pool:          volume.Pool,
		BackingVolume: volume.VolumeName,
		Encrypted:     res.Encrypted && luksIsMapperPath(volume.Device),
	}
}

// ListResources lists all resources from database with live status
func (rm *ResourceManager) ListResources(ctx context.Context) ([]*ResourceInfo, error) {
	log := rm.resourcesLog()
	if rm.controller.db == nil {
		log.Debug("ListResources: no database")
		return nil, fmt.Errorf("database not available")
	}

	// Get resources from database
	dbResources, err := rm.controller.db.ListResources(ctx)
	log.Debug("ListResources: listed resource records", zap.Int("count", len(dbResources)), zap.Error(err))
	if err != nil {
		return nil, fmt.Errorf("failed to list resources from database: %w", err)
	}

	labels := rm.labelsByNode(ctx)
	var resources []*ResourceInfo
	for _, dbRes := range dbResources {
		// Parse nodeAddresses from comma-separated string
		var nodeAddresses []string
		if dbRes.Nodes != "" {
			nodeAddresses = strings.Split(dbRes.Nodes, ",")
		}

		// Volume metadata comes from the database: listing must not fan out
		// SSH status calls per resource, and the persisted records carry
		// everything the API exposes (id, size, pool, backing volume).
		var volumes []*ResourceVolumeInfo
		dbVolumes, err := rm.controller.db.ListVolumes(ctx, dbRes.Name)
		log.Debug("ListResources: resource", zap.String("resource", dbRes.Name), zap.Strings("nodes", nodeAddresses),
			zap.Int("volumes", len(dbVolumes)), zap.Error(err))
		if err == nil {
			for _, volume := range dbVolumes {
				volumes = append(volumes, volumeInfoFromRecord(dbRes, volume))
			}
		}

		resources = append(resources, &ResourceInfo{
			Name:       dbRes.Name,
			Port:       uint32(dbRes.Port),
			Protocol:   dbRes.Protocol,
			Nodes:      nodeAddresses,
			Role:       "Unknown", // Live role comes from GetResource/ResourceStatus
			Volumes:    volumes,
			NodeStates: make(map[string]*ResourceNodeState),
			// Persisted diskless membership so the list view (and the web UI)
			// can distinguish quorum tiebreakers from diskless data clients
			// without a per-resource status fan-out.
			DisklessNodes:   splitCSV(dbRes.DisklessNodes),
			DisklessClients: splitCSV(dbRes.DisklessClients),
			Labels:          cloneStringMap(dbRes.Labels),
			Profile:         dbRes.Profile,
			// Same derivations the single-resource view makes. Omitting them
			// here left the list unable to flag a two-node resource with no
			// tiebreaker, or to tell an off-site DR from a local replica —
			// precisely the things a list is for.
			QuorumRisk: len(nodeAddresses) == 2 && dbRes.DisklessNodes == "" && dbRes.DisklessClients == "",
			WANMode:    dbRes.WANMode,
			DRNode:     dbRes.DRNode,
			Encrypted:  dbRes.Encrypted,
			FaultDomainRisk: faultDomainRisk(nodeAddresses, splitCSV(dbRes.DisklessNodes), labels,
				rm.faultDomainKey()),
		})
	}

	return resources, nil
}

// GetResourceStatusList adapts ResourceManager to alert.ResourceLister by returning
// status info for all resources managed by the controller.
func (rm *ResourceManager) GetResourceStatusList(ctx context.Context) ([]alert.ResourceStatusInfo, error) {
	log := rm.resourcesLog()
	if rm.controller.db == nil {
		log.Debug("GetResourceStatusList: no database")
		return nil, fmt.Errorf("database not available")
	}
	dbResources, err := rm.controller.db.ListResources(ctx)
	log.Debug("GetResourceStatusList: listed resource records", zap.Int("count", len(dbResources)), zap.Error(err))
	if err != nil {
		return nil, err
	}

	var result []alert.ResourceStatusInfo
	for _, dbRes := range dbResources {
		info, err := rm.GetResource(ctx, dbRes.Name)
		if err != nil || info == nil {
			log.Debug("GetResourceStatusList: no status; skipping", zap.String("resource", dbRes.Name), zap.Error(err))
			continue
		}
		// Nodes that hold no local copy on purpose: quorum tiebreakers and
		// diskless clients. They report Diskless as their normal state, so the
		// monitor has to be told, or it alerts on every one of them forever.
		diskless := disklessByDesign(rm.controller, dbRes)

		item := alert.ResourceStatusInfo{
			Name:       dbRes.Name,
			NodeStates: make(map[string]alert.NodeStateInfo, len(info.NodeStates)),
			// A CSI volume is promoted only while a pod has it mounted, and a
			// Proxmox VM disk only while its VM runs.
			IdleWithoutPrimary: externallyPromoted(dbRes.Name, dbRes.Labels) != "",
			Owner:              resourceOwner(dbRes.Labels),
		}
		for node, st := range info.NodeStates {
			state := alert.NodeStateInfo{
				DiskState:        st.DiskState,
				ReplicationState: st.Replication,
				Role:             st.Role,
				ExpectedDiskless: diskless[node] || diskless[rm.controller.ResolveHost(node)],
				Quorum:           st.Quorum,
				Connection:       st.Connection,
				OutOfSyncKiB:     st.OutOfSyncKiB,
				TLS:              st.TLS,
				WrittenKiB:       st.WrittenKiB,
				ReadKiB:          st.ReadKiB,
			}
			// Only forward completion the status source actually reported. A
			// text-parsed state has none, and passing its zero on would export
			// "0% synced" for every replica of a cluster that is fully in sync.
			if st.SyncPercentKnown {
				percent := st.SyncPercent
				state.SyncPercent = &percent
			}
			// DRBD names a peer by the hostname in its config, which is not
			// always the name the node is registered and commanded by
			// (vm-node-a for node-a). Alerts are read by people who know it
			// by the latter.
			key := node
			if rm.controller.nodes != nil {
				if name := rm.controller.nodes.GetNodeNameByAddress(node); name != "" {
					key = name
				}
			}
			item.NodeStates[key] = state
		}

		// For WAN resources, fold the haify-proxy pair's health into the status so
		// the alert monitor can surface a broken cross-site link.
		if dbRes.WANMode {
			item.WANEnabled = true
			start := time.Now()
			st, err := wanproxy.StatusMulti(ctx, rm.wanproxyDeployClient(), rm.wanMultiSpecFor(dbRes))
			switch {
			case err != nil:
				item.WANHealthy = false
				item.WANMessage = err.Error()
			default:
				item.WANHealthy = st.Healthy()
				item.WANMessage = wanStatusMessage(st)
				// The proxy's counters, for the WAN gauges in /metrics: one small
				// file read from a live primary, on a poll that already talks to it.
				if host := liveWANPrimary(st); host != "" {
					item.WANProxy = wanproxy.ReadNodeMetrics(ctx, rm.wanproxyDeployClient(), host, wanLegFor(st, host))
				}
			}
			log.Debug("GetResourceStatusList: WAN proxy status", zap.String("resource", dbRes.Name),
				zap.Bool("healthy", item.WANHealthy), zap.Duration("took", time.Since(start)), zap.Error(err))
		}

		log.Debug("GetResourceStatusList: resource status", zap.String("resource", dbRes.Name),
			zap.Int("node_states", len(item.NodeStates)), zap.Bool("idle_without_primary", item.IdleWithoutPrimary))
		result = append(result, item)
	}
	log.Debug("GetResourceStatusList: done", zap.Int("count", len(result)))
	return result, nil
}

// disklessByDesign returns the set of nodes that are meant to carry no local
// replica of the resource — quorum tiebreakers (DisklessNodes) and diskless
// data clients (DisklessClients).
//
// Entries are recorded by node name, while live DRBD status can key a node by
// either name or address depending on how it was reported, so each name is
// indexed under both.
func disklessByDesign(c *Controller, dbRes *database.Resource) map[string]bool {
	out := map[string]bool{}
	for _, list := range []string{dbRes.DisklessNodes, dbRes.DisklessClients} {
		for _, n := range splitCSV(list) {
			if n == "" {
				continue
			}
			out[n] = true
			if addr := c.ResolveHost(n); addr != "" {
				out[addr] = true
			}
		}
	}
	return out
}

// drbdStatusFromAnyHost returns the first usable `drbdadm status` answer.
//
// A host is skipped both when it cannot be reached and when the command it ran
// failed: a node that answers "no resources defined" is no more informative
// than one that answers nothing. The last error is returned when none work, so
// the caller can still render the resource with unknown state rather than
// failing outright — the UI has to draw something either way.
func (rm *ResourceManager) drbdStatusFromAnyHost(ctx context.Context, hosts []string, resource string) (*deployment.ExecResult, int, error) {
	log := rm.resourcesLog()
	var lastErr error
	order := rm.statusAskOrder(resource, hosts)
	for n, i := range order {
		host := hosts[i]
		start := time.Now()
		// Every host but the last gets statusAskTimeout: a powered-off node
		// lets the SSH connection hang rather than refusing it, and another
		// replica can answer instead. The last one gets whatever ctx allows.
		askCtx, cancel := ctx, context.CancelFunc(func() {})
		if n < len(order)-1 {
			askCtx, cancel = context.WithTimeout(ctx, statusAskTimeout)
		}
		result, err := rm.deployment.DRBDStatus(askCtx, []string{host}, resource)
		cancel()
		log.Debug("drbdStatusFromAnyHost: asked host", zap.String("resource", resource), zap.String("host", host),
			zap.Array("failed_hosts", resourcesFailedHosts{result}), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			lastErr = err
			rm.markHostUnreachable(host)
			continue
		}
		if result == nil || !result.AllSuccess() {
			lastErr = fmt.Errorf("drbdadm status for %s on %s did not succeed", resource, host)
			rm.markHostUnreachable(host)
			continue
		}
		rm.markHostReachable(host)
		return result, i, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no hosts to ask for %s", resource)
	}
	log.Debug("drbdStatusFromAnyHost: no host answered", zap.String("resource", resource), zap.Error(lastErr))
	return nil, -1, lastErr
}

// nodesLocalFirst returns nodes reordered so the one at idx comes first.
//
// parseNodeStatesFromStatus treats the head of the list as the node the status
// was read from, which held while only hosts[0] was ever asked. Once any
// reachable host can answer, the caller has to say which one did.
func nodesLocalFirst(nodes []string, idx int) []string {
	if idx <= 0 || idx >= len(nodes) {
		return nodes
	}
	out := make([]string, 0, len(nodes))
	out = append(out, nodes[idx])
	for i, n := range nodes {
		if i != idx {
			out = append(out, n)
		}
	}
	return out
}
