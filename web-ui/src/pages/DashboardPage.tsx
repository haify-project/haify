import { useEffect, useState } from 'react';
import { useIsFetching, useQuery, useQueryClient } from '@tanstack/react-query';
import { Link } from 'react-router';
import { Plus, RefreshCw, Server, ShieldCheck } from 'lucide-react';
import { api, poolRoomGiB } from '@/services/api';
import { type ClusterEvent } from '@/services/events';
import { EventRows } from '@/components/EventRows';
import { cn } from '@/lib/utils';
import { PageHeader } from '@/components/PageHeader';
import { StatBand, StatBandItem } from '@/components/StatBand';
import { SegmentBar } from '@/components/SegmentBar';
import { ControllerChip } from '@/components/ControllerChip';
import { StatusBadge } from '@/components/StatusBadge';
import { StatusTickCell, StatusTickHead } from '@/components/StatusTick';
import { RecordCard, RecordCards } from '@/components/RecordCard';
import { toneOf } from '@/components/status';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card';
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table';
import { Badge } from '@/components/ui/badge';
import { Progress } from '@/components/ui/progress';
import { Skeleton } from '@/components/ui/skeleton';
import { Separator } from '@/components/ui/separator';

// The five queries this page is made of, in one place: the Refresh button has
// to invalidate exactly the keys the page reads, and a key that drifts from its
// useQuery below produces a button that looks like it works and refreshes
// nothing.
const PAGE_QUERY_KEYS = {
  nodes: ['nodes'],
  pools: ['pools'],
  resources: ['resources'],
  gateways: ['gateways'],
  selfHa: ['selfha'],
  events: ['events', 'recent'],
} as const;

/** Coarse "how long ago", for a header line and a last-seen column. */
function agoLabel(ms: number): string {
  const secs = Math.max(0, Math.round(ms / 1000));
  if (secs < 5) return 'now';
  if (secs < 60) return `${secs}s ago`;
  if (secs < 3600) return `${Math.floor(secs / 60)}m ago`;
  if (secs < 86400) return `${Math.floor(secs / 3600)}h ago`;
  return `${Math.floor(secs / 86400)}d ago`;
}

export function DashboardPage() {
  const queryClient = useQueryClient();

  const {
    data: nodes,
    isLoading: nodesLoading,
    dataUpdatedAt: nodesUpdatedAt,
  } = useQuery({
    queryKey: PAGE_QUERY_KEYS.nodes,
    queryFn: () => api.getNodes(),
  });

  const { data: pools, isLoading: poolsLoading } = useQuery({
    queryKey: PAGE_QUERY_KEYS.pools,
    queryFn: () => api.getPools(),
  });

  const { data: resources, isLoading: resourcesLoading } = useQuery({
    queryKey: PAGE_QUERY_KEYS.resources,
    queryFn: () => api.getResources(),
  });

  const { data: gateways, isLoading: gatewaysLoading } = useQuery({
    queryKey: PAGE_QUERY_KEYS.gateways,
    queryFn: () => api.getGateways(),
  });

  const { data: selfHa, isLoading: selfHaLoading } = useQuery({
    queryKey: PAGE_QUERY_KEYS.selfHa,
    queryFn: () => api.getSelfHaStatus(),
  });

  // Everything on this page that reads "n seconds ago" is measured against a
  // fixed timestamp, so without a tick it freezes at whatever it said when the
  // page mounted — which is exactly the value an operator would trust. Five
  // seconds matches the queries' staleTime; it is not a poll.
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 5000);
    return () => clearInterval(id);
  }, []);

  const totalNodes = nodes?.nodes.length ?? 0;
  const onlineNodes =
    nodes?.nodes.filter((n) => n.state === 'online').length ?? 0;

  // selfHa.activeNode is an address; members are node names. Resolve the
  // active node's name so it displays as a name and highlights correctly.
  const nodeNameByAddr = new Map(
    (nodes?.nodes ?? []).map((n) => [n.address, n.name]),
  );
  const activeNodeName = selfHa?.activeNode
    ? nodeNameByAddr.get(selfHa.activeNode) ?? selfHa.activeNode
    : '';

  const rooms = (pools?.pools ?? []).map(poolRoomGiB);
  const totalStorage = Math.round(rooms.reduce((acc, r) => acc + r.total, 0));
  const freeStorage = Math.round(rooms.reduce((acc, r) => acc + r.free, 0));
  const usedStorage = Math.max(totalStorage - freeStorage, 0);
  const usagePct = totalStorage > 0 ? (usedStorage / totalStorage) * 100 : 0;

  const totalResources = resources?.resources.length ?? 0;

  const totalGateways = gateways?.gateways.length ?? 0;
  const runningGateways =
    gateways?.gateways.filter((g) => g.state === 'running').length ?? 0;

  // The segment bar is decorative, so the same node states are also spelled
  // out beside it — counted by the word the controller actually used rather
  // than by tone, because "3 online · 1 offline" says more than "3 ok · 1 bad".
  const nodeStateCounts = new Map<string, number>();
  for (const n of nodes?.nodes ?? []) {
    nodeStateCounts.set(n.state, (nodeStateCounts.get(n.state) ?? 0) + 1);
  }
  const nodeStateWords = [...nodeStateCounts]
    .map(([state, count]) => `${count} ${state}`)
    .join(' · ');

  const totalVolumes =
    resources?.resources.reduce((acc, r) => acc + r.volumes.length, 0) ?? 0;
  const drResources = resources?.resources.filter((r) => r.drNode).length ?? 0;
  const quorumRiskResources =
    resources?.resources.filter((r) => r.quorumRisk).length ?? 0;
  const resourceDetail = [
    `${totalVolumes} volume${totalVolumes === 1 ? '' : 's'}`,
    drResources > 0 ? `${drResources} replicated off-site` : null,
    quorumRiskResources > 0 ? `${quorumRiskResources} at quorum risk` : null,
  ]
    .filter(Boolean)
    .join(' · ');

  const stoppedGateways =
    gateways?.gateways.filter((g) => g.state !== 'running') ?? [];

  // Last-seen formatting is named once and used by both the card and the row:
  // a value derived twice is a value that can end up saying two different
  // things about the same node.
  const lastSeenLabel = (lastSeen: string) => {
    const ts = Number(lastSeen);
    return ts ? agoLabel(now - ts * 1000) : '-';
  };

  // Any of this page's queries in flight — the button says so rather than
  // looking inert while a wedged controller keeps it waiting.
  const isFetching = useIsFetching();

  const refresh = () => {
    for (const queryKey of Object.values(PAGE_QUERY_KEYS)) {
      queryClient.invalidateQueries({ queryKey });
    }
  };

  return (
    <div>
      <PageHeader
        title="Cluster overview"
        description={
          <>
            {/* Three cases, not two. Self-HA enabled with no active node is
                the failover window — the moment an operator is most likely to
                be reading this line, and the one where saying "standalone"
                would be a flat lie. An errored or unread query says nothing. */}
            {selfHa === undefined ? null : !selfHa.enabled ? (
              <>Standalone controller · </>
            ) : activeNodeName ? (
              <>
                Control plane on{' '}
                <span className="font-mono text-foreground">{activeNodeName}</span>
                {' · '}
              </>
            ) : (
              <>Control plane · no active node · </>
            )}
            {selfHa?.vip ? (
              <>
                VIP{' '}
                <span className="font-mono text-foreground">{selfHa.vip}</span>
                {' · '}
              </>
            ) : null}
            synced{' '}
            <span className="font-mono tabular-nums text-foreground">
              {nodesUpdatedAt ? agoLabel(now - nodesUpdatedAt) : '—'}
            </span>
          </>
        }
        actions={
          <>
            <Button variant="outline" onClick={refresh} disabled={isFetching > 0}>
              <RefreshCw className={cn(isFetching > 0 && 'animate-spin')} />
              Refresh
            </Button>
            {/* Resource creation is a dialog on the Resources page, not a route
                of its own, so this lands on the page that owns it. */}
            <Button asChild>
              <Link to="/resources">
                <Plus />
                New resource
              </Link>
            </Button>
          </>
        }
      />

      <div className="space-y-[18px]">
        {/* Four 27px figures in a non-wrapping row have ~90px each at 375px:
            the digits crush and the fourth column falls off the band's own
            overflow-hidden edge. Stacked below `sm`, with the dividers turning
            from vertical to horizontal so the band still reads as one object.
            The 2-up phone grid this wants instead needs StatBand to own its
            divider rule, which is not this page's to change. */}
        <StatBand className="flex-col divide-x-0 divide-y sm:flex-row sm:divide-x sm:divide-y-0">
          <StatBandItem
            label="Online nodes"
            value={onlineNodes}
            unit={`/${totalNodes}`}
            loading={nodesLoading}
            detail={
              <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
                <SegmentBar
                  segments={(nodes?.nodes ?? []).map((n) => toneOf(n.state))}
                />
                <span className="text-[11.5px]">{nodeStateWords || '—'}</span>
              </div>
            }
          />
          <StatBandItem
            label="Storage"
            value={freeStorage}
            unit={`of ${totalStorage} GB free`}
            grow={1.6}
            loading={poolsLoading}
            detail={
              <>
                <Progress value={usagePct} className="h-[5px]" />
                <p className="mt-1.5">
                  <span className="font-mono tabular-nums">{usedStorage}</span>{' '}
                  GB used (
                  <span className="font-mono tabular-nums">
                    {usagePct.toFixed(0)}%
                  </span>
                  )
                </p>
              </>
            }
          />
          <StatBandItem
            label="DRBD resources"
            value={totalResources}
            loading={resourcesLoading}
            detail={resourceDetail}
          />
          <StatBandItem
            label="Gateways"
            value={runningGateways}
            unit={`/${totalGateways}`}
            loading={gatewaysLoading}
            detail={
              stoppedGateways.length > 0 ? (
                <span className="text-status-warn-text">
                  {stoppedGateways
                    .slice(0, 2)
                    .map((g) => g.name)
                    .join(', ')}
                  {stoppedGateways.length > 2
                    ? ` +${stoppedGateways.length - 2} more`
                    : ''}{' '}
                  not running
                </span>
              ) : totalGateways > 0 ? (
                'all running'
              ) : (
                'none configured'
              )
            }
          />
        </StatBand>

        <div className="grid grid-cols-1 gap-5 lg:grid-cols-5">
        <Card className="gap-4 py-[18px] lg:col-span-3">
          <CardHeader className="flex flex-row items-center justify-between px-5">
            <CardTitle className="flex items-center gap-2.5">
              <ShieldCheck className="h-[17px] w-[17px] text-primary" />
              Controller Self-HA
            </CardTitle>
            {!selfHaLoading && (
              <StatusBadge status={selfHa?.enabled ? 'enabled' : 'disabled'} />
            )}
          </CardHeader>
          <CardContent className="px-5">
            {selfHaLoading ? (
              <div className="space-y-2">
                <Skeleton className="h-5 w-48" />
                <Skeleton className="h-5 w-64" />
              </div>
            ) : selfHa?.enabled ? (
              <div className="space-y-[18px]">
                {/* The VIP, the only long value here, needs about 160px: three
                    columns only once the card itself is wide enough for that,
                    else it takes a row of its own. */}
                <div className="@container">
                <div className="grid grid-cols-2 gap-x-4 gap-y-3.5 @lg:grid-cols-3">
                  <div className="min-w-0">
                    <div className="eyebrow">Active node</div>
                    <div className="mt-1.5 font-mono text-sm break-words">
                      {activeNodeName || '-'}
                    </div>
                  </div>
                  <div className="min-w-0">
                    <div className="eyebrow">Resource</div>
                    <div className="mt-1.5 font-mono text-sm break-words">
                      {selfHa.resource || '-'}
                    </div>
                  </div>
                  <div className="col-span-2 min-w-0 @lg:col-span-1">
                    <div className="eyebrow">VIP</div>
                    <div className="mt-1.5 font-mono text-sm whitespace-nowrap tabular-nums">
                      {selfHa.vip || '-'}
                    </div>
                  </div>
                </div>
                </div>
                {selfHa.nodes && selfHa.nodes.length > 0 && (
                  <>
                    <Separator />
                    <div>
                      <div className="eyebrow">Members</div>
                      <div className="mt-2.5 flex flex-wrap items-center gap-2">
                        {selfHa.nodes.map((n) => (
                          <Badge
                            key={n}
                            variant={
                              n === activeNodeName ? 'default' : 'secondary'
                            }
                            className="font-mono font-normal"
                          >
                            {n}
                            {n === activeNodeName && ' (active)'}
                          </Badge>
                        ))}
                      </div>
                    </div>
                  </>
                )}
              </div>
            ) : (
              <p className="text-sm text-muted-foreground">
                Standalone controller — self-HA is not enabled.
              </p>
            )}
          </CardContent>
        </Card>

        <RecentEvents />
        </div>

        {/* No CardHeader/CardContent here: the table draws its own row rule to
            the card's edges, which the card's padding would inset. */}
        <Card className="gap-0 overflow-hidden py-0">
          <div className="flex items-center justify-between px-5 pt-4 pb-3.5">
            <div className="text-[14.5px] font-semibold">Cluster nodes</div>
            <Link
              to="/nodes"
              className="text-[12.5px] text-primary hover:underline"
            >
              Manage nodes
            </Link>
          </div>
          {nodesLoading ? (
            <div className="space-y-2 px-5 pb-5">
              {Array.from({ length: 3 }).map((_, i) => (
                <Skeleton key={i} className="h-10 w-full" />
              ))}
            </div>
          ) : totalNodes === 0 ? (
            <div className="flex flex-col items-center justify-center gap-2 py-10 text-center">
              <Server className="h-8 w-8 text-muted-foreground" />
              <p className="text-sm text-muted-foreground">
                No nodes registered yet.
              </p>
            </div>
          ) : (
            <>
              {/* Below `md` the six columns put role, state and last-seen past
                  the right edge. The same rows render as cards instead — no
                  expand on this one: the Dashboard's node list is a summary,
                  and "Manage nodes" already leads to the full page. */}
              <RecordCards className="px-5 pb-5">
                {nodes?.nodes.map((node) => {
                  return (
                    <RecordCard
                      key={node.name}
                      status={node.state}
                      title={
                        <>
                          <span className="font-mono text-[14px] font-semibold">
                            {node.name}
                          </span>
                          <span className="text-[11.5px] text-muted-foreground">
                            {node.hostname}
                          </span>
                          {node.name === activeNodeName ? <ControllerChip /> : null}
                        </>
                      }
                      subtitle={
                        <span className="font-mono tabular-nums">
                          {node.address}
                        </span>
                      }
                      facts={[
                        {
                          label: 'State',
                          value: <StatusBadge status={node.state} />,
                        },
                        {
                          label: 'Last seen',
                          value: (
                            <span className="font-mono tabular-nums text-muted-foreground">
                              {lastSeenLabel(node.lastSeen)}
                            </span>
                          ),
                        },
                      ]}
                    />
                  );
                })}
              </RecordCards>

              <div className="hidden md:block">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <StatusTickHead />
                      <TableHead>Node</TableHead>
                      <TableHead>Address</TableHead>
                      <TableHead>State</TableHead>
                      <TableHead className="pr-5 text-right">
                        Last seen
                      </TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {nodes?.nodes.map((node) => {
                      return (
                        <TableRow key={node.name}>
                          <StatusTickCell status={node.state} />
                          <TableCell>
                            <div className="flex items-baseline gap-1.5">
                              <span className="font-mono font-medium">
                                {node.name}
                              </span>
                              <span className="text-[11.5px] text-muted-foreground">
                                {node.hostname}
                              </span>
                              {node.name === activeNodeName ? <ControllerChip /> : null}
                            </div>
                          </TableCell>
                          <TableCell className="font-mono tabular-nums">
                            {node.address}
                          </TableCell>
                          <TableCell>
                            <StatusBadge status={node.state} />
                          </TableCell>
                          <TableCell className="pr-5 text-right font-mono tabular-nums text-muted-foreground">
                            {lastSeenLabel(node.lastSeen)}
                          </TableCell>
                        </TableRow>
                      );
                    })}
                  </TableBody>
                </Table>
              </div>
            </>
          )}
        </Card>
      </div>
    </div>
  );
}

/**
 * What just happened to the cluster, read back from the controller's bounded
 * in-memory history. The SSE stream in services/events.ts is the live tail of
 * the same bus — this is how a page that was not open at the time still shows
 * the failover that woke someone up.
 */
function RecentEvents() {
  const { data, isLoading, isError } = useQuery({
    queryKey: PAGE_QUERY_KEYS.events,
    queryFn: () => api.listEvents({ limit: 4 }),
  });

  // The controller returns oldest first; newest belongs at the top of a card
  // this short.
  const events: ClusterEvent[] = [...(data?.events ?? [])].reverse();

  return (
    <Card className="gap-4 py-[18px] lg:col-span-2">
      <CardHeader className="flex flex-row items-center justify-between px-5">
        <CardTitle>Recent events</CardTitle>
        <Link to="/notifications" className="text-[12.5px] text-primary hover:underline">
          All events
        </Link>
      </CardHeader>
      <CardContent className="px-5">
        {isLoading ? (
          <div className="space-y-3">
            <Skeleton className="h-9 w-full" />
            <Skeleton className="h-9 w-full" />
            <Skeleton className="h-9 w-full" />
          </div>
        ) : isError ? (
          <p className="text-sm text-muted-foreground">Could not read the event history.</p>
        ) : events.length === 0 ? (
          <p className="text-sm text-muted-foreground">
            Nothing yet — the controller has published no events since it started.
          </p>
        ) : (
          <EventRows events={events} />
        )}
      </CardContent>
    </Card>
  );
}
