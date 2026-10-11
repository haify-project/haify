import { Badge } from '@/components/ui/badge';
import { Progress } from '@/components/ui/progress';
import { type Pool } from '../../services/api';
import { AddDiskDialog, DeletePoolDialog } from './PoolDialogs';
import { poolTypeBadgeClass, poolTypeLabel } from './poolTypes';

// Thin pool utilisation thresholds, mirroring the controller's alert defaults
// (pkg/alert). Kept in step so a pool the UI colours red is a pool that has
// already paged someone, rather than two different opinions of "full".
const THIN_NEAR_FULL = 85;
const THIN_FULL = 95;

function thinUsageClass(percent: number): string {
  if (percent >= THIN_FULL) return 'text-destructive font-medium';
  if (percent >= THIN_NEAR_FULL) return 'text-amber-600 font-medium';
  return '';
}

export function PoolItem({ pool, nodeName }: { pool: Pool; nodeName?: string }) {
  const total = Number(pool.totalGb);
  const free = Number(pool.freeGb);

  // A thin pool's capacity is not its volume group's capacity. Haify builds the
  // pool from every free extent, so vgFree is zero from the moment the pool
  // exists and stays there — a bar driven by it reads 100% full whether the
  // pool is empty or about to refuse writes, which is exactly what it did
  // while node-a was failing on 2026-08-09. When there is a thin pool, its own
  // utilisation is the only figure worth putting on the bar.
  const thin = pool.thinPoolLv
    ? {
        lv: pool.thinPoolLv,
        data: pool.thinDataPercent ?? 0,
        meta: pool.thinMetadataPercent ?? 0,
        outOfSpace: pool.thinOutOfSpace ?? false,
      }
    : null;

  // Absent thin figures, fall back to the volume group rather than drawing a
  // 0% bar for a pool that simply did not report — an older agent, or a group
  // that genuinely holds no thin pool.
  const usedPercent = thin
    ? thin.data
    : total > 0
      ? ((total - free) / total) * 100
      : 0;

  // The recorded type says "vg" for a group that was adopted or converted to
  // thin later; a thin pool LV in it is what makes it a thin pool.
  const shownType = thin && pool.type === 'vg' ? 'thin_pool' : pool.type;

  return (
    <div className="rounded-lg border bg-muted/40 p-3">
      <div className="mb-2 flex items-center justify-between">
        <span className="text-sm font-medium">{pool.name}</span>
        <div className="flex items-center gap-1">
          {thin?.outOfSpace && (
            <Badge variant="destructive">out of space</Badge>
          )}
          <Badge variant="outline" className={poolTypeBadgeClass(shownType)}>
            {poolTypeLabel(shownType)}
          </Badge>
          <AddDiskDialog pool={pool} nodeName={nodeName ?? pool.node} />
          <DeletePoolDialog pool={pool} />
        </div>
      </div>

      {thin ? (
        <>
          <div className="mb-1 flex justify-between text-xs text-muted-foreground">
            <span className={thinUsageClass(thin.data)}>
              {thin.data.toFixed(1)}% of pool used
            </span>
            <span>{total} GB total</span>
          </div>
          <Progress value={usedPercent} className="h-2" />
          {/* The volume group's own free space is still worth seeing — it is
              what an extension would draw on — but as a footnote, not as the
              headline health figure it used to be. */}
          <div className="mt-1 flex flex-wrap items-center gap-x-3 text-[0.65rem] text-muted-foreground">
            <span className="font-mono">{thin.lv}</span>
            <span className={thinUsageClass(thin.meta)}>
              metadata {thin.meta.toFixed(1)}%
            </span>
            <span>VG {free} GB unallocated</span>
            {pool.hasVdo && (
              <span>VDO {(pool.vdoPhysicalPercent ?? 0).toFixed(1)}% physical</span>
            )}
          </div>
          {thin.outOfSpace && (
            <p className="mt-1 text-[0.65rem] text-destructive">
              LVM reports this pool out of data space: writes are failing, and
              any DRBD replica on it will drop to Diskless.
            </p>
          )}
        </>
      ) : (
        <>
          <div className="mb-1 flex justify-between text-xs text-muted-foreground">
            <span>{free} GB free</span>
            <span>{total} GB total</span>
          </div>
          <Progress value={usedPercent} className="h-2" />
        </>
      )}

      {pool.cached && (
        <div className="mt-2 flex flex-wrap items-center gap-1 text-xs text-muted-foreground">
          <span>Cache:</span>
          <Badge
            variant="outline"
            className={
              pool.cacheMode === 'writeback'
                ? 'border-amber-500/50 text-amber-600'
                : undefined
            }
          >
            {pool.cacheMode}
          </Badge>
          <span className="font-mono text-[0.65rem]">{pool.cacheDevice}</span>
          <span>
            {pool.cacheHitPercent ?? 0}% hit &bull; {pool.cacheUsedPercent ?? 0}%
            used
          </span>
          {/* Dirty is the share of the cache that exists nowhere else on this
              node, so it is only worth surfacing when there is some. */}
          {(pool.cacheDirtyPercent ?? 0) > 0 && (
            <span className="text-amber-600">
              {pool.cacheDirtyPercent}% not yet on disk
            </span>
          )}
          {pool.cacheDegraded && (
            <Badge variant="destructive">cache degraded</Badge>
          )}
        </div>
      )}

      {pool.devices && pool.devices.length > 0 && (
        <div className="mt-2 flex flex-wrap items-center gap-1">
          <span className="text-xs text-muted-foreground">
            Devices ({pool.devices.length}):
          </span>
          {pool.devices.map((d) => (
            <Badge
              key={d}
              variant="secondary"
              className="font-mono text-[0.65rem]"
            >
              {d}
            </Badge>
          ))}
        </div>
      )}
    </div>
  );
}
