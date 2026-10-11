package mcpserver

import (
	haifypb "github.com/haify-project/haify/api/proto/v1"
)

// Output of the live HA status tools (haify_ha_promoter_status and the
// promoter part of haify_ha_status): where a resource's drbd-reactor chain
// runs, what each unit of it is, whether each standby could take it over, and
// what outside the chain uses its mounts.

type haStatusIn struct {
	Resource string `json:"resource,omitempty" jsonschema:"empty for every HA resource"`
}

type haServiceOut struct {
	Unit     string `json:"unit"`
	Kind     string `json:"kind,omitempty" jsonschema:"promote, mount, service_ip, nfs, iscsi, nvmeof, smb, database, vm, haify, target or service"`
	Label    string `json:"label,omitempty" jsonschema:"what the unit is for: mount path, service IP, export, LUN, or the unit's own description"`
	Status   string `json:"status"`
	SubState string `json:"sub_state,omitempty"`
	Since    string `json:"since,omitempty" jsonschema:"RFC 3339 time the unit last became active"`
	Restarts int32  `json:"restarts,omitempty"`
	MainPID  int32  `json:"main_pid,omitempty"`
}

type haStandbyOut struct {
	Node      string   `json:"node"`
	Ready     bool     `json:"ready" jsonschema:"whether this node could take the chain over now"`
	Reasons   []string `json:"reasons,omitempty" jsonschema:"what would stop the takeover there"`
	DiskState string   `json:"disk_state,omitempty"`
}

type haOutsideOut struct {
	Unit        string   `json:"unit"`
	Description string   `json:"description,omitempty"`
	Processes   int32    `json:"processes"`
	MountPoints []string `json:"mount_points,omitempty"`
}

type haPromoterOut struct {
	Resource   string         `json:"resource"`
	PrimaryOn  string         `json:"primary_on,omitempty"`
	Status     string         `json:"status"`
	ActiveNode string         `json:"active_node,omitempty" jsonschema:"Haify node running the chain"`
	Chain      []haServiceOut `json:"chain,omitempty" jsonschema:"the units in start order, the services target last"`
	Standbys   []haStandbyOut `json:"standbys,omitempty"`
	Outside    []haOutsideOut `json:"outside,omitempty" jsonschema:"services outside the chain using its mounts on the active node; they hold the unmount up on a failover"`
	// Login sessions using the mounts are counted, not listed.
	OutsideSessions         int32 `json:"outside_sessions,omitempty"`
	OutsideSessionProcesses int32 `json:"outside_session_processes,omitempty"`
}

type haStatusOut struct {
	Promoters []haPromoterOut `json:"promoters"`
}

// haPromoterOutFrom renders a promoter's status for an MCP client.
func haPromoterOutFrom(p *haifypb.HaPromoterStatus) haPromoterOut {
	out := haPromoterOut{
		Resource:                p.GetDrbdResource(),
		PrimaryOn:               p.GetPrimaryOn(),
		Status:                  p.GetStatus(),
		ActiveNode:              p.GetActiveNode(),
		OutsideSessions:         p.GetOutsideSessions(),
		OutsideSessionProcesses: p.GetOutsideSessionProcesses(),
	}
	units := append([]*haifypb.HaServiceStatus{}, p.GetDeps()...)
	if t := p.GetTarget(); t.GetName() != "" {
		units = append(units, t)
	}
	for _, u := range units {
		out.Chain = append(out.Chain, haServiceOut{
			Unit: u.GetName(), Kind: u.GetKind(), Label: u.GetLabel(), Status: u.GetStatus(),
			SubState: u.GetSubState(), Since: u.GetSince(), Restarts: u.GetRestarts(), MainPID: u.GetMainPid(),
		})
	}
	for _, s := range p.GetStandbys() {
		out.Standbys = append(out.Standbys, haStandbyOut{
			Node: s.GetNode(), Ready: s.GetReady(), Reasons: s.GetReasons(), DiskState: s.GetDiskState(),
		})
	}
	for _, o := range p.GetOutside() {
		out.Outside = append(out.Outside, haOutsideOut{
			Unit: o.GetUnit(), Description: o.GetDescription(), Processes: o.GetProcesses(), MountPoints: o.GetMountPoints(),
		})
	}
	return out
}
