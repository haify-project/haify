import { type ApiResponse, type RequestFn } from './client';

export interface GFSRetention {
  hourly?: number;
  daily?: number;
  weekly?: number;
  monthly?: number;
  yearly?: number;
}

export interface SnapshotSchedule {
  name: string;
  resource: string;
  cron: string;
  enabled: boolean;
  keep?: GFSRetention;
  lastRun?: string;
  nextRun?: string;
}

export interface SnapshotSchedulesResponse extends ApiResponse {
  schedules: SnapshotSchedule[];
}

export interface Snapshot {
  name: string;
  volume: string;
  sizeGb: number;
  createdAt: string;
  /** "filesystem" or "crash" for a snapshot of a guest's volume; unset for any other. */
  consistency?: string;
}

export interface SnapshotsResponse extends ApiResponse {
  snapshots: Snapshot[];
}

export const snapshotsApi = (request: RequestFn) => ({
  // ==================== Snapshot schedules ====================
  getSnapshotSchedules: () =>
    request<SnapshotSchedulesResponse>('/snapshot-schedules'),

  createSnapshotSchedule: (data: {
    resource: string;
    cron: string;
    keep: GFSRetention;
    enabled: boolean;
  }) =>
    request<ApiResponse>('/snapshot-schedules', {
      method: 'POST',
      body: JSON.stringify(data),
    }),

  deleteSnapshotSchedule: (name: string) =>
    request<ApiResponse>(
      `/snapshot-schedules/${encodeURIComponent(name)}`,
      { method: 'DELETE' },
    ),

  // ==================== Snapshots ====================
  listSnapshots: (volume: string, node?: string) =>
    request<SnapshotsResponse>(`/volumes/${volume}/snapshots${node ? `?node=${node}` : ''}`),

  createSnapshot: (volume: string, snapshotName: string, node?: string) =>
    request<ApiResponse>(`/volumes/${volume}/snapshots`, {
      method: 'POST',
      body: JSON.stringify({ snapshotName, node }),
    }),

  deleteSnapshot: (volume: string, snapshotName: string, node?: string) =>
    request<ApiResponse>(`/volumes/${volume}/snapshots/${snapshotName}${node ? `?node=${node}` : ''}`, { method: 'DELETE' }),

  restoreSnapshot: (volume: string, snapshotName: string, node?: string) =>
    request<ApiResponse>(`/volumes/${volume}/snapshots/${snapshotName}/restore`, {
      method: 'POST',
      body: JSON.stringify({ node }),
    }),
});
