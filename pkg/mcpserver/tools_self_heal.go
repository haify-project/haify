package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"

	haifypb "github.com/haify-project/haify/api/proto/v1"
)

// What self-healing is doing. The events say what it decided, each reason
// once; this is the tool for "why has the replica not moved yet".

type selfHealEvictNodeOut struct {
	Node             string `json:"node"`
	OfflineSince     string `json:"offline_since,omitempty" jsonschema:"RFC3339"`
	RemainingSeconds int64  `json:"remaining_seconds" jsonschema:"until its replicas are replaced; 0 when that is due"`
	State            string `json:"state" jsonschema:"waiting, due, exempt (in maintenance or opted out) or evicted"`
	Reason           string `json:"reason,omitempty"`
	Replicas         uint32 `json:"replicas" jsonschema:"resources that still hold a replica on it"`
}

type selfHealEvictOut struct {
	Mode              string                 `json:"mode" jsonschema:"off, dry-run or on"`
	AfterMinutes      uint32                 `json:"after_minutes"`
	MaxOfflinePercent uint32                 `json:"max_offline_percent"`
	Gate              string                 `json:"gate,omitempty" jsonschema:"why nothing is evicted now although a node is due"`
	Syncing           string                 `json:"syncing,omitempty" jsonschema:"resource whose replacement replica is still syncing; the next replacement waits for it"`
	Nodes             []selfHealEvictNodeOut `json:"nodes"`
}

type selfHealMoveOut struct {
	Resource    string   `json:"resource"`
	From        string   `json:"from"`
	To          string   `json:"to"`
	State       string   `json:"state,omitempty" jsonschema:"disk state of the new replica"`
	SyncPercent *float64 `json:"sync_percent,omitempty"`
}

type selfHealClientOut struct {
	Resource         string `json:"resource"`
	Node             string `json:"node"`
	InUseSince       string `json:"in_use_since" jsonschema:"RFC3339; since when the node has used the volume as a diskless client without a break"`
	InUseSeconds     int64  `json:"in_use_seconds"`
	RemainingSeconds int64  `json:"remaining_seconds" jsonschema:"of uninterrupted use until it is due a replica of its own; 0 when due"`
	State            string `json:"state" jsonschema:"counting, due, skipped (gets no replica: see reason), waiting (another self-healing sync runs first), planned (dry run) or failed (retried at retry_at)"`
	Skip             string `json:"skip,omitempty" jsonschema:"kind of reason for skipped: optout, pinned, cooldown, pool, promoter, degraded, migration, ..."`
	Reason           string `json:"reason,omitempty"`
	RetryAt          string `json:"retry_at,omitempty" jsonschema:"RFC3339"`
}

type selfHealSwapOut struct {
	Resource       string   `json:"resource"`
	Node           string   `json:"node" jsonschema:"the former client, now a replica"`
	StartedAt      string   `json:"started_at,omitempty" jsonschema:"RFC3339"`
	Phase          string   `json:"phase" jsonschema:"syncing (the new replica is not UpToDate yet), removing (it is, and an old replica is to go) or tiebreaker (the old replica is gone and its node is being kept as the third vote)"`
	SyncPercent    *float64 `json:"sync_percent,omitempty"`
	ReplicasNow    uint32   `json:"replicas_now"`
	ReplicasTarget uint32   `json:"replicas_target"`
	Removed        string   `json:"removed,omitempty"`
	Attempts       uint32   `json:"attempts,omitempty" jsonschema:"failed tries of the current step"`
	NextTry        string   `json:"next_try,omitempty" jsonschema:"RFC3339"`
	Detail         string   `json:"detail,omitempty" jsonschema:"why the current step waits"`
}

type selfHealCooldownOut struct {
	Resource string `json:"resource"`
	MovedAt  string `json:"moved_at" jsonschema:"RFC3339"`
	Until    string `json:"until" jsonschema:"RFC3339; no further move to its user before this"`
}

type selfHealDiskfulOut struct {
	Mode          string                `json:"mode" jsonschema:"off, dry-run or on"`
	AfterMinutes  uint32                `json:"after_minutes"`
	CooldownHours uint32                `json:"cooldown_hours"`
	Clients       []selfHealClientOut   `json:"clients"`
	Swaps         []selfHealSwapOut     `json:"swaps"`
	Cooldowns     []selfHealCooldownOut `json:"cooldowns"`
}

type selfHealStatusOut struct {
	AutoEvict   selfHealEvictOut   `json:"auto_evict"`
	AutoDiskful selfHealDiskfulOut `json:"auto_diskful"`
	Moves       []selfHealMoveOut  `json:"moves" jsonschema:"replica moves under way (move-replica, rebalance)"`
	Note        string             `json:"note,omitempty"`
}

func selfHealStatusOutFrom(r *haifypb.GetSelfHealStatusResponse) selfHealStatusOut {
	ev, df := r.GetAutoEvict(), r.GetAutoDiskful()
	out := selfHealStatusOut{Note: r.GetMessage(), Moves: []selfHealMoveOut{},
		AutoEvict: selfHealEvictOut{Mode: ev.GetMode(), AfterMinutes: ev.GetAfterMinutes(), MaxOfflinePercent: ev.GetMaxOfflinePercent(),
			Gate: ev.GetGate(), Syncing: ev.GetSyncing(), Nodes: []selfHealEvictNodeOut{}},
		AutoDiskful: selfHealDiskfulOut{Mode: df.GetMode(), AfterMinutes: df.GetAfterMinutes(), CooldownHours: df.GetCooldownHours(),
			Clients: []selfHealClientOut{}, Swaps: []selfHealSwapOut{}, Cooldowns: []selfHealCooldownOut{}},
	}
	for _, n := range ev.GetNodes() {
		out.AutoEvict.Nodes = append(out.AutoEvict.Nodes, selfHealEvictNodeOut{Node: n.GetNode(), OfflineSince: unixRFC3339(n.GetOfflineSince()),
			RemainingSeconds: n.GetRemainingSeconds(), State: n.GetState(), Reason: n.GetReason(), Replicas: n.GetReplicas()})
	}
	for _, m := range r.GetMoves() {
		out.Moves = append(out.Moves, selfHealMoveOut{Resource: m.GetResource(), From: m.GetFrom(), To: m.GetTo(),
			State: m.GetState(), SyncPercent: m.SyncPercent})
	}
	for _, c := range df.GetClients() {
		out.AutoDiskful.Clients = append(out.AutoDiskful.Clients, selfHealClientOut{Resource: c.GetResource(), Node: c.GetNode(),
			InUseSince: unixRFC3339(c.GetInUseSince()), InUseSeconds: c.GetInUseSeconds(), RemainingSeconds: c.GetRemainingSeconds(),
			State: c.GetState(), Skip: c.GetSkip(), Reason: c.GetReason(), RetryAt: unixRFC3339(c.GetRetryAt())})
	}
	for _, sw := range df.GetSwaps() {
		out.AutoDiskful.Swaps = append(out.AutoDiskful.Swaps, selfHealSwapOut{Resource: sw.GetResource(), Node: sw.GetNode(),
			StartedAt: unixRFC3339(sw.GetStartedAt()), Phase: sw.GetPhase(), SyncPercent: sw.SyncPercent,
			ReplicasNow: sw.GetReplicasNow(), ReplicasTarget: sw.GetReplicasTarget(), Removed: sw.GetRemoved(),
			Attempts: sw.GetAttempts(), NextTry: unixRFC3339(sw.GetNextTry()), Detail: sw.GetDetail()})
	}
	for _, cd := range df.GetCooldowns() {
		out.AutoDiskful.Cooldowns = append(out.AutoDiskful.Cooldowns, selfHealCooldownOut{Resource: cd.GetResource(),
			MovedAt: unixRFC3339(cd.GetMovedAt()), Until: unixRFC3339(cd.GetUntil())})
	}
	return out
}

func (s *Server) registerSelfHealTools(srv *mcp.Server) {
	addRead(s, srv, readOnlyTool("haify_self_heal_status", "Self-healing status",
		"What self-healing is doing and waiting for. auto_evict: the nodes that do not answer, how long until each "+
			"one's replicas are replaced, and what holds that back. moves: replica moves under way and how far the new "+
			"replica has synced. auto_diskful: every node using a volume as a diskless client, how long it has been "+
			"in use and how long is left before it gets a replica of its own; for one that is passed over, why; the "+
			"swaps in progress with their phase and sync progress; and the resources whose replica already moved and "+
			"will not move again before the cooldown ends. Use it when asked why a replica has not moved to where the "+
			"volume is used, or what a self-healing sync is waiting for. A mode of off means that loop does not run "+
			"([self_heal] in controller.toml)."),
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, selfHealStatusOut, error) {
			s.logger.Debug("haify_self_heal_status: reading")
			resp, err := s.client.GetSelfHealStatus(ctx)
			if err != nil {
				s.logger.Debug("haify_self_heal_status: failed", zap.Error(err))
				return nil, selfHealStatusOut{}, err
			}
			out := selfHealStatusOutFrom(resp)
			s.logger.Debug("haify_self_heal_status: read", zap.Int("offline_nodes", len(out.AutoEvict.Nodes)),
				zap.Int("clients", len(out.AutoDiskful.Clients)), zap.Int("swaps", len(out.AutoDiskful.Swaps)),
				zap.Int("moves", len(out.Moves)))
			return nil, out, nil
		})
}
