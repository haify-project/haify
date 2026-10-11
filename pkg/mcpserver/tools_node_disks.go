package mcpserver

import (
	"context"

	"github.com/haify-project/haify/pkg/util"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type nodeDisksIn struct {
	Node string `json:"node,omitempty" jsonschema:"only this node; empty for every online node"`
}

type nodeDiskOut struct {
	Node   string `json:"node"`
	Device string `json:"device" jsonschema:"pass this to haify_pool_create, haify_pool_add_disk or haify_pool_replace_disk"`
	ByID   string `json:"by_id,omitempty" jsonschema:"the /dev/disk/by-id name of the physical disk, stable across reboots"`
	Size   string `json:"size"`
	Model  string `json:"model,omitempty"`
	Serial string `json:"serial,omitempty"`
	State  string `json:"state" jsonschema:"free (usable as it is), wipe (holds only old data; usable with wipe after the user confirms), pool (already in a Haify pool) or busy (in use; never usable)"`
	Pool   string `json:"pool,omitempty"`
	Detail string `json:"detail,omitempty" jsonschema:"what a wipe disk holds, or what a busy disk is used by"`
}

type nodeDisksOut struct {
	Disks []nodeDiskOut `json:"disks"`
}

func (s *Server) registerNodeDiskTools(srv *mcp.Server) {
	addReadWithin(s, srv, readOnlyTool("haify_node_disks", "List the disks on the nodes",
		"List every disk on the nodes and whether a pool can use it: free, holding only old data (usable with wipe), "+
			"already in a pool, or in use. Use it when a disk has been added to a server, before creating a pool or "+
			"adding or replacing a disk. Probes the nodes over SSH, so it takes a few seconds."),
		poolDisksTimeout,
		func(ctx context.Context, _ *mcp.CallToolRequest, in nodeDisksIn) (*mcp.CallToolResult, nodeDisksOut, error) {
			disks, err := s.client.ListNodeDisks(ctx, in.Node)
			if err != nil {
				return nil, nodeDisksOut{}, err
			}
			out := nodeDisksOut{Disks: make([]nodeDiskOut, 0, len(disks))}
			for _, d := range disks {
				out.Disks = append(out.Disks, nodeDiskOut{Node: d.Node, Device: d.Device, ByID: d.ById,
					Size: util.FormatBytes(d.SizeBytes), Model: d.Model, Serial: d.Serial, State: d.State,
					Pool: d.Pool, Detail: d.Detail})
			}
			return nil, out, nil
		})
}
