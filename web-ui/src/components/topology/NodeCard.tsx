import { FLOW_COLOR } from '@/components/flow/FlowLink';
import { formatRate } from '@/components/flow/rates';
import { railTone, TONE_FILL, type PlacedNode } from './placement';

export const NODE_W = 224;
export const NODE_H = 84;

export function SiteFrame({
  x,
  y,
  w,
  h,
  label,
  sub,
  dashed,
}: {
  x: number;
  y: number;
  w: number;
  h: number;
  label: string;
  sub?: string;
  dashed?: boolean;
}) {
  return (
    <g>
      <rect
        x={x}
        y={y}
        width={w}
        height={h}
        rx={12}
        fill="var(--muted)"
        stroke="var(--border)"
        strokeWidth={1}
        // The DR frame is dashed for the same reason its legs are: what is
        // inside it is a copy that lags.
        strokeDasharray={dashed ? '5 4' : undefined}
      />
      <text
        x={x + 18}
        y={y + 22}
        className="font-sans text-[11px] font-medium tracking-[0.07em] uppercase"
        fill="var(--muted-foreground)"
      >
        {label}
      </text>
      {sub && (
        <text
          x={x + w - 18}
          y={y + 22}
          textAnchor="end"
          className="font-mono text-[11px]"
          fill="var(--muted-foreground)"
        >
          {sub}
        </text>
      )}
    </g>
  );
}

/** Rough advance width for the chip's 11px sans label — SVG cannot measure. */
const chipW = (label: string) => label.length * 6.4 + 16;

export function NodeCard({
  x,
  y,
  node,
  backing,
  writeRate,
}: {
  x: number;
  y: number;
  node: PlacedNode;
  backing: string;
  /** KiB/s written on this node, when it is the Primary and reported it. */
  writeRate?: number;
}) {
  const { name, kind, state } = node;
  const diskless = kind === 'tiebreaker' || kind === 'client';
  const isPrimary = state?.role === 'Primary';

  // Diskless nodes are their own kind of thing, and they say so in a word:
  // colour alone would leave "why is this box different" unanswerable.
  const roleLabel = diskless && !isPrimary ? 'Diskless' : state?.role || 'Role unknown';
  const qualifier =
    kind === 'tiebreaker'
      ? 'tiebreaker'
      : kind === 'client'
        ? 'client'
        : kind === 'dr'
          ? 'off-site'
          : undefined;

  const facts = [state?.diskState || 'disk state unknown', diskless ? undefined : backing]
    .filter(Boolean)
    .join(' · ');

  return (
    <g>
      <rect
        x={x}
        y={y}
        width={NODE_W}
        height={NODE_H}
        rx={10}
        fill="var(--card)"
        // A Primary is the node actually serving; it is what an operator looks
        // for first, so it gets the only accented outline in the drawing.
        stroke={isPrimary ? 'var(--primary)' : 'var(--border)'}
        strokeWidth={isPrimary ? 1.6 : 1}
        strokeDasharray={kind === 'dr' || diskless ? '5 4' : undefined}
      />
      <rect
        aria-hidden
        x={x + 3}
        y={y + 10}
        width={3.5}
        height={NODE_H - 20}
        rx={2}
        fill={TONE_FILL[railTone(node)]}
      />
      <text x={x + 16} y={y + 26} className="font-mono text-[13.5px] font-semibold" fill="var(--foreground)">
        {name}
      </text>
      {writeRate !== undefined && (
        <text
          x={x + NODE_W - 12}
          y={y + 26}
          textAnchor="end"
          className="font-mono text-[10.5px] tabular-nums"
          fill={FLOW_COLOR.write}
        >
          {`writes ${formatRate(writeRate)}`}
        </text>
      )}

      {/* RoleChip's vocabulary in SVG: Primary in the accent tint, everything
          else grey, and the word carries the meaning either way. */}
      <rect
        x={x + 16}
        y={y + 36}
        width={chipW(roleLabel)}
        height={19}
        rx={4}
        fill={isPrimary ? 'var(--accent)' : 'var(--secondary)'}
      />
      <text
        x={x + 24}
        y={y + 49.5}
        className={`font-sans text-[11px] ${isPrimary ? 'font-semibold' : ''}`}
        fill={isPrimary ? 'var(--accent-foreground)' : 'var(--secondary-foreground)'}
      >
        {roleLabel}
      </text>
      {qualifier && (
        <text
          x={x + 24 + chipW(roleLabel)}
          y={y + 49.5}
          className="font-mono text-[10.5px]"
          fill="var(--muted-foreground)"
        >
          {qualifier}
        </text>
      )}

      <text x={x + 16} y={y + 72} className="font-mono text-[10.5px]" fill="var(--muted-foreground)">
        {facts}
      </text>
    </g>
  );
}

function Swatch({ dash, color = 'var(--muted-foreground)' }: { dash?: string; color?: string }) {
  return (
    <svg aria-hidden width="18" height="6" className="shrink-0">
      <line x1="1" y1="3" x2="17" y2="3" stroke={color} strokeWidth="2" strokeLinecap="round" strokeDasharray={dash} />
    </svg>
  );
}

function Dot({ color }: { color: string }) {
  return <i aria-hidden className="inline-block size-1.5 rounded-full" style={{ background: color }} />;
}

/** HTML, not SVG, so the words do not scale with the drawing. */
export function Legend({
  protocol,
  hasWAN,
  hasTiebreaker,
  hasClient,
}: {
  protocol: string;
  hasWAN: boolean;
  hasTiebreaker: boolean;
  hasClient: boolean;
}) {
  return (
    <div className="mb-3 flex flex-wrap items-center gap-x-4 gap-y-1.5 text-[11.5px] text-muted-foreground">
      <span className="inline-flex items-center gap-1.5">
        <Swatch color="var(--primary)" />
        {protocol ? `protocol ${protocol}` : 'synchronous'}
      </span>
      {hasWAN && (
        <span className="inline-flex items-center gap-1.5">
          <Swatch dash="4 3" />
          WAN, protocol A
        </span>
      )}
      {hasTiebreaker && (
        <span className="inline-flex items-center gap-1.5">
          <Swatch dash="0.5 4" />
          tiebreaker
        </span>
      )}
      {hasClient && (
        <span className="inline-flex items-center gap-1.5">
          <Swatch dash="6 2 1 2" />
          client
        </span>
      )}
      <span className="inline-flex items-center gap-1.5">
        <Dot color={FLOW_COLOR.write} />
        writes
      </span>
      {hasClient && (
        <span className="inline-flex items-center gap-1.5">
          <Dot color={FLOW_COLOR.read} />
          reads
        </span>
      )}
      <span className="inline-flex items-center gap-1.5">
        <Dot color={FLOW_COLOR.resync} />
        resync
      </span>
      <span className="inline-flex items-center gap-1.5">
        <Swatch dash="4 3" color="var(--status-bad)" />
        disconnected
      </span>
    </div>
  );
}
