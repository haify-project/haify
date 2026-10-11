package mcpserver

import (
	"context"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/client"
)

// ControllerClient is the subset of *client.HaifyClient the MCP server uses.
// Tools depend on this interface so tests can substitute a mock without a
// running controller.
type ControllerClient interface {
	// Nodes
	ListNodes(ctx context.Context) ([]*haifypb.NodeInfo, error)
	RegisterNode(ctx context.Context, name, address string) (*haifypb.NodeInfo, error)
	UnregisterNode(ctx context.Context, address string) error
	HealthCheck(ctx context.Context, node string) (*client.NodeHealthInfo, error)

	// Pools
	ListPools(ctx context.Context) ([]*haifypb.PoolInfo, error)
	CreatePool(ctx context.Context, name, poolType, node string, disks []string, sizeGB uint64, wipe bool) error
	CreateZFSPoolOptions(ctx context.Context, name, node string, vdevs []string, compression string, dedup, wipe bool) error
	DeletePool(ctx context.Context, pool, node string) error
	AddDiskToPool(ctx context.Context, pool, disk, node string, wipe bool) error
	ListNodeDisks(ctx context.Context, node string) ([]*haifypb.NodeDiskInfo, error)

	// Storage upkeep
	TrimPools(ctx context.Context, node string) (*haifypb.TrimPoolsResponse, error)
	ListPoolDisks(ctx context.Context, pool, node string) ([]*haifypb.PoolDiskInfo, error)
	RemovePoolDisk(ctx context.Context, pool, node, disk string) (*haifypb.StorageJobResponse, error)
	ReplacePoolDisk(ctx context.Context, pool, node, oldDisk, newDisk string, wipe bool) (*haifypb.StorageJobResponse, error)
	MoveVolume(ctx context.Context, resource string, volumeID int32, pool string) (*haifypb.StorageJobResponse, error)
	ListStorageJobs(ctx context.Context, includeFinished bool) ([]*haifypb.StorageJobInfo, error)

	// Resources
	ListResources(ctx context.Context) ([]*haifypb.ResourceInfo, error)
	ResourceStatus(ctx context.Context, name string) (*haifypb.ResourceStatus, error)
	CreateResourceWithPoolAndType(ctx context.Context, name string, port uint32, nodes []string, protocol string, sizeGB uint32, pool string, storageType string, drbdOptions map[string]string) error
	CreateResourceWithVolumes(ctx context.Context, name string, port uint32, nodes []string, protocol, storageType string, drbdOptions map[string]string, volumes []*haifypb.VolumeSpec) error
	AdoptResource(ctx context.Context, name string, nodes []string, port uint32, protocol string) (*haifypb.AdoptResourceResponse, error)
	DeleteResource(ctx context.Context, name string) error
	SetPrimary(ctx context.Context, resource, node string, force bool) error
	PromoteForNode(ctx context.Context, resource, node string) error
	SetSecondary(ctx context.Context, resource, node string) error
	SetDualPrimary(ctx context.Context, resource string, enable bool) error
	AddVolume(ctx context.Context, resource, volume, pool string, sizeGB uint32) error
	UpdateResourceOptions(ctx context.Context, resource string, options map[string]string) error
	RemoveVolume(ctx context.Context, resource string, volumeID uint32) error
	ResizeVolume(ctx context.Context, resource string, volumeID uint32, sizeGB uint32) error
	CreateFilesystem(ctx context.Context, resource string, volumeID uint32, node, fstype string) error
	MountResource(ctx context.Context, resource string, volumeID uint32, path, node, fstype string) error
	UnmountResource(ctx context.Context, resource string, volumeID uint32, node string) error

	// Snapshots (storage-type aware)
	CreateLvmSnapshot(ctx context.Context, pool, lvName, snapshotName, node, size string) error
	ListLvmSnapshots(ctx context.Context, pool, node, resource string) ([]*haifypb.SnapshotInfo, error)
	DeleteLvmSnapshot(ctx context.Context, pool, snapshotName, node string) error
	RestoreLvmSnapshot(ctx context.Context, pool, snapshotName, node string) error
	CreateZFSSnapshot(ctx context.Context, dataset, snapshotName, node string) error
	ListZFSSnapshots(ctx context.Context, dataset, node string) ([]*haifypb.SnapshotInfo, error)
	DeleteZFSSnapshot(ctx context.Context, snapshot, node string) error
	RestoreZFSSnapshot(ctx context.Context, dataset, snapshotName, node string) error

	// Snapshot schedules (cron-driven, GFS retention)
	CreateSnapshotSchedule(ctx context.Context, resource, cron string, keep *haifypb.GFSRetention, enabled bool) error
	ListSnapshotSchedules(ctx context.Context) ([]*haifypb.SnapshotScheduleInfo, error)
	DeleteSnapshotSchedule(ctx context.Context, name string) error

	// Gateways
	ListGateways(ctx context.Context) ([]*haifypb.GatewayInfo, error)
	GetGateway(ctx context.Context, id string) (*haifypb.GatewayInfo, error)
	CreateNFSGateway(ctx context.Context, req *haifypb.CreateNFSGatewayRequest) (*haifypb.CreateNFSGatewayResponse, error)
	CreateISCSIGateway(ctx context.Context, req *haifypb.CreateISCSIGatewayRequest) (*haifypb.CreateISCSIGatewayResponse, error)
	CreateNVMeGateway(ctx context.Context, req *haifypb.CreateNVMeGatewayRequest) (*haifypb.CreateNVMeGatewayResponse, error)
	CreateSMBGateway(ctx context.Context, req *haifypb.CreateSMBGatewayRequest) (*haifypb.CreateSMBGatewayResponse, error)
	ListSMBShares(ctx context.Context, resource string) ([]*haifypb.SMBShareInfo, error)
	AddSMBShare(ctx context.Context, resource string, share *haifypb.SMBShareInfo) error
	RemoveSMBShare(ctx context.Context, resource, name string) error
	ListSMBUsers(ctx context.Context, resource string) ([]string, error)
	StartGateway(ctx context.Context, id string) error
	StopGateway(ctx context.Context, id string) error
	DeleteGateway(ctx context.Context, id string) error

	// NFS exports
	AddNFSExport(ctx context.Context, resource, exportPath string, fsid int32, clientSpec, options string) error
	RemoveNFSExport(ctx context.Context, resource, exportPath string) error
	ListNFSExports(ctx context.Context, resource string) ([]*haifypb.NFSExportInfo, error)

	// iSCSI configuration
	AddISCSILUN(ctx context.Context, resource string, lun int32, device string) error
	RemoveISCSILUN(ctx context.Context, resource string, lun int32) error
	ListISCSILUNs(ctx context.Context, resource string) ([]*haifypb.ISCSILUNInfo, error)
	AddISCSIInitiator(ctx context.Context, resource, initiator string) error
	RemoveISCSIInitiator(ctx context.Context, resource, initiator string) error
	ListISCSIInitiators(ctx context.Context, resource string) ([]string, error)
	SetISCSIChap(ctx context.Context, resource, username, password string, mutual bool) error
	GetISCSIChap(ctx context.Context, resource string) (*haifypb.GetISCSIChapResponse, error)

	// NVMe-oF configuration
	AddNVMeNamespace(ctx context.Context, resource, device string) error
	RemoveNVMeNamespace(ctx context.Context, resource string, namespaceID int32) error
	ListNVMeNamespaces(ctx context.Context, resource string) ([]*haifypb.NVMeNamespaceInfo, error)
	AddNVMeHost(ctx context.Context, resource, hostNQN string) error
	RemoveNVMeHost(ctx context.Context, resource, hostNQN string) error
	ListNVMeHosts(ctx context.Context, resource string) ([]string, error)

	// HA
	SetResourceLabels(ctx context.Context, resource string, set map[string]string, remove []string) (map[string]string, error)
	MakeHa(ctx context.Context, resource string, services []string, mountPoint, fsType, vip string, ocfAgents []*haifypb.OcfAgent, startItems []*haifypb.HaStartItem) (string, error)
	ListHa(ctx context.Context) ([]*haifypb.HaConfigInfo, error)
	GetHa(ctx context.Context, resource string) (*haifypb.HaConfigInfo, error)
	EvictHa(ctx context.Context, resource string) error
	DeleteHa(ctx context.Context, resource string) error
	GetSelfHaStatus(ctx context.Context) (*client.SelfHaStatus, error)
	EnableSelfHa(ctx context.Context, vip, pool string, sizeGB, port uint32, nodes []string) (string, string, error)
	DisableSelfHa(ctx context.Context, node string) error
	GetHaStatus(ctx context.Context, resource string) ([]*haifypb.HaPromoterStatus, error)
	GetHaToml(ctx context.Context, resource string) (*haifypb.GetHaTomlResponse, error)
	SyncHaToml(ctx context.Context, resource, content string) (string, error)
	ListResourceAgents(ctx context.Context) ([]*haifypb.ResourceAgentInfo, error)
	GetResourceAgentMetadata(ctx context.Context, provider, name string) (*haifypb.GetResourceAgentMetadataResponse, error)

	// Observability: what the cluster did, not just what it is. An assistant
	// with only state queries can describe the cluster but cannot explain how
	// it got there — which is most of what an operator actually asks.
	ListEvents(ctx context.Context, req *haifypb.ListEventsRequest) (*haifypb.ListEventsResponse, error)
	ListAuditEvents(ctx context.Context, req *haifypb.ListAuditEventsRequest) (*haifypb.ListAuditEventsResponse, error)
	ListControllerLogs(ctx context.Context, req *haifypb.ListControllerLogsRequest) (*haifypb.ListControllerLogsResponse, error)
	// The journals and kernel messages the controller does not hold. Read-only
	// and collector-named: there is no path from here to running a command.
	CollectNodeDiagnostics(ctx context.Context, req *haifypb.CollectNodeDiagnosticsRequest) (*haifypb.CollectNodeDiagnosticsResponse, error)
	// Cluster inspection. Running one changes nothing on the cluster.
	RunInspection(ctx context.Context, areas []string) (*haifypb.InspectionReport, error)
	GetInspection(ctx context.Context, id string) (*haifypb.InspectionReport, error)

	// Notification channels. Listing and testing are exposed; creating one is
	// not, for the same reason a backup target cannot be created from here — a
	// bot URL is a bearer credential, and anything passed as a tool argument is
	// recorded in the conversation that passed it.
	ListNotifyChannels(ctx context.Context) ([]*haifypb.NotifyChannelInfo, []string, error)
	TestNotifyChannel(ctx context.Context, name string) (string, error)

	// Topology changes
	AddReplica(ctx context.Context, resource, node string) error
	RemoveReplica(ctx context.Context, resource, node string) error
	AttachDisklessClient(ctx context.Context, resource, node string) error
	DetachDisklessClient(ctx context.Context, resource, node string) error
	SetTiebreaker(ctx context.Context, resource, node string) (string, string, error)
	AddDR(ctx context.Context, resource, drNode, drEndpoint string, wanPort uint32, egressAddress string) (uint32, error)
	RepairWanProxy(ctx context.Context, name string, dryRun bool) (*haifypb.RepairWanProxyResponse, error)

	// Node lifecycle
	DrainNode(ctx context.Context, name string) ([]string, error)
	UndrainNode(ctx context.Context, name string) error
	SetNodeLabels(ctx context.Context, node string, labels map[string]string, replace bool) (*haifypb.NodeInfo, error)
	ConvertPoolToThin(ctx context.Context, node, pool string) error

	// ZFS
	ListZFSpools(ctx context.Context) ([]*haifypb.PoolInfo, error)
	DeleteZFSPool(ctx context.Context, name, node string) error
	CreateZFSDataset(ctx context.Context, datasetPath, node string) error
	DeleteZFSDataset(ctx context.Context, datasetPath, node string) error
	CreateZFSVolume(ctx context.Context, poolName, volumeName, size, node string) error
	ResizeZFSVolume(ctx context.Context, volumePath, newSize, node string) error
	CloneZFSSnapshot(ctx context.Context, snapshot, clonePath, node string) error

	// Fast tier
	AddPoolCache(ctx context.Context, node, pool, device, mode string) (string, uint64, error)
	RemovePoolCache(ctx context.Context, node, pool string) error

	// Off-cluster backups. AddBackupTarget is absent on purpose: it carries a
	// credential, and MCP tool arguments are recorded by the caller.
	ListBackupTargets(ctx context.Context) ([]*haifypb.BackupTargetInfo, error)
	DeleteBackupTarget(ctx context.Context, name string, force bool) error
	CreateBackup(ctx context.Context, resource, target, node string, full bool) (*haifypb.BackupInfo, error)
	ListBackups(ctx context.Context, resource, target string) ([]*haifypb.BackupInfo, error)
	RestoreBackup(ctx context.Context, id, resource, node string) (*haifypb.BackupInfo, error)
	DeleteBackup(ctx context.Context, id, node string, force bool) error
	ImportBackups(ctx context.Context, target, node string) (*haifypb.ImportBackupsResponse, error)
	CreateBackupSchedule(ctx context.Context, req *haifypb.CreateBackupScheduleRequest) (*haifypb.BackupScheduleInfo, error)
	ListBackupSchedules(ctx context.Context) ([]*haifypb.BackupScheduleInfo, error)
	DeleteBackupSchedule(ctx context.Context, name string) error

	// Database applications (`haify app`). CreateApp returns the generated
	// password; the tool does not pass it on (see tools_db_apps.go).
	CreateApp(ctx context.Context, req client.AppCreateRequest) (*haifypb.CreateAppResponse, error)
	ListApps(ctx context.Context) ([]*haifypb.AppInfo, error)
	GetAppStatus(ctx context.Context, name string) (*haifypb.GetAppStatusResponse, error)
	DeleteApp(ctx context.Context, name string, deleteData bool) (string, error)
	FailoverApp(ctx context.Context, name string) (*haifypb.FailoverAppResponse, error)
	SnapshotApp(ctx context.Context, name, snapshot string) (*haifypb.SnapshotAppResponse, error)
}

// compile-time check: the real gRPC client satisfies the interface.
var _ ControllerClient = (*client.HaifyClient)(nil)
