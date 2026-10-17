import { Fragment, useCallback, useState } from 'react';
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { useSearchParams } from 'react-router';
import { Plus, Boxes } from 'lucide-react';
import { api, type ResourceListItem } from '@/services/api';
import { PageHeader } from '@/components/PageHeader';
import { StatFilterStrip } from '@/components/list/StatFilterStrip';
import { FilterSelect, ListToolbar, SearchBox } from '@/components/list/ListToolbar';
import { Pager } from '@/components/list/Pager';
import { Button } from '@/components/ui/button';
import { Skeleton } from '@/components/ui/skeleton';
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table';
import { useDescribePage } from '@/lib/pageContext';
import { ResourceProfilesPage } from './ResourceProfilesPage';
import { type RowDialog } from './resources/types';
import { ResourceListRow } from './resources/ResourceListRow';
import { ResourceListCard } from './resources/ResourceListCard';
import { ResourceDialogs } from './resources/ResourceDialogs';
import { CreateResourceDialog } from './resources/CreateResourceDialog';
import { STATES, USED_BY } from './resources/listModel';

// Resources a page at a time. The controller filters, sorts and pages them
// with each one's health from its last poll, so the page asks for fifty rows
// and the counts however many thousand resources there are, and nothing about
// a resource is fetched on its own until its row is opened.

const PAGE = 50;

const STATE_FILTERS = ['', 'healthy', 'resyncing', 'degraded', 'no_quorum', 'offsite'] as const;

export function ResourcesPage() {
  const [params, setParams] = useSearchParams();
  const tab = params.get('tab') === 'profiles' ? 'profiles' : 'resources';
  const q = params.get('q') ?? '';
  const state = params.get('state') ?? '';
  const usedBy = params.get('used_by') ?? '';
  const node = params.get('node') ?? '';
  const pool = params.get('pool') ?? '';
  const sort = params.get('sort') ?? 'attention';
  const offset = Number(params.get('offset') ?? 0) || 0;

  // Every change of filter starts again from the first page.
  const update = useCallback(
    (patch: Record<string, string>) =>
      setParams(
        (cur) => {
          const next = new URLSearchParams(cur);
          for (const [k, v] of Object.entries(patch)) {
            if (v) next.set(k, v);
            else next.delete(k);
          }
          if (!('offset' in patch)) next.delete('offset');
          return next;
        },
        { replace: true },
      ),
    [setParams],
  );
  const setQuery = useCallback((v: string) => update({ q: v }), [update]);

  const page = useQuery({
    queryKey: ['resource-page', { q, state, usedBy, node, pool, sort, offset }],
    queryFn: () => api.getResourcePage({ query: q, state, usedBy, node, pool, sort, offset, limit: PAGE }),
    placeholderData: keepPreviousData,
    refetchInterval: 15_000,
    meta: { live: true },
    enabled: tab === 'resources',
  });
  const { data: pools } = useQuery({ queryKey: ['pools'], queryFn: () => api.getPools() });
  const { data: nodes } = useQuery({ queryKey: ['nodes'], queryFn: () => api.getNodes() });
  const { data: profiles } = useQuery({ queryKey: ['resource-profiles'], queryFn: () => api.getResourceProfiles() });

  const rows = page.data?.resources ?? [];
  const counts = page.data?.counts ?? {};
  const total = page.data?.total ?? 0;
  const [expanded, setExpanded] = useState<string[]>(q ? [q] : []);
  const [dialog, setDialog] = useState<{ resource: string; kind: RowDialog } | null>(null);
  const [createOpen, setCreateOpen] = useState(false);
  const toggle = (name: string) =>
    setExpanded((cur) => (cur.includes(name) ? cur.filter((n) => n !== name) : [...cur, name]));

  useDescribePage(
    tab === 'profiles'
      ? 'Resource profiles'
      : ['Resources', state && (STATES[state]?.label ?? 'off-site').toLowerCase(), q && `matching "${q}"`]
          .filter(Boolean)
          .join(', '),
  );

  // With problems first and no state chosen, the list falls into two groups,
  // headed where each begins on this page.
  const grouped = sort === 'attention' && !state;
  const attention = counts.attention ?? 0;
  const groupBefore = (r: ResourceListItem, i: number): string | null => {
    if (!grouped) return null;
    const bad = (r.health?.state ?? 'unknown') !== 'healthy';
    const prevBad = i > 0 ? (rows[i - 1].health?.state ?? 'unknown') !== 'healthy' : null;
    if (i === 0 || bad !== prevBad) {
      return bad ? `Needs attention · ${attention.toLocaleString('en-US')}` : `Healthy · ${(counts.healthy ?? 0).toLocaleString('en-US')}`;
    }
    return null;
  };

  const header = (
    <PageHeader
      title={tab === 'profiles' ? 'Resource profiles' : 'Resources'}
      actions={
        tab === 'profiles' ? (
          <Button variant="outline" onClick={() => setParams({}, { replace: true })}>
            Resources
          </Button>
        ) : (
          <>
            <Button variant="outline" onClick={() => setParams({ tab: 'profiles' }, { replace: true })}>
              Profiles
            </Button>
            <Button onClick={() => setCreateOpen(true)}>
              <Plus />
              Create resource
            </Button>
          </>
        )
      }
    />
  );

  if (tab === 'profiles') {
    return (
      <div className="@container mx-auto max-w-[1600px]">
        {header}
        <ResourceProfilesPage />
      </div>
    );
  }

  return (
    <div className="@container mx-auto max-w-[1600px]">
      {header}
      <StatFilterStrip
        aria-label="Show resources in one state"
        loading={page.isLoading}
        value={state as (typeof STATE_FILTERS)[number]}
        onChange={(v) => update({ state: v })}
        options={STATE_FILTERS.map((v) => ({
          value: v,
          label: v === '' ? 'All' : v === 'offsite' ? 'Off-site' : STATES[v].label,
          color: v && v !== 'offsite' ? STATES[v].color : undefined,
          count: counts[v === '' ? 'all' : v] ?? 0,
        }))}
      />

      <div className="mt-4">
        <ListToolbar
          end={
            <FilterSelect
              label="Sort"
              value={sort}
              anyLabel={null}
              onChange={(v) => update({ sort: v === 'attention' ? '' : v })}
              options={[
                { value: 'attention', label: 'Problems first' },
                { value: 'name', label: 'Name' },
                { value: 'size', label: 'Size' },
                { value: 'writes', label: 'Writes' },
              ]}
            />
          }
        >
          <SearchBox value={q} onChange={setQuery} placeholder="Name, PVC, VM or label" aria-label="Search resources" />
          <FilterSelect
            label="Used by"
            value={usedBy}
            onChange={(v) => update({ used_by: v })}
            options={[...Object.entries(USED_BY).map(([value, label]) => ({ value, label })), { value: 'none', label: 'Nothing' }]}
          />
          <FilterSelect
            label="Node"
            value={node}
            onChange={(v) => update({ node: v })}
            options={(nodes?.nodes ?? []).map((n) => ({ value: n.name, label: n.name }))}
          />
          <FilterSelect
            label="Pool"
            value={pool}
            onChange={(v) => update({ pool: v })}
            options={[...new Set((pools?.pools ?? []).map((p) => p.name))].map((p) => ({ value: p, label: p }))}
          />
        </ListToolbar>
      </div>

      <div className="tile mt-3 overflow-hidden">
        {page.isLoading ? (
          <div className="space-y-3 p-5">
            {[0, 1, 2, 3].map((i) => (
              <Skeleton key={i} className="h-9 w-full" />
            ))}
          </div>
        ) : rows.length === 0 ? (
          (counts.all ?? 0) === 0 ? (
            <div className="flex flex-col items-center gap-3 px-5 py-16 text-center">
              <Boxes className="size-8 text-muted-foreground" />
              <p className="text-sm text-muted-foreground">No resources yet.</p>
              <Button variant="outline" onClick={() => setCreateOpen(true)}>
                <Plus />
                Create resource
              </Button>
            </div>
          ) : (
            <p className="px-5 py-16 text-center text-sm text-muted-foreground">No resource matches these filters.</p>
          )
        ) : (
          <>
            <div className="@min-[720px]:hidden">
              {rows.map((r, i) => (
                <Fragment key={r.name}>
                  {groupBefore(r, i) && (
                    <div className="border-b border-border bg-muted/50 px-4 py-1.5 text-[12px] text-muted-foreground">{groupBefore(r, i)}</div>
                  )}
                  <ResourceListCard
                    resource={r}
                    expanded={expanded.includes(r.name)}
                    onToggle={() => toggle(r.name)}
                    onOpenDialog={(kind) => setDialog({ resource: r.name, kind })}
                  />
                </Fragment>
              ))}
            </div>
            <Table className="hidden @min-[720px]:table">
              <TableHeader>
                <TableRow>
                  <TableHead>Resource</TableHead>
                  <TableHead className="hidden @min-[860px]:table-cell">Used by</TableHead>
                  <TableHead>State</TableHead>
                  <TableHead>Replicas</TableHead>
                  <TableHead className="text-right">Size</TableHead>
                  <TableHead className="hidden text-right @min-[1000px]:table-cell">Out of sync</TableHead>
                  <TableHead className="text-right">Writes</TableHead>
                  <TableHead className="w-10" />
                </TableRow>
              </TableHeader>
              <TableBody>
                {rows.map((r, i) => {
                  const g = groupBefore(r, i);
                  return (
                    <Fragment key={r.name}>
                      {g && (
                        <TableRow className="hover:bg-transparent">
                          <TableCell colSpan={8} className="h-auto bg-muted/50 py-1.5 text-[12px] text-muted-foreground">
                            {g}
                          </TableCell>
                        </TableRow>
                      )}
                      <ResourceListRow
                        resource={r}
                        expanded={expanded.includes(r.name)}
                        onToggle={() => toggle(r.name)}
                        onOpenDialog={(kind) => setDialog({ resource: r.name, kind })}
                      />
                    </Fragment>
                  );
                })}
              </TableBody>
            </Table>
          </>
        )}
        {!page.isLoading && total > 0 && (
          <Pager offset={offset} limit={PAGE} total={total} onOffset={(o) => update({ offset: o ? String(o) : '' })} />
        )}
      </div>

      {/* Dialogs for the rows on this page only, mounted here rather than in a
          row so the card and the table row share one. */}
      {rows.map((r) => (
        <ResourceDialogs
          key={r.name}
          resource={r}
          kind={dialog?.resource === r.name ? dialog.kind : null}
          onSelect={(kind) => setDialog(kind ? { resource: r.name, kind } : null)}
          pools={pools?.pools ?? []}
          nodes={nodes?.nodes ?? []}
        />
      ))}

      <CreateResourceDialog
        open={createOpen}
        onOpenChange={setCreateOpen}
        nodes={nodes?.nodes ?? []}
        pools={pools?.pools ?? []}
        profiles={profiles?.profiles ?? []}
      />
    </div>
  );
}
