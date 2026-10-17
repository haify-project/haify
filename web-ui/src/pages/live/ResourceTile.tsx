import { Link } from 'react-router';
import { cn } from '@/lib/utils';
import { TONE_BG, TONE_TEXT } from '@/components/status';
import { FlowLink, LinkLabel, type FlowRun } from '@/components/flow/FlowLink';
import { edgeLabel } from '@/components/flow/labels';
import { formatRate } from '@/components/flow/rates';
import { useElementWidth } from '@/components/flow/useLiveStatus';
import type { FlowEdge, LinkKind } from '@/components/flow/model';
import type { ResourceCard } from './resourceModel';

// One resource on the live map: the node serving it on the left, a line to
// every other member on the right, with what is moving on each.

const ROW = 34;
const PILL_H = 26;
const GUTTER = 64;

const KIND_MARK: Partial<Record<LinkKind, string>> = { tiebreaker: 'tiebreaker', client: 'client', wan: 'DR' };

function runsOf(edge: FlowEdge | undefined, fwd: string, back: string, from: string): FlowRun[] {
  if (!edge) return [];
  return edge.flows.map((f) => ({ d: f.from === from ? fwd : back, kind: f.kind, level: f.level }));
}

function Pill({
  name,
  kind,
  primary,
  disk,
  width,
}: {
  name: string;
  kind: LinkKind;
  primary: boolean;
  disk?: string;
  width: number;
}) {
  const marks = [primary ? 'primary' : '', KIND_MARK[kind] ?? ''].filter(Boolean);
  return (
    <div
      className={cn(
        'flex items-center gap-1.5 overflow-hidden rounded-md border bg-card px-2 text-[11.5px]',
        primary ? 'border-primary/70' : 'border-border',
        disk && 'border-status-warn/70',
      )}
      style={{ width, height: PILL_H }}
      title={[name, ...marks, disk].filter(Boolean).join(' · ')}
    >
      <span className={cn('min-w-0 truncate font-mono', primary && 'font-semibold')}>{name}</span>
      {marks.length > 0 && (
        <span className={cn('shrink-0 text-[10.5px]', primary ? 'text-accent-foreground' : 'text-muted-foreground')}>
          {marks.join(' · ')}
        </span>
      )}
      {disk && <span className="shrink-0 text-[10.5px] text-status-warn-text">{disk}</span>}
    </div>
  );
}

function Diagram({ card, reduced }: { card: ResourceCard; reduced: boolean }) {
  const [ref, width] = useElementWidth<HTMLDivElement>();
  const rows = Math.max(1, card.peers.length);
  const height = rows * ROW;
  const pillW = Math.max(96, Math.floor((width - GUTTER) / 2));
  const cx = pillW;
  const px = width - pillW;
  const cy = height / 2;
  const rowY = (i: number) => i * ROW + ROW / 2;
  const centre = card.centre;

  return (
    <div ref={ref} className="relative w-full" style={{ height }}>
      {width > 0 && centre && (
        <>
          <svg className="absolute inset-0" width={width} height={height} aria-hidden>
            {card.peers.map((p, i) => {
              const y = rowY(i);
              const fwd = `M ${cx} ${cy} C ${cx + GUTTER / 2} ${cy}, ${px - GUTTER / 2} ${y}, ${px} ${y}`;
              const back = `M ${px} ${y} C ${px - GUTTER / 2} ${y}, ${cx + GUTTER / 2} ${cy}, ${cx} ${cy}`;
              const e = p.edge;
              return (
                <FlowLink
                  key={p.name}
                  d={fwd}
                  kind={e?.kind ?? p.kind}
                  state={e?.state ?? 'unknown'}
                  runs={runsOf(e, fwd, back, centre.name)}
                  reduced={reduced}
                  width={1.6}
                  dot={3.2}
                />
              );
            })}
          </svg>
          <div className="absolute left-0" style={{ top: cy - PILL_H / 2 }}>
            <Pill {...centre} width={pillW} />
          </div>
          {card.peers.map((p, i) => (
            <div key={p.name} className="absolute" style={{ left: px, top: rowY(i) - PILL_H / 2 }}>
              <Pill name={p.name} kind={p.kind} primary={p.primary} disk={p.disk} width={pillW} />
            </div>
          ))}
          <svg className="pointer-events-none absolute inset-0" width={width} height={height}>
            {card.peers.map((p, i) => {
              const l = edgeLabel(p.edge) ?? edgeLabel(p.other);
              if (!l) return null;
              const text = l.sub ? `${l.text} · ${l.sub}` : l.text;
              return (
                <LinkLabel key={p.name} x={cx + (px - cx) / 2} y={(cy + rowY(i)) / 2} tone={l.tone} size={10.5} text={text} />
              );
            })}
          </svg>
        </>
      )}
    </div>
  );
}

export function ResourceTile({ card, reduced }: { card: ResourceCard; reduced: boolean }) {
  return (
    <div
      className={cn(
        'flex min-w-0 flex-col gap-2.5 rounded-xl border bg-card px-3.5 py-3 shadow-xs',
        card.tone === 'bad' ? 'border-status-bad/70' : card.tone === 'warn' ? 'border-status-warn/70' : 'border-border',
      )}
    >
      <div className="flex min-w-0 items-center justify-between gap-2">
        <Link
          to={`/resources?q=${encodeURIComponent(card.name)}`}
          className="min-w-0 truncate font-mono text-[13px] font-semibold hover:text-primary hover:underline"
          title={card.name}
        >
          {card.name}
        </Link>
        <span
          className={cn('flex shrink-0 items-center gap-1 text-[11px]', TONE_TEXT[card.tone])}
          title={card.healthTitle}
        >
          <i aria-hidden className={cn('size-1.5 rounded-full', TONE_BG[card.tone])} />
          {card.health}
        </span>
      </div>

      {card.usedBy.length > 0 && (
        <div className="flex min-w-0 flex-wrap gap-1">
          {card.usedBy.map((u) => (
            <span
              key={u}
              className="max-w-full truncate rounded bg-secondary px-1.5 py-0.5 text-[11px] text-secondary-foreground"
              title={u}
            >
              {u}
            </span>
          ))}
        </div>
      )}

      {card.pending ? (
        <div className="h-[34px] animate-pulse rounded-md bg-muted" />
      ) : card.centre ? (
        <Diagram card={card} reduced={reduced} />
      ) : (
        <p className="text-[11.5px] text-muted-foreground">No members</p>
      )}

      <div className="mt-auto flex items-center justify-between text-[11px] text-muted-foreground">
        <span className="font-mono tabular-nums">{card.sizeGb} GB</span>
        {card.writeRate !== undefined && (
          <span className="font-mono text-primary tabular-nums">writes {formatRate(card.writeRate)}</span>
        )}
      </div>
    </div>
  );
}
