package inspect

import (
	"encoding/json"
	"fmt"
	"strings"

	"go.uber.org/zap"
)

// DRBDResource is one resource in `drbdsetup status --json`, as the node that
// ran it sees it: its own role and disks, and each peer over its connection.
//
// Every node is probed, not one per resource, because a connection can be
// stuck on one side only — WFBitMapS here, Established there — and a single
// node's view reports whichever half it happens to hold.
type DRBDResource struct {
	Name        string           `json:"name"`
	NodeID      int              `json:"node-id"`
	Role        string           `json:"role"`
	Suspended   bool             `json:"suspended"`
	Devices     []DRBDDevice     `json:"devices"`
	Connections []DRBDConnection `json:"connections"`
}

// DRBDDevice is one local volume.
type DRBDDevice struct {
	Volume    int    `json:"volume"`
	DiskState string `json:"disk-state"`
	Quorum    *bool  `json:"quorum"`
}

// DRBDConnection is one peer.
type DRBDConnection struct {
	PeerNodeID      int              `json:"peer-node-id"`
	Name            string           `json:"name"`
	ConnectionState string           `json:"connection-state"`
	PeerRole        string           `json:"peer-role"`
	PeerDevices     []DRBDPeerDevice `json:"peer_devices"`
}

// DRBDPeerDevice is one volume of one peer.
type DRBDPeerDevice struct {
	Volume            int      `json:"volume"`
	ReplicationState  string   `json:"replication-state"`
	PeerDiskState     string   `json:"peer-disk-state"`
	PercentResyncDone *float64 `json:"percent-resync-done"`
}

// ParseDRBDStatus decodes `drbdsetup status --json` for every resource. Empty
// output is a node with no resources up, not an error.
func ParseDRBDStatus(out string) ([]DRBDResource, error) {
	out = strings.TrimSpace(out)
	if out == "" {
		zap.L().Debug("ParseDRBDStatus: empty output, no resources up")
		return nil, nil
	}
	var res []DRBDResource
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		zap.L().Debug("ParseDRBDStatus: JSON does not parse", zap.Int("bytes", len(out)), zap.Error(err))
		return nil, fmt.Errorf("drbdsetup status --json unreadable: %w", err)
	}
	zap.L().Debug("ParseDRBDStatus: parsed", zap.Int("count", len(res)))
	return res, nil
}

// Connected reports whether the link to this peer is up.
func (c DRBDConnection) Connected() bool { return strings.EqualFold(c.ConnectionState, "Connected") }

// stuckReplication lists the replication states that are steps of a
// connection handshake. Each lasts milliseconds when the handshake completes;
// one that is still showing on a Connected link means it did not, and the
// replica behind it is not being brought up to date.
var stuckReplication = map[string]bool{
	"WFBitMapS": true, "WFBitMapT": true, "WFSyncUUID": true,
}

// syncing reports a resync in progress on any volume of this peer.
func (c DRBDConnection) syncing() (bool, float64) {
	for _, pd := range c.PeerDevices {
		switch pd.ReplicationState {
		case "SyncSource", "SyncTarget", "PausedSyncS", "PausedSyncT":
			done := 0.0
			if pd.PercentResyncDone != nil {
				done = *pd.PercentResyncDone
			}
			zap.L().Debug("DRBDConnection.syncing: resync in progress", zap.String("peer", c.Name), zap.Int("volume", pd.Volume),
				zap.String("replication_state", pd.ReplicationState), zap.Float64("percent_done", done))
			return true, done
		}
	}
	return false, 0
}

// diskStates lists the distinct local disk states.
func (r *DRBDResource) diskStates() []string {
	var out []string
	seen := map[string]bool{}
	for _, d := range r.Devices {
		if !seen[d.DiskState] {
			seen[d.DiskState] = true
			out = append(out, d.DiskState)
		}
	}
	return out
}

// worstDisk returns the first local disk state that is not UpToDate, or "".
func (r *DRBDResource) worstDisk(expectDiskless bool) string {
	for _, d := range r.Devices {
		switch d.DiskState {
		case "UpToDate":
			continue
		case "Diskless":
			if expectDiskless {
				continue
			}
		}
		zap.L().Debug("DRBDResource.worstDisk: disk not UpToDate", zap.String("resource", r.Name), zap.Int("volume", d.Volume),
			zap.String("disk_state", d.DiskState), zap.Bool("expect_diskless", expectDiskless))
		return d.DiskState
	}
	return ""
}

// quorumLost reports a device that says it has no quorum.
func (r *DRBDResource) quorumLost() bool {
	for _, d := range r.Devices {
		if d.Quorum != nil && !*d.Quorum {
			zap.L().Debug("DRBDResource.quorumLost: volume without quorum", zap.String("resource", r.Name), zap.Int("volume", d.Volume))
			return true
		}
	}
	return false
}
