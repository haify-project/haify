package controller

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/backup"
	"github.com/haify-project/haify/pkg/deployment"
)

// ==================== Backup Adapter ====================

// BackupDeploymentClient adapts the controller's deploymentClient to
// backup.DeploymentClient. It mirrors WanproxyDeploymentClient, so the backup
// path runs over exactly the SSH transport the rest of the controller uses and
// stays fakeable in tests.
type BackupDeploymentClient struct {
	dc deploymentClient
}

// NewBackupDeploymentClient creates the adapter.
func NewBackupDeploymentClient(dc deploymentClient) backup.DeploymentClient {
	return &BackupDeploymentClient{dc: dc}
}

func newBackupDeploymentClient(dc deploymentClient) backup.DeploymentClient {
	return &BackupDeploymentClient{dc: dc}
}

func (a *BackupDeploymentClient) Exec(ctx context.Context, hosts []string, cmd string) (*backup.Result, error) {
	// The command can carry a target's credentials inline (rclone flags), so
	// only its length is traced.
	start := time.Now()
	res, err := a.dc.Exec(ctx, hosts, cmd)
	if err != nil {
		zap.L().Debug("BackupDeploymentClient.Exec: exec failed", zap.Strings("hosts", hosts),
			zap.Int("cmd_len", len(cmd)), zap.Duration("took", time.Since(start)), zap.Error(err))
		return nil, err
	}
	if ce := zap.L().Check(zap.DebugLevel, "BackupDeploymentClient.Exec: exec finished"); ce != nil {
		ce.Write(zap.Strings("hosts", hosts), zap.Int("cmd_len", len(cmd)),
			zap.Strings("failed_hosts", tracedFailedHosts(res)), zap.Duration("took", time.Since(start)))
	}
	return execResultToBackup(res), nil
}

func (a *BackupDeploymentClient) PutSecret(ctx context.Context, hosts []string, content, relPath string) (*backup.Result, error) {
	start := time.Now()
	res, err := a.dc.DistributeSecret(ctx, hosts, content, relPath)
	zap.L().Debug("BackupDeploymentClient.PutSecret: secret distributed", zap.Strings("hosts", hosts),
		zap.String("path", relPath), zap.Bool("has_content", content != ""),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, err
	}
	return configResultToBackup(res, hosts), nil
}

func execResultToBackup(res *deployment.ExecResult) *backup.Result {
	out := &backup.Result{Hosts: make(map[string]*backup.HostResult)}
	if res == nil {
		return out
	}
	for host, hr := range res.Hosts {
		out.Hosts[host] = &backup.HostResult{Host: hr.Host, Output: hr.Output, Success: hr.Success, Err: hr.Error}
	}
	return out
}

// configResultToBackup converts a ConfigResult. Like the WAN adapter it falls
// back to the aggregate Success flag when the per-host map is empty, so
// AllSuccess stays accurate for a distribute that reported success in bulk.
func configResultToBackup(res *deployment.ConfigResult, hosts []string) *backup.Result {
	out := &backup.Result{Hosts: make(map[string]*backup.HostResult)}
	if res == nil {
		return out
	}
	for host, hr := range res.Hosts {
		out.Hosts[host] = &backup.HostResult{Host: hr.Host, Output: hr.Output, Success: hr.Success, Err: hr.Error}
	}
	if len(out.Hosts) == 0 {
		zap.L().Debug("configResultToBackup: no per-host results, using aggregate", zap.Strings("hosts", hosts),
			zap.Bool("success", res.Success))
		for _, h := range hosts {
			out.Hosts[h] = &backup.HostResult{Host: h, Success: res.Success}
		}
	}
	return out
}
