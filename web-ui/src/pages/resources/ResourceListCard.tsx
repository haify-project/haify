import { ChevronDown, ChevronRight } from 'lucide-react';
import { fmtRate, fmtSize } from '@/components/charts/chartKit';
import { type ResourceListItem } from '@/services/api';
import { type RowDialog } from './types';
import { ResourceDetail } from './ResourceDetail';
import { ResourceActionsMenu } from './ResourceActionsMenu';
import { displayName, sizeBytes, USED_BY } from './listModel';
import { ReplicaBars, StateWord } from './ResourceListRow';

// The same resource as ResourceListRow, stacked for a narrow page.

export function ResourceListCard({
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
  const writes = r.health?.writeBytesPerSec;
  return (
    <div className="border-b border-border last:border-0">
      <div className="flex items-start gap-2 px-4 py-3">
        <button
          type="button"
          onClick={onToggle}
          aria-expanded={expanded}
          aria-label={`${expanded ? 'Hide' : 'Show'} detail for ${r.name}`}
          className="flex min-w-0 flex-1 items-start gap-2 text-left text-[13px] outline-none focus-visible:ring-[3px] focus-visible:ring-ring/50"
        >
          <Chevron className="mt-0.5 size-3.5 shrink-0 text-muted-foreground" />
          <span className="min-w-0 flex-1 space-y-1.5">
            <span className="flex items-center justify-between gap-2">
              <span className="truncate font-medium">{title}</span>
              <StateWord state={r.health?.state ?? 'unknown'} />
            </span>
            {sub && <span className="block truncate font-mono text-[11.5px] text-muted-foreground">{sub}</span>}
            <ReplicaBars r={r} />
            <span className="block text-[12px] text-muted-foreground tabular-nums">
              {[USED_BY[r.usedBy ?? ''], fmtSize(sizeBytes(r)), writes !== undefined ? `${fmtRate(writes)} writes` : '']
                .filter(Boolean)
                .join(' · ')}
            </span>
          </span>
        </button>
        <ResourceActionsMenu resource={r} onSelect={onOpenDialog} />
      </div>
      {expanded && (
        <div className="bg-muted/40 px-4 py-4">
          <ResourceDetail resource={r} onOpenDialog={onOpenDialog} />
        </div>
      )}
    </div>
  );
}
