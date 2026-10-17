import { useEffect, useState } from 'react';
import { useIsFetching, useQuery, useQueryClient } from '@tanstack/react-query';
import { RefreshCw } from 'lucide-react';
import { api, poolRoomGiB } from '@/services/api';
import { eventTime } from '@/services/events';
import { cn } from '@/lib/utils';
import { toneOf, TONE_TEXT, type StatusTone } from '@/components/status';
import { useReducedMotion } from '@/components/flow/motion';
import { Button } from '@/components/ui/button';
import { ClusterHero, type HeroCallout } from './dashboard/ClusterHero';
import { BarsViz, GaugeViz, MetricTile, RingsViz } from './dashboard/MetricTile';
import { TodayTimeline } from './dashboard/TodayTimeline';
import { Insights } from './dashboard/Insights';

// Every query this page reads, in one place: Refresh has to invalidate
// exactly these keys, and one that drifts from its useQuery below produces a
// button that looks like it works and refreshes nothing.
const PAGE_QUERY_KEYS = {
  nodes: ['nodes'],
  pools: ['pools'],
  resources: ['resources'],
  gateways: ['gateways'],
  selfHa: ['selfha'],
  events: ['events', 'today'],
  schedules: ['snapshot-schedules'],
  inspection: ['inspection', 'latest'],
} as const;

/** GiB as the unit a person would say it in. */
function capacity(gib: number): string {
  if (gib >= 1024) return `${(gib / 1024).toFixed(gib >= 10240 ? 0 : 1)} TiB`;
  return `${Math.round(gib)} GiB`;
}

function greeting(now: number): string {
  const h = new Date(now).getHours();
  if (h < 5) return 'Good evening,';
  if (h < 12) return 'Good morning,';
  if (h < 18) return 'Good afternoon,';
  return 'Good evening,';
}

export function DashboardPage() {
  const queryClient = useQueryClient();
  const reduced = useReducedMotion();
  const nodes = useQuery({ queryKey: PAGE_QUERY_KEYS.nodes, queryFn: () => api.getNodes() });
  const pools = useQuery({ queryKey: PAGE_QUERY_KEYS.pools, queryFn: () => api.getPools() });
  const resources = useQuery({ queryKey: PAGE_QUERY_KEYS.resources, queryFn: () => api.getResources() });
  const gateways = useQuery({ queryKey: PAGE_QUERY_KEYS.gateways, queryFn: () => api.getGateways() });
  const selfHa = useQuery({ queryKey: PAGE_QUERY_KEYS.selfHa, queryFn: () => api.getSelfHaStatus() });
  const events = useQuery({ queryKey: PAGE_QUERY_KEYS.events, queryFn: () => api.listEvents({ limit: 200 }) });
  const schedules = useQuery({ queryKey: PAGE_QUERY_KEYS.schedules, queryFn: () => api.getSnapshotSchedules() });
  // A cluster that has never been inspected answers with an error; that is
  // an empty card, not something to retry.
  const inspection = useQuery({
    queryKey: PAGE_QUERY_KEYS.inspection,
    queryFn: () => api.getLatestInspection(),
    retry: false,
  });

  // "Now" for the timeline's marker and every "ago"; a tick, not a poll.
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 30_000);
    return () => clearInterval(id);
  }, []);

  const isFetching = useIsFetching();
  const refresh = () => {
    for (const queryKey of Object.values(PAGE_QUERY_KEYS)) queryClient.invalidateQueries({ queryKey });
  };

  // Nodes and the control plane.
  const nodeList = [...(nodes.data?.nodes ?? [])].sort((a, b) => a.name.localeCompare(b.name));
  const online = nodeList.filter((n) => n.state === 'online').length;
  const offline = nodeList.filter((n) => toneOf(n.state) === 'bad');
  const ha = selfHa.data;
  const activeName = ha?.activeNode
    ? (nodeList.find((n) => n.address === ha.activeNode)?.name ?? ha.activeNode)
    : '';

  // Capacity.
  const poolList = pools.data?.pools ?? [];
  const rooms = poolList.map(poolRoomGiB);
  const total = rooms.reduce((s, r) => s + r.total, 0);
  const free = rooms.reduce((s, r) => s + Math.max(0, Math.min(r.total, r.free)), 0);
  const usedPct = total > 0 ? ((total - free) / total) * 100 : 0;
  const poolFill = rooms.map((r) => (r.total > 0 ? 1 - Math.max(0, r.free) / r.total : 0));
  const fullest = poolFill.length ? poolFill.indexOf(Math.max(...poolFill)) : -1;

  // Resources.
  const resList = resources.data?.resources ?? [];
  const atRisk = resList.filter((r) => r.quorumRisk).length;
  const volumes = resList.reduce((s, r) => s + r.volumes.length, 0);
  const offsite = resList.filter((r) => r.drNode).length;

  // Gateways.
  const gwList = gateways.data?.gateways ?? [];
  const gwRunning = gwList.filter((g) => g.state === 'running' || g.state === 'started').length;
  const gwStopped = gwList.filter((g) => !(g.state === 'running' || g.state === 'started'));
  const gwTypes = [...new Set(gwList.map((g) => g.type.toUpperCase().replace('NVMEOF', 'NVMe-oF').replace('ISCSI', 'iSCSI')))];

  // Today's events.
  const evList = events.data?.events ?? [];
  const day0 = new Date(now).setHours(0, 0, 0, 0);
  const todays = evList.filter((e) => {
    const t = eventTime(e);
    return t !== null && t >= day0;
  });
  const perHour = Array.from({ length: 24 }, () => 0);
  for (const e of todays) perHour[new Date(eventTime(e)!).getHours()]++;
  const peak = Math.max(1, ...perHour);
  const critical = todays.filter((e) => e.severity === 'critical').length;
  const warnings = todays.filter((e) => e.severity === 'warning').length;

  // Snapshots.
  const schedList = (schedules.data?.schedules ?? []).filter((s) => s.enabled);
  const covered = new Set(schedList.map((s) => s.resource)).size;
  const nextRun = schedList
    .map((s) => (s.nextRun ? Date.parse(s.nextRun) : NaN))
    .filter((t) => Number.isFinite(t) && t > now)
    .sort((a, b) => a - b)[0];

  // The latest inspection.
  const report = inspection.data?.report;
  const sum = report?.summary;
  const checks = (sum?.pass ?? 0) + (sum?.warn ?? 0) + (sum?.fail ?? 0) + (sum?.error ?? 0);
  const problems = (sum?.warn ?? 0) + (sum?.fail ?? 0) + (sum?.error ?? 0);

  const toLookAt = offline.length + atRisk + gwStopped.length + (sum?.fail ?? 0) + (sum?.error ?? 0);

  const capTone: StatusTone = usedPct >= 90 ? 'bad' : usedPct >= 80 ? 'warn' : 'ok';
  const callouts: [HeroCallout, HeroCallout, HeroCallout] = [
    {
      label: 'Control plane',
      value: !ha ? '—' : !ha.enabled ? 'standalone' : activeName || 'none',
      status: !ha ? '' : !ha.enabled ? 'Self-HA off' : activeName ? 'Self-HA' : 'Moving',
      tone: !ha || !ha.enabled ? 'idle' : activeName ? 'ok' : 'bad',
    },
    {
      label: 'Capacity',
      value: total > 0 ? usedPct.toFixed(0) : '—',
      unit: '% used',
      status: total > 0 ? `${capacity(free)} free` : pools.isLoading ? '' : 'No pools',
      tone: capTone,
    },
    {
      label: 'Nodes',
      value: String(online),
      unit: `/ ${nodeList.length} online`,
      status: offline.length ? `${offline.length} offline` : 'All online',
      tone: offline.length ? 'bad' : 'ok',
    },
  ];

  return (
    <div className="mx-auto max-w-[1440px]">
      <div className="mb-5 flex flex-wrap items-end justify-between gap-3">
        <div>
          <div className="text-[22px] leading-tight font-light text-foreground/80">{greeting(now)}</div>
          <h1 className="text-[36px] leading-tight font-semibold tracking-tight">Haify.</h1>
          <p className="mt-1 text-[14px] text-muted-foreground">Here&apos;s your storage today.</p>
        </div>
        <div className="flex items-center gap-3">
          <span className="text-[13px] text-muted-foreground">
            {new Date(now).toLocaleDateString(undefined, { weekday: 'short', month: 'short', day: 'numeric', year: 'numeric' })}
          </span>
          <Button variant="outline" size="sm" onClick={refresh} disabled={isFetching > 0} className="rounded-full">
            <RefreshCw className={cn(isFetching > 0 && 'animate-spin')} />
            Refresh
          </Button>
        </div>
      </div>

      <div className="grid grid-cols-1 gap-4 xl:grid-cols-12">
        <section
          className="hidden flex-col self-start xl:sticky xl:top-0 xl:col-span-5 xl:flex 2xl:col-span-4"
          aria-label="Cluster"
        >
          <ClusterHero
            nodes={nodeList.map((n) => ({ name: n.name, state: n.state, active: n.name === activeName }))}
            callouts={callouts}
            reduced={reduced}
          />
        </section>

        {/* Below xl the drawing has no room; its three readings stay. */}
        <div className="grid grid-cols-1 gap-3 sm:grid-cols-3 xl:hidden">
          {callouts.map((c) => (
            <div key={c.label} className="tile p-3">
              <div className="text-[11.5px] text-muted-foreground">{c.label}</div>
              <div className="mt-0.5 truncate text-[18px] font-semibold tabular-nums">
                {c.value}
                {c.unit && <span className="ml-1 text-[11px] font-normal text-muted-foreground">{c.unit}</span>}
              </div>
              <div className={cn('truncate text-[11px]', c.tone === 'ok' ? 'text-muted-foreground' : TONE_TEXT[c.tone])}>
                {c.status}
              </div>
            </div>
          ))}
        </div>

        <div className="flex flex-col gap-4 xl:col-span-7 2xl:col-span-8">
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-2 2xl:grid-cols-3">
            <MetricTile
              title="Resources"
              href="/resources"
              tint="mint"
              value={resList.length}
              unit="DRBD resources"
              loading={resources.isLoading}
              viz={<RingsViz fraction={resList.length ? (resList.length - atRisk) / resList.length : 0} />}
              footLeft={atRisk ? `${atRisk} at quorum risk` : 'All hold quorum'}
              footRight={`${volumes} volumes`}
            />
            <MetricTile
              title="Inspection"
              href="/notifications"
              tint="peach"
              value={report ? (sum?.pass ?? 0) : '—'}
              unit={report ? `of ${checks} checks passed` : 'not run yet'}
              loading={inspection.isLoading}
              viz={report ? <GaugeViz fraction={checks ? (sum?.pass ?? 0) / checks : 0} color="var(--status-warn)" /> : undefined}
              footLeft={report ? `${problems} to look at` : undefined}
              footRight={
                report?.finishedAtUnixMs
                  ? new Date(Number(report.finishedAtUnixMs)).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', hour12: false })
                  : undefined
              }
            />
            <MetricTile
              title="Pools"
              href="/pools"
              value={poolList.length}
              unit={total > 0 ? `${capacity(total)} in all` : 'storage pools'}
              loading={pools.isLoading}
              viz={poolFill.length ? <BarsViz values={poolFill} highlight={fullest} /> : undefined}
              footLeft={fullest >= 0 ? 'Fullest pool' : undefined}
              footRight={fullest >= 0 ? `${Math.round(poolFill[fullest] * 100)}%` : undefined}
            />
            <MetricTile
              title="Gateways"
              href="/gateways"
              value={gwRunning}
              unit={gwList.length ? `of ${gwList.length} serving` : 'no gateways'}
              loading={gateways.isLoading}
              footLeft={gwStopped.length ? `${gwStopped.length} stopped` : gwTypes.join(' · ') || undefined}
            />
            <MetricTile
              title="Events today"
              href="/notifications"
              tint="rose"
              value={todays.length}
              unit="since midnight"
              loading={events.isLoading}
              viz={<BarsViz values={perHour.map((c) => c / peak)} highlight={new Date(now).getHours()} />}
              footLeft={critical ? `${critical} critical` : warnings ? `${warnings} warnings` : 'No warnings'}
            />
            <MetricTile
              title="Snapshots"
              href="/resources"
              value={schedList.length}
              unit={`schedule${schedList.length === 1 ? '' : 's'}`}
              loading={schedules.isLoading}
              footLeft={
                <span className="flex items-center gap-2">
                  <span className="relative h-1.5 w-24 overflow-hidden rounded-full bg-secondary">
                    <span
                      className="absolute inset-y-0 left-0 rounded-full bg-status-ok"
                      style={{ width: `${resList.length ? (covered / resList.length) * 100 : 0}%` }}
                    />
                  </span>
                  {covered}/{resList.length}
                </span>
              }
              footRight={
                nextRun ? `next ${new Date(nextRun).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', hour12: false })}` : undefined
              }
            />
          </div>

          {/* Side by side wherever the column is wide: below xl it is the
              page's full width, at 2xl the wider of the two columns. */}
          <div className="grid grid-cols-1 gap-4 lg:grid-cols-12 xl:grid-cols-1 2xl:grid-cols-12">
            <div className="lg:col-span-7 xl:col-span-1 2xl:col-span-7">
              <TodayTimeline now={now} events={evList} schedules={schedList} inspection={report} />
            </div>
            <div className="lg:col-span-5 xl:col-span-1 2xl:col-span-5">
              <Insights now={now} report={report} events={evList} loading={inspection.isLoading} />
            </div>
          </div>
        </div>
      </div>
    </div>
  );
}
