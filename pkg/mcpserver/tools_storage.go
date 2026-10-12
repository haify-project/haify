package mcpserver

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/util"
)

// Storage upkeep: the disks under the pools, trimming, and the long jobs that
// move data between disks and pools.

// poolDisksTimeout covers one SSH probe per node, smartctl included.
const poolDisksTimeout = 3 * time.Minute

type poolDisksIn struct {
	Pool string `json:"pool,omitempty" jsonschema:"only this pool"`
	Node string `json:"node,omitempty" jsonschema:"only this node"`
}

type poolDiskOut struct {
	Node   string `json:"node"`
	Pool   string `json:"pool"`
	Device string `json:"device" jsonschema:"the disk as the pool knows it (the LVM physical volume); pass this to remove/replace"`
	Size   string `json:"size"`
	Used   string `json:"used" jsonschema:"allocated to logical volumes; this much has to move before the disk can leave"`
	Health string `json:"health" jsonschema:"ok, warn, fail, or unknown (no smartctl on the node, or a device without SMART such as a virtual disk)"`
	Detail string `json:"detail,omitempty" jsonschema:"why: the SMART or NVMe findings, or why health is unknown"`
	Model  string `json:"model,omitempty"`
	Serial string `json:"serial,omitempty"`
}

type poolDisksOut struct {
	Disks []poolDiskOut `json:"disks"`
}

type poolTrimIn struct {
	Node string `json:"node,omitempty" jsonschema:"only this node; empty for every online node"`
}

type trimResultOut struct {
	Node    string `json:"node"`
	Mount   string `json:"mount"`
	Trimmed string `json:"trimmed,omitempty"`
	Error   string `json:"error,omitempty"`
}

type poolTrimOut struct {
	Summary string          `json:"summary"`
	Results []trimResultOut `json:"results"`
}

type poolRemoveDiskIn struct {
	Pool string `json:"pool" jsonschema:"pool name"`
	Node string `json:"node" jsonschema:"node the disk is in"`
	Disk string `json:"disk" jsonschema:"the device as haify_pool_disks lists it"`
}

type poolReplaceDiskIn struct {
	Pool    string `json:"pool" jsonschema:"pool name"`
	Node    string `json:"node" jsonschema:"node the disk is in"`
	Disk    string `json:"disk" jsonschema:"the disk to replace, as haify_pool_disks lists it"`
	NewDisk string `json:"new_disk" jsonschema:"an empty disk on the same node to move its data to"`
	Wipe    bool   `json:"wipe,omitempty" jsonschema:"erase devices that hold only old data (a filesystem, partition table, or the label of an array or pool that is not running). Without it such a device is refused with what it holds; show that to the user and get their confirmation before retrying with wipe. A device in use is refused either way."`
}

type moveVolumeIn struct {
	Resource string `json:"resource" jsonschema:"resource name"`
	Volume   int32  `json:"volume" jsonschema:"volume id (0 for a single-volume resource)"`
	Pool     string `json:"pool" jsonschema:"target pool; it must exist on every node holding a replica"`
}

type jobStartOut struct {
	JobID  string `json:"job_id" jsonschema:"follow it with haify_storage_jobs"`
	Detail string `json:"detail"`
}

type storageJobsIn struct {
	All bool `json:"all,omitempty" jsonschema:"include finished jobs (kept 14 days)"`
}

type storageJobOut struct {
	ID       string `json:"id"`
	Kind     string `json:"kind" jsonschema:"remove-disk, replace-disk or move-volume"`
	State    string `json:"state" jsonschema:"running, done or failed"`
	Subject  string `json:"subject"`
	Progress string `json:"progress,omitempty"`
	Message  string `json:"message,omitempty" jsonschema:"the outcome, or why it failed"`
	Started  string `json:"started" jsonschema:"RFC3339"`
	Updated  string `json:"updated" jsonschema:"RFC3339"`
}

type storageJobsOut struct {
	Jobs []storageJobOut `json:"jobs"`
}

func unixRFC3339(sec int64) string {
	if sec == 0 {
		return ""
	}
	return time.Unix(sec, 0).UTC().Format(time.RFC3339)
}

// registerStorageTools adds disk, trim and storage job tools.
func (s *Server) registerStorageTools(srv *mcp.Server) {
	addReadWithin(s, srv, readOnlyTool("haify_pool_disks", "List disks and their health",
		"List the disks under the storage pools on every node, with their size, how much of each is allocated, and "+
			"their health from SMART or the NVMe health log (wear, reallocated or pending sectors, media errors, the "+
			"drive's own verdict). Use it before replacing or removing a disk, and when a disk.health event or "+
			"inspection finding names one. Probes every node over SSH, so it takes a few seconds."),
		poolDisksTimeout,
		func(ctx context.Context, _ *mcp.CallToolRequest, in poolDisksIn) (*mcp.CallToolResult, poolDisksOut, error) {
			s.logger.Debug("haify_pool_disks: listing pool disks", zap.String("pool", in.Pool), zap.String("node", in.Node))
			disks, err := s.client.ListPoolDisks(ctx, in.Pool, in.Node)
			if err != nil {
				s.logger.Debug("haify_pool_disks: list failed", zap.String("pool", in.Pool), zap.String("node", in.Node), zap.Error(err))
				return nil, poolDisksOut{}, err
			}
			out := poolDisksOut{Disks: make([]poolDiskOut, 0, len(disks))}
			for _, d := range disks {
				s.logger.Debug("haify_pool_disks: disk", zap.String("node", d.Node), zap.String("pool", d.Pool),
					zap.String("device", d.Device), zap.Uint64("size_bytes", d.SizeBytes), zap.String("health", d.Health))
				out.Disks = append(out.Disks, poolDiskOut{Node: d.Node, Pool: d.Pool, Device: d.Device,
					Size: util.FormatBytes(d.SizeBytes), Used: util.FormatBytes(d.UsedBytes), Health: d.Health,
					Detail: d.HealthDetail, Model: d.Model, Serial: d.Serial})
			}
			s.logger.Debug("haify_pool_disks: listed", zap.Int("count", len(out.Disks)))
			return nil, out, nil
		})

	addRead(s, srv, readOnlyTool("haify_storage_jobs", "List storage jobs",
		"List the long storage jobs: disks being emptied or replaced, volumes moving between pools. Running jobs "+
			"by default. A job runs on its node and survives a controller restart; poll this until it is done or failed."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in storageJobsIn) (*mcp.CallToolResult, storageJobsOut, error) {
			s.logger.Debug("haify_storage_jobs: listing storage jobs", zap.Bool("all", in.All))
			jobs, err := s.client.ListStorageJobs(ctx, in.All)
			if err != nil {
				s.logger.Debug("haify_storage_jobs: list failed", zap.Error(err))
				return nil, storageJobsOut{}, err
			}
			out := storageJobsOut{Jobs: make([]storageJobOut, 0, len(jobs))}
			for _, j := range jobs {
				s.logger.Debug("haify_storage_jobs: job", zap.String("job_id", j.Id), zap.String("kind", j.Kind),
					zap.String("state", j.State), zap.String("subject", j.Subject))
				out.Jobs = append(out.Jobs, storageJobOut{ID: j.Id, Kind: j.Kind, State: j.State, Subject: j.Subject,
					Progress: j.Progress, Message: j.Message, Started: unixRFC3339(j.StartedUnix), Updated: unixRFC3339(j.UpdatedUnix)})
			}
			s.logger.Debug("haify_storage_jobs: listed", zap.Int("count", len(out.Jobs)))
			return nil, out, nil
		})

	addWrite(s, srv, writeTool("haify_pool_trim", "Trim thin pools",
		"Run fstrim now on every mounted DRBD filesystem, on the node serving it. The discards reach every replica, "+
			"so each node's thin pool gets back the blocks the filesystem freed. Use it when a thin pool is fuller "+
			"than its volumes' contents explain. The controller already does this daily ([storage.thin] trim_schedule)."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in poolTrimIn) (*mcp.CallToolResult, poolTrimOut, error) {
			s.logger.Debug("haify_pool_trim: trimming pools", zap.String("node", in.Node))
			resp, err := s.client.TrimPools(ctx, in.Node)
			if err != nil {
				s.logger.Debug("haify_pool_trim: trim failed", zap.String("node", in.Node), zap.Error(err))
				return nil, poolTrimOut{}, err
			}
			out := poolTrimOut{Summary: resp.Message, Results: make([]trimResultOut, 0, len(resp.Results))}
			for _, r := range resp.Results {
				s.logger.Debug("haify_pool_trim: mount result", zap.String("node", r.Node), zap.String("mount", r.Mount),
					zap.Uint64("trimmed_bytes", r.Bytes), zap.String("error", r.Error))
				t := trimResultOut{Node: r.Node, Mount: r.Mount, Error: r.Error}
				if r.Error == "" {
					t.Trimmed = util.FormatBytes(r.Bytes)
				}
				out.Results = append(out.Results, t)
			}
			if !resp.Success && len(resp.Results) == 0 {
				s.logger.Debug("haify_pool_trim: controller refused with no results", zap.String("message", resp.Message))
				return nil, poolTrimOut{}, fmt.Errorf("%s", resp.Message)
			}
			s.logger.Debug("haify_pool_trim: done", zap.Bool("success", resp.Success), zap.Int("count", len(out.Results)))
			return nil, out, nil
		})

	addWrite(s, srv, writeTool("haify_pool_remove_disk", "Remove a disk from a pool",
		"Move a disk's data onto the pool's other disks (pvmove, while the pool stays in use), then take it out of "+
			"the pool. Refused when the other disks lack room, and for a pool's only disk (use haify_pool_replace_disk). "+
			"Returns a job id; follow it with haify_storage_jobs."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in poolRemoveDiskIn) (*mcp.CallToolResult, jobStartOut, error) {
			s.logger.Debug("haify_pool_remove_disk: removing disk", zap.String("pool", in.Pool), zap.String("node", in.Node),
				zap.String("disk", in.Disk))
			resp, err := s.client.RemovePoolDisk(ctx, in.Pool, in.Node, in.Disk)
			if err != nil {
				s.logger.Debug("haify_pool_remove_disk: remove failed", zap.String("pool", in.Pool), zap.String("node", in.Node),
					zap.String("disk", in.Disk), zap.Error(err))
				return nil, jobStartOut{}, err
			}
			s.logger.Debug("haify_pool_remove_disk: job started", zap.String("job_id", resp.JobId))
			return nil, jobStartOut{JobID: resp.JobId, Detail: resp.Message}, nil
		})

	addWrite(s, srv, writeTool("haify_pool_replace_disk", "Replace a disk in a pool",
		"Replace a failing or too-small disk: the new disk joins the pool, the old one's data moves to it, and the "+
			"old disk leaves the pool. The pool stays in use throughout. Returns a job id; follow it with haify_storage_jobs."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in poolReplaceDiskIn) (*mcp.CallToolResult, jobStartOut, error) {
			s.logger.Debug("haify_pool_replace_disk: replacing disk", zap.String("pool", in.Pool), zap.String("node", in.Node),
				zap.String("disk", in.Disk), zap.String("new_disk", in.NewDisk), zap.Bool("wipe", in.Wipe))
			resp, err := s.client.ReplacePoolDisk(ctx, in.Pool, in.Node, in.Disk, in.NewDisk, in.Wipe)
			if err != nil {
				s.logger.Debug("haify_pool_replace_disk: replace failed", zap.String("pool", in.Pool), zap.String("node", in.Node),
					zap.String("disk", in.Disk), zap.Error(err))
				return nil, jobStartOut{}, err
			}
			s.logger.Debug("haify_pool_replace_disk: job started", zap.String("job_id", resp.JobId))
			return nil, jobStartOut{JobID: resp.JobId, Detail: resp.Message}, nil
		})

	// Destructive: the volume's snapshots on the old pool are deleted with it.
	addWrite(s, srv, destructiveTool("haify_resource_move_volume", "Move a volume to another pool",
		"Move a resource's volume to another pool, one node at a time (secondaries first): detach, create the new "+
			"backing volume, full resync from the peers, delete the old one. The resource keeps serving; a Primary "+
			"reads and writes over the network while its own disk is rebuilt. The volume's snapshots in the old pool "+
			"are DELETED — confirm with the user and offer a backup first. Needs two or more diskful replicas; refused "+
			"for encrypted resources and while snapshots are locked. Returns a job id; follow it with haify_storage_jobs."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in moveVolumeIn) (*mcp.CallToolResult, jobStartOut, error) {
			s.logger.Debug("haify_resource_move_volume: moving volume", zap.String("resource", in.Resource),
				zap.Int32("volume", in.Volume), zap.String("pool", in.Pool))
			resp, err := s.client.MoveVolume(ctx, in.Resource, in.Volume, in.Pool)
			if err != nil {
				s.logger.Debug("haify_resource_move_volume: move failed", zap.String("resource", in.Resource),
					zap.Int32("volume", in.Volume), zap.String("pool", in.Pool), zap.Error(err))
				return nil, jobStartOut{}, err
			}
			s.logger.Debug("haify_resource_move_volume: job started", zap.String("job_id", resp.JobId))
			return nil, jobStartOut{JobID: resp.JobId, Detail: resp.Message}, nil
		})
}
