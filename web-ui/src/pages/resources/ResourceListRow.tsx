import { ChevronDown, ChevronRight } from 'lucide-react';
import { TableCell, TableRow } from '@/components/ui/table';
import { fmtBytes, fmtRate, fmtSize } from '@/components/charts/chartKit';
import { num, type ResourceListItem } from '@/services/api';
import { type RowDialog } from './types';
import { ResourceDetail } from './ResourceDetail';
import { ResourceActionsMenu } from './ResourceActionsMenu';
import { displayName, REPLICA_COLOR, sizeBytes, STATES, USED_BY } from './listModel';

// One resource in the list, from what the last health poll saw: no request of
// its own until it is opened.

export function StateWord({ state }: { state: string }) {
  const s = STATES[state] ?? STATES.unknown;
  return (
    <span className="inline-flex items-center gap-2 whitespace-nowrap">
      <span className="size-[7px] rounded-full" style={{ background: s.color }} />
      {s.label}
    </span>
  );
}

export function ReplicaBars({ r }: { r: ResourceListItem }) {
  const reps = r.health?.replicas ?? [];
  return (
    <span className="flex min-w-0 items-center gap-[3px]" title={reps.map((x) => `${x.node} ${x.state}`).join(', ')}>
      {reps.map((x) => (
        <span
          key={x.node}
          className="h-1.5 shrink-0 rounded-[3px]"
          style={{ width: x.state === 'tiebreaker' ? 8 : 18, background: REPLICA_COLOR[x.state] ?? 'var(--status-idle)' }}
        />
      ))}
      <span className="ml-2 truncate text-foreground/80">{r.health?.summary ?? ''}</span>
    </span>
  );
}

export function ResourceListRow({
  resource: r,
  expanded,
  onToggle,
  onOpenDialog,
}: {
  resource: ResourceListItem;
  expanded: boolean;
  onToggle: () => void;
  onOpenDialog: (d: RowDialog) => void;
}) {
  const { title, sub } = displayName(r);
  const Chevron = expanded ? ChevronDown : ChevronRight;
  const oos = num(r.health?.outOfSyncBytes);
  const writes = r.health?.writeBytesPerSec;
  return (
    <>
      <TableRow className={expanded ? 'border-b-0' : undefined}>
        <TableCell className="max-w-[280px] overflow-hidden">
          <button
            type="button"
            onClick={onToggle}
            aria-expanded={expanded}
            aria-label={`${expanded ? 'Hide' : 'Show'} detail for ${r.name}`}
            className="flex w-full min-w-0 items-start gap-2 text-left outline-none focus-visible:ring-[3px] focus-visible:ring-ring/50"
          >
            <Chevron className="mt-0.5 size-3.5 shrink-0 text-muted-foreground" />
            <span className="min-w-0">
              <span className="block truncate font-medium">{title}</span>
              {sub && <span className="block truncate font-mono text-[11.5px] text-muted-foreground">{sub}</span>}
            </span>
          </button>
        </TableCell>
        <TableCell className="hidden @min-[860px]:table-cell">
          {USED_BY[r.usedBy ?? ''] && (
            <span className="rounded-md bg-secondary px-1.5 py-0.5 text-[11.5px] text-foreground/75">{USED_BY[r.usedBy ?? '']}</span>
          )}
        </TableCell>
        <TableCell>
          <StateWord state={r.health?.state ?? 'unknown'} />
        </TableCell>
        <TableCell className="max-w-[240px] overflow-hidden">
          <ReplicaBars r={r} />
        </TableCell>
        <TableCell className="text-right tabular-nums">{fmtSize(sizeBytes(r))}</TableCell>
        <TableCell className="hidden text-right tabular-nums @min-[1000px]:table-cell">{oos ? fmtBytes(oos) : '—'}</TableCell>
        <TableCell className="text-right tabular-nums">{writes !== undefined ? fmtRate(writes) : '—'}</TableCell>
        <TableCell className="pr-4 text-right">
          <ResourceActionsMenu resource={r} onSelect={onOpenDialog} />
        </TableCell>
      </TableRow>
      {expanded && (
        <TableRow className="hover:bg-transparent">
          <TableCell colSpan={8} className="bg-muted/40 p-0 whitespace-normal">
            <div className="py-5 pr-6 pl-9">
              <ResourceDetail resource={r} onOpenDialog={onOpenDialog} />
            </div>
          </TableCell>
        </TableRow>
      )}
    </>
  );
}
