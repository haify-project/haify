package client

import (
	"context"
	"fmt"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// Off-cluster backup shipping. See pkg/backup for what this is and is not:
// every backup is a FULL image, and only a "completed" one is restorable.

// AddBackupTarget stores (or replaces) a backup repository. The secret in req
// is write-only server-side and is never returned by any other call.
func (c *HaifyClient) AddBackupTarget(ctx context.Context, req *haifypb.AddBackupTargetRequest) error {
	log := zap.L()
	log.Debug("HaifyClient.AddBackupTarget: sending request", zap.String("name", req.GetName()), zap.String("kind", req.GetKind()),
		zap.String("endpoint", req.GetEndpoint()), zap.String("bucket", req.GetBucket()), zap.String("host", req.GetHost()),
		zap.String("share", req.GetShare()), zap.String("prefix", req.GetPrefix()), zap.Bool("has_user", req.GetUser() != ""),
		zap.Bool("has_secret", req.GetSecret() != ""), zap.String("lock_mode", req.GetLockMode()), zap.Uint32("lock_days", req.GetLockDays()))
	resp, err := c.client.AddBackupTarget(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.AddBackupTarget: AddBackupTarget failed", zap.Error(err))
		return err
	}
	if !resp.Success {
		log.Debug("HaifyClient.AddBackupTarget: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.AddBackupTarget: done")
	return nil
}

// ListBackupTargets returns every backup target, without their secrets.
func (c *HaifyClient) ListBackupTargets(ctx context.Context) ([]*haifypb.BackupTargetInfo, error) {
	log := zap.L()
	log.Debug("HaifyClient.ListBackupTargets: sending request")
	resp, err := c.client.ListBackupTargets(ctx, &haifypb.ListBackupTargetsRequest{})
	if err != nil {
		log.Debug("HaifyClient.ListBackupTargets: ListBackupTargets failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.ListBackupTargets: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.ListBackupTargets: done", zap.Int("count", len(resp.Targets)))
	return resp.Targets, nil
}

// DeleteBackupTarget removes a backup target definition.
func (c *HaifyClient) DeleteBackupTarget(ctx context.Context, name string, force bool) error {
	log := zap.L()
	log.Debug("HaifyClient.DeleteBackupTarget: sending request", zap.String("name", name), zap.Bool("force", force))
	resp, err := c.client.DeleteBackupTarget(ctx, &haifypb.DeleteBackupTargetRequest{Name: name, Force: force})
	if err != nil {
		log.Debug("HaifyClient.DeleteBackupTarget: DeleteBackupTarget failed", zap.Error(err))
		return err
	}
	if !resp.Success {
		log.Debug("HaifyClient.DeleteBackupTarget: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.DeleteBackupTarget: done")
	return nil
}

// CreateBackup ships a crash-consistent copy of a resource to a target:
// incremental on the last backup when it can be, full when full is set or it
// cannot.
func (c *HaifyClient) CreateBackup(ctx context.Context, resource, target, node string, full bool) (*haifypb.BackupInfo, error) {
	log := zap.L()
	log.Debug("HaifyClient.CreateBackup: sending request", zap.String("resource", resource), zap.String("target", target), zap.String("node", node), zap.Bool("full", full))
	resp, err := c.client.CreateBackup(ctx, &haifypb.CreateBackupRequest{
		Resource: resource, Target: target, Node: node, Full: full,
	})
	if err != nil {
		log.Debug("HaifyClient.CreateBackup: CreateBackup failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.CreateBackup: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.CreateBackup: done", zap.String("backup_id", resp.Backup.GetId()))
	return resp.Backup, nil
}

// ListBackups returns backup records, newest first. Empty filters match all.
func (c *HaifyClient) ListBackups(ctx context.Context, resource, target string) ([]*haifypb.BackupInfo, error) {
	log := zap.L()
	log.Debug("HaifyClient.ListBackups: sending request", zap.String("resource", resource), zap.String("target", target))
	resp, err := c.client.ListBackups(ctx, &haifypb.ListBackupsRequest{Resource: resource, Target: target})
	if err != nil {
		log.Debug("HaifyClient.ListBackups: ListBackups failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.ListBackups: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.ListBackups: done", zap.Int("count", len(resp.Backups)))
	return resp.Backups, nil
}

// RestoreBackup writes a completed backup back into a resource.
func (c *HaifyClient) RestoreBackup(ctx context.Context, id, resource, node string) (*haifypb.BackupInfo, error) {
	log := zap.L()
	log.Debug("HaifyClient.RestoreBackup: sending request", zap.String("id", id), zap.String("resource", resource), zap.String("node", node))
	resp, err := c.client.RestoreBackup(ctx, &haifypb.RestoreBackupRequest{
		Id: id, Resource: resource, Node: node,
	})
	if err != nil {
		log.Debug("HaifyClient.RestoreBackup: RestoreBackup failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.RestoreBackup: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.RestoreBackup: done")
	return resp.Backup, nil
}

// DeleteBackup removes a backup's objects and its record.
func (c *HaifyClient) DeleteBackup(ctx context.Context, id, node string, force bool) error {
	log := zap.L()
	log.Debug("HaifyClient.DeleteBackup: sending request", zap.String("id", id), zap.String("node", node), zap.Bool("force", force))
	resp, err := c.client.DeleteBackup(ctx, &haifypb.DeleteBackupRequest{Id: id, Node: node, Force: force})
	if err != nil {
		log.Debug("HaifyClient.DeleteBackup: DeleteBackup failed", zap.Error(err))
		return err
	}
	if !resp.Success {
		log.Debug("HaifyClient.DeleteBackup: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.DeleteBackup: done")
	return nil
}

// ImportBackups rebuilds backup records from the manifests on a target.
func (c *HaifyClient) ImportBackups(ctx context.Context, target, node string) (*haifypb.ImportBackupsResponse, error) {
	return c.ImportBackupsAsOf(ctx, target, node, "")
}

// ImportBackupsAsOf is ImportBackups reading the target as it was at asOf
// (RFC3339); empty reads it as it is.
func (c *HaifyClient) ImportBackupsAsOf(ctx context.Context, target, node, asOf string) (*haifypb.ImportBackupsResponse, error) {
	log := zap.L()
	log.Debug("HaifyClient.ImportBackupsAsOf: sending request", zap.String("target", target), zap.String("node", node), zap.String("as_of", asOf))
	resp, err := c.client.ImportBackups(ctx, &haifypb.ImportBackupsRequest{Target: target, Node: node, AsOf: asOf})
	if err != nil {
		log.Debug("HaifyClient.ImportBackupsAsOf: ImportBackups failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.ImportBackupsAsOf: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.ImportBackupsAsOf: done", zap.Int("imported", len(resp.Imported)), zap.Int("skipped", len(resp.Skipped)))
	return resp, nil
}

// CreateBackupSchedule creates or replaces the schedule backing resource up to target.
func (c *HaifyClient) CreateBackupSchedule(ctx context.Context, req *haifypb.CreateBackupScheduleRequest) (*haifypb.BackupScheduleInfo, error) {
	log := zap.L()
	log.Debug("HaifyClient.CreateBackupSchedule: sending request", zap.String("resource", req.GetResource()),
		zap.String("target", req.GetTarget()), zap.String("cron", req.GetCron()), zap.Bool("enabled", req.GetEnabled()))
	resp, err := c.client.CreateBackupSchedule(ctx, req)
	if err != nil {
		log.Debug("HaifyClient.CreateBackupSchedule: CreateBackupSchedule failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.CreateBackupSchedule: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.CreateBackupSchedule: done")
	return resp.Schedule, nil
}

// ListBackupSchedules returns every backup schedule.
func (c *HaifyClient) ListBackupSchedules(ctx context.Context) ([]*haifypb.BackupScheduleInfo, error) {
	log := zap.L()
	log.Debug("HaifyClient.ListBackupSchedules: sending request")
	resp, err := c.client.ListBackupSchedules(ctx, &haifypb.ListBackupSchedulesRequest{})
	if err != nil {
		log.Debug("HaifyClient.ListBackupSchedules: ListBackupSchedules failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.ListBackupSchedules: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.ListBackupSchedules: done", zap.Int("count", len(resp.Schedules)))
	return resp.Schedules, nil
}

// DeleteBackupSchedule stops a schedule; its backups stay.
func (c *HaifyClient) DeleteBackupSchedule(ctx context.Context, name string) error {
	log := zap.L()
	log.Debug("HaifyClient.DeleteBackupSchedule: sending request", zap.String("name", name))
	resp, err := c.client.DeleteBackupSchedule(ctx, &haifypb.DeleteBackupScheduleRequest{Name: name})
	if err != nil {
		log.Debug("HaifyClient.DeleteBackupSchedule: DeleteBackupSchedule failed", zap.Error(err))
		return err
	}
	if !resp.Success {
		log.Debug("HaifyClient.DeleteBackupSchedule: controller reported failure", zap.String("message", resp.Message))
		return fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.DeleteBackupSchedule: done")
	return nil
}

// RunBackupSchedule runs a schedule now and waits for the backup.
func (c *HaifyClient) RunBackupSchedule(ctx context.Context, name string) (*haifypb.BackupScheduleInfo, error) {
	log := zap.L()
	log.Debug("HaifyClient.RunBackupSchedule: sending request", zap.String("name", name))
	resp, err := c.client.RunBackupSchedule(ctx, &haifypb.RunBackupScheduleRequest{Name: name})
	if err != nil {
		log.Debug("HaifyClient.RunBackupSchedule: RunBackupSchedule failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.RunBackupSchedule: controller reported failure", zap.String("message", resp.Message))
		return resp.Schedule, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.RunBackupSchedule: done")
	return resp.Schedule, nil
}
