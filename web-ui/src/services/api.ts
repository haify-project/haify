import { createRequest } from './api/client';
import { nodesApi } from './api/nodes';
import { poolsApi } from './api/pools';
import { resourcesApi } from './api/resources';
import { profilesApi } from './api/profiles';
import { snapshotsApi } from './api/snapshots';
import { haApi } from './api/ha';
import { gatewaysApi } from './api/gateways';
import { logsApi } from './api/logs';
import { selfHaApi } from './api/selfHa';
import { rbacApi } from './api/rbac';
import { notifyApi } from './api/notify';
import { storageApi } from './api/storage';

export { getApiToken, setApiToken, setAuthPromptHandler } from './api/client';
export type { ApiResponse } from './api/client';
export type { Node, NodesResponse, HealthInfo } from './api/nodes';
export type { Pool, PoolsResponse } from './api/pools';
export { poolRoomGiB } from './api/pools';
export type {
  Resource,
  Volume,
  NodeState,
  ResourceStatus,
  QuorumInfo,
  WANMetrics,
  NodeResourceState,
  ResourcesResponse,
  ResourceOption,
  ResourceOptionsResponse,
} from './api/resources';
export type { ResourceProfile, ResourceProfilesResponse } from './api/profiles';
export type {
  GFSRetention,
  SnapshotSchedule,
  SnapshotSchedulesResponse,
  Snapshot,
  SnapshotsResponse,
} from './api/snapshots';
export type {
  HaConfig,
  OcfAgentSpec,
  HaStartItem,
  ResourceAgentSummary,
  ResourceAgentsResponse,
  OcfAgentParameter,
  ResourceAgentMetadata,
  HaUnit,
  HaUnitKind,
  HaStandby,
  HaOutsideService,
  HaPromoterStatus,
  HaStatusResponse,
  HaTomlResponse,
  HaConfigsResponse,
} from './api/ha';
export type {
  Gateway,
  GatewaysResponse,
  NFSExport,
  ISCSILUN,
  NVMeNamespace,
  SMBShare,
} from './api/gateways';
export type {
  AuditEvent,
  AuditEventsResponse,
  AuditQuery,
  ClusterEventsResponse,
  EventQuery,
  ControllerLogEntry,
  ControllerLogsResponse,
  ControllerLogQuery,
} from './api/logs';
export type { SelfHaStatus } from './api/selfHa';
export type { RbacWhoami, RbacPolicy, RbacUserRole, RbacPolicies } from './api/rbac';
export type { NotifyChannel, NotifyChannelInput } from './api/notify';
export type { PoolDisk, NodeDisk, TrimResult, StorageJob, StorageJobResponse } from './api/storage';

const request = createRequest();

export const api = {
  ...nodesApi(request),
  ...poolsApi(request),
  ...resourcesApi(request),
  ...profilesApi(request),
  ...snapshotsApi(request),
  ...haApi(request),
  ...gatewaysApi(request),
  ...logsApi(request),
  ...selfHaApi(request),
  ...rbacApi(request),
  ...notifyApi(request),
  ...storageApi(request),
};
