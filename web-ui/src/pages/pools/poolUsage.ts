import { num, type Pool } from '@/services/api';

/** How full a pool is, in percent: its thin pool's data when it has one, the
 *  volume group's allocation otherwise. */
export function usedPercent(p: Pool): number {
  if (p.thinPoolLv) return p.thinDataPercent ?? 0;
  const total = Number(p.totalGb);
  return total > 0 ? ((total - Number(p.freeGb)) / total) * 100 : 0;
}

/** The bytes a new volume can draw on, and in all. */
export function poolBytes(p: Pool): { capacity: number; available: number } {
  const capacity = num(p.capacityBytes) || Number(p.totalGb) * 1024 ** 3;
  const available = num(p.availableBytes) || Number(p.freeGb) * 1024 ** 3;
  return { capacity, available };
}

export function fullnessColor(pct: number): string {
  return pct >= 90 ? 'var(--status-bad)' : pct >= 80 ? 'var(--status-warn)' : 'var(--primary)';
}
