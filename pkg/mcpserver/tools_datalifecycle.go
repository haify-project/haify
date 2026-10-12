package mcpserver

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"

	haifypb "github.com/haify-project/haify/api/proto/v1"
)

// Tools for the three things that decide where a resource's bytes live and how
// long they survive: encryption of the disks under it, a fast tier in front of
// them, and a copy of them somewhere else entirely.
//
// Encryption has no tool of its own — it is a flag on resource creation, and
// belongs with the other creation parameters rather than as a separate verb.
// The read side surfaces it wherever a resource is reported.
//
// There is deliberately no tool for ADDING a backup target. Doing so requires
// an object-store secret, and anything passed as a tool argument is recorded in
// the conversation that called it — the one place a credential must not end up.
// The CLI reads it from HAIFY_BACKUP_SECRET or a file for the same reason.
// Listing and deleting targets are exposed; creating one stays out of band.

type poolCacheAddIn struct {
	Node   string `json:"node" jsonschema:"node holding the pool"`
	Pool   string `json:"pool" jsonschema:"LVM pool to put a cache in front of; must already be a thin pool"`
	Device string `json:"device" jsonschema:"fast block device, e.g. /dev/nvme0n1"`
	Mode   string `json:"mode,omitempty" jsonschema:"writethrough (default) or writeback. writeback acknowledges a write once it reaches the SSD, so a node that dies with a dirty cache takes acknowledged writes with it and the surviving replica may not have them either. Only name it when that is acceptable."`
}

type poolCacheAddOut struct {
	Status    string `json:"status"`
	Detail    string `json:"detail,omitempty"`
	Mode      string `json:"mode" jsonschema:"the mode read back from LVM after attaching"`
	SizeBytes uint64 `json:"size_bytes"`
}

type poolCacheRemoveIn struct {
	Node string `json:"node"`
	Pool string `json:"pool"`
}

type backupTargetOut struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Description string `json:"description,omitempty"`
	Prefix      string `json:"prefix,omitempty"`
}

type backupTargetListOut struct {
	Targets []backupTargetOut `json:"targets"`
}

type backupCreateIn struct {
	Resource string `json:"resource"`
	Target   string `json:"target" jsonschema:"name of a configured backup target"`
	Node     string `json:"node,omitempty" jsonschema:"node to read the snapshot from; empty lets the controller choose"`
	Full     bool   `json:"full,omitempty" jsonschema:"take a full backup even when an incremental is possible"`
}

type backupOut struct {
	ID         string `json:"id"`
	Resource   string `json:"resource"`
	Target     string `json:"target"`
	Node       string `json:"node,omitempty"`
	Backend    string `json:"backend,omitempty"`
	State      string `json:"state" jsonschema:"running, completed, or failed. Only completed is restorable."`
	Error      string `json:"error,omitempty"`
	Kind       string `json:"kind,omitempty" jsonschema:"full, or incremental: restorable only with every backup down to its full one"`
	Parent     string `json:"parent,omitempty"`
	TotalBytes uint64 `json:"total_bytes"`
	Changed    uint64 `json:"changed_bytes,omitempty" jsonschema:"for an incremental, how much it carries"`
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
}

type backupListIn struct {
	Resource string `json:"resource,omitempty" jsonschema:"filter by resource"`
	Target   string `json:"target,omitempty" jsonschema:"filter by target"`
}

type backupListOut struct {
	Backups []backupOut `json:"backups"`
}

type backupRestoreIn struct {
	ID       string `json:"id" jsonschema:"backup id to restore"`
	Resource string `json:"resource,omitempty" jsonschema:"restore into this resource instead of the one it came from"`
	Node     string `json:"node,omitempty"`
}

type backupDeleteIn struct {
	ID    string `json:"id"`
	Node  string `json:"node,omitempty"`
	Force bool   `json:"force,omitempty" jsonschema:"remove the record even if the objects cannot be deleted from the target"`
}

func backupToOut(b *haifypb.BackupInfo) backupOut {
	if b == nil {
		return backupOut{}
	}
	out := backupOut{
		ID: b.Id, Resource: b.Resource, Target: b.Target, Node: b.Node,
		Backend: b.Backend, State: b.State, Error: b.Error, Kind: b.Kind, Parent: b.Parent,
		TotalBytes: b.TotalBytes, StartedAt: b.StartedAt, FinishedAt: b.FinishedAt,
	}
	for _, v := range b.Volumes {
		out.Changed += v.ChangedBytes
	}
	return out
}

// registerDataLifecycleTools adds pool-cache and backup tools.
func (s *Server) registerDataLifecycleTools(srv *mcp.Server) {
	addWrite(s, srv, writeTool("haify_pool_add_cache", "Put an SSD cache in front of a pool",
		"Attach a fast device as a cache for an LVM thin pool, so every volume in it reads and writes through the "+
			"SSD. Defaults to writethrough. Writeback is faster and must be asked for by name: it acknowledges a "+
			"write once it reaches the SSD, so a node that dies with a dirty cache takes acknowledged writes with "+
			"it. Requires a thin pool — a thick pool has no single LV every volume passes through."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in poolCacheAddIn) (*mcp.CallToolResult, poolCacheAddOut, error) {
			s.logger.Debug("haify_pool_add_cache: attaching cache", zap.String("node", in.Node), zap.String("pool", in.Pool),
				zap.String("device", in.Device), zap.String("mode", in.Mode))
			mode, size, err := s.client.AddPoolCache(ctx, in.Node, in.Pool, in.Device, in.Mode)
			if err != nil {
				s.logger.Debug("haify_pool_add_cache: attach failed", zap.String("node", in.Node), zap.String("pool", in.Pool),
					zap.Error(err))
				return nil, poolCacheAddOut{}, err
			}
			s.logger.Debug("haify_pool_add_cache: attached", zap.String("pool", in.Pool), zap.String("mode", mode),
				zap.Uint64("size_bytes", size))
			return nil, poolCacheAddOut{
				Status:    "ok",
				Detail:    fmt.Sprintf("cache attached to %s on %s in %s mode", in.Pool, in.Node, mode),
				Mode:      mode,
				SizeBytes: size,
			}, nil
		})

	addWrite(s, srv, destructiveTool("haify_pool_remove_cache", "Remove a pool's SSD cache",
		"Detach the cache from a pool and release the device. A writeback cache is flushed first; if the flush "+
			"cannot be confirmed this reports failure rather than letting the SSD be pulled with data still on it."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in poolCacheRemoveIn) (*mcp.CallToolResult, opResult, error) {
			s.logger.Debug("haify_pool_remove_cache: removing cache", zap.String("node", in.Node), zap.String("pool", in.Pool))
			if err := s.client.RemovePoolCache(ctx, in.Node, in.Pool); err != nil {
				s.logger.Debug("haify_pool_remove_cache: remove failed", zap.String("node", in.Node), zap.String("pool", in.Pool),
					zap.Error(err))
				return nil, opResult{}, err
			}
			s.logger.Debug("haify_pool_remove_cache: removed", zap.String("node", in.Node), zap.String("pool", in.Pool))
			return nil, ok("cache removed from " + in.Pool + " on " + in.Node), nil
		})

	addRead(s, srv, readOnlyTool("haify_backup_target_list", "List backup targets",
		"List the configured off-cluster destinations backups can be shipped to (S3, SMB, WebDAV). Credentials "+
			"are never returned."),
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, backupTargetListOut, error) {
			s.logger.Debug("haify_backup_target_list: listing backup targets")
			ts, err := s.client.ListBackupTargets(ctx)
			if err != nil {
				s.logger.Debug("haify_backup_target_list: list failed", zap.Error(err))
				return nil, backupTargetListOut{}, err
			}
			out := backupTargetListOut{Targets: make([]backupTargetOut, 0, len(ts))}
			for _, t := range ts {
				s.logger.Debug("haify_backup_target_list: target", zap.String("name", t.Name), zap.String("kind", t.Kind),
					zap.String("prefix", t.Prefix))
				out.Targets = append(out.Targets, backupTargetOut{
					Name: t.Name, Kind: t.Kind, Description: t.Description, Prefix: t.Prefix,
				})
			}
			s.logger.Debug("haify_backup_target_list: listed", zap.Int("count", len(out.Targets)))
			return nil, out, nil
		})

	addWrite(s, srv, destructiveTool("haify_backup_target_delete", "Delete a backup target",
		"Remove a backup destination. Backups already shipped there become unrestorable through Haify."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			Name  string `json:"name"`
			Force bool   `json:"force,omitempty"`
		}) (*mcp.CallToolResult, opResult, error) {
			s.logger.Debug("haify_backup_target_delete: deleting target", zap.String("name", in.Name), zap.Bool("force", in.Force))
			if err := s.client.DeleteBackupTarget(ctx, in.Name, in.Force); err != nil {
				s.logger.Debug("haify_backup_target_delete: delete failed", zap.String("name", in.Name), zap.Error(err))
				return nil, opResult{}, err
			}
			s.logger.Debug("haify_backup_target_delete: deleted", zap.String("name", in.Name))
			return nil, ok("backup target " + in.Name + " deleted"), nil
		})

	addRead(s, srv, readOnlyTool("haify_backup_list", "List backups",
		"List backups shipped off the cluster, with their state. Only a backup in state 'completed' can be "+
			"restored; 'running' means it is still uploading and 'failed' means it is not a usable copy."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in backupListIn) (*mcp.CallToolResult, backupListOut, error) {
			s.logger.Debug("haify_backup_list: listing backups", zap.String("resource", in.Resource), zap.String("target", in.Target))
			bs, err := s.client.ListBackups(ctx, in.Resource, in.Target)
			if err != nil {
				s.logger.Debug("haify_backup_list: list failed", zap.Error(err))
				return nil, backupListOut{}, err
			}
			out := backupListOut{Backups: make([]backupOut, 0, len(bs))}
			for _, b := range bs {
				s.logger.Debug("haify_backup_list: backup", zap.String("id", b.Id), zap.String("resource", b.Resource),
					zap.String("target", b.Target), zap.String("state", b.State), zap.String("kind", b.Kind))
				out.Backups = append(out.Backups, backupToOut(b))
			}
			s.logger.Debug("haify_backup_list: listed", zap.Int("count", len(out.Backups)))
			return nil, out, nil
		})

	addWrite(s, srv, writeTool("haify_backup_create", "Back a resource up off-cluster",
		"Snapshot a resource and ship the image to a configured target. This is the only copy that survives losing "+
			"the cluster: snapshots live in the same pool, and WAN DR is a replica, so a deletion replicates to it. "+
			"After the first, a backup to the same target is incremental: only the blocks changed since the last one "+
			"are sent, read from thin-pool metadata. Set full to start a new chain. An incremental cannot be deleted "+
			"while a later one is built on it."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in backupCreateIn) (*mcp.CallToolResult, backupOut, error) {
			s.logger.Debug("haify_backup_create: backing up", zap.String("resource", in.Resource), zap.String("target", in.Target),
				zap.String("node", in.Node), zap.Bool("full", in.Full))
			b, err := s.client.CreateBackup(ctx, in.Resource, in.Target, in.Node, in.Full)
			if err != nil {
				s.logger.Debug("haify_backup_create: backup failed", zap.String("resource", in.Resource),
					zap.String("target", in.Target), zap.Error(err))
				return nil, backupOut{}, err
			}
			s.logger.Debug("haify_backup_create: started", zap.String("id", b.GetId()), zap.String("state", b.GetState()),
				zap.String("kind", b.GetKind()), zap.String("parent", b.GetParent()))
			return nil, backupToOut(b), nil
		})

	addWrite(s, srv, destructiveTool("haify_backup_restore", "Restore a backup",
		"Write a backup image back onto a resource, overwriting it from byte zero. Refused when the resource is "+
			"Primary anywhere, when a gateway exports it, or when the destination is smaller than the image. "+
			"Confirm with the operator before calling: this destroys whatever the resource currently holds."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in backupRestoreIn) (*mcp.CallToolResult, backupOut, error) {
			s.logger.Debug("haify_backup_restore: restoring", zap.String("id", in.ID), zap.String("resource", in.Resource),
				zap.String("node", in.Node))
			b, err := s.client.RestoreBackup(ctx, in.ID, in.Resource, in.Node)
			if err != nil {
				s.logger.Debug("haify_backup_restore: restore failed", zap.String("id", in.ID), zap.Error(err))
				return nil, backupOut{}, err
			}
			s.logger.Debug("haify_backup_restore: done", zap.String("id", b.GetId()), zap.String("state", b.GetState()))
			return nil, backupToOut(b), nil
		})

	addWrite(s, srv, destructiveTool("haify_backup_delete", "Delete a backup",
		"Remove a backup and its objects from the target. Irreversible."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in backupDeleteIn) (*mcp.CallToolResult, opResult, error) {
			s.logger.Debug("haify_backup_delete: deleting backup", zap.String("id", in.ID), zap.String("node", in.Node),
				zap.Bool("force", in.Force))
			if err := s.client.DeleteBackup(ctx, in.ID, in.Node, in.Force); err != nil {
				s.logger.Debug("haify_backup_delete: delete failed", zap.String("id", in.ID), zap.Error(err))
				return nil, opResult{}, err
			}
			s.logger.Debug("haify_backup_delete: deleted", zap.String("id", in.ID))
			return nil, ok("backup " + in.ID + " deleted"), nil
		})
}
