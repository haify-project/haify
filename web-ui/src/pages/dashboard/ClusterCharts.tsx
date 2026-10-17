import { LineChart } from '@/components/charts/LineChart';
import { Donut, FullnessHistogram, StackedBars } from '@/components/charts/BarCharts';
import { fmtCount, fmtRate, fmtTick } from '@/components/charts/chartKit';
import { num, type ClusterSample, type ClusterSummary } from '@/services/api';
import { ChartCard, ChartEmpty } from './ChartCard';

// The two rows of charts under the numbers: throughput and capacity over
// time, then today's events, pools by fullness and resource states.

const WRITE = 'var(--primary)';
const READ = 'var(--status-idle)';
const CAP = 'oklch(0.66 0.13 62)';

export function ThroughputCard({ samples, s }: { samples: ClusterSample[]; s?: ClusterSummary }) {
  const times = samples.map((x) => num(x.unix));
  const now = s?.replication?.writeBytesPerSec;
  return (
    <ChartCard
      title="Throughput, last 24 hours"
      value={now !== undefined ? fmtRate(now) : '–'}
      unit="writes now"
      legend={[
        { name: 'Writes', color: WRITE },
        { name: 'Reads', color: READ },
      ]}
    >
      {samples.length < 2 ? (
        <ChartEmpty height={200}>Throughput is charted from the next few health checks on.</ChartEmpty>
      ) : (
        <LineChart
          ariaLabel="Write and read throughput over the last 24 hours"
          height={200}
          times={times}
          format={fmtRate}
          floor={1e6}
          series={[
            { name: 'Reads', values: samples.map((x) => num(x.readBytesPerSec)), color: READ, width: 1.6 },
            { name: 'Writes', values: samples.map((x) => num(x.writeBytesPerSec)), color: WRITE, area: true },
          ]}
        />
      )}
    </ChartCard>
  );
}

/** Days until the pools are full at the last 30 days' rate, or null. */
function daysToFull(samples: ClusterSample[]): { slope: number; days: number | null } {
  const pts = samples
    .map((x) => ({ t: num(x.unix), p: num(x.capacityTotalBytes) ? num(x.capacityUsedBytes) / num(x.capacityTotalBytes) : NaN }))
    .filter((x) => Number.isFinite(x.p));
  if (pts.length < 2) return { slope: 0, days: null };
  const from = pts[pts.length - 1].t - 30 * 86400;
  const recent = pts.filter((x) => x.t >= from);
  const n = recent.length;
  const mt = recent.reduce((a, x) => a + x.t, 0) / n;
  const mp = recent.reduce((a, x) => a + x.p, 0) / n;
  const cov = recent.reduce((a, x) => a + (x.t - mt) * (x.p - mp), 0);
  const vt = recent.reduce((a, x) => a + (x.t - mt) ** 2, 0);
  const slope = vt ? (cov / vt) * 86400 : 0; // fraction per day
  const last = recent[n - 1].p;
  return { slope, days: slope > 0 ? (1 - last) / slope : null };
}

export function CapacityCard({ samples, s }: { samples: ClusterSample[]; s?: ClusterSummary }) {
  const used = num(s?.capacity?.usedBytes);
  const total = num(s?.capacity?.totalBytes);
  const pct = samples.map((x) => (num(x.capacityTotalBytes) ? (num(x.capacityUsedBytes) / num(x.capacityTotalBytes)) * 100 : NaN));
  const { slope, days } = daysToFull(samples);
  const n = pct.length;
  // The projection runs a third past the end of the history, at the recent rate.
  const ahead = Math.max(Math.round(n / 3), 1);
  const step = n > 1 ? (num(samples[n - 1].unix) - num(samples[0].unix)) / (n - 1) : 0;
  const last = pct[n - 1];
  const projected = [
    ...Array(Math.max(n - 1, 0)).fill(NaN),
    last,
    ...Array.from({ length: ahead }, (_, k) => Math.min(last + slope * 100 * (((k + 1) * step) / 86400), 100)),
  ];
  const times = [...samples.map((x) => num(x.unix)), ...Array.from({ length: ahead }, (_, k) => num(samples[n - 1]?.unix) + (k + 1) * step)];
  const meta =
    days === null
      ? 'not growing'
      : days > 3 * 365
        ? 'growing slowly'
        : `full around ${new Date(Date.now() + days * 86400_000).toLocaleDateString([], { month: 'short', year: 'numeric' })}`;
  return (
    <ChartCard title="Capacity used, last 90 days" value={total ? `${Math.round((used / total) * 100)}%` : '–'} unit={total ? meta : undefined}>
      {n < 2 ? (
        <ChartEmpty height={200}>Capacity is charted from the next few health checks on.</ChartEmpty>
      ) : (
        <LineChart
          ariaLabel="Capacity used over the last 90 days, with the trend ahead"
          height={200}
          times={times}
          max={100}
          format={(v) => `${Math.round(v)}%`}
          threshold={{ value: 90, color: 'var(--status-warn)' }}
          xLabels={[
            { i: 0, text: fmtTick(times[0], 3 * 86400) },
            { i: n - 1, text: 'today' },
            { i: times.length - 1, text: 'trend' },
          ]}
          series={[
            { name: 'Used', values: [...pct, ...Array(ahead).fill(NaN)], color: CAP, area: true },
            { name: 'Trend', values: projected, color: CAP, width: 1.6, dashed: true },
          ]}
        />
      )}
    </ChartCard>
  );
}

export function EventsByHourCard({ s }: { s?: ClusterSummary }) {
  const hours = s?.eventsByHour ?? [];
  const nowHour = new Date().getHours();
  return (
    <ChartCard
      title="Events by hour, today"
      value={fmtCount(num(s?.today?.events))}
      legend={[
        { name: 'Critical', color: 'var(--status-bad)' },
        { name: 'Warning', color: 'var(--status-warn)' },
        { name: 'Info', color: 'var(--status-idle)' },
      ]}
    >
      <StackedBars
        ariaLabel="Events per hour today by severity"
        slots={Array.from({ length: 24 }, (_, h) => [num(hours[h]?.info), num(hours[h]?.warning), num(hours[h]?.critical)])}
        colors={['var(--status-idle)', 'var(--status-warn)', 'var(--status-bad)']}
        labels={['00:00', '06:00', '12:00', '18:00', '24:00']}
        emptyFrom={nowHour + 1}
      />
    </ChartCard>
  );
}

export function PoolFullnessCard({ s }: { s?: ClusterSummary }) {
  const buckets = s?.poolFullness?.length === 10 ? s.poolFullness.map(num) : Array(10).fill(0);
  return (
    <ChartCard title="Pools by how full they are" value={fmtCount(num(s?.capacity?.pools))} unit="pools">
      <FullnessHistogram buckets={buckets} />
    </ChartCard>
  );
}

export function ResourceStateCard({ s }: { s?: ClusterSummary }) {
  const r = s?.resources ?? {};
  const total = num(r.total);
  const rows = [
    { name: 'Healthy', value: num(r.healthy), color: 'var(--status-ok)' },
    { name: 'Resyncing', value: num(r.resyncing), color: 'var(--primary)' },
    { name: 'Degraded', value: num(r.degraded), color: 'var(--status-warn)' },
    { name: 'Without quorum', value: num(r.noQuorum), color: 'var(--status-bad)' },
    ...(num(r.unknown) ? [{ name: 'No status', value: num(r.unknown), color: 'var(--status-idle)' }] : []),
  ];
  const healthyPct = total ? (num(r.healthy) / total) * 100 : 0;
  return (
    <ChartCard
      title="Resource state"
      value={total ? `${healthyPct >= 99.95 || healthyPct === 0 ? Math.round(healthyPct) : healthyPct.toFixed(1)}%` : '–'}
      unit={total ? 'healthy' : undefined}
    >
      <div className="flex items-center gap-5">
        <Donut parts={rows} center={fmtCount(total)} caption="resources" />
        <div className="flex min-w-0 flex-1 flex-col gap-2 text-[13px]">
          {rows.map((x) => (
            <div key={x.name} className="flex items-center justify-between gap-2">
              <span className="flex min-w-0 items-center gap-2 truncate">
                <span className="size-2 shrink-0 rounded-[2px]" style={{ background: x.color }} />
                {x.name}
              </span>
              <b className="font-semibold tabular-nums">{fmtCount(x.value)}</b>
            </div>
          ))}
        </div>
      </div>
    </ChartCard>
  );
}
