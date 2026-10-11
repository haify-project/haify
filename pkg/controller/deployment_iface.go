package controller

import (
	"context"
	"os"

	"github.com/haify-project/haify/pkg/deployment"
)

type deploymentClient interface {
	DistributeConfig(ctx context.Context, hosts []string, content, remotePath string, opts ...deployment.ConfigOption) (*deployment.ConfigResult, error)
	// DistributeSecret writes content at mode 0600 relative to the login user's
	// home without it ever reaching a command line. DistributeConfig is not an
	// alternative for credentials; see its implementation for why.
	DistributeSecret(ctx context.Context, hosts []string, content, relPath string) (*deployment.ConfigResult, error)
	// InstallFile installs a local file (a binary) at an absolute path on
	// every host, root-owned at mode.
	InstallFile(ctx context.Context, hosts []string, localPath, remotePath string, mode os.FileMode) (*deployment.ConfigResult, error)
	DeleteConfig(ctx context.Context, hosts []string, remotePath string) error
	Exec(ctx context.Context, hosts []string, cmd string, opts ...deployment.ExecOption) (*deployment.ExecResult, error)

	PVCreate(ctx context.Context, hosts []string, device string, opts ...deployment.LVMOption) (*deployment.ExecResult, error)
	VGCreate(ctx context.Context, hosts []string, vgName string, devices []string) (*deployment.ExecResult, error)
	LVCreate(ctx context.Context, hosts []string, vgName, lvName, size string) (*deployment.ExecResult, error)
	LVCreateThinPool(ctx context.Context, hosts []string, vgName, poolName, size string) (*deployment.ExecResult, error)
	LVCreateThinVolume(ctx context.Context, hosts []string, vgName, poolName, lvName, size string) (*deployment.ExecResult, error)
	LVRemove(ctx context.Context, hosts []string, lvPath string) (*deployment.ExecResult, error)
	LVCreateSnapshot(ctx context.Context, hosts []string, vgName, lvName, snapshotName, size string) (*deployment.ExecResult, error)
	LVCreateThinSnapshot(ctx context.Context, hosts []string, vgName, lvName, snapshotName string) (*deployment.ExecResult, error)
	LVIsThin(ctx context.Context, host, vgName, lvName string) (bool, error)
	LVSizeBytes(ctx context.Context, host, vgName, lvName string) (uint64, error)
	VGFreeBytes(ctx context.Context, host, vgName string) (uint64, error)
	LVCreateThinPoolAllFree(ctx context.Context, hosts []string, vgName, poolName string, metadataBytes uint64) (*deployment.ExecResult, error)
	LVExists(ctx context.Context, host, vgName, lvName string) (bool, error)
	LVThinPoolIn(ctx context.Context, host, vgName string) (string, error)
	LVExtendThinPoolMetadata(ctx context.Context, hosts []string, vgName, poolName string, sizeBytes uint64) (*deployment.ExecResult, error)
	LVExtendThinPoolAllFree(ctx context.Context, hosts []string, vgName, poolName string) (*deployment.ExecResult, error)
	LVExtendThinPoolPercentFree(ctx context.Context, hosts []string, vgName, poolName string, percent int) (*deployment.ExecResult, error)
	LVSCacheReport(ctx context.Context, hosts []string, vgName string) (*deployment.ExecResult, error)
	LVSThinReport(ctx context.Context, hosts []string, vgName string) (*deployment.ExecResult, error)
	ProbeBlockDevice(ctx context.Context, host, device string) (*deployment.ExecResult, error)
	ListBlockDevices(ctx context.Context, host string) (*deployment.ExecResult, error)
	WipeDevice(ctx context.Context, host, device string, partitions []string) (*deployment.ExecResult, error)
	ZFSAddVdev(ctx context.Context, host, pool, device string) (*deployment.ExecResult, error)
	VGExtend(ctx context.Context, hosts []string, vgName, device string) (*deployment.ExecResult, error)
	VGReduceAndRemovePV(ctx context.Context, hosts []string, vgName, device string) (*deployment.ExecResult, error)
	LVCreateCacheVol(ctx context.Context, hosts []string, vgName, lvName, device string) (*deployment.ExecResult, error)
	LVConvertToCache(ctx context.Context, hosts []string, vgName, lvName, cacheVol, mode string) (*deployment.ExecResult, error)
	LVUncache(ctx context.Context, hosts []string, vgName, lvName string) (*deployment.ExecResult, error)
	DRBDDetach(ctx context.Context, host, resource string) (*deployment.ExecResult, error)
	DRBDAttach(ctx context.Context, host, resource string) (*deployment.ExecResult, error)
	LVRemoveSnapshot(ctx context.Context, hosts []string, vgName, snapshotName string) (*deployment.ExecResult, error)
	LVListSnapshots(ctx context.Context, hosts []string, vgName string) (*deployment.ExecResult, error)
	LVMergeSnapshot(ctx context.Context, hosts []string, vgName, snapshotName string) (*deployment.ExecResult, error)

	ZFSCreatePool(ctx context.Context, hosts []string, poolName string, vdevs []string, opts ...deployment.ZFSOption) (*deployment.ExecResult, error)
	ZFSDestroyPool(ctx context.Context, hosts []string, poolName string) (*deployment.ExecResult, error)
	ZFSListPools(ctx context.Context, hosts []string) (*deployment.ExecResult, error)
	ZFSCreateDataset(ctx context.Context, hosts []string, datasetName string, opts ...deployment.ZFSOption) (*deployment.ExecResult, error)
	ZFSCreateThinDataset(ctx context.Context, hosts []string, poolName, datasetName, size string) (*deployment.ExecResult, error)
	ZFSDestroyDataset(ctx context.Context, hosts []string, datasetName string) (*deployment.ExecResult, error)
	ZFSSnapshot(ctx context.Context, hosts []string, dataset, snapshotName string) (*deployment.ExecResult, error)
	ZFSRollback(ctx context.Context, hosts []string, dataset, snapshotName string) (*deployment.ExecResult, error)
	ZFSClone(ctx context.Context, hosts []string, snapshot, clonePath string) (*deployment.ExecResult, error)
	ZFSListSnapshots(ctx context.Context, hosts []string, dataset string) (*deployment.ExecResult, error)
	ZFSDestroySnapshot(ctx context.Context, hosts []string, snapshot string) (*deployment.ExecResult, error)
	ZFSSetQuota(ctx context.Context, hosts []string, dataset, quota string) (*deployment.ExecResult, error)
	ZFSResizeVolume(ctx context.Context, hosts []string, volumePath, newSize string) (*deployment.ExecResult, error)

	DRBDUp(ctx context.Context, hosts []string, resource string) (*deployment.ExecResult, error)
	DRBDDown(ctx context.Context, hosts []string, resource string) (*deployment.ExecResult, error)
	DRBDPrimary(ctx context.Context, host, resource string, force bool) (*deployment.HostResult, error)
	DRBDSecondary(ctx context.Context, host, resource string) (*deployment.HostResult, error)
	DRBDCreateMD(ctx context.Context, hosts []string, resource string, maxPeers int) (*deployment.ExecResult, error)
	DRBDStatus(ctx context.Context, hosts []string, resource string) (*deployment.ExecResult, error)
	DRBDStatusJSON(ctx context.Context, hosts []string, resource string) (*deployment.ExecResult, error)
	ReactorReload(ctx context.Context, hosts []string) (*deployment.ExecResult, error)
	ReactorPromoterStatusByResource(ctx context.Context, host, resource string) (*deployment.ReactorPromoterStatus, error)
	ReactorStatusJSON(ctx context.Context, host string) (*deployment.ReactorStatus, error)
}

var _ deploymentClient = (*deployment.Client)(nil)
