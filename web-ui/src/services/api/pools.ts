import { type ApiResponse, type RequestFn } from './client';

export interface Pool {
  name: string;
  type: string;
  node: string;
  totalGb: string;
  freeGb: string;
  devices: string[];
  thin: boolean;
  compression: string;
  // Storage tiering (lvmcache). cacheMode is the field that matters
  // operationally: writeback puts the SSD in the durability path, writethrough
  // does not.
  cached?: boolean;
  cacheMode?: string;
  cacheSizeBytes?: string;
  cacheUsedPercent?: number;
  cacheHitPercent?: number;
  cacheDirtyPercent?: number;
  cacheDevice?: string;
  cacheDegraded?: boolean;
  // Thin pool utilisation. totalGb/freeGb above describe the VOLUME GROUP, and
  // Haify builds its thin pool from every free extent, so freeGb is zero for the
  // whole life of such a pool however empty it is. These are the fields that
  // say whether the next write will succeed.
  //
  // thinPoolLv is empty when the group holds no thin pool; that — not a zero
  // percentage — distinguishes "no thin pool" from "a thin pool at 0%".
  thinPoolLv?: string;
  thinSizeBytes?: string;
  thinDataPercent?: number;
  thinMetadataPercent?: number;
  thinOutOfSpace?: boolean;
  // VDO-backed thin pools: physical usage under the thin pool, which can run
  // out while thinDataPercent still shows room.
  hasVdo?: boolean;
  vdoPhysicalPercent?: number;
  vdoSavingPercent?: number;
  // Exact capacity. totalGb/freeGb are rounded to whole gibibytes.
  totalBytes?: string;
  freeBytes?: string;
  // What new volumes can actually get: the thin pool's own size and room for
  // a thin pool, the volume group's otherwise.
  capacityBytes?: string;
  availableBytes?: string;
}

export interface PoolsResponse extends ApiResponse {
  pools: Pool[];
}

export const poolsApi = (request: RequestFn) => ({
  // ==================== Pools ====================
  getPools: () => request<PoolsResponse>('/pools'),

  getPool: (name: string) =>
    request<ApiResponse & { pool: Pool }>(`/pools/${name}`),

  createPool: (data: {
    name: string;
    type: string;
    node: string;
    disks?: string[];
    sizeGb?: number;
    wipe?: boolean;
  }) =>
    request<ApiResponse>('/pools', {
      method: 'POST',
      body: JSON.stringify(data),
    }),

  deletePool: (name: string, node?: string) =>
    request<ApiResponse>(`/pools/${name}${node ? `?node=${node}` : ''}`, { method: 'DELETE' }),

  // ZFS pools live on a dedicated API: creation takes vdevs (whole devices
  // or partitions), not the LVM disks/VG flow.
  createZFSPool: (data: { name: string; node: string; vdevs: string[]; wipe?: boolean }) =>
    request<ApiResponse>('/zfs/pools', {
      method: 'POST',
      body: JSON.stringify(data),
    }),

  deleteZFSPool: (name: string, node?: string) =>
    request<ApiResponse>(`/zfs/pools/${name}${node ? `?node=${node}` : ''}`, {
      method: 'DELETE',
    }),

  addDisk: (pool: string, disk: string, node?: string, wipe = false) =>
    request<ApiResponse>(`/pools/${pool}/disks`, {
      method: 'POST',
      body: JSON.stringify({ disk, node, wipe }),
    }),
});

const GIB = 1024 ** 3;

/** A pool's size and the room left in it for new volumes, in GiB. Prefers the
 *  controller's capacity figures, which count a thin pool rather than its
 *  volume group (whose free space is zero once the thin pool takes it all). */
export function poolRoomGiB(pool: Pool): { total: number; free: number } {
  if (pool.capacityBytes && Number(pool.capacityBytes) > 0) {
    return {
      total: Number(pool.capacityBytes) / GIB,
      free: Number(pool.availableBytes ?? 0) / GIB,
    };
  }
  return { total: Number(pool.totalGb), free: Number(pool.freeGb) };
}
