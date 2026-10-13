package controller

// Rewriting a live resource config to take in one more diskful replica.

import (
	"fmt"
	"strings"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/wanproxy"
)

// addReplicaToConfig adds a diskful node to a live resource config.
func (rm *ResourceManager) addReplicaToConfig(content, resource, node, addr string,
	vols []*database.Volume, port, legIndex, bindPort int, wan bool, drNode string,
	encrypted bool) (string, error) {

	log := rm.controller.traceLogger()
	log.Debug("addReplicaToConfig: start", zap.String("resource", resource), zap.String("node", node),
		zap.String("address", addr), zap.Int("volumes", len(vols)), zap.Int("port", port), zap.Int("leg_index", legIndex),
		zap.Int("bind_port", bindPort), zap.Bool("wan", wan), zap.String("dr_node", drNode), zap.Bool("encrypted", encrypted))
	name := rm.controller.nodes.GetDRBDNameByRef(node)
	blocks := parseOnBlocks(content)
	for _, b := range blocks {
		if b.name == name {
			log.Debug("addReplicaToConfig: node already in config", zap.String("drbd_name", name))
			return "", fmt.Errorf("node %q is already in the config of %q", node, resource)
		}
	}
	nextID := 0
	for _, b := range blocks {
		if b.nodeID >= nextID {
			nextID = b.nodeID + 1
		}
	}
	log.Debug("addReplicaToConfig: new node identity", zap.String("drbd_name", name), zap.Int("node_id", nextID))

	var stanza strings.Builder
	fmt.Fprintf(&stanza, "\n    on %s {\n", name)
	fmt.Fprintf(&stanza, "        address   %s:%d;\n", addr, port)
	fmt.Fprintf(&stanza, "        node-id   %d;\n", nextID)
	for _, v := range vols {
		// The crypt container is what DRBD attaches on an encrypted resource.
		// Its name comes from pool and volume, which are identical on every
		// node, so the newcomer's stanza names the same device as its peers
		// even though the key behind it is its own.
		disk := fmt.Sprintf("/dev/%s/%s", v.Pool, v.VolumeName)
		if encrypted {
			disk = luksMapperPath(v.Pool, v.VolumeName)
		}
		log.Debug("addReplicaToConfig: volume stanza", zap.Int("volume", v.VolumeID), zap.String("disk", disk))
		fmt.Fprintf(&stanza, "        volume %d {\n", v.VolumeID)
		fmt.Fprintf(&stanza, "            device    minor %d;\n", minorForVolume(content, v.VolumeID))
		fmt.Fprintf(&stanza, "            disk      %s;\n", disk)
		fmt.Fprintf(&stanza, "            meta-disk internal;\n")
		fmt.Fprintf(&stanza, "        }\n")
	}
	fmt.Fprintf(&stanza, "    }\n")

	out, err := insertBeforeResourceClose(stripConnectionMesh(content), stanza.String())
	if err != nil {
		log.Debug("addReplicaToConfig: inserting the host stanza failed", zap.Error(err))
		return "", err
	}

	// Rebuild the mesh over the LAN hosts, which now include the newcomer. A
	// WAN-attached host stays out: a mesh entry would pair it with every other
	// host on that host's LAN address, which is unroutable from the other site.
	mesh := lanHostNames(parseOnBlocks(out))
	log.Debug("addReplicaToConfig: LAN mesh", zap.Strings("hosts", mesh), zap.Bool("emit_mesh", len(mesh) > 1))
	if len(mesh) > 1 {
		out, err = insertBeforeResourceClose(out, buildConnectionMesh(mesh))
		if err != nil {
			log.Debug("addReplicaToConfig: inserting the mesh failed", zap.Error(err))
			return "", err
		}
	}

	// Two-site resource: the newcomer also needs an explicit leg to the DR, on
	// the same port layout the other legs use.
	if wan && drNode != "" {
		legPort := port + legIndex
		log.Debug("addReplicaToConfig: adding WAN leg", zap.String("dr_node", drNode), zap.Int("port", legPort),
			zap.Int("bind_port", bindPort))
		var leg strings.Builder
		fmt.Fprintf(&leg, "\n    connection {\n")
		fmt.Fprintf(&leg, "        host %s address 127.0.0.1:%d;\n", name, bindPort)
		fmt.Fprintf(&leg, "        host %s address 127.0.0.1:%d;\n",
			rm.controller.nodes.GetDRBDNameByRef(drNode), legPort)
		// Written with the defaults; the caller converges the whole config on
		// the resource's tuning (applyWANOptions).
		writeWANLegOptions(&leg, wanproxy.Tuning{})
		fmt.Fprintf(&leg, "    }\n")
		out, err = insertBeforeResourceClose(out, leg.String())
		if err != nil {
			log.Debug("addReplicaToConfig: inserting the WAN leg failed", zap.Error(err))
			return "", err
		}
	}
	log.Debug("addReplicaToConfig: done", zap.String("resource", resource), zap.String("node", node))
	return out, nil
}
