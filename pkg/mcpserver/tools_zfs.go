package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"
)

// ZFS tools.
//
// The controller has had a full ZFS surface for a while — pools, datasets,
// volumes, snapshots — and ControllerClient already declared the methods, but
// none of it was ever wrapped as a tool. An assistant could create a resource
// with storage_type "zfs" and then had no way to inspect or manage what it had
// created.
//
// LVM snapshots are already covered by the storage-type-aware haify_snapshot_*
// tools; what is missing here is everything below the resource layer.

type zfsPoolListOut struct {
	Pools []poolOut `json:"pools"`
}

type zfsPoolDeleteIn struct {
	Name string `json:"name" jsonschema:"ZFS pool name"`
	Node string `json:"node" jsonschema:"node the pool lives on"`
}

type zfsDatasetIn struct {
	DatasetPath string `json:"dataset_path" jsonschema:"full dataset path, e.g. tank/data"`
	Node        string `json:"node" jsonschema:"node the dataset lives on"`
}

type zfsVolumeCreateIn struct {
	Pool   string `json:"pool" jsonschema:"ZFS pool name"`
	Volume string `json:"volume" jsonschema:"volume (zvol) name within the pool"`
	Size   string `json:"size" jsonschema:"size with unit, e.g. 10G"`
	Node   string `json:"node"`
}

type zfsVolumeResizeIn struct {
	VolumePath string `json:"volume_path" jsonschema:"full zvol path, e.g. tank/vol1"`
	NewSize    string `json:"new_size" jsonschema:"new size with unit, e.g. 20G; ZFS volumes grow, they do not shrink"`
	Node       string `json:"node"`
}

type zfsSnapshotCloneIn struct {
	Snapshot  string `json:"snapshot" jsonschema:"source snapshot, e.g. tank/data@snap1"`
	ClonePath string `json:"clone_path" jsonschema:"dataset path for the clone"`
	Node      string `json:"node"`
}

// registerZFSTools adds ZFS pool, dataset, volume and clone tools.
func (s *Server) registerZFSTools(srv *mcp.Server) {
	addRead(s, srv, readOnlyTool("haify_zfs_pool_list", "List ZFS pools",
		"List ZFS pools known to the controller, with capacity. ZFS pools are an alternative backing store to LVM "+
			"volume groups; a resource picks one via storage_type at creation."),
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, zfsPoolListOut, error) {
			s.logger.Debug("haify_zfs_pool_list: listing ZFS pools")
			pools, err := s.client.ListZFSpools(ctx)
			if err != nil {
				s.logger.Debug("haify_zfs_pool_list: list failed", zap.Error(err))
				return nil, zfsPoolListOut{}, err
			}
			out := zfsPoolListOut{Pools: make([]poolOut, 0, len(pools))}
			for _, p := range pools {
				s.logger.Debug("haify_zfs_pool_list: pool", zap.String("pool", p.Name), zap.String("node", p.Node),
					zap.String("type", p.Type))
				out.Pools = append(out.Pools, poolOut{
					Name:    p.Name,
					Type:    p.Type,
					Node:    p.Node,
					TotalGB: p.TotalGb,
					FreeGB:  p.FreeGb,
					Devices: p.Devices,
				})
			}
			s.logger.Debug("haify_zfs_pool_list: listed", zap.Int("count", len(out.Pools)))
			return nil, out, nil
		})

	addWrite(s, srv, destructiveTool("haify_zfs_pool_delete", "Delete a ZFS pool",
		"Destroy a ZFS pool and everything in it. Irreversible: every dataset, volume and snapshot on the pool "+
			"goes with it. Refuse to run this without explicit confirmation naming the pool."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in zfsPoolDeleteIn) (*mcp.CallToolResult, opResult, error) {
			s.logger.Debug("haify_zfs_pool_delete: deleting ZFS pool", zap.String("pool", in.Name), zap.String("node", in.Node))
			if err := s.client.DeleteZFSPool(ctx, in.Name, in.Node); err != nil {
				s.logger.Debug("haify_zfs_pool_delete: failed", zap.String("pool", in.Name), zap.String("node", in.Node), zap.Error(err))
				return nil, opResult{}, err
			}
			s.logger.Debug("haify_zfs_pool_delete: done", zap.String("pool", in.Name), zap.String("node", in.Node))
			return nil, ok("ZFS pool " + in.Name + " deleted on " + in.Node), nil
		})

	addWrite(s, srv, writeTool("haify_zfs_dataset_create", "Create a ZFS dataset",
		"Create a ZFS filesystem dataset. Datasets hold files; use haify_zfs_volume_create for a block device."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in zfsDatasetIn) (*mcp.CallToolResult, opResult, error) {
			s.logger.Debug("haify_zfs_dataset_create: creating dataset", zap.String("dataset", in.DatasetPath), zap.String("node", in.Node))
			if err := s.client.CreateZFSDataset(ctx, in.DatasetPath, in.Node); err != nil {
				s.logger.Debug("haify_zfs_dataset_create: failed", zap.String("dataset", in.DatasetPath), zap.String("node", in.Node), zap.Error(err))
				return nil, opResult{}, err
			}
			s.logger.Debug("haify_zfs_dataset_create: done", zap.String("dataset", in.DatasetPath), zap.String("node", in.Node))
			return nil, ok("dataset " + in.DatasetPath + " created on " + in.Node), nil
		})

	addWrite(s, srv, destructiveTool("haify_zfs_dataset_delete", "Delete a ZFS dataset",
		"Destroy a ZFS dataset and its contents. Irreversible."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in zfsDatasetIn) (*mcp.CallToolResult, opResult, error) {
			s.logger.Debug("haify_zfs_dataset_delete: deleting dataset", zap.String("dataset", in.DatasetPath), zap.String("node", in.Node))
			if err := s.client.DeleteZFSDataset(ctx, in.DatasetPath, in.Node); err != nil {
				s.logger.Debug("haify_zfs_dataset_delete: failed", zap.String("dataset", in.DatasetPath), zap.String("node", in.Node), zap.Error(err))
				return nil, opResult{}, err
			}
			s.logger.Debug("haify_zfs_dataset_delete: done", zap.String("dataset", in.DatasetPath), zap.String("node", in.Node))
			return nil, ok("dataset " + in.DatasetPath + " deleted on " + in.Node), nil
		})

	addWrite(s, srv, writeTool("haify_zfs_volume_create", "Create a ZFS volume (zvol)",
		"Create a ZFS block device. This is what a DRBD resource backs onto when its storage type is zfs."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in zfsVolumeCreateIn) (*mcp.CallToolResult, opResult, error) {
			s.logger.Debug("haify_zfs_volume_create: creating zvol", zap.String("pool", in.Pool), zap.String("volume", in.Volume), zap.String("size", in.Size), zap.String("node", in.Node))
			if err := s.client.CreateZFSVolume(ctx, in.Pool, in.Volume, in.Size, in.Node); err != nil {
				s.logger.Debug("haify_zfs_volume_create: failed", zap.String("pool", in.Pool), zap.String("volume", in.Volume), zap.String("size", in.Size), zap.String("node", in.Node), zap.Error(err))
				return nil, opResult{}, err
			}
			s.logger.Debug("haify_zfs_volume_create: done", zap.String("pool", in.Pool), zap.String("volume", in.Volume), zap.String("size", in.Size), zap.String("node", in.Node))
			return nil, ok("zvol " + in.Pool + "/" + in.Volume + " created on " + in.Node), nil
		})

	addWrite(s, srv, writeTool("haify_zfs_volume_resize", "Resize a ZFS volume",
		"Grow a ZFS volume. Resizing the backing volume alone does not grow a DRBD resource on top of it — "+
			"use haify_resource_resize_volume for that."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in zfsVolumeResizeIn) (*mcp.CallToolResult, opResult, error) {
			s.logger.Debug("haify_zfs_volume_resize: resizing zvol", zap.String("volume", in.VolumePath), zap.String("new_size", in.NewSize), zap.String("node", in.Node))
			if err := s.client.ResizeZFSVolume(ctx, in.VolumePath, in.NewSize, in.Node); err != nil {
				s.logger.Debug("haify_zfs_volume_resize: failed", zap.String("volume", in.VolumePath), zap.String("new_size", in.NewSize), zap.String("node", in.Node), zap.Error(err))
				return nil, opResult{}, err
			}
			s.logger.Debug("haify_zfs_volume_resize: done", zap.String("volume", in.VolumePath), zap.String("new_size", in.NewSize), zap.String("node", in.Node))
			return nil, ok("zvol " + in.VolumePath + " resized to " + in.NewSize), nil
		})

	addWrite(s, srv, writeTool("haify_zfs_snapshot_clone", "Clone a ZFS snapshot",
		"Create a writable dataset from a ZFS snapshot. The clone shares blocks with its origin, so the origin "+
			"snapshot cannot be destroyed while the clone exists."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in zfsSnapshotCloneIn) (*mcp.CallToolResult, opResult, error) {
			s.logger.Debug("haify_zfs_snapshot_clone: cloning snapshot", zap.String("snapshot", in.Snapshot), zap.String("clone_path", in.ClonePath), zap.String("node", in.Node))
			if err := s.client.CloneZFSSnapshot(ctx, in.Snapshot, in.ClonePath, in.Node); err != nil {
				s.logger.Debug("haify_zfs_snapshot_clone: failed", zap.String("snapshot", in.Snapshot), zap.String("clone_path", in.ClonePath), zap.String("node", in.Node), zap.Error(err))
				return nil, opResult{}, err
			}
			s.logger.Debug("haify_zfs_snapshot_clone: done", zap.String("snapshot", in.Snapshot), zap.String("clone_path", in.ClonePath), zap.String("node", in.Node))
			return nil, ok("cloned " + in.Snapshot + " to " + in.ClonePath), nil
		})
}
