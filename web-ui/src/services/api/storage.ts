import { queryString, type ApiResponse, type RequestFn } from './client';

// Storage upkeep: the disks under the pools, trimming, and the long jobs that
// move data between disks and pools. 64-bit integers arrive as strings.

export interface PoolDisk {
  node: string;
  pool: string;
  device: string;
  sizeBytes?: string;
  usedBytes?: string;
  // "ok", "warn", "fail", or "unknown": no smartctl on the node, or a device
  // with no SMART at all, such as a virtual disk.
  health: string;
  healthDetail?: string;
  model?: string;
  serial?: string;
}

// A disk on a node and whether a pool can use it. state: "free" (as it is),
// "wipe" (holds only old data; usable when the caller asks for it to be
// erased), "pool" (in a Haify pool) or "busy" (in use; never touched).
export interface NodeDisk {
  node: string;
  device: string;
  byId?: string;
  type?: string;
  sizeBytes?: string;
  model?: string;
  serial?: string;
  transport?: string;
  rotational?: boolean;
  state: 'free' | 'wipe' | 'pool' | 'busy';
  pool?: string;
  detail?: string;
}

export interface TrimResult {
  node: string;
  mount: string;
  bytes?: string;
  error?: string;
}

export interface StorageJob {
  id: string;
  // "remove-disk", "replace-disk" or "move-volume".
  kind: string;
  // "running", "done" or "failed".
  state: string;
  subject: string;
  progress?: string;
  message?: string;
  startedUnix?: string;
  updatedUnix?: string;
}

export interface StorageJobResponse extends ApiResponse {
  jobId: string;
}

// The controller answers a refused request with HTTP 200 and success: false;
// these calls turn that into an error so a mutation's onError sees it.
async function checked<T extends ApiResponse>(p: Promise<T>): Promise<T> {
  const r = await p;
  if (!r.success) throw new Error(r.message || 'request failed');
  return r;
}

export const storageApi = (request: RequestFn) => ({
  getPoolDisks: (pool?: string, node?: string) =>
    checked(
      request<ApiResponse & { disks?: PoolDisk[] }>(`/storage/disks${queryString({ pool, node })}`),
    ),

  getNodeDisks: (node?: string) =>
    checked(
      request<ApiResponse & { disks?: NodeDisk[] }>(`/storage/node-disks${queryString({ node })}`),
    ),

  // Not checked: success is false when one filesystem failed, and the
  // results still say which ones were trimmed.
  trimPools: (node?: string) =>
    request<ApiResponse & { results?: TrimResult[] }>('/storage/trim', {
      method: 'POST',
      body: JSON.stringify({ node: node ?? '' }),
    }),

  removePoolDisk: (data: { pool: string; node: string; disk: string }) =>
    checked(
      request<StorageJobResponse>('/storage/disks/remove', {
        method: 'POST',
        body: JSON.stringify(data),
      }),
    ),

  replacePoolDisk: (data: { pool: string; node: string; oldDisk: string; newDisk: string; wipe?: boolean }) =>
    checked(
      request<StorageJobResponse>('/storage/disks/replace', {
        method: 'POST',
        body: JSON.stringify(data),
      }),
    ),

  moveVolume: (data: { resource: string; volumeId: number; pool: string }) =>
    checked(
      request<StorageJobResponse>('/storage/volumes/move', {
        method: 'POST',
        body: JSON.stringify(data),
      }),
    ),

  getStorageJobs: (includeFinished = false) =>
    checked(
      request<ApiResponse & { jobs?: StorageJob[] }>(
        `/storage/jobs${queryString({ includeFinished })}`,
      ),
    ),
});
