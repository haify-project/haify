import { cn } from '@/lib/utils';
import { TONE_BG, TONE_TEXT } from '@/components/status';
import { formatRate } from '@/components/flow/rates';
import type { MapNode } from './clusterModel';

const plural = (n: number, word: string) => `${n} ${word}${n === 1 ? '' : 's'}`;

function poolTone(pct: number) {
  if (pct >= 90) return 'bg-status-bad';
  if (pct >= 80) return 'bg-status-warn';
  return 'bg-primary';
}

export function NodeTile({ node, width, height }: { node: MapNode; width: number; height: number }) {
  const serving = node.primaries > 0;
  const extras = [
    node.tiebreakers ? plural(node.tiebreakers, 'tiebreaker') : '',
    node.clients ? plural(node.clients, 'client') : '',
  ].filter(Boolean);
  return (
    <div
      className={cn(
        'flex flex-col gap-1.5 overflow-hidden rounded-xl border bg-card px-3 py-2.5 shadow-xs',
        serving ? 'border-primary/70' : 'border-border',
        node.tone === 'bad' && 'border-status-bad/70',
      )}
      style={{ width, height }}
    >
      <div className="flex min-w-0 items-center justify-between gap-2">
        <span className="truncate font-mono text-[13.5px] font-semibold" title={node.name}>
          {node.name}
        </span>
        <span className={cn('flex shrink-0 items-center gap-1 text-[11px]', TONE_TEXT[node.tone])}>
          <i aria-hidden className={cn('size-1.5 rounded-full', TONE_BG[node.tone])} />
          {node.state.toLowerCase()}
        </span>
      </div>

      {node.poolPct !== undefined ? (
        <div className="flex items-center gap-2 text-[11px] text-muted-foreground">
          <span className="relative h-1.5 min-w-0 flex-1 overflow-hidden rounded-full bg-muted">
            <span
              className={cn('absolute inset-y-0 left-0 rounded-full', poolTone(node.poolPct))}
              style={{ width: `${Math.min(100, Math.max(1, node.poolPct))}%` }}
            />
          </span>
          <span className="shrink-0 font-mono tabular-nums">{Math.round(node.poolPct)}%</span>
        </div>
      ) : (
        <div className="text-[11px] text-muted-foreground">no pool</div>
      )}

      <div className="truncate text-[11.5px] text-muted-foreground">
        <span className={cn(serving && 'font-medium text-accent-foreground')}>
          {node.primaries} primary
        </span>
        {' · '}
        {plural(node.replicas, 'replica')}
      </div>
      {extras.length > 0 && (
        <div className="truncate text-[11px] text-muted-foreground">{extras.join(' · ')}</div>
      )}
      {node.writeRate !== undefined && (
        <div className="mt-auto truncate font-mono text-[11px] text-primary tabular-nums">
          writes {formatRate(node.writeRate)}
        </div>
      )}
    </div>
  );
}
