import { Fragment, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { ChevronDown, ChevronRight, Database } from 'lucide-react';
import { api, type Pool } from '@/services/api';
import { PageHeader } from '@/components/PageHeader';
import { StatFilterStrip } from '@/components/list/StatFilterStrip';
import { FilterSelect, ListToolbar, SearchBox } from '@/components/list/ListToolbar';
import { Pager } from '@/components/list/Pager';
import { Skeleton } from '@/components/ui/skeleton';
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table';
import { fmtSize } from '@/components/charts/chartKit';
import { PoolItem } from './pools/PoolItem';
import { AddDiskDialog, CreatePoolDialog, DeletePoolDialog } from './pools/PoolDialogs';
import { TrimButton } from './pools/TrimButton';
import { StorageJobsPanel } from './pools/StorageJobsPanel';
import { DisksPanel } from './pools/DisksPanel';
import { UnusedDisksPanel } from './pools/UnusedDisksPanel';
import { poolTypeLabel } from './pools/poolTypes';
import { fullnessColor, poolBytes, usedPercent } from './pools/poolUsage';

// Every pool on every node in one table, fullest first: the question a pools
// page answers is which one fills next, and a card per node made it a search.

type Band = '' | 'above90' | 'band80' | 'under80';
const PAGE = 50;

export function PoolsPage() {
  const { data: pools, isLoading } = useQuery({ queryKey: ['pools'], queryFn: () => api.getPools() });
  const { data: nodes } = useQuery({ queryKey: ['nodes'], queryFn: () => api.getNodes() });
  const nodeName: Record<string, string> = Object.fromEntries((nodes?.nodes ?? []).map((n) => [n.address, n.name]));
  const nameOf = (p: Pool) => nodeName[p.node] || p.node;

  const [band, setBand] = useState<Band>('');
  const [query, setQuery] = useState('');
  const [node, setNode] = useState('');
  const [type, setType] = useState('');
  const [sort, setSort] = useState('fullest');
  const [offset, setOffset] = useState(0);
  const [open, setOpen] = useState<string[]>([]);

  const list = pools?.pools ?? [];
  const inBand = (p: Pool, b: Band) => {
    const pct = usedPercent(p);
    return b === '' || (b === 'above90' ? pct >= 90 : b === 'band80' ? pct >= 80 && pct < 90 : pct < 80);
  };
  const shownType = (p: Pool) => (p.thinPoolLv && p.type === 'vg' ? 'thin_pool' : p.type);
  const needle = query.trim().toLowerCase();
  const filtered = list
    .filter((p) => inBand(p, band))
    .filter((p) => !node || nameOf(p) === node)
    .filter((p) => !type || shownType(p) === type)
    .filter((p) => !needle || `${p.name} ${nameOf(p)} ${p.devices?.join(' ')}`.toLowerCase().includes(needle))
    .sort((a, b) =>
      sort === 'name'
        ? a.name.localeCompare(b.name) || nameOf(a).localeCompare(nameOf(b))
        : sort === 'node'
          ? nameOf(a).localeCompare(nameOf(b)) || a.name.localeCompare(b.name)
          : sort === 'free'
            ? poolBytes(b).available - poolBytes(a).available
            : usedPercent(b) - usedPercent(a),
    );
  const visible = filtered.slice(offset, offset + PAGE);
  const key = (p: Pool) => `${p.node}/${p.name}`;
  const reset = <T,>(set: (v: T) => void) => (v: T) => {
    set(v);
    setOffset(0);
  };

  return (
    <div className="@container mx-auto max-w-[1600px]">
      <PageHeader
        title="Storage pools"
        actions={
          <>
            <TrimButton />
            <CreatePoolDialog nodes={nodes?.nodes ?? []} />
          </>
        }
      />
      <StatFilterStrip
        aria-label="Show pools by how full they are"
        loading={isLoading}
        value={band}
        onChange={reset(setBand)}
        options={[
          { value: '', label: 'All', count: list.length },
          { value: 'above90', label: 'Above 90%', color: 'var(--status-bad)', count: list.filter((p) => inBand(p, 'above90')).length },
          { value: 'band80', label: '80–90%', color: 'var(--status-warn)', count: list.filter((p) => inBand(p, 'band80')).length },
          { value: 'under80', label: 'Under 80%', color: 'var(--status-ok)', count: list.filter((p) => inBand(p, 'under80')).length },
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
                { value: 'fullest', label: 'Fullest first' },
                { value: 'free', label: 'Most free' },
                { value: 'name', label: 'Name' },
                { value: 'node', label: 'Node' },
              ]}
            />
          }
        >
          <SearchBox value={query} onChange={reset(setQuery)} placeholder="Pool, node or device" aria-label="Search pools" />
          <FilterSelect
            label="Node"
            value={node}
            onChange={reset(setNode)}
            options={[...new Set(list.map(nameOf))].sort().map((n) => ({ value: n, label: n }))}
          />
          <FilterSelect
            label="Type"
            value={type}
            onChange={reset(setType)}
            options={[...new Set(list.map(shownType))].map((t) => ({ value: t, label: poolTypeLabel(t) }))}
          />
        </ListToolbar>
      </div>

      <StorageJobsPanel />
      <UnusedDisksPanel pools={list} nodeNames={nodeName} />

      <div className="tile mt-3 overflow-hidden">
        {isLoading ? (
          <div className="space-y-3 p-5">
            {[0, 1, 2].map((i) => (
              <Skeleton key={i} className="h-9 w-full" />
            ))}
          </div>
        ) : list.length === 0 ? (
          <div className="flex flex-col items-center gap-3 py-16 text-center text-sm text-muted-foreground">
            <Database className="size-8" />
            No storage pools yet.
          </div>
        ) : visible.length === 0 ? (
          <p className="px-5 py-16 text-center text-sm text-muted-foreground">No pool matches these filters.</p>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Pool</TableHead>
                <TableHead>Node</TableHead>
                <TableHead className="w-[26%]">Used</TableHead>
                <TableHead className="hidden text-right @min-[760px]:table-cell">Size</TableHead>
                <TableHead className="text-right">Free</TableHead>
                <TableHead className="hidden text-right @min-[900px]:table-cell">Metadata</TableHead>
                <TableHead className="w-20" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {visible.map((p) => {
                const pct = usedPercent(p);
                const { capacity, available } = poolBytes(p);
                const isOpen = open.includes(key(p));
                const Chevron = isOpen ? ChevronDown : ChevronRight;
                return (
                  <Fragment key={key(p)}>
                    <TableRow className={isOpen ? 'border-b-0' : undefined}>
                      <TableCell>
                        <button
                          type="button"
                          aria-expanded={isOpen}
                          onClick={() => setOpen((o) => (isOpen ? o.filter((k) => k !== key(p)) : [...o, key(p)]))}
                          className="flex items-start gap-2 text-left outline-none focus-visible:ring-[3px] focus-visible:ring-ring/50"
                        >
                          <Chevron className="mt-0.5 size-3.5 shrink-0 text-muted-foreground" />
                          <span>
                            <span className="block font-medium">{p.name}</span>
                            <span className="block text-[11.5px] text-muted-foreground">
                              {poolTypeLabel(shownType(p))}
                              {p.thinOutOfSpace && <span className="text-status-bad-text"> · out of space</span>}
                              {p.cacheDegraded && <span className="text-status-bad-text"> · cache degraded</span>}
                            </span>
                          </span>
                        </button>
                      </TableCell>
                      <TableCell>{nameOf(p)}</TableCell>
                      <TableCell>
                        <span className="flex items-center gap-3">
                          <span className="h-[6px] min-w-[60px] flex-1 overflow-hidden rounded-full bg-secondary">
                            <span className="block h-full rounded-full" style={{ width: `${Math.min(pct, 100)}%`, background: fullnessColor(pct) }} />
                          </span>
                          <span className="w-12 text-right tabular-nums">{pct.toFixed(pct < 10 ? 1 : 0)}%</span>
                        </span>
                      </TableCell>
                      <TableCell className="hidden text-right tabular-nums @min-[760px]:table-cell">{fmtSize(capacity)}</TableCell>
                      <TableCell className="text-right tabular-nums">{fmtSize(available)}</TableCell>
                      <TableCell className="hidden text-right tabular-nums @min-[900px]:table-cell">
                        {p.thinPoolLv ? `${(p.thinMetadataPercent ?? 0).toFixed(0)}%` : '—'}
                      </TableCell>
                      <TableCell className="pr-3 text-right whitespace-nowrap">
                        <AddDiskDialog pool={p} nodeName={nameOf(p)} />
                        <DeletePoolDialog pool={p} />
                      </TableCell>
                    </TableRow>
                    {isOpen && (
                      <TableRow className="hover:bg-transparent">
                        <TableCell colSpan={7} className="bg-muted/40 whitespace-normal">
                          <PoolItem pool={p} nodeName={nameOf(p)} />
                        </TableCell>
                      </TableRow>
                    )}
                  </Fragment>
                );
              })}
            </TableBody>
          </Table>
        )}
        {filtered.length > 0 && <Pager offset={offset} limit={PAGE} total={filtered.length} onOffset={setOffset} />}
      </div>

      {list.length > 0 && (
        <div className="mt-6">
          <DisksPanel />
        </div>
      )}
    </div>
  );
}
