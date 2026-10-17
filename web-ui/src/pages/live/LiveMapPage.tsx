import { useEffect, useMemo, useState } from 'react';
import { Link, useSearchParams } from 'react-router';
import { useQueries, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '@/services/api';
import { subscribeEvents, type ClusterEvent } from '@/services/events';
import { cn } from '@/lib/utils';
import { PageHeader } from '@/components/PageHeader';
import { EventRows } from '@/components/EventRows';
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card';
import { Skeleton } from '@/components/ui/skeleton';
import { resourceFlows, type ResourceFlows } from '@/components/flow/model';
import { computeRates, formatRate } from '@/components/flow/rates';
import { useReducedMotion } from '@/components/flow/motion';
import { LIVE_META, LIVE_POLL_MS } from '@/components/flow/useLiveStatus';
import { Legend } from '@/components/topology/NodeCard';
import { SegmentedFilter } from '@/components/SegmentedFilter';
import { buildClusterMap } from './clusterModel';
import { ClusterMap } from './ClusterMap';
import { buildResourceCard, sortCards, usersOf } from './resourceModel';
import { ResourceTile } from './ResourceTile';

type View = 'nodes' | 'resources';

const LIST_POLL_MS = 10_000;

export function LiveMapPage() {
  const qc = useQueryClient();
  const reduced = useReducedMotion();
  const [params, setParams] = useSearchParams();
  const view: View = params.get('view') === 'resources' ? 'resources' : 'nodes';
  const setView = (v: View) =>
    setParams(
      (p) => {
        if (v === 'resources') p.set('view', v);
        else p.delete('view');
        return p;
      },
      { replace: true },
    );

  const nodes = useQuery({ queryKey: ['nodes'], queryFn: () => api.getNodes(), refetchInterval: LIST_POLL_MS });
  const pools = useQuery({ queryKey: ['pools'], queryFn: () => api.getPools(), refetchInterval: LIST_POLL_MS * 3 });
  const resources = useQuery({
    queryKey: ['resources'],
    queryFn: () => api.getResources(),
    refetchInterval: LIST_POLL_MS,
  });
  const list = useMemo(() => resources.data?.resources ?? [], [resources.data]);

  // Each status read runs drbdsetup on a node, so a large cluster is polled
  // proportionally slower.
  const interval = Math.max(LIVE_POLL_MS, list.length * 250);
  const statusQueries = useQueries({
    queries: list.map((r) => ({
      queryKey: ['resource-status', r.name],
      queryFn: () => api.resourceStatus(r.name),
      refetchInterval: interval,
      meta: LIVE_META,
    })),
  });
  // When each status last changed, answered or failed: the memos below key on it.
  const stamp = statusQueries.map((q) => `${q.dataUpdatedAt}:${q.status}`).join(',');

  // What uses each resource, for the resource view.
  const gateways = useQuery({
    queryKey: ['gateways'],
    queryFn: () => api.getGateways(),
    refetchInterval: LIST_POLL_MS * 3,
    enabled: view === 'resources',
  });
  const haConfigs = useQuery({
    queryKey: ['ha'],
    queryFn: () => api.getHaConfigs(),
    refetchInterval: LIST_POLL_MS * 3,
    enabled: view === 'resources',
  });

  const flows = useMemo(() => {
    const out = new Map<string, ResourceFlows>();
    list.forEach((r, i) => {
      const q = statusQueries[i];
      const status = q?.data?.status;
      const rates = computeRates(JSON.stringify(['resource-status', r.name]), status, q?.dataUpdatedAt ?? 0);
      out.set(r.name, resourceFlows(r, status, rates));
    });
    return out;
    // statusQueries is a new array every render; stamp says when it changed.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [list, stamp]);

  const map = useMemo(
    () => buildClusterMap(nodes.data?.nodes ?? [], pools.data?.pools ?? [], list, flows),
    [nodes.data, pools.data, list, flows],
  );

  const cards = useMemo(() => {
    if (view !== 'resources') return [];
    const gws = gateways.data?.gateways ?? [];
    const ha = haConfigs.data?.configs ?? [];
    return sortCards(
      list.map((r, i) =>
        buildResourceCard(
          r,
          statusQueries[i]?.data?.status,
          flows.get(r.name),
          usersOf(r, gws, ha),
          statusQueries[i]?.isPending ?? true,
        ),
      ),
    );
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [view, list, flows, stamp, gateways.data, haConfigs.data]);

  // The latest events: the history to start with, then the live stream.
  const recent = useQuery({ queryKey: ['events', 'live'], queryFn: () => api.listEvents({ limit: 5 }) });
  const [streamed, setStreamed] = useState<ClusterEvent[]>([]);
  const [connected, setConnected] = useState(false);
  const [streamOff, setStreamOff] = useState(false);
  useEffect(
    () =>
      subscribeEvents({
        onStatus: setConnected,
        onDisabled: () => setStreamOff(true),
        onEvent: (e) => {
          setStreamed((prev) => [e, ...prev.filter((x) => x.id !== e.id)].slice(0, 5));
          if (e.resource) qc.invalidateQueries({ queryKey: ['resource-status', e.resource] });
          qc.invalidateQueries({ queryKey: ['nodes'] });
        },
      }),
    [qc],
  );
  const events = useMemo(() => {
    const seen = new Set(streamed.map((e) => e.id));
    const history = [...(recent.data?.events ?? [])].reverse().filter((e) => !seen.has(e.id));
    return [...streamed, ...history].slice(0, 5);
  }, [streamed, recent.data]);

  const online = map.nodes.filter((n) => n.tone === 'ok').length;
  const totalWrites = map.nodes.reduce<number | undefined>(
    (s, n) => (n.writeRate === undefined ? s : (s ?? 0) + n.writeRate),
    undefined,
  );
  const hasWAN = map.links.some((l) => l.kind === 'wan');
  const loading = nodes.isLoading || resources.isLoading;

  return (
    <div className="min-w-0">
      <PageHeader
        title="Live map"
        description={
          <span className="flex flex-wrap items-center gap-x-2 gap-y-1">
            <span>
              {online}/{map.nodes.length} nodes online · {list.length} resource{list.length === 1 ? '' : 's'}
            </span>
            {totalWrites !== undefined && (
              <span className="font-mono tabular-nums text-foreground">writes {formatRate(totalWrites)}</span>
            )}
          </span>
        }
        actions={
          !streamOff && (
            <span className="flex items-center gap-1.5 text-[12px] text-muted-foreground">
              <i
                aria-hidden
                className={cn('size-1.5 rounded-full', connected ? 'animate-pulse bg-status-ok' : 'bg-status-idle')}
              />
              {connected ? 'live' : 'reconnecting'}
            </span>
          )
        }
      />

      <Card className="gap-3 py-4">
        <CardContent className="px-4 sm:px-5">
          <SegmentedFilter
            aria-label="Show"
            className="mb-3"
            value={view}
            onChange={setView}
            options={[
              { value: 'nodes', label: 'Nodes' },
              { value: 'resources', label: 'Resources', count: list.length },
            ]}
          />
          <Legend
            protocol=""
            hasWAN={hasWAN}
            hasTiebreaker={
              view === 'nodes'
                ? map.links.some((l) => l.kind === 'tiebreaker')
                : cards.some((c) => c.peers.some((p) => p.kind === 'tiebreaker'))
            }
            hasClient={
              view === 'nodes'
                ? map.links.some((l) => l.kind === 'client')
                : cards.some((c) => c.centre?.kind === 'client' || c.peers.some((p) => p.kind === 'client'))
            }
          />
          {loading ? (
            <Skeleton className="h-[320px] w-full" />
          ) : map.nodes.length === 0 ? (
            <p className="py-16 text-center text-[13px] text-muted-foreground">
              No nodes yet.{' '}
              <Link to="/nodes" className="font-medium text-primary hover:underline">
                Go to Nodes
              </Link>
            </p>
          ) : view === 'nodes' ? (
            <ClusterMap nodes={map.nodes} links={map.links} reduced={reduced} />
          ) : cards.length === 0 ? (
            <p className="py-16 text-center text-[13px] text-muted-foreground">
              No resources yet.{' '}
              <Link to="/resources" className="font-medium text-primary hover:underline">
                Go to Resources
              </Link>
            </p>
          ) : (
            <div className="grid grid-cols-1 gap-3 md:grid-cols-2 xl:grid-cols-3">
              {cards.map((c) => (
                <ResourceTile key={c.name} card={c} reduced={reduced} />
              ))}
            </div>
          )}
        </CardContent>
      </Card>

      <Card className="mt-6 gap-4 py-[18px]">
        <CardHeader className="flex flex-row items-center justify-between px-5">
          <CardTitle>Latest events</CardTitle>
          <Link to="/notifications" className="text-[12.5px] text-primary hover:underline">
            All events
          </Link>
        </CardHeader>
        <CardContent className="px-5">
          {recent.isLoading ? (
            <Skeleton className="h-9 w-full" />
          ) : events.length === 0 ? (
            <p className="text-sm text-muted-foreground">No events yet.</p>
          ) : (
            <EventRows events={events} />
          )}
        </CardContent>
      </Card>
    </div>
  );
}
