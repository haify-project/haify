import { LineChart } from '@/components/charts/LineChart';
import { fmtBytes, fmtCount, fmtMs, fmtPct, fmtRate } from '@/components/charts/chartKit';
import { cn } from '@/lib/utils';
import { num, type ClusterSample, type ClusterStats, type HistoryRange, type StatSeries } from '@/services/api';
import { ChartCard, ChartEmpty } from './ChartCard';

// "Stats": the nodes' own statistics (CPU, memory, disk latency, network)
// when the controller can read them, and the controller's out-of-sync and
// API figures, all over one chosen range.

export const STAT_RANGES: HistoryRange[] = ['1h', '6h', '24h', '7d', '30d'];

export function RangePicker({ value, onChange }: { value: HistoryRange; onChange: (r: HistoryRange) => void }) {
  return (
    <div role="group" aria-label="Time range" className="flex gap-0.5 rounded-lg bg-secondary p-[3px]">
      {STAT_RANGES.map((r) => (
        <button
          key={r}
          type="button"
          aria-pressed={value === r}
          onClick={() => onChange(r)}
          className={cn(
            'rounded-md px-2.5 py-1 text-[12px] transition-colors',
            value === r ? 'bg-card font-medium text-foreground shadow-sm' : 'text-foreground/70 hover:text-foreground',
          )}
        >
          {r}
        </button>
      ))}
    </div>
  );
}

function BandCard({
  title,
  series,
  format,
  word,
  busiestWord,
  color,
  max,
  threshold,
  total,
}: {
  title: string;
  series?: StatSeries;
  format: (v: number) => string;
  word: string;
  busiestWord: string;
  color: string;
  max?: number;
  threshold?: number;
  /** Show the sum over nodes rather than the average. */
  total?: boolean;
}) {
  const times = (series?.unix ?? []).map(num);
  const line = (total ? series?.sum : series?.avg) ?? [];
  return (
    <ChartCard
      title={title}
      value={series ? format(total ? num(series.nowSum) : num(series.nowAvg)) : '–'}
      unit={word}
      meta={series?.busiest?.node ? `${busiestWord} ${series.busiest.node} ${format(num(series.busiest.value))}` : undefined}
    >
      {times.length < 2 ? (
        <ChartEmpty height={124}>No readings in this range.</ChartEmpty>
      ) : (
        <LineChart
          ariaLabel={`${title} across nodes`}
          height={124}
          times={times}
          format={format}
          max={max}
          threshold={threshold !== undefined ? { value: threshold, color: 'var(--status-warn)' } : undefined}
          band={total ? undefined : { lo: (series?.min ?? []).map(num), hi: (series?.max ?? []).map(num), color }}
          series={[{ name: total ? 'All nodes' : 'Average', values: line.map(num), color, area: total }]}
        />
      )}
    </ChartCard>
  );
}

export function StatsBlock({
  range,
  onRange,
  stats,
  samples,
}: {
  range: HistoryRange;
  onRange: (r: HistoryRange) => void;
  stats?: ClusterStats;
  samples: ClusterSample[];
}) {
  const times = samples.map((x) => num(x.unix));
  const oos = samples.map((x) => num(x.outOfSyncBytes));
  const req = samples.map((x) => num(x.apiRequestsPerSec));
  const err = samples.map((x) => num(x.apiErrorsPerSec));
  const nodes = stats?.available ? num(stats.nodes) : 0;
  const slowest = stats?.slowestDisks ?? [];
  const slowMax = Math.max(...slowest.map((d) => num(d.value)), 0.000001);
  const lastReq = req.length ? req[req.length - 1] : 0;
  const lastOos = oos.length ? oos[oos.length - 1] : 0;

  return (
    <section aria-labelledby="stats-heading" className="mt-7">
      <div className="flex items-center justify-between gap-3">
        <h2 id="stats-heading" className="text-[15px] font-semibold">
          Stats
          {nodes > 0 && <span className="ml-2.5 text-[12px] font-normal text-muted-foreground">{fmtCount(nodes)} nodes</span>}
        </h2>
        <RangePicker value={range} onChange={onRange} />
      </div>
      {stats?.available && (
        <div className="mt-3 grid grid-cols-1 gap-4 sm:grid-cols-2 2xl:grid-cols-4">
          <BandCard title="CPU" series={stats.cpu} format={fmtPct} word="average" busiestWord="busiest node" color="var(--primary)" max={1} />
          <BandCard title="Memory" series={stats.memory} format={fmtPct} word="average" busiestWord="busiest node" color="oklch(0.55 0.13 290)" max={1} threshold={0.9} />
          <BandCard
            title="Disk write latency"
            series={stats.diskWriteLatency}
            format={fmtMs}
            word="average"
            busiestWord="slowest node"
            color="oklch(0.62 0.13 62)"
            threshold={0.005}
          />
          <BandCard title="Network sent" series={stats.network} format={fmtRate} word="all nodes" busiestWord="busiest node" color="var(--status-ok)" total />
        </div>
      )}
      <div className={cn('mt-4 grid grid-cols-1 gap-4', stats?.available ? 'lg:grid-cols-3' : 'lg:grid-cols-2')}>
        <ChartCard title="Out of sync" value={fmtBytes(lastOos)} unit="now">
          {times.length < 2 ? (
            <ChartEmpty height={130}>No readings in this range yet.</ChartEmpty>
          ) : (
            <LineChart ariaLabel="Data out of sync over time" height={130} times={times} format={(v) => fmtBytes(v, 0)} floor={1e9}
              series={[{ name: 'Out of sync', values: oos, color: 'var(--status-warn)', area: true }]} />
          )}
        </ChartCard>
        <ChartCard
          title="Controller API"
          value={`${lastReq < 10 ? lastReq.toFixed(1) : Math.round(lastReq)}/s`}
          unit="requests"
          legend={[
            { name: 'Requests', color: 'var(--primary)' },
            { name: 'Errors', color: 'var(--status-bad)' },
          ]}
        >
          {times.length < 2 ? (
            <ChartEmpty height={130}>No readings in this range yet.</ChartEmpty>
          ) : (
            <LineChart ariaLabel="Controller API requests and errors per second" height={130} times={times} floor={1}
              format={(v) => `${v < 10 && v > 0 ? v.toFixed(1) : Math.round(v)}/s`}
              series={[
                { name: 'Requests', values: req, color: 'var(--primary)' },
                { name: 'Errors', values: err, color: 'var(--status-bad)', bars: true },
              ]} />
          )}
        </ChartCard>
        {stats?.available && (
          <ChartCard
            title="Slowest disks now"
            value={fmtCount(slowest.filter((d) => num(d.value) > 0.005).length)}
            unit={`of ${fmtCount(nodes)} nodes above 5 ms`}
          >
            <div className="flex flex-col gap-2.5 pt-1">
              {slowest.length === 0 && <ChartEmpty height={110}>No disk readings.</ChartEmpty>}
              {slowest.map((d) => {
                const v = num(d.value);
                return (
                  <div key={d.node} className="grid grid-cols-[56px_1fr_64px] items-center gap-3 text-[12.5px]">
                    <span className="truncate">{d.node}</span>
                    <span className="h-2 overflow-hidden rounded-full bg-secondary">
                      <span
                        className="block h-full rounded-full"
                        style={{
                          width: `${(v / slowMax) * 100}%`,
                          background: v > 0.01 ? 'var(--status-bad)' : v > 0.005 ? 'var(--status-warn)' : 'var(--status-idle)',
                        }}
                      />
                    </span>
                    <b className="text-right font-medium tabular-nums">{fmtMs(v)}</b>
                  </div>
                );
              })}
            </div>
          </ChartCard>
        )}
      </div>
    </section>
  );
}
