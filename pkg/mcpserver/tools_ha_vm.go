package mcpserver

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"

	haifypb "github.com/haify-project/haify/api/proto/v1"
)

// Tools for HA resources that run a libvirt guest: moving the guest, live or
// not, and keeping its definition the same on every replica.

// liveEvictWait is how long haify_ha_evict follows a live move before it
// returns with the move still running. A move can take longer than a tool
// call may; haify_ha_migration_status picks it up from there.
const liveEvictWait = 4 * time.Minute

type haEvictIn struct {
	Resource       string `json:"resource" jsonschema:"DRBD resource name"`
	Live           bool   `json:"live,omitempty" jsonschema:"move the libvirt guest of an HA resource created with vm to another replica without stopping it; refused for a resource that runs no guest"`
	To             string `json:"to,omitempty" jsonschema:"with live: the node to move the guest to; omit to let Haify pick a replica that is UpToDate, connected and has the guest's definition"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"with live: give the move up, leaving the guest where it is, when its memory has not been copied after this long (default 1800)"`
}

type haMigrationOut struct {
	Resource   string   `json:"resource"`
	Domain     string   `json:"domain"`
	SourceNode string   `json:"source_node"`
	TargetNode string   `json:"target_node"`
	Phase      string   `json:"phase" jsonschema:"checking, releasing, window, migrating, settling or done"`
	Done       bool     `json:"done"`
	Outcome    string   `json:"outcome,omitempty" jsonschema:"once done: moved, stayed (the guest runs where it was, never stopped), restarted (it was found stopped and HA started it again) or attention (message lists what is left to do by hand)"`
	ActiveNode string   `json:"active_node,omitempty" jsonschema:"once done: the node the guest runs on"`
	Progress   uint32   `json:"progress_percent,omitempty" jsonschema:"memory copied so far while migrating"`
	Message    string   `json:"message"`
	Steps      []string `json:"steps,omitempty"`
}

type haEvictOut struct {
	Status    string          `json:"status" jsonschema:"ok when the eviction was done or the live move was started"`
	Detail    string          `json:"detail,omitempty"`
	Migration *haMigrationOut `json:"migration,omitempty" jsonschema:"with live: the move as far as it got during this call; when done is false, follow it with haify_ha_migration_status"`
}

type haMigrationStatusOut struct {
	Found     bool            `json:"found"`
	Migration *haMigrationOut `json:"migration,omitempty"`
}

type vmDefinitionSyncIn struct {
	Resource string `json:"resource" jsonschema:"DRBD resource name"`
	From     string `json:"from,omitempty" jsonschema:"node whose definition to keep; omit to take the one node whose copy was edited, or to repair the others from the copy Haify keeps"`
}

type vmDefinitionNodeOut struct {
	Node    string `json:"node"`
	State   string `json:"state" jsonschema:"ok, missing, changed (edited on this node since the last sync), uuid_mismatch or unreachable"`
	UUID    string `json:"uuid,omitempty"`
	Running bool   `json:"running,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

type vmDefinitionOut struct {
	Resource   string                `json:"resource"`
	Domain     string                `json:"domain"`
	UUID       string                `json:"uuid"`
	SourceNode string                `json:"source_node,omitempty"`
	Consistent bool                  `json:"consistent"`
	Message    string                `json:"message"`
	Nodes      []vmDefinitionNodeOut `json:"nodes"`
}

func haMigrationOutFrom(m *haifypb.HaMigration) *haMigrationOut {
	if m == nil {
		return nil
	}
	return &haMigrationOut{Resource: m.GetResource(), Domain: m.GetDomain(), SourceNode: m.GetSourceNode(),
		TargetNode: m.GetTargetNode(), Phase: m.GetPhase(), Done: m.GetDone(), Outcome: m.GetOutcome(),
		ActiveNode: m.GetActiveNode(), Progress: m.GetProgressPercent(), Message: m.GetMessage(), Steps: m.GetSteps()}
}

func vmDefinitionOutFrom(d *haifypb.VmDefinitionResponse) vmDefinitionOut {
	out := vmDefinitionOut{Resource: d.GetResource(), Domain: d.GetDomain(), UUID: d.GetUuid(), SourceNode: d.GetSourceNode(),
		Consistent: d.GetConsistent(), Message: d.GetMessage(), Nodes: []vmDefinitionNodeOut{}}
	for _, n := range d.GetNodes() {
		out.Nodes = append(out.Nodes, vmDefinitionNodeOut{Node: n.GetNode(), State: n.GetState(), UUID: n.GetUuid(),
			Running: n.GetRunning(), Detail: n.GetDetail()})
	}
	return out
}

func (s *Server) registerHaEvictTools(srv *mcp.Server) {
	addWrite(s, srv, destructiveTool("haify_ha_evict", "Evict HA resource",
		"Planned switchover of one HA resource: its active node stops the services and another node takes over. "+
			"It moves exactly the resource named and nothing else on the node — haify-meta is the controller with its "+
			"VIP and the AI Copilot, any other name is that resource's own service. For haify-meta the call returns as "+
			"soon as the switchover is launched, and when this server runs in the same chain (as Haify sets it up) it "+
			"moves too: expect this connection to drop for several seconds, which is not a failure; check "+
			"haify_ha_promoter_status for where it landed. For any other resource the call returns once another node "+
			"has taken over, and fails if none did. Never stop or restart a drbd-reactor-managed service by hand "+
			"instead: the promoter treats that as a fault and fails the resource over. "+
			"A resource that runs a libvirt guest (created with vm) restarts the guest on the other node; with live "+
			"the guest is migrated there without stopping, optionally to the node named in to. A live move is refused "+
			"before anything changes when the target has no UpToDate, connected copy or lacks the guest's definition. "+
			"If it fails later the guest stays, running, where it was. The call follows the move for a few minutes; "+
			"when it returns with done false, haify_ha_migration_status shows how it ends."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in haEvictIn) (*mcp.CallToolResult, haEvictOut, error) {
			s.logger.Debug("haify_ha_evict: evicting resource from its active node", zap.String("resource", in.Resource),
				zap.Bool("live", in.Live), zap.String("to", in.To), zap.Int("timeout_seconds", in.TimeoutSeconds))
			if !in.Live {
				if in.To != "" {
					s.logger.Debug("haify_ha_evict: target named without live", zap.String("resource", in.Resource))
					return nil, haEvictOut{}, fmt.Errorf("to names where a live move goes; without live, drbd-reactor decides which replica takes over")
				}
				if err := s.client.EvictHa(ctx, in.Resource); err != nil {
					s.logger.Debug("haify_ha_evict: failed", zap.String("resource", in.Resource), zap.Error(err))
					return nil, haEvictOut{}, err
				}
				s.logger.Debug("haify_ha_evict: done", zap.String("resource", in.Resource))
				return nil, haEvictOut{Status: "ok", Detail: fmt.Sprintf("resource %s evicted from its active node", in.Resource)}, nil
			}
			started, err := s.client.EvictHaLive(ctx, in.Resource, in.To, time.Duration(in.TimeoutSeconds)*time.Second)
			if err != nil {
				s.logger.Debug("haify_ha_evict: live move refused", zap.String("resource", in.Resource), zap.Error(err))
				return nil, haEvictOut{}, err
			}
			s.logger.Debug("haify_ha_evict: live move started", zap.String("resource", in.Resource),
				zap.String("source", started.GetSourceNode()), zap.String("target", started.GetTargetNode()))
			waitCtx, cancel := context.WithTimeout(ctx, liveEvictWait)
			defer cancel()
			// An error here is the wait running out, not the move failing.
			m, werr := s.client.WaitHaMigration(waitCtx, in.Resource, 3*time.Second, nil)
			out := haEvictOut{Status: "ok", Detail: started.GetMessage(), Migration: haMigrationOutFrom(m)}
			switch {
			case m != nil && m.GetDone():
				out.Detail = m.GetMessage()
			case werr != nil:
				out.Detail += "; still running, follow it with haify_ha_migration_status"
			}
			s.logger.Debug("haify_ha_evict: returning", zap.String("resource", in.Resource), zap.Bool("done", m.GetDone()),
				zap.String("outcome", m.GetOutcome()), zap.NamedError("wait_error", werr))
			return nil, out, nil
		})

	addRead(s, srv, readOnlyTool("haify_ha_migration_status", "Live move of an HA guest",
		"Show the live move of an HA resource's libvirt guest that haify_ha_evict started with live: the one running, "+
			"or the last one that ran. While it runs: its phase and how much memory has been copied. Once done: whether "+
			"the guest moved, stayed where it was, or needs attention, and the node it runs on."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in haResourceIn) (*mcp.CallToolResult, haMigrationStatusOut, error) {
			s.logger.Debug("haify_ha_migration_status: reading", zap.String("resource", in.Resource))
			m, err := s.client.GetHaMigration(ctx, in.Resource)
			if err != nil {
				s.logger.Debug("haify_ha_migration_status: read failed", zap.String("resource", in.Resource), zap.Error(err))
				return nil, haMigrationStatusOut{}, err
			}
			s.logger.Debug("haify_ha_migration_status: read", zap.String("resource", in.Resource), zap.Bool("found", m != nil),
				zap.String("phase", m.GetPhase()), zap.Bool("done", m.GetDone()))
			return nil, haMigrationStatusOut{Found: m != nil, Migration: haMigrationOutFrom(m)}, nil
		})

	addReadWithin(s, srv, readOnlyTool("haify_vm_definition_status", "Guest definition on each replica",
		"For an HA resource that runs a libvirt guest: whether every replica that may run the guest has its "+
			"definition, under the same UUID, unchanged since Haify last synced it. A replica without it cannot take "+
			"the guest over, and one with another UUID refuses a live move."),
		time.Minute,
		func(ctx context.Context, _ *mcp.CallToolRequest, in haResourceIn) (*mcp.CallToolResult, vmDefinitionOut, error) {
			s.logger.Debug("haify_vm_definition_status: reading", zap.String("resource", in.Resource))
			d, err := s.client.GetVmDefinition(ctx, in.Resource)
			if err != nil {
				s.logger.Debug("haify_vm_definition_status: read failed", zap.String("resource", in.Resource), zap.Error(err))
				return nil, vmDefinitionOut{}, err
			}
			s.logger.Debug("haify_vm_definition_status: read", zap.String("resource", in.Resource), zap.Bool("consistent", d.GetConsistent()))
			return nil, vmDefinitionOutFrom(d), nil
		})

	addWrite(s, srv, writeTool("haify_vm_definition_sync", "Sync guest definition",
		"Give every replica of an HA resource the same definition of its libvirt guest: after the guest was "+
			"changed on one node (memory, a disk), or to repair a replica that lacks it. The running guest is not "+
			"touched; a change applies when it next starts. Without from, the one node whose copy was edited is the "+
			"source; name from when more than one was."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in vmDefinitionSyncIn) (*mcp.CallToolResult, vmDefinitionOut, error) {
			s.logger.Debug("haify_vm_definition_sync: syncing", zap.String("resource", in.Resource), zap.String("from", in.From))
			d, err := s.client.SyncVmDefinition(ctx, in.Resource, in.From)
			if err != nil {
				s.logger.Debug("haify_vm_definition_sync: failed", zap.String("resource", in.Resource), zap.Error(err))
				return nil, vmDefinitionOut{}, err
			}
			s.logger.Debug("haify_vm_definition_sync: synced", zap.String("resource", in.Resource), zap.Bool("consistent", d.GetConsistent()))
			return nil, vmDefinitionOutFrom(d), nil
		})
}
