import { type ApiResponse, type RequestFn } from './client';
import type { Resource } from './resources';

// The dashboard's numbers (GET /v1/cluster/summary), the cluster's totals over
// time (/v1/cluster/history) and the nodes' statistics from Prometheus
// (/v1/cluster/stats). 64-bit integers arrive as strings and zero values are
// left out, so every field is optional and read through num().

export const num = (v: number | string | undefined | null): number => (v == null ? 0 : Number(v));

export interface AttentionItem {
  type: string;
  severity: string;
  count?: number;
  subjects?: string[];
}

export interface ClusterSummary extends ApiResponse {
  observedUnix?: string;
  resources?: {
    total?: number;
    healthy?: number;
    resyncing?: number;
    degraded?: number;
    noQuorum?: number;
    unknown?: number;
    volumes?: number;
  };
  nodes?: { total?: number; online?: number; offline?: number; draining?: number };
  capacity?: { totalBytes?: string; usedBytes?: string; pools?: number; poolsAbove90?: number; pools80To90?: number };
  gateways?: { total?: number; serving?: number };
  replication?: {
    replicas?: number;
    disklessClients?: number;
    tiebreakers?: number;
    offsite?: number;
    encryptedLinks?: number;
    writeBytesPerSec?: number;
    readBytesPerSec?: number;
    outOfSyncBytes?: string;
  };
  protection?: {
    snapshotSchedules?: number;
    covered?: number;
    schedulesRunToday?: number;
    backupsToday?: number;
    backupsFailedToday?: number;
  };
  today?: {
    events?: number;
    critical?: number;
    warning?: number;
    failovers?: number;
    nodesLost?: number;
    replicasMoved?: number;
    poolsExtended?: number;
    inspectionPassed?: number;
    inspectionTotal?: number;
  };
  attention?: AttentionItem[];
  eventsByHour?: { critical?: number; warning?: number; info?: number }[];
  poolFullness?: number[];
  controlPlane?: string;
}

export interface ClusterSample {
  unix?: string;
  writeBytesPerSec?: number;
  readBytesPerSec?: number;
  outOfSyncBytes?: string;
  capacityUsedBytes?: string;
  capacityTotalBytes?: string;
  apiRequestsPerSec?: number;
  apiErrorsPerSec?: number;
}

export interface ClusterHistory extends ApiResponse {
  samples?: ClusterSample[];
}

export interface NodeStat {
  node?: string;
  value?: number;
}

export interface StatSeries {
  unix?: string[];
  avg?: number[];
  min?: number[];
  max?: number[];
  sum?: number[];
  nowAvg?: number;
  nowSum?: number;
  busiest?: NodeStat;
}

export interface ClusterStats extends ApiResponse {
  available?: boolean;
  nodes?: number;
  cpu?: StatSeries;
  memory?: StatSeries;
  diskWriteLatency?: StatSeries;
  network?: StatSeries;
  slowestDisks?: NodeStat[];
}

/** A replica as the last health poll saw it. */
export interface ReplicaHealth {
  node: string;
  /** ok | resyncing | bad | tiebreaker */
  state: string;
  role?: string;
}

export interface ResourceHealth {
  /** healthy | resyncing | degraded | no_quorum | unknown */
  state: string;
  summary?: string;
  replicas?: ReplicaHealth[];
  outOfSyncBytes?: string;
  writeBytesPerSec?: number;
}

export interface ResourceListItem extends Resource {
  health?: ResourceHealth;
  owner?: string;
  usedBy?: string;
}

export interface ResourcePageQuery {
  query?: string;
  state?: string;
  usedBy?: string;
  node?: string;
  pool?: string;
  sort?: string;
  offset?: number;
  limit?: number;
}

export interface ResourcePage extends ApiResponse {
  resources?: ResourceListItem[];
  total?: number;
  counts?: Record<string, number>;
}

export type HistoryRange = '1h' | '6h' | '24h' | '7d' | '30d' | '90d';

export const clusterApi = (request: RequestFn) => ({
  getClusterSummary: () => request<ClusterSummary>('/cluster/summary'),
  getClusterHistory: (range: HistoryRange) => request<ClusterHistory>(`/cluster/history?range=${range}`),
  getClusterStats: (range: HistoryRange) => request<ClusterStats>(`/cluster/stats?range=${range}`),
  getResourcePage: (q: ResourcePageQuery) => {
    const p = new URLSearchParams();
    for (const [k, v] of Object.entries(q)) {
      if (v !== undefined && v !== '' && v !== 0) p.set(k === 'usedBy' ? 'used_by' : k, String(v));
    }
    return request<ResourcePage>(`/resources?${p.toString()}`);
  },
});
