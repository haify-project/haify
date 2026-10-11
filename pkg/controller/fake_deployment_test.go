package controller

import (
	"context"
	"os"
	"sync"

	"github.com/haify-project/haify/pkg/deployment"
	"github.com/stretchr/testify/assert"
)

type execCall struct {
	hosts []string
	cmd   string
}

type fakeDeploymentClient struct {
	lvIsThinFunc                        func(ctx context.Context, host, vgName, lvName string) (bool, error)
	lvCreateThinSnapshotFunc            func(ctx context.Context, hosts []string, vgName, lvName, snapshotName string) (*deployment.ExecResult, error)
	vgFreeBytesFunc                     func(host, vgName string) (uint64, error)
	execFunc                            func(ctx context.Context, hosts []string, cmd string, opts ...deployment.ExecOption) (*deployment.ExecResult, error)
	pvCreateFunc                        func(ctx context.Context, hosts []string, device string, opts ...deployment.LVMOption) (*deployment.ExecResult, error)
	vgCreateFunc                        func(ctx context.Context, hosts []string, vgName string, devices []string) (*deployment.ExecResult, error)
	lvCreateFunc                        func(ctx context.Context, hosts []string, vgName, lvName, size string) (*deployment.ExecResult, error)
	lvCreateThinPoolFunc                func(ctx context.Context, hosts []string, vgName, poolName, size string) (*deployment.ExecResult, error)
	lvCreateThinVolumeFunc              func(ctx context.Context, hosts []string, vgName, poolName, lvName, size string) (*deployment.ExecResult, error)
	lvCreateThinPoolAllFreeFunc         func(ctx context.Context, hosts []string, vgName, poolName string, metadataBytes uint64) (*deployment.ExecResult, error)
	drbdDetachFunc                      func(ctx context.Context, host, resource string) (*deployment.ExecResult, error)
	lvRemoveFunc                        func(ctx context.Context, hosts []string, lvPath string) (*deployment.ExecResult, error)
	lvExistsFunc                        func(ctx context.Context, host, vgName, lvName string) (bool, error)
	lvThinPoolInFunc                    func(ctx context.Context, host, vgName string) (string, error)
	lvExtendThinPoolMetadataFunc        func(ctx context.Context, hosts []string, vgName, poolName string, sizeBytes uint64) (*deployment.ExecResult, error)
	lvExtendThinPoolAllFreeFunc         func(ctx context.Context, hosts []string, vgName, poolName string) (*deployment.ExecResult, error)
	lvExtendThinPoolPercentFreeFunc     func(hosts []string, vgName, poolName string, percent int) (*deployment.ExecResult, error)
	lvSizeBytesFunc                     func(host, vgName, lvName string) (uint64, error)
	lvsCacheReportFunc                  func(ctx context.Context, hosts []string, vgName string) (*deployment.ExecResult, error)
	lvsThinReportFunc                   func(ctx context.Context, hosts []string, vgName string) (*deployment.ExecResult, error)
	lvRemoveSnapshotFunc                func(ctx context.Context, hosts []string, vgName, snapshotName string) (*deployment.ExecResult, error)
	probeBlockDeviceFunc                func(ctx context.Context, host, device string) (*deployment.ExecResult, error)
	vgExtendFunc                        func(ctx context.Context, hosts []string, vgName, device string) (*deployment.ExecResult, error)
	vgReduceAndRemovePVFunc             func(ctx context.Context, hosts []string, vgName, device string) (*deployment.ExecResult, error)
	lvCreateCacheVolFunc                func(ctx context.Context, hosts []string, vgName, lvName, device string) (*deployment.ExecResult, error)
	lvConvertToCacheFunc                func(ctx context.Context, hosts []string, vgName, lvName, cacheVol, mode string) (*deployment.ExecResult, error)
	lvUncacheFunc                       func(ctx context.Context, hosts []string, vgName, lvName string) (*deployment.ExecResult, error)
	drbdAttachFunc                      func(ctx context.Context, host, resource string) (*deployment.ExecResult, error)
	zfsCreatePoolFunc                   func(ctx context.Context, hosts []string, poolName string, vdevs []string, opts ...deployment.ZFSOption) (*deployment.ExecResult, error)
	zfsDestroyPoolFunc                  func(ctx context.Context, hosts []string, poolName string) (*deployment.ExecResult, error)
	zfsListPoolsFunc                    func(ctx context.Context, hosts []string) (*deployment.ExecResult, error)
	zfsListSnapshotsFunc                func(ctx context.Context, hosts []string, dataset string) (*deployment.ExecResult, error)
	lvListSnapshotsFunc                 func(ctx context.Context, hosts []string, vgName string) (*deployment.ExecResult, error)
	lvMergeSnapshotFunc                 func(ctx context.Context, hosts []string, vgName, snapshotName string) (*deployment.ExecResult, error)
	distributeConfigFunc                func(ctx context.Context, hosts []string, content, remotePath string, opts ...deployment.ConfigOption) (*deployment.ConfigResult, error)
	distributeSecretFunc                func(ctx context.Context, hosts []string, content, relPath string) (*deployment.ConfigResult, error)
	drbdCreateMDFunc                    func(ctx context.Context, hosts []string, resource string) (*deployment.ExecResult, error)
	drbdUpFunc                          func(ctx context.Context, hosts []string, resource string) (*deployment.ExecResult, error)
	drbdDownFunc                        func(ctx context.Context, hosts []string, resource string) (*deployment.ExecResult, error)
	drbdPrimaryFunc                     func(ctx context.Context, host, resource string, force bool) (*deployment.HostResult, error)
	drbdSecondaryFunc                   func(ctx context.Context, host, resource string) (*deployment.HostResult, error)
	drbdStatusFunc                      func(ctx context.Context, hosts []string, resource string) (*deployment.ExecResult, error)
	drbdStatusJSONFunc                  func(ctx context.Context, hosts []string, resource string) (*deployment.ExecResult, error)
	reactorReloadFunc                   func(ctx context.Context, hosts []string) (*deployment.ExecResult, error)
	reactorPromoterStatusByResourceFunc func(ctx context.Context, host, resource string) (*deployment.ReactorPromoterStatus, error)
	execCalls                           []execCall
	execMu                              sync.Mutex // guards execCalls: the HA status probes call Exec concurrently
	pvCreateCalls                       []struct {
		hosts  []string
		device string
	}
	vgCreateCalls []struct {
		hosts   []string
		vgName  string
		devices []string
	}
	lvCreateCalls []struct {
		hosts                []string
		vgName, lvName, size string
	}
	lvCreateThinPoolCalls []struct {
		hosts                  []string
		vgName, poolName, size string
	}
	lvCreateThinVolumeCalls []struct {
		hosts                          []string
		vgName, poolName, lvName, size string
	}
	zfsCreatePoolCalls []struct {
		hosts    []string
		poolName string
		vdevs    []string
		optCount int
	}
	zfsDestroyPoolCalls []struct {
		hosts    []string
		poolName string
	}
	distributedConfigs []struct {
		hosts               []string
		content, remotePath string
	}
	distributedSecrets []struct {
		hosts   []string
		content string
		relPath string
	}
	installedFiles []struct {
		hosts                 []string
		localPath, remotePath string
	}
	deleteConfigCalls []struct {
		hosts      []string
		remotePath string
	}
	drbdCreateMDCalls []struct {
		hosts    []string
		resource string
		maxPeers int
	}
	drbdDownCalls []struct {
		hosts    []string
		resource string
	}
	drbdUpCalls []struct {
		hosts    []string
		resource string
	}
	drbdStatusCalls []struct {
		hosts    []string
		resource string
	}
	lvRemoveCalls []struct {
		hosts  []string
		lvPath string
	}
}

func cloneStrings(values []string) []string {
	out := make([]string, len(values))
	copy(out, values)
	return out
}

func successExecResult(hosts []string, output string) *deployment.ExecResult {
	result := &deployment.ExecResult{Hosts: make(map[string]*deployment.HostResult, len(hosts))}
	for _, host := range hosts {
		result.Hosts[host] = &deployment.HostResult{
			Host:    host,
			Success: true,
			Output:  output,
		}
	}
	return result
}

func (f *fakeDeploymentClient) DistributeConfig(ctx context.Context, hosts []string, content, remotePath string, opts ...deployment.ConfigOption) (*deployment.ConfigResult, error) {
	f.distributedConfigs = append(f.distributedConfigs, struct {
		hosts      []string
		content    string
		remotePath string
	}{hosts: cloneStrings(hosts), content: content, remotePath: remotePath})
	if f.distributeConfigFunc != nil {
		return f.distributeConfigFunc(ctx, hosts, content, remotePath, opts...)
	}
	return &deployment.ConfigResult{Success: true, Path: remotePath}, nil
}

func (f *fakeDeploymentClient) DistributeSecret(ctx context.Context, hosts []string, content, relPath string) (*deployment.ConfigResult, error) {
	f.distributedSecrets = append(f.distributedSecrets, struct {
		hosts   []string
		content string
		relPath string
	}{hosts: cloneStrings(hosts), content: content, relPath: relPath})
	if f.distributeSecretFunc != nil {
		return f.distributeSecretFunc(ctx, hosts, content, relPath)
	}
	return &deployment.ConfigResult{Success: true, Path: relPath}, nil
}

func (f *fakeDeploymentClient) InstallFile(ctx context.Context, hosts []string, localPath, remotePath string, mode os.FileMode) (*deployment.ConfigResult, error) {
	f.installedFiles = append(f.installedFiles, struct {
		hosts                 []string
		localPath, remotePath string
	}{hosts: cloneStrings(hosts), localPath: localPath, remotePath: remotePath})
	res := &deployment.ConfigResult{Success: true, Path: remotePath, Hosts: map[string]*deployment.HostResult{}}
	for _, h := range hosts {
		res.Hosts[h] = &deployment.HostResult{Host: h, Success: true}
	}
	return res, nil
}

func (f *fakeDeploymentClient) DeleteConfig(ctx context.Context, hosts []string, remotePath string) error {
	f.deleteConfigCalls = append(f.deleteConfigCalls, struct {
		hosts      []string
		remotePath string
	}{hosts: cloneStrings(hosts), remotePath: remotePath})
	return nil
}

func (f *fakeDeploymentClient) Exec(ctx context.Context, hosts []string, cmd string, opts ...deployment.ExecOption) (*deployment.ExecResult, error) {
	f.execMu.Lock()
	f.execCalls = append(f.execCalls, execCall{hosts: cloneStrings(hosts), cmd: cmd})
	f.execMu.Unlock()
	if f.execFunc != nil {
		return f.execFunc(ctx, hosts, cmd, opts...)
	}
	return successExecResult(hosts, ""), nil
}

func (f *fakeDeploymentClient) PVCreate(ctx context.Context, hosts []string, device string, opts ...deployment.LVMOption) (*deployment.ExecResult, error) {
	f.pvCreateCalls = append(f.pvCreateCalls, struct {
		hosts  []string
		device string
	}{hosts: cloneStrings(hosts), device: device})
	if f.pvCreateFunc != nil {
		return f.pvCreateFunc(ctx, hosts, device, opts...)
	}
	return successExecResult(hosts, ""), nil
}

func (f *fakeDeploymentClient) VGCreate(ctx context.Context, hosts []string, vgName string, devices []string) (*deployment.ExecResult, error) {
	f.vgCreateCalls = append(f.vgCreateCalls, struct {
		hosts   []string
		vgName  string
		devices []string
	}{hosts: cloneStrings(hosts), vgName: vgName, devices: cloneStrings(devices)})
	if f.vgCreateFunc != nil {
		return f.vgCreateFunc(ctx, hosts, vgName, devices)
	}
	return successExecResult(hosts, ""), nil
}

func (f *fakeDeploymentClient) LVCreate(ctx context.Context, hosts []string, vgName, lvName, size string) (*deployment.ExecResult, error) {
	f.lvCreateCalls = append(f.lvCreateCalls, struct {
		hosts                []string
		vgName, lvName, size string
	}{hosts: cloneStrings(hosts), vgName: vgName, lvName: lvName, size: size})
	if f.lvCreateFunc != nil {
		return f.lvCreateFunc(ctx, hosts, vgName, lvName, size)
	}
	return successExecResult(hosts, ""), nil
}

func (f *fakeDeploymentClient) LVCreateThinPool(ctx context.Context, hosts []string, vgName, poolName, size string) (*deployment.ExecResult, error) {
	f.lvCreateThinPoolCalls = append(f.lvCreateThinPoolCalls, struct {
		hosts                  []string
		vgName, poolName, size string
	}{hosts: cloneStrings(hosts), vgName: vgName, poolName: poolName, size: size})
	if f.lvCreateThinPoolFunc != nil {
		return f.lvCreateThinPoolFunc(ctx, hosts, vgName, poolName, size)
	}
	return successExecResult(hosts, ""), nil
}

func (f *fakeDeploymentClient) LVCreateThinVolume(ctx context.Context, hosts []string, vgName, poolName, lvName, size string) (*deployment.ExecResult, error) {
	f.lvCreateThinVolumeCalls = append(f.lvCreateThinVolumeCalls, struct {
		hosts                          []string
		vgName, poolName, lvName, size string
	}{hosts: cloneStrings(hosts), vgName: vgName, poolName: poolName, lvName: lvName, size: size})
	if f.lvCreateThinVolumeFunc != nil {
		return f.lvCreateThinVolumeFunc(ctx, hosts, vgName, poolName, lvName, size)
	}
	return successExecResult(hosts, ""), nil
}

func (f *fakeDeploymentClient) LVCreateSnapshot(ctx context.Context, hosts []string, vgName, lvName, snapshotName, size string) (*deployment.ExecResult, error) {
	return successExecResult(hosts, ""), nil
}

func (f *fakeDeploymentClient) LVCreateThinSnapshot(ctx context.Context, hosts []string, vgName, lvName, snapshotName string) (*deployment.ExecResult, error) {
	if f.lvCreateThinSnapshotFunc != nil {
		return f.lvCreateThinSnapshotFunc(ctx, hosts, vgName, lvName, snapshotName)
	}
	return successExecResult(hosts, ""), nil
}

func (f *fakeDeploymentClient) LVIsThin(ctx context.Context, host, vgName, lvName string) (bool, error) {
	if f.lvIsThinFunc != nil {
		return f.lvIsThinFunc(ctx, host, vgName, lvName)
	}
	return false, nil
}

func (f *fakeDeploymentClient) LVExtendThinPoolMetadata(ctx context.Context, hosts []string, vgName, poolName string, sizeBytes uint64) (*deployment.ExecResult, error) {
	if f.lvExtendThinPoolMetadataFunc != nil {
		return f.lvExtendThinPoolMetadataFunc(ctx, hosts, vgName, poolName, sizeBytes)
	}
	return successExecResult(hosts, ""), nil
}

func (f *fakeDeploymentClient) LVExtendThinPoolAllFree(ctx context.Context, hosts []string, vgName, poolName string) (*deployment.ExecResult, error) {
	if f.lvExtendThinPoolAllFreeFunc != nil {
		return f.lvExtendThinPoolAllFreeFunc(ctx, hosts, vgName, poolName)
	}
	return successExecResult(hosts, ""), nil
}

func (f *fakeDeploymentClient) LVExtendThinPoolPercentFree(ctx context.Context, hosts []string, vgName, poolName string, percent int) (*deployment.ExecResult, error) {
	if f.lvExtendThinPoolPercentFreeFunc != nil {
		return f.lvExtendThinPoolPercentFreeFunc(hosts, vgName, poolName, percent)
	}
	return successExecResult(hosts, ""), nil
}

func (f *fakeDeploymentClient) LVSCacheReport(ctx context.Context, hosts []string, vgName string) (*deployment.ExecResult, error) {
	if f.lvsCacheReportFunc != nil {
		return f.lvsCacheReportFunc(ctx, hosts, vgName)
	}
	return successExecResult(hosts, ""), nil
}

func (f *fakeDeploymentClient) LVSThinReport(ctx context.Context, hosts []string, vgName string) (*deployment.ExecResult, error) {
	if f.lvsThinReportFunc != nil {
		return f.lvsThinReportFunc(ctx, hosts, vgName)
	}
	return successExecResult(hosts, ""), nil
}

func (f *fakeDeploymentClient) ProbeBlockDevice(ctx context.Context, host, device string) (*deployment.ExecResult, error) {
	if f.probeBlockDeviceFunc != nil {
		return f.probeBlockDeviceFunc(ctx, host, device)
	}
	return successExecResult([]string{host}, ""), nil
}

func (f *fakeDeploymentClient) VGExtend(ctx context.Context, hosts []string, vgName, device string) (*deployment.ExecResult, error) {
	if f.vgExtendFunc != nil {
		return f.vgExtendFunc(ctx, hosts, vgName, device)
	}
	return successExecResult(hosts, ""), nil
}

func (f *fakeDeploymentClient) VGReduceAndRemovePV(ctx context.Context, hosts []string, vgName, device string) (*deployment.ExecResult, error) {
	if f.vgReduceAndRemovePVFunc != nil {
		return f.vgReduceAndRemovePVFunc(ctx, hosts, vgName, device)
	}
	return successExecResult(hosts, ""), nil
}

func (f *fakeDeploymentClient) LVCreateCacheVol(ctx context.Context, hosts []string, vgName, lvName, device string) (*deployment.ExecResult, error) {
	if f.lvCreateCacheVolFunc != nil {
		return f.lvCreateCacheVolFunc(ctx, hosts, vgName, lvName, device)
	}
	return successExecResult(hosts, ""), nil
}

func (f *fakeDeploymentClient) LVConvertToCache(ctx context.Context, hosts []string, vgName, lvName, cacheVol, mode string) (*deployment.ExecResult, error) {
	if f.lvConvertToCacheFunc != nil {
		return f.lvConvertToCacheFunc(ctx, hosts, vgName, lvName, cacheVol, mode)
	}
	return successExecResult(hosts, ""), nil
}

func (f *fakeDeploymentClient) LVUncache(ctx context.Context, hosts []string, vgName, lvName string) (*deployment.ExecResult, error) {
	if f.lvUncacheFunc != nil {
		return f.lvUncacheFunc(ctx, hosts, vgName, lvName)
	}
	return successExecResult(hosts, ""), nil
}

func (f *fakeDeploymentClient) LVThinPoolIn(ctx context.Context, host, vgName string) (string, error) {
	if f.lvThinPoolInFunc != nil {
		return f.lvThinPoolInFunc(ctx, host, vgName)
	}
	return "", nil
}

func (f *fakeDeploymentClient) LVExists(ctx context.Context, host, vgName, lvName string) (bool, error) {
	if f.lvExistsFunc != nil {
		return f.lvExistsFunc(ctx, host, vgName, lvName)
	}
	return true, nil
}

func (f *fakeDeploymentClient) LVSizeBytes(ctx context.Context, host, vgName, lvName string) (uint64, error) {
	if f.lvSizeBytesFunc != nil {
		return f.lvSizeBytesFunc(host, vgName, lvName)
	}
	return 6442450944, nil
}

func (f *fakeDeploymentClient) VGFreeBytes(ctx context.Context, host, vgName string) (uint64, error) {
	if f.vgFreeBytesFunc != nil {
		return f.vgFreeBytesFunc(host, vgName)
	}
	return 3 << 30, nil
}

func (f *fakeDeploymentClient) LVCreateThinPoolAllFree(ctx context.Context, hosts []string, vgName, poolName string, metadataBytes uint64) (*deployment.ExecResult, error) {
	if f.lvCreateThinPoolAllFreeFunc != nil {
		return f.lvCreateThinPoolAllFreeFunc(ctx, hosts, vgName, poolName, metadataBytes)
	}
	return successExecResult(hosts, ""), nil
}

func (f *fakeDeploymentClient) DRBDDetach(ctx context.Context, host, resource string) (*deployment.ExecResult, error) {
	if f.drbdDetachFunc != nil {
		return f.drbdDetachFunc(ctx, host, resource)
	}
	return successExecResult([]string{host}, ""), nil
}

func (f *fakeDeploymentClient) DRBDAttach(ctx context.Context, host, resource string) (*deployment.ExecResult, error) {
	if f.drbdAttachFunc != nil {
		return f.drbdAttachFunc(ctx, host, resource)
	}
	return successExecResult([]string{host}, ""), nil
}

func (f *fakeDeploymentClient) LVRemoveSnapshot(ctx context.Context, hosts []string, vgName, snapshotName string) (*deployment.ExecResult, error) {
	if f.lvRemoveSnapshotFunc != nil {
		return f.lvRemoveSnapshotFunc(ctx, hosts, vgName, snapshotName)
	}
	return successExecResult(hosts, ""), nil
}

func (f *fakeDeploymentClient) LVListSnapshots(ctx context.Context, hosts []string, vgName string) (*deployment.ExecResult, error) {
	if f.lvListSnapshotsFunc != nil {
		return f.lvListSnapshotsFunc(ctx, hosts, vgName)
	}
	return successExecResult(hosts, ""), nil
}

func (f *fakeDeploymentClient) LVMergeSnapshot(ctx context.Context, hosts []string, vgName, snapshotName string) (*deployment.ExecResult, error) {
	if f.lvMergeSnapshotFunc != nil {
		return f.lvMergeSnapshotFunc(ctx, hosts, vgName, snapshotName)
	}
	return successExecResult(hosts, ""), nil
}

func (f *fakeDeploymentClient) ZFSCreatePool(ctx context.Context, hosts []string, poolName string, vdevs []string, opts ...deployment.ZFSOption) (*deployment.ExecResult, error) {
	f.zfsCreatePoolCalls = append(f.zfsCreatePoolCalls, struct {
		hosts    []string
		poolName string
		vdevs    []string
		optCount int
	}{hosts: cloneStrings(hosts), poolName: poolName, vdevs: cloneStrings(vdevs), optCount: len(opts)})
	if f.zfsCreatePoolFunc != nil {
		return f.zfsCreatePoolFunc(ctx, hosts, poolName, vdevs, opts...)
	}
	return successExecResult(hosts, ""), nil
}

func (f *fakeDeploymentClient) ZFSDestroyPool(ctx context.Context, hosts []string, poolName string) (*deployment.ExecResult, error) {
	f.zfsDestroyPoolCalls = append(f.zfsDestroyPoolCalls, struct {
		hosts    []string
		poolName string
	}{hosts: cloneStrings(hosts), poolName: poolName})
	if f.zfsDestroyPoolFunc != nil {
		return f.zfsDestroyPoolFunc(ctx, hosts, poolName)
	}
	return successExecResult(hosts, ""), nil
}

func (f *fakeDeploymentClient) ZFSListPools(ctx context.Context, hosts []string) (*deployment.ExecResult, error) {
	if f.zfsListPoolsFunc != nil {
		return f.zfsListPoolsFunc(ctx, hosts)
	}
	return successExecResult(hosts, ""), nil
}

func (f *fakeDeploymentClient) ZFSCreateDataset(ctx context.Context, hosts []string, datasetName string, opts ...deployment.ZFSOption) (*deployment.ExecResult, error) {
	return successExecResult(hosts, ""), nil
}

func (f *fakeDeploymentClient) ZFSCreateThinDataset(ctx context.Context, hosts []string, poolName, datasetName, size string) (*deployment.ExecResult, error) {
	return successExecResult(hosts, ""), nil
}

func (f *fakeDeploymentClient) ZFSDestroyDataset(ctx context.Context, hosts []string, datasetName string) (*deployment.ExecResult, error) {
	return successExecResult(hosts, ""), nil
}

func (f *fakeDeploymentClient) ZFSSnapshot(ctx context.Context, hosts []string, dataset, snapshotName string) (*deployment.ExecResult, error) {
	return successExecResult(hosts, ""), nil
}

func (f *fakeDeploymentClient) ZFSRollback(ctx context.Context, hosts []string, dataset, snapshotName string) (*deployment.ExecResult, error) {
	return successExecResult(hosts, ""), nil
}

func (f *fakeDeploymentClient) ZFSClone(ctx context.Context, hosts []string, snapshot, clonePath string) (*deployment.ExecResult, error) {
	return successExecResult(hosts, ""), nil
}

func (f *fakeDeploymentClient) ZFSListSnapshots(ctx context.Context, hosts []string, dataset string) (*deployment.ExecResult, error) {
	if f.zfsListSnapshotsFunc != nil {
		return f.zfsListSnapshotsFunc(ctx, hosts, dataset)
	}
	return successExecResult(hosts, ""), nil
}

func (f *fakeDeploymentClient) ZFSDestroySnapshot(ctx context.Context, hosts []string, snapshot string) (*deployment.ExecResult, error) {
	return successExecResult(hosts, ""), nil
}

func (f *fakeDeploymentClient) ZFSSetQuota(ctx context.Context, hosts []string, dataset, quota string) (*deployment.ExecResult, error) {
	return successExecResult(hosts, ""), nil
}

func (f *fakeDeploymentClient) ZFSResizeVolume(ctx context.Context, hosts []string, volumePath, newSize string) (*deployment.ExecResult, error) {
	return successExecResult(hosts, ""), nil
}

func (f *fakeDeploymentClient) DRBDUp(ctx context.Context, hosts []string, resource string) (*deployment.ExecResult, error) {
	f.drbdUpCalls = append(f.drbdUpCalls, struct {
		hosts    []string
		resource string
	}{hosts: cloneStrings(hosts), resource: resource})
	if f.drbdUpFunc != nil {
		return f.drbdUpFunc(ctx, hosts, resource)
	}
	return successExecResult(hosts, ""), nil
}

func (f *fakeDeploymentClient) DRBDDown(ctx context.Context, hosts []string, resource string) (*deployment.ExecResult, error) {
	f.drbdDownCalls = append(f.drbdDownCalls, struct {
		hosts    []string
		resource string
	}{hosts: cloneStrings(hosts), resource: resource})
	if f.drbdDownFunc != nil {
		return f.drbdDownFunc(ctx, hosts, resource)
	}
	return successExecResult(hosts, ""), nil
}

func (f *fakeDeploymentClient) LVRemove(ctx context.Context, hosts []string, lvPath string) (*deployment.ExecResult, error) {
	f.lvRemoveCalls = append(f.lvRemoveCalls, struct {
		hosts  []string
		lvPath string
	}{hosts: cloneStrings(hosts), lvPath: lvPath})
	if f.lvRemoveFunc != nil {
		return f.lvRemoveFunc(ctx, hosts, lvPath)
	}
	return successExecResult(hosts, ""), nil
}

func (f *fakeDeploymentClient) DRBDPrimary(ctx context.Context, host, resource string, force bool) (*deployment.HostResult, error) {
	if f.drbdPrimaryFunc != nil {
		return f.drbdPrimaryFunc(ctx, host, resource, force)
	}
	return &deployment.HostResult{Host: host, Success: true}, nil
}

func (f *fakeDeploymentClient) DRBDSecondary(ctx context.Context, host, resource string) (*deployment.HostResult, error) {
	if f.drbdSecondaryFunc != nil {
		return f.drbdSecondaryFunc(ctx, host, resource)
	}
	return &deployment.HostResult{Host: host, Success: true}, nil
}

func (f *fakeDeploymentClient) DRBDCreateMD(ctx context.Context, hosts []string, resource string, maxPeers int) (*deployment.ExecResult, error) {
	f.drbdCreateMDCalls = append(f.drbdCreateMDCalls, struct {
		hosts    []string
		resource string
		maxPeers int
	}{hosts: cloneStrings(hosts), resource: resource, maxPeers: maxPeers})
	if f.drbdCreateMDFunc != nil {
		return f.drbdCreateMDFunc(ctx, hosts, resource)
	}
	return successExecResult(hosts, ""), nil
}

func (f *fakeDeploymentClient) DRBDStatus(ctx context.Context, hosts []string, resource string) (*deployment.ExecResult, error) {
	f.drbdStatusCalls = append(f.drbdStatusCalls, struct {
		hosts    []string
		resource string
	}{hosts: cloneStrings(hosts), resource: resource})
	if f.drbdStatusFunc != nil {
		return f.drbdStatusFunc(ctx, hosts, resource)
	}
	return successExecResult(hosts, ""), nil
}

func (f *fakeDeploymentClient) DRBDStatusJSON(ctx context.Context, hosts []string, resource string) (*deployment.ExecResult, error) {
	if f.drbdStatusJSONFunc != nil {
		return f.drbdStatusJSONFunc(ctx, hosts, resource)
	}
	// No JSON status by default: an empty output makes parseNodeStatesFromJSON
	// return an error so the caller keeps its text-parsed states.
	return successExecResult(hosts, ""), nil
}

func (f *fakeDeploymentClient) ReactorReload(ctx context.Context, hosts []string) (*deployment.ExecResult, error) {
	if f.reactorReloadFunc != nil {
		return f.reactorReloadFunc(ctx, hosts)
	}
	return successExecResult(hosts, ""), nil
}

func (f *fakeDeploymentClient) ReactorPromoterStatusByResource(ctx context.Context, host, resource string) (*deployment.ReactorPromoterStatus, error) {
	if f.reactorPromoterStatusByResourceFunc != nil {
		return f.reactorPromoterStatusByResourceFunc(ctx, host, resource)
	}
	return nil, assert.AnError
}

func (f *fakeDeploymentClient) ReactorStatusJSON(ctx context.Context, host string) (*deployment.ReactorStatus, error) {
	return nil, assert.AnError
}
