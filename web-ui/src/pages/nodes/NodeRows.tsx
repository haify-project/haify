import {
  Trash2,
  Loader2,
  ChevronRight,
  ChevronDown,
  MoreHorizontal,
  Copy,
  Activity,
} from 'lucide-react';
import { type Node } from '@/services/api';
import { cn } from '@/lib/utils';
import { StatusTickCell } from '@/components/StatusTick';
import { ControllerChip } from '@/components/ControllerChip';
import { TONE_BG, TONE_TEXT } from '@/components/status';
import { Button } from '@/components/ui/button';
import { TableCell, TableRow } from '@/components/ui/table';
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu';
import { formatLastSeen, formatAge, type Readiness, type HealthResult } from './health';
import { NodeDetail } from './NodeDetail';

/** Body cells span the tick gutter plus six columns; one constant so the
 *  expanded row cannot drift out of the grid when a column is added. */
const COLUMN_COUNT = 7;

interface NodeRowsProps {
  node: Node;
  isController: boolean;
  isOnline: boolean;
  isChecking: boolean;
  isDeleting: boolean;
  isOpen: boolean;
  panelId: string;
  result?: HealthResult;
  readiness: Readiness | null;
  error?: string;
  onToggle: () => void;
  onCheck: () => void;
  onCopyAddress: () => void;
  onUnregister: () => void;
}

/** The summary row and, when open, the detail row beneath it — a fragment
 *  rather than a component boundary, because both are `<tr>`s of one table. */
export function NodeRows({
  node,
  isController,
  isOnline,
  isChecking,
  isDeleting,
  isOpen,
  panelId,
  result,
  readiness,
  error,
  onToggle,
  onCheck,
  onCopyAddress,
  onUnregister,
}: NodeRowsProps) {
  const Chevron = isOpen ? ChevronDown : ChevronRight;
  const stale = result?.stale ?? false;

  return (
    <>
      <TableRow className={cn(isOpen && 'border-b-transparent')}>
        {/* The tick is colour only, and StatusTick's contract is that it repeats
            a status the row already states. Offline says so in the Last seen
            cell; the sr-only word covers every other state. */}
        <StatusTickCell status={node.state}>
          <span className="sr-only">{node.state}</span>
        </StatusTickCell>
        <TableCell>
          {/* The expand affordance is the name itself, so the target is the
              width of the cell and still a real button for the keyboard. */}
          <button
            type="button"
            onClick={onToggle}
            aria-expanded={isOpen}
            // The panel row only exists while open, so pointing at it when
            // closed leaves every collapsed row with a dangling IDREF.
            aria-controls={isOpen ? panelId : undefined}
            className="flex items-center gap-2.5 rounded-md text-left outline-none focus-visible:ring-[3px] focus-visible:ring-ring/50"
          >
            <Chevron
              className={cn('size-3.5', isOpen ? 'text-foreground' : 'text-muted-foreground')}
            />
            <span className="flex items-baseline gap-[7px]">
              <span className="font-mono text-[14px] font-semibold">{node.name}</span>
              <span className="text-[11.5px] text-muted-foreground">{node.hostname}</span>
              {isController ? <ControllerChip /> : null}
            </span>
          </button>
        </TableCell>
        <TableCell className="font-mono tabular-nums text-muted-foreground">
          {node.address}
        </TableCell>
        <TableCell className="hidden font-mono tabular-nums text-muted-foreground @min-[1000px]:table-cell">
          {node.version || (node.localAgent ? 'Local agent' : '-')}
        </TableCell>
        <TableCell>
          {readiness ? (
            <span className="flex items-center gap-2.5">
              <span className="flex items-center gap-1.5" aria-hidden>
                {readiness.dots.map((dot) => (
                  <span
                    key={dot.label}
                    title={`${dot.label}: ${dot.word}`}
                    className={cn('size-[7px] rounded-full', TONE_BG[dot.tone])}
                  />
                ))}
              </span>
              <span className="sr-only">
                {readiness.dots.map((d) => `${d.label} ${d.word}`).join(', ')}
              </span>
              <span
                className={cn(
                  'text-[12.5px]',
                  stale ? 'text-muted-foreground' : TONE_TEXT[readiness.tone],
                )}
              >
                {readiness.word}
                {stale ? ' · stale' : ''}
              </span>
            </span>
          ) : (
            <span className="text-[12.5px] text-muted-foreground">not checked</span>
          )}
        </TableCell>
        <TableCell
          className={cn(
            'font-mono tabular-nums',
            isOnline ? 'text-muted-foreground' : TONE_TEXT.bad,
          )}
          title={formatLastSeen(node.lastSeen)}
        >
          {isOnline ? formatAge(node.lastSeen) : `offline · ${formatAge(node.lastSeen)}`}
        </TableCell>
        <TableCell className="pr-5 text-right">
          <span className="inline-flex items-center gap-1.5">
            {isChecking ? (
              <Loader2 className="size-3.5 animate-spin text-muted-foreground" />
            ) : null}
            <NodeMenu
              node={node}
              isDeleting={isDeleting}
              isChecking={isChecking}
              isOnline={isOnline}
              onCheck={onCheck}
              onCopyAddress={onCopyAddress}
              onUnregister={onUnregister}
            />
          </span>
        </TableCell>
      </TableRow>

      {isOpen ? (
        <TableRow className="hover:bg-transparent">
          <TableCell
            colSpan={COLUMN_COUNT}
            id={panelId}
            role="region"
            aria-label={`Detail for ${node.name}`}
            // whitespace-normal: TableCell's nowrap is for one-line data
            // cells and would hold the panel's captions on one line.
            className="h-auto bg-muted/50 p-0 whitespace-normal"
          >
            <div className="py-1 pr-6 pb-[22px] pl-[46px]">
              <NodeDetail
                node={node}
                isOnline={isOnline}
                isChecking={isChecking}
                result={result}
                error={error}
                onCheck={onCheck}
              />
            </div>
          </TableCell>
        </TableRow>
      ) : null}
    </>
  );
}

/** The row's actions, shared by the table row and the phone card so the two
 *  cannot offer different ones. */
export function NodeMenu({
  node,
  isDeleting,
  isChecking,
  isOnline,
  onCheck,
  onCopyAddress,
  onUnregister,
}: {
  node: Node;
  isDeleting: boolean;
  isChecking: boolean;
  isOnline: boolean;
  onCheck: () => void;
  onCopyAddress: () => void;
  onUnregister: () => void;
}) {
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button
          variant="ghost"
          size="icon-sm"
          disabled={isDeleting}
          aria-label={`Actions for ${node.name}`}
        >
          {isDeleting ? <Loader2 className="animate-spin" /> : <MoreHorizontal />}
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" className="w-[220px]">
        {/* Expanding a row already runs this; the item is here for a re-check
            without opening the row, and it is the reason the table no longer
            carries a button per row. */}
        <DropdownMenuItem disabled={isChecking || !isOnline} onSelect={onCheck}>
          <Activity />
          Check health
        </DropdownMenuItem>
        <DropdownMenuItem onSelect={onCopyAddress}>
          <Copy />
          Copy address
        </DropdownMenuItem>
        <DropdownMenuSeparator />
        <DropdownMenuItem variant="destructive" onSelect={onUnregister}>
          <Trash2 />
          Unregister node…
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
