package controller

// Rewriting a resource's DRBD config into the two-site shape when a DR site is
// attached: the DR host stanza, the narrowed mesh, the WAN legs and the
// primary-site quorum.

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/database"
)

// addDRToConfig rewrites a LAN resource config into the two-site shape.
//
// Three edits, all additive to the primary site:
//   - a host stanza for the DR node, with its own volumes and a fresh node-id
//     (on an encrypted resource each volume's disk is its crypt container);
//   - connection-mesh narrowed to the primary site, because a mesh entry for the
//     DR would pair it with the replicas on their LAN addresses, which cannot
//     reach it;
//   - one explicit `connection` per replica carrying the loopback endpoints of
//     that replica's WAN leg, plus protocol A and pull-ahead so the WAN cannot
//     stall writes in the primary site.
func (rm *ResourceManager) addDRToConfig(content, resource, drNode string, primaries, tiebreakers []string,
	drAddr string, vols []*database.Volume, port int, bindPorts []int, encrypted bool) (string, error) {

	log := rm.controller.traceLogger()
	log.Debug("addDRToConfig: start", zap.String("resource", resource), zap.String("dr_node", drNode),
		zap.Strings("primaries", primaries), zap.Strings("tiebreakers", tiebreakers), zap.String("dr_addr", drAddr),
		zap.Int("volumes", len(vols)), zap.Int("port", port), zap.Ints("bind_ports", bindPorts), zap.Bool("encrypted", encrypted))
	drName := rm.controller.nodes.GetDRBDNameByRef(drNode)
	blocks := parseOnBlocks(content)
	for _, b := range blocks {
		if b.name == drName {
			log.Debug("addDRToConfig: DR node already in config", zap.String("resource", resource), zap.String("drbd_name", drName))
			return "", fmt.Errorf("node %q is already in the config of %q", drNode, resource)
		}
	}
	nextID := 0
	for _, b := range blocks {
		if b.nodeID >= nextID {
			nextID = b.nodeID + 1
		}
	}
	log.Debug("addDRToConfig: DR node identity", zap.String("drbd_name", drName), zap.Int("node_id", nextID),
		zap.Int("existing_hosts", len(blocks)))

	// 1. The DR host stanza. Its address is loopback: every connection it has
	//    is a WAN leg terminated by its local acceptor.
	var dr strings.Builder
	fmt.Fprintf(&dr, "\n    on %s {\n", drName)
	fmt.Fprintf(&dr, "        address   127.0.0.1:%d;\n", port)
	fmt.Fprintf(&dr, "        node-id   %d;\n", nextID)
	for _, v := range vols {
		// The container's name comes from pool and volume, so it matches the
		// primaries' stanzas although the key behind it is the DR's own.
		disk := fmt.Sprintf("/dev/%s/%s", v.Pool, v.VolumeName)
		if encrypted {
			disk = luksMapperPath(v.Pool, v.VolumeName)
		}
		log.Debug("addDRToConfig: DR volume stanza", zap.Int("volume", v.VolumeID), zap.String("disk", disk))
		fmt.Fprintf(&dr, "        volume %d {\n", v.VolumeID)
		fmt.Fprintf(&dr, "            device    minor %d;\n", minorForVolume(content, v.VolumeID))
		fmt.Fprintf(&dr, "            disk      %s;\n", disk)
		fmt.Fprintf(&dr, "            meta-disk internal;\n")
		fmt.Fprintf(&dr, "        }\n")
	}
	fmt.Fprintf(&dr, "    }\n")

	// 2 + 3. Insert before the resource's closing brace, dropping any existing
	//        connection-mesh so it can be re-emitted for the primary site only.
	lines := strings.Split(content, "\n")
	out := make([]string, 0, len(lines)+40)
	inMesh := false
	closeIdx := -1
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "connection-mesh") {
			inMesh = true
			continue
		}
		if inMesh {
			if strings.Contains(line, "}") {
				inMesh = false
			}
			continue
		}
		if trimmed == "}" {
			closeIdx = len(out) // resource-level close; keep updating, last wins
		}
		out = append(out, line)
	}
	if closeIdx < 0 {
		log.Debug("addDRToConfig: no closing brace in config", zap.String("resource", resource))
		return "", fmt.Errorf("malformed config for %q: no closing brace", resource)
	}

	var tail strings.Builder
	tail.WriteString(dr.String())

	lanHosts := make([]string, 0, len(primaries)+len(tiebreakers))
	for _, n := range append(append([]string{}, primaries...), tiebreakers...) {
		lanHosts = append(lanHosts, rm.controller.nodes.GetDRBDNameByRef(n))
	}
	log.Debug("addDRToConfig: primary-site mesh", zap.Strings("lan_hosts", lanHosts), zap.Bool("emit_mesh", len(lanHosts) > 1))
	if len(lanHosts) > 1 {
		tail.WriteString("\n    connection-mesh {\n        hosts")
		for _, h := range lanHosts {
			fmt.Fprintf(&tail, " %s", h)
		}
		tail.WriteString(";\n    }\n")
	}

	for i, p := range primaries {
		legPort := port + i
		fmt.Fprintf(&tail, "\n    connection {\n")
		bind := legPort + wanDRBDBindOffset
		if i < len(bindPorts) && bindPorts[i] > 0 {
			bind = bindPorts[i]
		}
		log.Debug("addDRToConfig: WAN leg", zap.String("node", p), zap.Int("port", legPort), zap.Int("bind_port", bind))
		fmt.Fprintf(&tail, "        host %s address 127.0.0.1:%d;\n",
			rm.controller.nodes.GetDRBDNameByRef(p), bind)
		fmt.Fprintf(&tail, "        host %s address 127.0.0.1:%d;\n", drName, legPort)
		fmt.Fprintf(&tail, "        net {\n")
		fmt.Fprintf(&tail, "            protocol A;\n")
		fmt.Fprintf(&tail, "            on-congestion pull-ahead;\n")
		fmt.Fprintf(&tail, "            congestion-fill 400M;\n")
		fmt.Fprintf(&tail, "            csums-alg %s;\n", wanCsumsAlg)
		fmt.Fprintf(&tail, "        }\n")
		fmt.Fprintf(&tail, "    }\n")
	}

	final := append([]string{}, out[:closeIdx]...)
	final = append(final, strings.Split(strings.TrimRight(tail.String(), "\n"), "\n")...)
	final = append(final, out[closeIdx:]...)
	log.Debug("addDRToConfig: done", zap.String("resource", resource), zap.Int("lines", len(final)))
	return strings.Join(final, "\n"), nil
}

// minorForVolume finds the device minor an existing config assigns to a volume,
// so the DR's stanza uses the same one (a resource's minor is cluster-wide).
func minorForVolume(content string, volumeID int) int {
	inVol := false
	depth := 0
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if !inVol && strings.HasPrefix(trimmed, fmt.Sprintf("volume %d", volumeID)) {
			inVol = true
			depth = strings.Count(line, "{") - strings.Count(line, "}")
			continue
		}
		if inVol {
			if m, ok := parseAnyDeviceMinor(line); ok {
				zap.L().Debug("minorForVolume: found in volume block", zap.Int("volume", volumeID), zap.Int("minor", m))
				return m
			}
			depth += strings.Count(line, "{") - strings.Count(line, "}")
			if depth <= 0 {
				inVol = false
			}
		}
	}
	// Volume 0 may be written without an explicit `volume 0 {` wrapper.
	if volumeID == 0 {
		for _, line := range strings.Split(content, "\n") {
			if m, ok := parseAnyDeviceMinor(line); ok {
				zap.L().Debug("minorForVolume: volume 0 without a wrapper", zap.Int("minor", m))
				return m
			}
		}
	}
	zap.L().Debug("minorForVolume: no minor found, using the volume number", zap.Int("volume", volumeID))
	return volumeID
}

// quorumLineRe matches the `quorum <value>;` line inside a resource's options.
var quorumLineRe = regexp.MustCompile(`(?m)^(\s*)quorum\s+\S+;`)

// primarySiteQuorum renders the `quorum` value for a resource with an off-site
// DR: a majority of the nodes that could actually take over.
//
// DRBD counts every configured node toward a majority, but the DR is an
// asynchronous copy that is never promoted automatically — it cannot take over,
// so letting it vote on whether the primary site may accept writes is backwards.
// Worse, it actively costs availability: adding a DR to a 3-node resource lifts
// the bar from 2 votes to 3, so the site that used to survive one local failure
// no longer does. That is how a quorum tiebreaker can be added, a DR attached,
// and the tiebreaker's vote silently cancelled out.
//
// The narrower guarantee is deliberate and bounded: the excluded node is
// unreachable from the primary site's network by construction — it is reached
// only through a proxy tunnel — so it cannot form a rival quorate partition with
// any local node. This is not the same as picking a small number arbitrarily.
//
// This is the only place the number is computed. It used to be derived here and
// again, independently, at each of the two call sites that narrow a resource's
// quorum, while the tests asserted on a third copy that nothing called — so the
// arithmetic the cluster actually ran was unguarded.
func primarySiteQuorum(localVoters int) string {
	if localVoters < 1 {
		localVoters = 1
	}
	return strconv.Itoa(localVoters/2 + 1)
}

// setLocalSiteQuorum rewrites a config's quorum to a majority of the primary
// site, so attaching a DR does not raise the bar the local nodes must clear.
//
// Without this, add-dr makes a resource LESS available: DRBD counts the new
// member toward `majority`, so a 3-node resource that survived one local failure
// suddenly needs 3 of 4 votes and no longer does. The operator asked for an
// off-site copy and silently got a downgrade to local resilience.
func setLocalSiteQuorum(content string, localVoters int) string {
	want := primarySiteQuorum(localVoters)
	explicit := quorumLineRe.MatchString(content)
	zap.L().Debug("setLocalSiteQuorum: quorum for the primary site", zap.Int("local_voters", localVoters),
		zap.String("quorum", want), zap.Bool("replace_existing", explicit))
	if explicit {
		return quorumLineRe.ReplaceAllString(content, "${1}quorum "+want+";")
	}
	// No explicit quorum: the resource is relying on DRBD's default, so state it
	// rather than leaving the new member to shift a majority nobody wrote down.
	return strings.Replace(content, "    options {",
		"    options {\n        quorum "+want+";", 1)
}
