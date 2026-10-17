package mcpserver

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"
)

// Volume-level tools of a resource: adding, removing and growing volumes,
// DRBD options, and the filesystem on a volume.

type volumeAddIn struct {
	Resource string `json:"resource" jsonschema:"DRBD resource name"`
	Volume   string `json:"volume" jsonschema:"volume name"`
	Pool     string `json:"pool" jsonschema:"backing storage pool"`
	SizeGB   uint32 `json:"size_gb" jsonschema:"volume size in GiB"`
}

type resourceSetOptionsIn struct {
	Resource string            `json:"resource" jsonschema:"DRBD resource name"`
	Options  map[string]string `json:"options" jsonschema:"DRBD options as section/key -> value (e.g. net/max-buffers: 8000, on-no-quorum: suspend-io, disk/on-io-error: detach); a bare key defaults to the resource-level options section; an empty value removes the key"`
}

type resourceOptionOut struct {
	Key    string `json:"key"`
	Value  string `json:"value,omitempty"`
	Secret bool   `json:"secret,omitempty"`
}

type resourceGetOptionsOut struct {
	Options []resourceOptionOut `json:"options"`
}

type volumeRemoveIn struct {
	Resource string `json:"resource" jsonschema:"DRBD resource name"`
	VolumeID uint32 `json:"volume_id" jsonschema:"volume ID within the resource"`
}

type volumeResizeIn struct {
	Resource string `json:"resource" jsonschema:"DRBD resource name"`
	VolumeID uint32 `json:"volume_id" jsonschema:"volume ID within the resource"`
	SizeGB   uint32 `json:"size_gb" jsonschema:"new size in GiB (must be larger than current)"`
}

type filesystemIn struct {
	Resource string `json:"resource" jsonschema:"DRBD resource name"`
	VolumeID uint32 `json:"volume_id" jsonschema:"volume ID within the resource (0 for the first volume)"`
	Fstype   string `json:"fstype" jsonschema:"filesystem type: ext4 or xfs"`
	Node     string `json:"node" jsonschema:"node where the resource is Primary"`
}

type mountIn struct {
	Resource string `json:"resource" jsonschema:"DRBD resource name"`
	VolumeID uint32 `json:"volume_id" jsonschema:"volume ID within the resource (0 for the first volume)"`
	Path     string `json:"path" jsonschema:"mount point, e.g. /mnt/data"`
	Node     string `json:"node" jsonschema:"node where the resource is Primary"`
	Fstype   string `json:"fstype,omitempty" jsonschema:"filesystem type (default ext4)"`
}

type unmountIn struct {
	Resource string `json:"resource" jsonschema:"DRBD resource name"`
	VolumeID uint32 `json:"volume_id" jsonschema:"volume ID within the resource"`
	Node     string `json:"node" jsonschema:"node where the volume is mounted"`
}

// registerResourceVolumeTools adds the per-volume resource tools.
func (s *Server) registerResourceVolumeTools(srv *mcp.Server) {
	addWrite(s, srv, writeTool("haify_resource_add_volume", "Add volume",
		"Add another replicated volume to an existing DRBD resource."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in volumeAddIn) (*mcp.CallToolResult, opResult, error) {
			s.logger.Debug("haify_resource_add_volume: adding volume", zap.String("resource", in.Resource), zap.String("volume", in.Volume), zap.String("pool", in.Pool), zap.Uint32("size_gb", in.SizeGB))
			if err := s.client.AddVolume(ctx, in.Resource, in.Volume, in.Pool, in.SizeGB); err != nil {
				s.logger.Debug("haify_resource_add_volume: failed", zap.String("resource", in.Resource), zap.String("volume", in.Volume), zap.String("pool", in.Pool), zap.Uint32("size_gb", in.SizeGB), zap.Error(err))
				return nil, opResult{}, err
			}
			s.logger.Debug("haify_resource_add_volume: done", zap.String("resource", in.Resource), zap.String("volume", in.Volume), zap.String("pool", in.Pool), zap.Uint32("size_gb", in.SizeGB))
			return nil, ok(fmt.Sprintf("volume %s (%d GiB) added to resource %s", in.Volume, in.SizeGB, in.Resource)), nil
		})

	addWrite(s, srv, writeTool("haify_resource_set_options", "Set DRBD options",
		"Update DRBD options on an existing resource and apply them live with "+
			"`drbdadm adjust`, without recreating it. Options use the "+
			"\"section/key\" form (e.g. net/max-buffers, disk/on-io-error); a bare "+
			"key defaults to the resource-level options section, and an empty value "+
			"removes a key. A node's drbdadm checks the new config first, so an "+
			"option DRBD does not accept changes nothing; if applying fails, the "+
			"previous config is put back."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in resourceSetOptionsIn) (*mcp.CallToolResult, opResult, error) {
			s.logger.Debug("haify_resource_set_options: setting options", zap.String("resource", in.Resource),
				zap.Array("option_keys", keysOf(in.Options)))
			if len(in.Options) == 0 {
				s.logger.Debug("haify_resource_set_options: refusing, no options given")
				return nil, opResult{}, fmt.Errorf("options must not be empty")
			}
			if err := s.client.UpdateResourceOptions(ctx, in.Resource, in.Options); err != nil {
				s.logger.Debug("haify_resource_set_options: failed", zap.String("resource", in.Resource), zap.Error(err))
				return nil, opResult{}, err
			}
			s.logger.Debug("haify_resource_set_options: applied", zap.String("resource", in.Resource), zap.Int("count", len(in.Options)))
			return nil, ok(fmt.Sprintf("applied %d option(s) to resource %s and adjusted", len(in.Options), in.Resource)), nil
		})

	addRead(s, srv, readOnlyTool("haify_resource_get_options", "Show DRBD options",
		"List the DRBD options a resource's config sets, as section/key and value. A shared-secret is "+
			"reported as set, without its value."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			Resource string `json:"resource" jsonschema:"DRBD resource name"`
		}) (*mcp.CallToolResult, resourceGetOptionsOut, error) {
			s.logger.Debug("haify_resource_get_options: reading", zap.String("resource", in.Resource))
			opts, err := s.client.GetResourceOptions(ctx, in.Resource)
			if err != nil {
				s.logger.Debug("haify_resource_get_options: failed", zap.String("resource", in.Resource), zap.Error(err))
				return nil, resourceGetOptionsOut{}, err
			}
			out := resourceGetOptionsOut{Options: make([]resourceOptionOut, 0, len(opts))}
			for _, o := range opts {
				out.Options = append(out.Options, resourceOptionOut{Key: o.Key, Value: o.Value, Secret: o.Secret})
			}
			s.logger.Debug("haify_resource_get_options: read", zap.String("resource", in.Resource), zap.Int("count", len(out.Options)))
			return nil, out, nil
		})

	addWrite(s, srv, destructiveTool("haify_resource_remove_volume", "Remove volume",
		"Remove a volume from a DRBD resource on all nodes. Data on the volume is lost."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in volumeRemoveIn) (*mcp.CallToolResult, opResult, error) {
			s.logger.Debug("haify_resource_remove_volume: removing volume", zap.String("resource", in.Resource), zap.Uint32("volume", in.VolumeID))
			if err := s.client.RemoveVolume(ctx, in.Resource, in.VolumeID); err != nil {
				s.logger.Debug("haify_resource_remove_volume: failed", zap.String("resource", in.Resource), zap.Uint32("volume", in.VolumeID), zap.Error(err))
				return nil, opResult{}, err
			}
			s.logger.Debug("haify_resource_remove_volume: done", zap.String("resource", in.Resource), zap.Uint32("volume", in.VolumeID))
			return nil, ok(fmt.Sprintf("volume %d removed from resource %s", in.VolumeID, in.Resource)), nil
		})

	addWrite(s, srv, writeTool("haify_resource_resize_volume", "Resize volume",
		"Grow a DRBD volume on all nodes. Shrinking is not supported. The space is not usable "+
			"until the filesystem on the Primary is grown too (resize2fs/xfs_growfs) — this tool "+
			"does not do that."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in volumeResizeIn) (*mcp.CallToolResult, opResult, error) {
			s.logger.Debug("haify_resource_resize_volume: resizing volume", zap.String("resource", in.Resource), zap.Uint32("volume", in.VolumeID), zap.Uint32("size_gb", in.SizeGB))
			if err := s.client.ResizeVolume(ctx, in.Resource, in.VolumeID, in.SizeGB); err != nil {
				s.logger.Debug("haify_resource_resize_volume: failed", zap.String("resource", in.Resource), zap.Uint32("volume", in.VolumeID), zap.Uint32("size_gb", in.SizeGB), zap.Error(err))
				return nil, opResult{}, err
			}
			s.logger.Debug("haify_resource_resize_volume: done", zap.String("resource", in.Resource), zap.Uint32("volume", in.VolumeID), zap.Uint32("size_gb", in.SizeGB))
			return nil, ok(fmt.Sprintf("volume %d of resource %s resized to %d GiB", in.VolumeID, in.Resource, in.SizeGB)), nil
		})

	addWrite(s, srv, destructiveTool("haify_resource_create_filesystem", "Create filesystem",
		"Format a DRBD volume with a filesystem on the Primary node. Any existing data is destroyed."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in filesystemIn) (*mcp.CallToolResult, opResult, error) {
			s.logger.Debug("haify_resource_create_filesystem: formatting volume", zap.String("resource", in.Resource), zap.Uint32("volume", in.VolumeID), zap.String("node", in.Node), zap.String("fstype", in.Fstype))
			if err := s.client.CreateFilesystem(ctx, in.Resource, in.VolumeID, in.Node, in.Fstype); err != nil {
				s.logger.Debug("haify_resource_create_filesystem: failed", zap.String("resource", in.Resource), zap.Uint32("volume", in.VolumeID), zap.String("node", in.Node), zap.String("fstype", in.Fstype), zap.Error(err))
				return nil, opResult{}, err
			}
			s.logger.Debug("haify_resource_create_filesystem: done", zap.String("resource", in.Resource), zap.Uint32("volume", in.VolumeID), zap.String("node", in.Node), zap.String("fstype", in.Fstype))
			return nil, ok(fmt.Sprintf("%s filesystem created on %s volume %d (node %s)",
				in.Fstype, in.Resource, in.VolumeID, in.Node)), nil
		})

	addWrite(s, srv, writeTool("haify_resource_mount", "Mount volume",
		"Mount a DRBD volume on its Primary node."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in mountIn) (*mcp.CallToolResult, opResult, error) {
			s.logger.Debug("haify_resource_mount: mounting volume", zap.String("resource", in.Resource),
				zap.Uint32("volume", in.VolumeID), zap.String("path", in.Path), zap.String("node", in.Node),
				zap.String("fstype", in.Fstype))
			fstype := in.Fstype
			if fstype == "" {
				s.logger.Debug("haify_resource_mount: no filesystem type given, using ext4")
				fstype = "ext4"
			}
			if err := s.client.MountResource(ctx, in.Resource, in.VolumeID, in.Path, in.Node, fstype); err != nil {
				s.logger.Debug("haify_resource_mount: failed", zap.String("resource", in.Resource), zap.Error(err))
				return nil, opResult{}, err
			}
			s.logger.Debug("haify_resource_mount: mounted", zap.String("resource", in.Resource), zap.String("path", in.Path))
			return nil, ok(fmt.Sprintf("%s volume %d mounted at %s on %s", in.Resource, in.VolumeID, in.Path, in.Node)), nil
		})

	addWrite(s, srv, destructiveTool("haify_resource_unmount", "Unmount volume",
		"Unmount a DRBD volume on a node."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in unmountIn) (*mcp.CallToolResult, opResult, error) {
			s.logger.Debug("haify_resource_unmount: unmounting volume", zap.String("resource", in.Resource), zap.Uint32("volume", in.VolumeID), zap.String("node", in.Node))
			if err := s.client.UnmountResource(ctx, in.Resource, in.VolumeID, in.Node); err != nil {
				s.logger.Debug("haify_resource_unmount: failed", zap.String("resource", in.Resource), zap.Uint32("volume", in.VolumeID), zap.String("node", in.Node), zap.Error(err))
				return nil, opResult{}, err
			}
			s.logger.Debug("haify_resource_unmount: done", zap.String("resource", in.Resource), zap.Uint32("volume", in.VolumeID), zap.String("node", in.Node))
			return nil, ok(fmt.Sprintf("%s volume %d unmounted on %s", in.Resource, in.VolumeID, in.Node)), nil
		})
}
