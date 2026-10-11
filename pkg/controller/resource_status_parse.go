package controller

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Helper functions for parsing DRBD status output

type volumeInfo struct {
	id     int
	device string
	sizeGB uint64
}

// isIndentedStatusLine reports whether a raw drbdadm/drbdsetup status line is
// indented. Peer and per-volume detail lines are indented; the local resource
// line is not.
func isIndentedStatusLine(line string) bool {
	return strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")
}

// fieldValue returns the value of the first "key:value" field in fields, or ""
// when none carries that key. drbdadm separates fields by spaces and sometimes
// trails them with a comma.
func fieldValue(fields []string, key string) string {
	for _, f := range fields {
		if strings.HasPrefix(f, key) {
			return strings.TrimSuffix(strings.TrimPrefix(f, key), ",")
		}
	}
	return ""
}

// isPeerDiskLine reports whether a status line describes a peer's disk rather
// than the peer itself. "peer-disk:" contains "disk:", so anything matching on
// the latter has to exclude the former first.
func isPeerDiskLine(trimmed string) bool {
	return strings.Contains(trimmed, "peer-disk:")
}

// localStatusLine returns the trimmed local resource line from drbdadm or
// drbdsetup status output. The local line is the first unindented line that
// carries a "role:" field; unindented lines without one (such as the
// "drbdsetup status <res> --verbose" command echo printed by
// "drbdadm status --verbose") are skipped.
func localStatusLine(output string) string {
	for _, line := range strings.Split(output, "\n") {
		if line == "" || isIndentedStatusLine(line) {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if strings.Contains(trimmed, "role:") {
			return trimmed
		}
	}
	return ""
}

func parseRoleFromStatus(output string) string {
	for _, field := range strings.Fields(localStatusLine(output)) {
		if strings.HasPrefix(field, "role:") {
			role := strings.TrimSuffix(strings.TrimPrefix(field, "role:"), ",")
			switch role {
			case "Primary", "Secondary":
				return role
			}
		}
	}
	return "Unknown"
}

// parseNodeStatesFromStatus parses each node's role and disk state from DRBD status output
// Format:
//
//	ha_res role:Primary
//	  disk:UpToDate open:no
//	node2 role:Secondary
//	  peer-disk:UpToDate
func parseNodeStatesFromStatus(output string, nodeAddresses []string) map[string]*ResourceNodeState {
	nodeStates := make(map[string]*ResourceNodeState)
	lines := strings.Split(output, "\n")

	// Get local node's role (first line with role:)
	localRole := "Unknown"
	localDiskState := "Unknown"

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		// Local resource line: the first unindented line carrying "role:".
		// Skips the "drbdsetup status <res> --verbose" command echo emitted
		// by "drbdadm status --verbose"; peer role lines are indented.
		if localRole == "Unknown" && !isIndentedStatusLine(line) && strings.Contains(trimmed, "role:") {
			parts := strings.Fields(trimmed)
			for _, p := range parts {
				if strings.HasPrefix(p, "role:") {
					localRole = strings.TrimPrefix(p, "role:")
					localRole = strings.TrimSuffix(localRole, ",")
					break
				}
			}
		}
		// Local disk can be a standalone "disk:UpToDate" line or a verbose
		// volume line such as "volume:0 minor:0 disk:UpToDate ..."
		if !strings.Contains(trimmed, "peer-disk:") && (strings.HasPrefix(trimmed, "disk:") || (strings.Contains(trimmed, "volume:") && strings.Contains(trimmed, "disk:"))) {
			parts := strings.Fields(trimmed)
			for _, p := range parts {
				if strings.HasPrefix(p, "disk:") {
					localDiskState = strings.TrimPrefix(p, "disk:")
					localDiskState = strings.TrimSuffix(localDiskState, ",")
					break
				}
			}
		}
	}

	// Set local node state (first node in list)
	if len(nodeAddresses) > 0 {
		nodeStates[nodeAddresses[0]] = &ResourceNodeState{
			Role:      localRole,
			DiskState: localDiskState,
		}
	}

	// Parse peer nodeAddresses: "  node2 role:Secondary"
	currentNode := ""
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		parts := strings.Fields(trimmed)

		// Check if this line starts with a node name followed by "role:"
		// This matches "node2 role:Secondary" pattern
		if len(parts) >= 2 {
			role := ""
			for _, part := range parts[1:] {
				if strings.HasPrefix(part, "role:") {
					role = strings.TrimSuffix(strings.TrimPrefix(part, "role:"), ",")
					break
				}
			}
			if role != "" {
				// Find which node this is
				matched := false
				for _, node := range nodeAddresses {
					if node == nodeAddresses[0] {
						continue // Skip local node
					}
					if parts[0] == node {
						currentNode = node
						matched = true
						if _, exists := nodeStates[currentNode]; !exists {
							nodeStates[currentNode] = &ResourceNodeState{Role: role}
						} else {
							nodeStates[currentNode].Role = role
						}
						break
					}
				}
				// A role-carrying line whose name is not a tracked node is
				// either the local resource line or a peer absent from
				// nodeAddresses (e.g. a diskless quorum tiebreaker). Reset
				// currentNode so its following peer-disk line is not
				// misattributed to the previously matched node.
				if !matched {
					currentNode = ""
				}
			}
		}

		// A peer that is not Connected prints exactly one line and no role:
		//   "  haify-e connection:Connecting"
		// Recording it is the whole point — see ResourceNodeState.Connection.
		// It also resets currentNode, because the line carries no role and the
		// next peer-disk line (if any) is not this peer's.
		if len(parts) >= 2 && !isPeerDiskLine(trimmed) {
			if conn := fieldValue(parts[1:], "connection:"); conn != "" {
				for _, node := range nodeAddresses {
					if node == nodeAddresses[0] || parts[0] != node {
						continue
					}
					if st, exists := nodeStates[node]; exists {
						st.Connection = conn
					} else {
						nodeStates[node] = &ResourceNodeState{Connection: conn}
					}
					break
				}
				if fieldValue(parts[1:], "role:") == "" {
					currentNode = ""
				}
			}
		}

		// Check for peer-disk state (belongs to currentNode)
		if strings.Contains(trimmed, "peer-disk:") && currentNode != "" {
			parts := strings.Fields(trimmed)
			for _, p := range parts {
				if strings.HasPrefix(p, "peer-disk:") {
					diskState := strings.TrimSuffix(strings.TrimPrefix(p, "peer-disk:"), ",")
					if _, exists := nodeStates[currentNode]; !exists {
						nodeStates[currentNode] = &ResourceNodeState{DiskState: diskState}
					} else {
						nodeStates[currentNode].DiskState = diskState
					}
					break
				}
			}
		}
	}

	return nodeStates
}

func parseVolumesFromStatus(output string) []volumeInfo {
	var volumes []volumeInfo
	seen := make(map[int]bool)

	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.Contains(trimmed, "volume:") || strings.Contains(trimmed, "peer-disk:") {
			continue
		}

		fields := strings.Fields(trimmed)
		volumeID := -1
		minor := -1
		var sizeGB uint64
		for i, field := range fields {
			switch {
			case strings.HasPrefix(field, "volume:"):
				value := strings.TrimPrefix(field, "volume:")
				if value == "" && i+1 < len(fields) {
					value = fields[i+1]
				}
				if parsed, err := strconv.Atoi(strings.TrimSuffix(value, ",")); err == nil {
					volumeID = parsed
				}
			case strings.HasPrefix(field, "minor:"):
				if parsed, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(field, "minor:"), ",")); err == nil {
					minor = parsed
				}
			case strings.HasPrefix(field, "size:"):
				if parsed, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(field, "size:"), ","), 10, 64); err == nil {
					sizeGB = parsed / 1024 / 1024
				}
			}
		}

		if volumeID < 0 || seen[volumeID] {
			continue
		}

		device := ""
		if minor >= 0 {
			device = fmt.Sprintf("/dev/drbd%d", minor)
		}
		volumes = append(volumes, volumeInfo{
			id:     volumeID,
			device: device,
			sizeGB: sizeGB,
		})
		seen[volumeID] = true
	}

	return volumes
}

// drbdsetupStatus mirrors the subset of `drbdsetup status <res> --json` output
// that carries live role, disk and replication/resync state. Fields absent in
// steady state (notably "done") are treated as fully in sync.
type drbdsetupStatus struct {
	Name        string           `json:"name"`
	Role        string           `json:"role"`
	Devices     []drbdDevice     `json:"devices"`
	Connections []drbdConnection `json:"connections"`
}

// drbdDevice is one volume of the answering node in drbdsetup status --json.
type drbdDevice struct {
	Volume    int    `json:"volume"`
	DiskState string `json:"disk-state"`
	// Quorum reports whether the local node currently holds DRBD quorum for
	// this device. It is a pointer so a missing field (older drbd, or a
	// diskless view) is distinguishable from an explicit false. A resource
	// configured with `quorum majority` + `on-no-quorum io-error` blocks I/O
	// on any node that has lost quorum, which is what makes a guarded
	// force-promote of a quorate survivor safe.
	Quorum *bool `json:"quorum"`
	// Written is KiB written to this node's backing disk since the
	// device came up: the application's writes on a Primary, the
	// replicated ones on a Secondary, and resync traffic on either.
	Written *uint64 `json:"written"`
	// Read is KiB read from this node's DRBD device since it came up.
	Read *uint64 `json:"read"`
}

// drbdConnection is one peer in drbdsetup status --json.
type drbdConnection struct {
	Name string `json:"name"`
	// ConnectionState is "Connected", "Connecting", "StandAlone", ... It is
	// the only field a peer whose link is down carries any truth in: DRBD
	// leaves peer-role and every peer_device empty for such a peer, and
	// reading those empties as facts is the whole reason this is parsed.
	ConnectionState string           `json:"connection-state"`
	TLS             bool             `json:"tls"`
	PeerRole        string           `json:"peer-role"`
	PeerDevices     []drbdPeerDevice `json:"peer_devices"`
}

// drbdPeerDevice is one volume of one connection in drbdsetup status --json.
type drbdPeerDevice struct {
	Volume           int    `json:"volume"`
	ReplicationState string `json:"replication-state"`
	PeerDiskState    string `json:"peer-disk-state"`
	// Done is the resync completion percentage (0..100) drbdsetup emits
	// on a peer_device while resyncing. PercentInSync is accepted as an
	// alias for robustness across drbd versions. Both are absent in
	// steady state, so a nil value means "fully in sync".
	Done          *float64 `json:"done"`
	PercentInSync *float64 `json:"percent-in-sync"`
	// OutOfSyncKiB is how much of the volume DRBD knows differs from
	// this peer. On an Established peer it is only ever non-zero after
	// an online verify found blocks that disagree.
	OutOfSyncKiB uint64 `json:"out-of-sync"`
	// PercentResyncDone is a running resync's or verify's progress.
	PercentResyncDone *float64 `json:"percent-resync-done"`
	// Sent and Received are KiB exchanged with this peer since the
	// connection came up: replicated writes and resync data alike.
	Sent     *uint64 `json:"sent"`
	Received *uint64 `json:"received"`
}

// parseNodeStatesFromJSON parses `drbdsetup status <res> --json` into per-node
// states keyed by node name. localNode is the name of the queried node (the
// JSON top-level resource); its peers — including any diskless quorum
// tiebreaker — come from connections[]. It returns an error when the JSON is
// empty or cannot be decoded so the caller can fall back to the text parser.
func parseNodeStatesFromJSON(output, localNode string) (map[string]*ResourceNodeState, error) {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return nil, fmt.Errorf("empty drbdsetup status json")
	}
	var resources []drbdsetupStatus
	if err := json.Unmarshal([]byte(trimmed), &resources); err != nil {
		return nil, fmt.Errorf("decode drbdsetup status json: %w", err)
	}
	if len(resources) == 0 {
		return nil, fmt.Errorf("drbdsetup status json contained no resources")
	}
	res := resources[0]

	states := make(map[string]*ResourceNodeState)

	// Local node: role + disk from the top-level resource. A node has no
	// replication relationship to itself, so it is fully in sync (100).
	local := &ResourceNodeState{Role: res.Role, SyncPercent: 100, SyncPercentKnown: true}
	if len(res.Devices) > 0 {
		local.DiskState = res.Devices[0].DiskState
		// Quorum is only ever the queried node's own verdict; the peers below
		// deliberately leave it nil rather than assume they agree.
		local.Quorum = res.Devices[0].Quorum
		local.WrittenKiB = sumCounter(res.Devices, func(d drbdDevice) *uint64 { return d.Written })
		local.ReadKiB = sumCounter(res.Devices, func(d drbdDevice) *uint64 { return d.Read })
	}
	states[localNode] = local

	// Each peer (including a diskless quorum tiebreaker) is one connection.
	for _, conn := range res.Connections {
		if conn.Name == "" {
			continue
		}
		peer := &ResourceNodeState{
			Role:             conn.PeerRole,
			Connection:       conn.ConnectionState,
			TLS:              conn.TLS,
			SyncPercent:      100,
			SyncPercentKnown: true,
		}
		for _, pd := range conn.PeerDevices {
			peer.OutOfSyncKiB += pd.OutOfSyncKiB
		}
		peer.SentKiB = sumCounter(conn.PeerDevices, func(pd drbdPeerDevice) *uint64 { return pd.Sent })
		peer.ReceivedKiB = sumCounter(conn.PeerDevices, func(pd drbdPeerDevice) *uint64 { return pd.Received })
		if len(conn.PeerDevices) > 0 {
			pd := conn.PeerDevices[0]
			peer.DiskState = pd.PeerDiskState
			peer.Replication = pd.ReplicationState
			switch {
			case pd.Done != nil:
				peer.SyncPercent = *pd.Done
			case pd.PercentInSync != nil:
				peer.SyncPercent = *pd.PercentInSync
			}
		}
		states[conn.Name] = peer
	}

	return states, nil
}

// sumCounter totals one cumulative counter over volumes, or nil when there are
// none or any volume did not report it: a partial sum would read as a sudden
// drop in traffic.
func sumCounter[T any](items []T, counter func(T) *uint64) *uint64 {
	if len(items) == 0 {
		return nil
	}
	var total uint64
	for _, item := range items {
		v := counter(item)
		if v == nil {
			return nil
		}
		total += *v
	}
	return &total
}
