import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { api, type HistoryRange } from '@/services/api';
import { Kpis } from './dashboard/Kpis';
import {
  CapacityCard,
  EventsByHourCard,
  PoolFullnessCard,
  ResourceStateCard,
  ThroughputCard,
} from './dashboard/ClusterCharts';
import { StatsBlock } from './dashboard/StatsBlock';
import { StatSections } from './dashboard/StatSections';

// The cluster in numbers. Everything on the page is a count or a total the
// controller computes, so it reads the same at five nodes or five hundred and
// costs one small request per panel however many resources there are.

const RANGE_KEY = 'haify.dashboard.range';

function readRange(): HistoryRange {
  try {
    const v = localStorage.getItem(RANGE_KEY);
    if (v === '1h' || v === '6h' || v === '24h' || v === '7d' || v === '30d') return v;
  } catch {
    /* default below */
  }
  return '24h';
}

export function DashboardPage() {
  const [range, setRange] = useState<HistoryRange>(readRange);
  const live = { meta: { live: true }, refetchInterval: 15_000 } as const;

  const summary = useQuery({ queryKey: ['cluster-summary'], queryFn: () => api.getClusterSummary(), ...live });
  const day = useQuery({ queryKey: ['cluster-history', '24h'], queryFn: () => api.getClusterHistory('24h'), ...live });
  const quarter = useQuery({
    queryKey: ['cluster-history', '90d'],
    queryFn: () => api.getClusterHistory('90d'),
    meta: { live: true },
    refetchInterval: 600_000,
  });
  const ranged = useQuery({
    queryKey: ['cluster-history', range],
    queryFn: () => api.getClusterHistory(range),
    meta: { live: true },
    refetchInterval: range === '1h' || range === '6h' ? 15_000 : 120_000,
  });
  const stats = useQuery({
    queryKey: ['cluster-stats', range],
    queryFn: () => api.getClusterStats(range),
    meta: { live: true },
    refetchInterval: range === '1h' ? 30_000 : 120_000,
  });

  const s = summary.data;
  const pickRange = (r: HistoryRange) => {
    setRange(r);
    try {
      localStorage.setItem(RANGE_KEY, r);
    } catch {
      /* remembered for this visit only */
    }
  };

  return (
    <div className="mx-auto max-w-[1600px]">
      <h1 className="sr-only">Dashboard</h1>
      <Kpis s={s} loading={summary.isLoading} />

      <div className="mt-4 grid grid-cols-1 gap-4 xl:grid-cols-[2fr_1fr]">
        <ThroughputCard samples={day.data?.samples ?? []} s={s} />
        <CapacityCard samples={quarter.data?.samples ?? []} s={s} />
      </div>

      <div className="mt-4 grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
        <EventsByHourCard s={s} />
        <PoolFullnessCard s={s} />
        <ResourceStateCard s={s} />
      </div>

      <StatsBlock range={range} onRange={pickRange} stats={stats.data} samples={ranged.data?.samples ?? []} />

      <StatSections s={s} />
    </div>
  );
}
