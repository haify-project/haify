package mcpserver

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"
)

// ---- output types ----

type nodeOut struct {
	Name     string `json:"name"`
	Address  string `json:"address"`
	Hostname string `json:"hostname,omitempty"`
	State    string `json:"state"`
	Version  string `json:"version,omitempty"`
}

type nodeListOut struct {
	Nodes []nodeOut `json:"nodes"`
}

type nodeHealthOut struct {
	Node                    string   `json:"node"`
	Error                   string   `json:"error,omitempty" jsonschema:"set when the health check itself failed"`
	DrbdInstalled           bool     `json:"drbd_installed"`
	DrbdVersion             string   `json:"drbd_version,omitempty"`
	DrbdReactorInstalled    bool     `json:"drbd_reactor_installed"`
	DrbdReactorVersion      string   `json:"drbd_reactor_version,omitempty"`
	DrbdReactorRunning      bool     `json:"drbd_reactor_running"`
	ResourceAgentsInstalled bool     `json:"resource_agents_installed"`
	AvailableAgents         []string `json:"available_agents,omitempty"`
}

type healthCheckOut struct {
	Results []nodeHealthOut `json:"results"`
}

type poolOut struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Node string `json:"node"`
	// TotalGB and FreeGB describe the VOLUME GROUP. Haify builds its thin pool
	// from nearly all of it, so FreeGB is near zero for the whole life of such
	// a pool however empty it is — do not read it as "the pool is full".
	TotalGB uint64   `json:"total_gb" jsonschema:"volume group size; not the thin pool's"`
	FreeGB  uint64   `json:"free_gb" jsonschema:"UNALLOCATED extents in the volume group. Structurally zero for any pool Haify created, whatever its utilisation. Judge fullness from thin_data_percent, not from this"`
	Devices []string `json:"devices,omitempty"`
	// CapacityGB and AvailableGB are what new volumes are carved from: the
	// thin pool when the group holds one, else the group itself.
	CapacityGB  float64 `json:"capacity_gb" jsonschema:"size of what new volumes are carved from: the thin pool if there is one, else the pool"`
	AvailableGB float64 `json:"available_gb" jsonschema:"room left for new volumes and their writes; judge fullness from this"`
	// Thin is true when the group holds a thin pool now, or was created as one.
	Thin        bool   `json:"thin" jsonschema:"the pool holds a thin pool (over-provisioned: available_gb is not a cap on a new volume's size)"`
	Compression string `json:"compression,omitempty"`
	// CompressRatio is what ZFS compression achieves on the pool (1.85 =
	// 1.85x); omitted when unknown.
	CompressRatio float64 `json:"compress_ratio,omitempty"`
	// Thin pool utilisation. Empty thin_pool_lv means the group holds no thin
	// pool; that, and not a zero percentage, is how "no thin pool" is told
	// apart from "a thin pool at 0%".
	ThinPoolLV      string  `json:"thin_pool_lv,omitempty" jsonschema:"the thin pool logical volume; empty when the group holds none"`
	ThinSizeBytes   uint64  `json:"thin_size_bytes,omitempty" jsonschema:"capacity of the thin pool itself, which thin_data_percent is a percentage of"`
	ThinDataPercent float64 `json:"thin_data_percent,omitempty" jsonschema:"how full the thin pool is. THIS is the number that says whether writes will succeed"`
	ThinMetaPercent float64 `json:"thin_metadata_percent,omitempty" jsonschema:"thin pool metadata utilisation; exhausting it stops writes just as completely as data, and it fills for unrelated reasons"`
	ThinOutOfSpace  bool    `json:"thin_out_of_space,omitempty" jsonschema:"LVM reports the pool out of data space: writes are already failing and any DRBD replica on it will drop to Diskless"`
}

type poolListOut struct {
	Pools []poolOut `json:"pools"`
}

// ---- input types ----

type nodeRegisterIn struct {
	Name    string `json:"name" jsonschema:"node name, e.g. node1"`
	Address string `json:"address" jsonschema:"node address (hostname or IP) the controller reaches over SSH"`
}

type nodeUnregisterIn struct {
	Address string `json:"address" jsonschema:"address of the node to unregister"`
}

type healthCheckIn struct {
	Nodes []string `json:"nodes,omitempty" jsonschema:"nodes to check; defaults to all registered nodes"`
}

type poolCreateIn struct {
	Name    string   `json:"name" jsonschema:"pool name, e.g. data-pool"`
	Type    string   `json:"type" jsonschema:"pool type: lvm (plain VG), lvm-thin (VG with thin pool), lvm-thin-vdo (thin pool on VDO: dedup and compression), or zfs"`
	Nodes   []string `json:"nodes" jsonschema:"nodes to create the pool on"`
	Devices []string `json:"devices" jsonschema:"block devices to use, e.g. [\"/dev/sdb\"]"`
	SizeGB  uint64   `json:"size_gb,omitempty" jsonschema:"thin pool size in GiB (lvm-thin only); 0 uses the whole VG"`
	Wipe    bool     `json:"wipe,omitempty" jsonschema:"erase devices that hold only old data (a filesystem, partition table, or the label of an array or pool that is not running). Without it such a device is refused with what it holds; show that to the user and get their confirmation before retrying with wipe. A device in use is refused either way."`
}

type poolDeleteIn struct {
	Name string `json:"name" jsonschema:"pool name"`
	Node string `json:"node" jsonschema:"node where the pool exists"`
}

type poolAddDiskIn struct {
	Pool    string   `json:"pool" jsonschema:"pool name"`
	Devices []string `json:"devices" jsonschema:"block devices to add"`
	Nodes   []string `json:"nodes" jsonschema:"nodes to add the devices on"`
	Wipe    bool     `json:"wipe,omitempty" jsonschema:"erase devices that hold only old data (a filesystem, partition table, or the label of an array or pool that is not running). Without it such a device is refused with what it holds; show that to the user and get their confirmation before retrying with wipe. A device in use is refused either way."`
}

// registerClusterTools adds node, health, and pool tools.
func (s *Server) registerClusterTools(srv *mcp.Server) {
	addRead(s, srv, readOnlyTool("haify_node_list", "List nodes",
		"List all storage nodes registered with the Haify controller, including their state and version."),
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, nodeListOut, error) {
			s.logger.Debug("haify_node_list: listing nodes")
			nodes, err := s.client.ListNodes(ctx)
			if err != nil {
				s.logger.Debug("haify_node_list: list failed", zap.Error(err))
				return nil, nodeListOut{}, err
			}
			out := nodeListOut{Nodes: make([]nodeOut, 0, len(nodes))}
			for _, n := range nodes {
				s.logger.Debug("haify_node_list: node", zap.String("node", n.Name), zap.String("address", n.Address),
					zap.String("state", n.State), zap.String("version", n.Version))
				out.Nodes = append(out.Nodes, nodeOut{
					Name:     n.Name,
					Address:  n.Address,
					Hostname: n.Hostname,
					State:    n.State,
					Version:  n.Version,
				})
			}
			s.logger.Debug("haify_node_list: listed", zap.Int("count", len(out.Nodes)))
			return nil, out, nil
		})

	addRead(s, srv, readOnlyTool("haify_node_health_check", "Check node health",
		"Check DRBD, drbd-reactor, and resource-agents availability on storage nodes. "+
			"Use this to diagnose why resources or gateways fail to start."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in healthCheckIn) (*mcp.CallToolResult, healthCheckOut, error) {
			s.logger.Debug("haify_node_health_check: checking nodes", zap.Strings("nodes", in.Nodes))
			targets := in.Nodes
			if len(targets) == 0 {
				s.logger.Debug("haify_node_health_check: no nodes given, checking every registered node")
				nodes, err := s.client.ListNodes(ctx)
				if err != nil {
					s.logger.Debug("haify_node_health_check: list nodes failed", zap.Error(err))
					return nil, healthCheckOut{}, fmt.Errorf("list nodes: %w", err)
				}
				for _, n := range nodes {
					targets = append(targets, n.Name)
				}
			}
			out := healthCheckOut{Results: make([]nodeHealthOut, 0, len(targets))}
			for _, node := range targets {
				start := time.Now()
				h, err := s.client.HealthCheck(ctx, node)
				if err != nil {
					s.logger.Debug("haify_node_health_check: check failed, reporting it for the node", zap.String("node", node),
						zap.Duration("took", time.Since(start)), zap.Error(err))
					out.Results = append(out.Results, nodeHealthOut{Node: node, Error: err.Error()})
					continue
				}
				s.logger.Debug("haify_node_health_check: node checked", zap.String("node", node),
					zap.Bool("drbd_installed", h.DrbdInstalled), zap.String("drbd_version", h.DrbdVersion),
					zap.Bool("drbd_reactor_running", h.DrbdReactorRunning),
					zap.Bool("resource_agents_installed", h.ResourceAgentsInstalled), zap.Duration("took", time.Since(start)))
				out.Results = append(out.Results, nodeHealthOut{
					Node:                    node,
					DrbdInstalled:           h.DrbdInstalled,
					DrbdVersion:             h.DrbdVersion,
					DrbdReactorInstalled:    h.DrbdReactorInstalled,
					DrbdReactorVersion:      h.DrbdReactorVersion,
					DrbdReactorRunning:      h.DrbdReactorRunning,
					ResourceAgentsInstalled: h.ResourceAgentsInstalled,
					AvailableAgents:         h.AvailableAgents,
				})
			}
			s.logger.Debug("haify_node_health_check: done", zap.Int("count", len(out.Results)))
			return nil, out, nil
		})

	addWrite(s, srv, writeTool("haify_node_register", "Register node",
		"Register a storage node with the Haify controller. The controller must reach the node over SSH."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in nodeRegisterIn) (*mcp.CallToolResult, opResult, error) {
			s.logger.Debug("haify_node_register: registering node", zap.String("node", in.Name), zap.String("address", in.Address))
			node, err := s.client.RegisterNode(ctx, in.Name, in.Address)
			if err != nil {
				s.logger.Debug("haify_node_register: register failed", zap.String("node", in.Name), zap.Error(err))
				return nil, opResult{}, err
			}
			s.logger.Debug("haify_node_register: registered", zap.String("node", node.Name), zap.String("state", node.State))
			return nil, ok(fmt.Sprintf("node %s (%s) registered, state %s", node.Name, node.Address, node.State)), nil
		})

	addWrite(s, srv, destructiveTool("haify_node_unregister", "Unregister node",
		"Unregister a storage node from the Haify controller. Resources on the node are not touched."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in nodeUnregisterIn) (*mcp.CallToolResult, opResult, error) {
			s.logger.Debug("haify_node_unregister: unregistering node", zap.String("address", in.Address))
			if err := s.client.UnregisterNode(ctx, in.Address); err != nil {
				s.logger.Debug("haify_node_unregister: unregister failed", zap.String("address", in.Address), zap.Error(err))
				return nil, opResult{}, err
			}
			s.logger.Debug("haify_node_unregister: unregistered", zap.String("address", in.Address))
			return nil, ok(fmt.Sprintf("node %s unregistered", in.Address)), nil
		})

	addRead(s, srv, readOnlyTool("haify_pool_list", "List storage pools",
		"List all storage pools (LVM volume groups, thin pools, ZFS pools) across all nodes with capacity info."),
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, poolListOut, error) {
			s.logger.Debug("haify_pool_list: listing pools")
			pools, err := s.client.ListPools(ctx)
			if err != nil {
				s.logger.Debug("haify_pool_list: list failed", zap.Error(err))
				return nil, poolListOut{}, err
			}
			out := poolListOut{Pools: make([]poolOut, 0, len(pools))}
			for _, p := range pools {
				s.logger.Debug("haify_pool_list: pool", zap.String("pool", p.Name), zap.String("node", p.Node),
					zap.String("type", p.Type), zap.Bool("thin", p.Thin), zap.Uint64("available_bytes", p.AvailableBytes),
					zap.Float64("thin_data_percent", p.ThinDataPercent), zap.Bool("thin_out_of_space", p.ThinOutOfSpace))
				out.Pools = append(out.Pools, poolOut{
					Name:            p.Name,
					Type:            p.Type,
					Node:            p.Node,
					TotalGB:         p.TotalGb,
					FreeGB:          p.FreeGb,
					CapacityGB:      gib(p.CapacityBytes),
					AvailableGB:     gib(p.AvailableBytes),
					Devices:         p.Devices,
					Thin:            p.Thin,
					Compression:     p.Compression,
					CompressRatio:   p.CompressRatio,
					ThinPoolLV:      p.ThinPoolLv,
					ThinSizeBytes:   p.ThinSizeBytes,
					ThinDataPercent: p.ThinDataPercent,
					ThinMetaPercent: p.ThinMetadataPercent,
					ThinOutOfSpace:  p.ThinOutOfSpace,
				})
			}
			s.logger.Debug("haify_pool_list: listed", zap.Int("count", len(out.Pools)))
			return nil, out, nil
		})

	addWrite(s, srv, writeTool("haify_pool_create", "Create storage pool",
		"Create a storage pool on one or more nodes. Types: lvm (plain VG), lvm-thin (VG + thin pool), lvm-thin-vdo (thin pool on VDO), zfs. "+
			"Find usable devices with haify_node_disks first."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in poolCreateIn) (*mcp.CallToolResult, opResult, error) {
			s.logger.Debug("haify_pool_create: creating pool", zap.String("pool", in.Name), zap.String("type", in.Type),
				zap.Strings("nodes", in.Nodes), zap.Strings("devices", in.Devices), zap.Uint64("size_gb", in.SizeGB),
				zap.Bool("wipe", in.Wipe))
			if len(in.Nodes) == 0 || len(in.Devices) == 0 {
				s.logger.Debug("haify_pool_create: refusing, nodes or devices missing")
				return nil, opResult{}, fmt.Errorf("nodes and devices are required")
			}
			var created, failed []string
			for _, node := range in.Nodes {
				s.logger.Debug("haify_pool_create: creating on node", zap.String("pool", in.Name), zap.String("node", node))
				start := time.Now()
				var err error
				switch in.Type {
				case "zfs":
					err = s.client.CreateZFSPoolOptions(ctx, in.Name, node, in.Devices, "", false, in.Wipe)
				case "lvm", "vg":
					err = s.client.CreatePool(ctx, in.Name, "vg", node, in.Devices, in.SizeGB, in.Wipe)
				case "lvm-thin", "thin_pool":
					err = s.client.CreatePool(ctx, in.Name, "thin_pool", node, in.Devices, in.SizeGB, in.Wipe)
				case "lvm-thin-vdo", "thin_vdo":
					err = s.client.CreatePool(ctx, in.Name, "thin_vdo", node, in.Devices, in.SizeGB, in.Wipe)
				default:
					s.logger.Debug("haify_pool_create: refusing unsupported pool type", zap.String("type", in.Type))
					return nil, opResult{}, fmt.Errorf("unsupported pool type %q (use lvm, lvm-thin, lvm-thin-vdo, or zfs)", in.Type)
				}
				if err != nil {
					s.logger.Debug("haify_pool_create: node failed, continuing", zap.String("pool", in.Name), zap.String("node", node),
						zap.Duration("took", time.Since(start)), zap.Error(err))
					failed = append(failed, fmt.Sprintf("%s: %v", node, err))
					continue
				}
				s.logger.Debug("haify_pool_create: created on node", zap.String("pool", in.Name), zap.String("node", node),
					zap.Duration("took", time.Since(start)))
				created = append(created, node)
			}
			if len(created) == 0 {
				s.logger.Debug("haify_pool_create: failed on every node", zap.String("pool", in.Name), zap.Int("failed", len(failed)))
				return nil, opResult{}, fmt.Errorf("pool creation failed on all nodes: %s", strings.Join(failed, "; "))
			}
			s.logger.Debug("haify_pool_create: done", zap.String("pool", in.Name), zap.Strings("created", created),
				zap.Int("failed", len(failed)))
			detail := fmt.Sprintf("pool %s (%s) created on %s", in.Name, in.Type, strings.Join(created, ", "))
			if len(failed) > 0 {
				detail += "; failed on " + strings.Join(failed, "; ")
			}
			return nil, ok(detail), nil
		})

	addWrite(s, srv, destructiveTool("haify_pool_delete", "Delete storage pool",
		"Delete a storage pool from a node. Fails if the pool still backs DRBD resources."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in poolDeleteIn) (*mcp.CallToolResult, opResult, error) {
			s.logger.Debug("haify_pool_delete: deleting pool", zap.String("pool", in.Name), zap.String("node", in.Node))
			if err := s.client.DeletePool(ctx, in.Name, in.Node); err != nil {
				s.logger.Debug("haify_pool_delete: delete failed", zap.String("pool", in.Name), zap.String("node", in.Node), zap.Error(err))
				return nil, opResult{}, err
			}
			s.logger.Debug("haify_pool_delete: deleted", zap.String("pool", in.Name), zap.String("node", in.Node))
			return nil, ok(fmt.Sprintf("pool %s deleted on %s", in.Name, in.Node)), nil
		})

	addWrite(s, srv, writeTool("haify_pool_add_disk", "Add disks to pool",
		"Add block devices to an existing storage pool on one or more nodes; the pool grows into them (a thin pool too). "+
			"Find usable devices with haify_node_disks first. Existing volumes do not grow by themselves, and a replicated "+
			"volume can only grow as far as the fullest of its nodes allows, so add disks on every node that holds a replica."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in poolAddDiskIn) (*mcp.CallToolResult, opResult, error) {
			s.logger.Debug("haify_pool_add_disk: adding disks", zap.String("pool", in.Pool), zap.Strings("nodes", in.Nodes),
				zap.Strings("devices", in.Devices), zap.Bool("wipe", in.Wipe))
			if len(in.Nodes) == 0 || len(in.Devices) == 0 {
				s.logger.Debug("haify_pool_add_disk: refusing, nodes or devices missing")
				return nil, opResult{}, fmt.Errorf("nodes and devices are required")
			}
			var failed []string
			added := 0
			for _, node := range in.Nodes {
				for _, dev := range in.Devices {
					start := time.Now()
					if err := s.client.AddDiskToPool(ctx, in.Pool, dev, node, in.Wipe); err != nil {
						s.logger.Debug("haify_pool_add_disk: device failed, continuing", zap.String("pool", in.Pool),
							zap.String("node", node), zap.String("device", dev), zap.Duration("took", time.Since(start)), zap.Error(err))
						failed = append(failed, fmt.Sprintf("%s %s: %v", node, dev, err))
						continue
					}
					s.logger.Debug("haify_pool_add_disk: device added", zap.String("pool", in.Pool), zap.String("node", node),
						zap.String("device", dev), zap.Duration("took", time.Since(start)))
					added++
				}
			}
			if added == 0 {
				s.logger.Debug("haify_pool_add_disk: failed everywhere", zap.String("pool", in.Pool), zap.Int("failed", len(failed)))
				return nil, opResult{}, fmt.Errorf("adding disks failed everywhere: %s", strings.Join(failed, "; "))
			}
			s.logger.Debug("haify_pool_add_disk: done", zap.String("pool", in.Pool), zap.Int("added", added),
				zap.Int("failed", len(failed)))
			detail := fmt.Sprintf("%d device(s) added to pool %s", added, in.Pool)
			if len(failed) > 0 {
				detail += "; failures: " + strings.Join(failed, "; ")
			}
			return nil, ok(detail), nil
		})
}

// gib renders a byte count in GiB, to two decimals.
func gib(b uint64) float64 {
	return math.Round(float64(b)/(1<<30)*100) / 100
}
