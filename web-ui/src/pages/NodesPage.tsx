import { useEffect, useRef, useState } from 'react';
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { Server, Plus, Loader2, Activity } from 'lucide-react';
import { api, type Node } from '@/services/api';
import { toast } from 'sonner';
import { cn, copyToClipboard } from '@/lib/utils';
import { PageHeader } from '@/components/PageHeader';
import { StatusTickHead } from '@/components/StatusTick';
import { ControllerChip } from '@/components/ControllerChip';
import { RecordCard, RecordCards } from '@/components/RecordCard';
import { TONE_TEXT } from '@/components/status';
import { Button } from '@/components/ui/button';
import {
  Table,
  TableBody,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table';
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from '@/components/ui/alert-dialog';
import { Skeleton } from '@/components/ui/skeleton';
import { StatFilterStrip } from '@/components/list/StatFilterStrip';
import { FilterSelect, ListToolbar, SearchBox } from '@/components/list/ListToolbar';
import { Pager } from '@/components/list/Pager';
import { formatAge, readinessOf, type HealthResult } from './nodes/health';
import { NodeRows, NodeMenu } from './nodes/NodeRows';
import { NodeDetail } from './nodes/NodeDetail';
import { RegisterNodeDialog } from './nodes/RegisterNodeDialog';

export function NodesPage() {
  const queryClient = useQueryClient();
  const { data: nodes, isLoading } = useQuery({
    queryKey: ['nodes'],
    queryFn: () => api.getNodes(),
  });

  // Which node is running the controller right now. It is the one fact in this
  // table that changes without anybody touching a node, and after a failover it
  // is the only row whose meaning moved.
  const { data: selfHa } = useQuery({
    queryKey: ['selfha'],
    queryFn: () => api.getSelfHaStatus(),
  });
  // activeNode is an address; the rows are keyed by name.
  const controllerNode = nodes?.nodes.find(
    (n) => n.address === selfHa?.activeNode,
  )?.name;

  // formatAge renders a relative time, so without a tick the column freezes at
  // whatever it said when the query last resolved. 5s matches the query's
  // staleTime — finer would re-render for nothing.
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 5000);
    return () => clearInterval(id);
  }, []);

  const [registerOpen, setRegisterOpen] = useState(false);
  const [expanded, setExpanded] = useState<string | null>(null);
  const [confirmNode, setConfirmNode] = useState<Node | null>(null);

  // Health is per node, not a single slot: the page can now show every node's
  // reading at once, and "Check all" fires several checks concurrently.
  const [health, setHealth] = useState<Record<string, HealthResult>>({});
  const [healthErrors, setHealthErrors] = useState<Record<string, string>>({});
  const [checking, setChecking] = useState<string[]>([]);

  const healthCheckMutation = useMutation({
    mutationFn: (nodeName: string) => api.healthCheck(nodeName),
    onMutate: (nodeName) => {
      setChecking((c) => (c.includes(nodeName) ? c : [...c, nodeName]));
      setHealthErrors(({ [nodeName]: _dropped, ...rest }) => rest);
    },
    onSuccess: (data, nodeName) => {
      setHealth((h) => ({ ...h, [nodeName]: { info: data.health, at: Date.now() } }));
    },
    onError: (e: Error, nodeName) => {
      toast.error(e.message);
      // The failure used to close the dialog and survive only as a toast. It
      // now stays where the answer was expected, so a dismissed toast is not
      // the only record of it.
      setHealthErrors((x) => ({ ...x, [nodeName]: e.message }));
      // A previous success is still on screen; a failed re-check does not
      // refresh it, so say so rather than presenting last hour's reading as
      // this one's.
      setHealth((h) => (h[nodeName] ? { ...h, [nodeName]: { ...h[nodeName], stale: true } } : h));
      // Only steal focus if the operator is not already reading another row —
      // during "Check all" the last failure to settle would otherwise win.
      setExpanded((cur) => (cur === null || cur === nodeName ? nodeName : cur));
    },
    onSettled: (_data, _error, nodeName) => {
      setChecking((c) => c.filter((n) => n !== nodeName));
    },
  });

  // Expanding a row IS the request to see this node's health.
  //
  // The panel used to open onto "No health check has run for this node yet"
  // beside a button — an empty box that made the operator ask for the thing
  // they had just asked for. The check is a read-only GET, so opening a row
  // simply runs it.
  //
  // Once per node per visit: the ref, not the health map, is what stops a
  // retry loop. A check that failed leaves no result, so keying off `health`
  // would re-fire on every render of a row whose node is unreachable — which
  // is exactly the node whose check takes an SSH timeout to fail.
  const autoChecked = useRef<Set<string>>(new Set());
  const runCheck = healthCheckMutation.mutate;
  useEffect(() => {
    if (!expanded || autoChecked.current.has(expanded)) return;
    const node = nodes?.nodes.find((n) => n.name === expanded);
    if (!node || node.state !== 'online') return;
    autoChecked.current.add(expanded);
    runCheck(expanded);
  }, [expanded, nodes, runCheck]);

  const unregisterMutation = useMutation({
    mutationFn: (address: string) => api.unregisterNode(address),
    onSuccess: () => {
      toast.success('Node unregistered');
      queryClient.invalidateQueries({ queryKey: ['nodes'] });
    },
    onError: (e: Error) => toast.error(e.message),
  });

  const list = nodes?.nodes ?? [];
  const online = list.filter((n) => n.state === 'online');

  // The same template as Resources: counts that filter, a search, a sort and
  // pages. Nodes number in the tens, so this happens here, not on the server.
  const [stateFilter, setStateFilter] = useState<'' | 'online' | 'offline' | 'maintenance'>('');
  const [query, setQuery] = useState('');
  const [sort, setSort] = useState('name');
  const [offset, setOffset] = useState(0);
  const PAGE = 50;
  const needle = query.trim().toLowerCase();
  const filtered = list
    .filter((n) => !stateFilter || n.state === stateFilter || (stateFilter === 'offline' && n.state !== 'online' && n.state !== 'maintenance'))
    .filter(
      (n) =>
        !needle ||
        [n.name, n.address, n.hostname, ...Object.entries(n.labels ?? {}).map(([k, v]) => `${k}=${v}`)]
          .join(' ')
          .toLowerCase()
          .includes(needle),
    )
    .sort((a, b) =>
      sort === 'state'
        ? (a.state === 'online' ? 1 : 0) - (b.state === 'online' ? 1 : 0) || a.name.localeCompare(b.name)
        : sort === 'seen'
          ? new Date(b.lastSeen ?? 0).getTime() - new Date(a.lastSeen ?? 0).getTime()
          : a.name.localeCompare(b.name),
    );
  const visible = filtered.slice(offset, offset + PAGE);
  const counts = {
    '': list.length,
    online: online.length,
    offline: list.filter((n) => n.state !== 'online' && n.state !== 'maintenance').length,
    maintenance: list.filter((n) => n.state === 'maintenance').length,
  };

  // health/healthErrors/confirmNode are keyed by node name, which outlives the
  // node. Register a different machine under a retired name and its row would
  // open showing the old cluster's readiness, stamped with the old time, having
  // never been checked. Drop what the controller no longer lists.
  useEffect(() => {
    if (!nodes) return;
    const live = new Set(list.map((n) => n.name));
    const prune = <T,>(m: Record<string, T>) =>
      Object.keys(m).every((k) => live.has(k))
        ? m
        : Object.fromEntries(Object.entries(m).filter(([k]) => live.has(k)));
    setHealth((h) => prune(h));
    setHealthErrors((x) => prune(x));
    setConfirmNode((c) => (c && !live.has(c.name) ? null : c));
  }, [nodes, list]);

  const checkedTimes = list.map((n) => health[n.name]?.at).filter((at): at is number => !!at);
  const lastCheck = checkedTimes.length ? Math.max(...checkedTimes) : null;

  const copyAddress = async (address: string) => {
    const ok = await copyToClipboard(address);
    if (ok) toast.success('Address copied');
    else toast.error('Could not copy address');
  };

  // N read-only GETs, one per online node. Offline nodes are excluded for the
  // same reason the per-row button is disabled for them. Each check is an SSH
  // round-trip on the controller, so they go a few at a time rather than all
  // at once on a large cluster.
  const checkAll = async () => {
    const names = online.map((n) => n.name);
    const CONCURRENCY = 4;
    for (let i = 0; i < names.length; i += CONCURRENCY) {
      await Promise.allSettled(
        names.slice(i, i + CONCURRENCY).map((n) => healthCheckMutation.mutateAsync(n)),
      );
    }
  };

  // Opening the page runs the check once, so the Readiness column says
  // something instead of "not checked" on every row — on a small cluster. Each
  // check is an SSH round trip, and on a large one a page view is not worth a
  // round of them; there the column fills as rows are opened or "Check all"
  // is pressed.
  const checkedOnOpen = useRef(false);
  useEffect(() => {
    if (checkedOnOpen.current || online.length === 0 || online.length > 12) return;
    checkedOnOpen.current = true;
    void checkAll();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [online.length]);

  return (
    // A container, so the table-or-cards switch below follows the width this
    // page actually has: the sidebar and a docked Copilot take theirs from the
    // viewport, and a viewport breakpoint cannot see either.
    <div className="@container">
      <PageHeader
        title="Nodes"
        description={
          lastCheck ? (
            <>
              Last health check{' '}
              <span className="tabular-nums text-foreground">{new Date(lastCheck).toLocaleTimeString()}</span>
            </>
          ) : undefined
        }
        actions={
          <>
            <Button
              variant="outline"
              onClick={checkAll}
              disabled={!online.length || checking.length > 0}
            >
              {checking.length > 0 ? (
                <Loader2 className="animate-spin" />
              ) : (
                <Activity />
              )}
              Check all
            </Button>
            <Button onClick={() => setRegisterOpen(true)}>
              <Plus />
              Register node
            </Button>
          </>
        }
      />

      <StatFilterStrip
        aria-label="Show nodes in one state"
        loading={isLoading}
        value={stateFilter}
        onChange={(v) => {
          setStateFilter(v);
          setOffset(0);
        }}
        options={[
          { value: '', label: 'All', count: counts[''] },
          { value: 'online', label: 'Online', color: 'var(--status-ok)', count: counts.online },
          { value: 'offline', label: 'Offline', color: 'var(--status-bad)', count: counts.offline },
          { value: 'maintenance', label: 'Draining', color: 'var(--status-warn)', count: counts.maintenance },
        ]}
      />
      <div className="mt-4 mb-3">
        <ListToolbar
          end={
            <FilterSelect
              label="Sort"
              value={sort}
              anyLabel={null}
              onChange={setSort}
              options={[
                { value: 'name', label: 'Name' },
                { value: 'state', label: 'Offline first' },
                { value: 'seen', label: 'Last seen' },
              ]}
            />
          }
        >
          <SearchBox
            value={query}
            onChange={(v) => {
              setQuery(v);
              setOffset(0);
            }}
            placeholder="Name, address or label"
            aria-label="Search nodes"
          />
        </ListToolbar>
      </div>

      {isLoading ? (
        <div className="tile">
          <div className="space-y-3 p-5">
            {Array.from({ length: 4 }).map((_, i) => (
              <Skeleton key={i} className="h-9 w-full" />
            ))}
          </div>
        </div>
      ) : !list.length ? (
        <div className="tile">
          <div className="flex flex-col items-center justify-center gap-3 py-16 text-center">
            <Server className="h-8 w-8 text-muted-foreground" />
            <p className="text-sm text-muted-foreground">
              No nodes registered. Register a storage node to get started.
            </p>
            <Button variant="outline" onClick={() => setRegisterOpen(true)}>
              <Plus />
              Register node
            </Button>
          </div>
        </div>
      ) : !visible.length ? (
        <div className="tile px-5 py-16 text-center text-sm text-muted-foreground">No node matches these filters.</div>
      ) : (
        <>
        {/* Below 760px of content the same rows are cards — a six-column
            table there puts readiness and last-seen past the right edge. The
            switch is on the page's own width, not the viewport's; `md:flex`
            undoes RecordCards' viewport-based hiding. */}
        <div className="@min-[760px]:hidden">
          <RecordCards className="md:flex">
            {visible.map((node) => {
              const isOnline = node.state === 'online';
              const isChecking = checking.includes(node.name);
              const result = health[node.name];
              const readiness = result ? readinessOf(result.info) : null;
              const isOpen = expanded === node.name;
              const panelId = `node-card-${node.name}`;
              return (
                <RecordCard
                  key={node.name}
                  status={node.state}
                  open={isOpen}
                  onToggle={() => setExpanded(isOpen ? null : node.name)}
                  detailId={panelId}
                  title={
                    <>
                      <span className="font-mono text-[14px] font-semibold">{node.name}</span>
                      <span className="text-[11.5px] text-muted-foreground">{node.hostname}</span>
                      {node.name === controllerNode ? <ControllerChip /> : null}
                    </>
                  }
                  subtitle={
                    <span className="font-mono tabular-nums">{node.address}</span>
                  }
                  actions={
                    <NodeMenu
                      node={node}
                      isDeleting={
                        unregisterMutation.isPending &&
                        unregisterMutation.variables === node.address
                      }
                      isChecking={isChecking}
                      isOnline={isOnline}
                      onCheck={() => healthCheckMutation.mutate(node.name)}
                      onCopyAddress={() => copyAddress(node.address)}
                      onUnregister={() => setConfirmNode(node)}
                    />
                  }
                  facts={[
                    {
                      label: 'State',
                      value: (
                        <span className={cn(isOnline ? TONE_TEXT.ok : TONE_TEXT.bad)}>
                          {node.state}
                        </span>
                      ),
                    },
                    {
                      label: 'Last seen',
                      value: (
                        <span className="font-mono tabular-nums">{formatAge(node.lastSeen)}</span>
                      ),
                    },
                    {
                      label: 'System',
                      value: (
                        <span className="font-mono text-muted-foreground">
                          {node.version || (node.localAgent ? 'Local agent' : '-')}
                        </span>
                      ),
                    },
                    {
                      label: 'Readiness',
                      value: readiness ? (
                        <span className={cn(result?.stale ? 'text-muted-foreground' : TONE_TEXT[readiness.tone])}>
                          {readiness.word}
                          {result?.stale ? ' · stale' : ''}
                        </span>
                      ) : (
                        <span className="text-muted-foreground">not checked</span>
                      ),
                    },
                  ]}
                >
                  <NodeDetail
                    node={node}
                    isOnline={isOnline}
                    isChecking={isChecking}
                    result={result}
                    error={healthErrors[node.name]}
                    onCheck={() => healthCheckMutation.mutate(node.name)}
                  />
                </RecordCard>
              );
            })}
          </RecordCards>
        </div>

        <div className="tile hidden overflow-hidden @min-[760px]:block">
            <Table>
              <TableHeader>
                <TableRow>
                  <StatusTickHead />
                  <TableHead>Node</TableHead>
                  <TableHead>Address</TableHead>
                  {/* The widest column and the one least read; under 1000px
                      it gives its room to the rest, and the expanded row still
                      shows it as Version. */}
                  <TableHead className="hidden @min-[1000px]:table-cell">System</TableHead>
                  <TableHead>Readiness</TableHead>
                  <TableHead>Last seen</TableHead>
                  <TableHead className="pr-5 text-right">
                    <span className="sr-only">Actions</span>
                  </TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {visible.map((node) => {
                  const isOnline = node.state === 'online';
                  const isChecking = checking.includes(node.name);
                  const isDeleting =
                    unregisterMutation.isPending &&
                    unregisterMutation.variables === node.address;
                  const result = health[node.name];
                  const readiness = result ? readinessOf(result.info) : null;
                  const isOpen = expanded === node.name;
                  const panelId = `node-detail-${node.name}`;

                  return (
                    <NodeRows
                      key={node.name}
                      node={node}
                      isController={node.name === controllerNode}
                      isOnline={isOnline}
                      isChecking={isChecking}
                      isDeleting={isDeleting}
                      isOpen={isOpen}
                      panelId={panelId}
                      result={result}
                      readiness={readiness}
                      error={healthErrors[node.name]}
                      onToggle={() => setExpanded(isOpen ? null : node.name)}
                      onCheck={() => healthCheckMutation.mutate(node.name)}
                      onCopyAddress={() => copyAddress(node.address)}
                      onUnregister={() => setConfirmNode(node)}
                    />
                  );
                })}
              </TableBody>
            </Table>
            <Pager offset={offset} limit={PAGE} total={filtered.length} onOffset={setOffset} />
        </div>
        </>
      )}

      <RegisterNodeDialog open={registerOpen} onOpenChange={setRegisterOpen} />

      {/* One dialog for the page, opened by whichever row's menu asked: a
          confirmation nested inside a menu unmounts with the menu. */}
      <AlertDialog
        open={!!confirmNode}
        onOpenChange={(open) => !open && setConfirmNode(null)}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Unregister node "{confirmNode?.name}"?</AlertDialogTitle>
            <AlertDialogDescription>
              This removes the node from the controller's inventory. Resources and pools
              hosted on this node will no longer be managed. This action cannot be undone.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              variant="destructive"
              onClick={() => {
                if (confirmNode) unregisterMutation.mutate(confirmNode.address);
              }}
            >
              Unregister
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
