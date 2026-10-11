import type { FlowKind, LinkKind, LinkState } from './model';
import type { LabelTone } from './labels';
import { dotMotion, useMovingDash } from './motion';

// One replication link in SVG: a base line whose stroke says what the link is
// and whether it is up, plus a moving dotted path per direction data travels.

export const FLOW_COLOR: Record<FlowKind, string> = {
  write: 'var(--primary)',
  read: 'var(--status-ok)',
  resync: 'var(--status-warn)',
};

export interface FlowRun {
  /** Path in the direction the data travels. */
  d: string;
  kind: FlowKind;
  level: number;
}

function baseStroke(kind: LinkKind, state: LinkState) {
  if (state === 'down') return { stroke: 'var(--status-bad)', dash: '6 5', opacity: 0.9 };
  if (state === 'unknown') return { stroke: 'var(--muted-foreground)', dash: '2 5', opacity: 0.45 };
  if (state === 'resync') return { stroke: 'var(--status-warn)', dash: undefined, opacity: 0.45 };
  if (state === 'paused') return { stroke: 'var(--status-warn)', dash: '3 4', opacity: 0.7 };
  if (kind === 'tiebreaker') return { stroke: 'var(--muted-foreground)', dash: '1 5', opacity: 0.6 };
  if (kind === 'client') return { stroke: 'var(--muted-foreground)', dash: '9 3 1 3', opacity: 0.55 };
  if (kind === 'wan') return { stroke: 'var(--muted-foreground)', dash: '6 5', opacity: 0.6 };
  if (state === 'busy') return { stroke: 'var(--primary)', dash: undefined, opacity: 0.35 };
  return { stroke: 'var(--muted-foreground)', dash: undefined, opacity: 0.35 };
}

function Dots({ run, width }: { run: FlowRun; width: number }) {
  const { speed, gap } = dotMotion(run.level);
  const ref = useMovingDash(speed, gap);
  return (
    <path
      ref={ref}
      d={run.d}
      fill="none"
      stroke={FLOW_COLOR[run.kind]}
      strokeWidth={width}
      strokeLinecap="round"
      strokeDasharray={`0.01 ${gap}`}
    />
  );
}

export function FlowLink({
  d,
  kind,
  state,
  runs,
  reduced,
  width = 1.6,
  dot = 3.6,
}: {
  /** The link's own path, either direction. */
  d: string;
  kind: LinkKind;
  state: LinkState;
  runs: FlowRun[];
  reduced: boolean;
  width?: number;
  dot?: number;
}) {
  const base = baseStroke(kind, state);
  return (
    <g>
      <path
        d={d}
        fill="none"
        stroke={base.stroke}
        strokeOpacity={base.opacity}
        strokeWidth={kind === 'tiebreaker' ? width * 0.9 : width}
        strokeLinecap={kind === 'tiebreaker' ? 'round' : undefined}
        strokeDasharray={base.dash}
      />
      {!reduced &&
        state !== 'down' &&
        runs.map((r, i) => <Dots key={`${r.kind}-${i}`} run={r} width={dot} />)}
    </g>
  );
}

/** A label sitting on a link, on a box of the surface colour so lines pass under it. */
export function LinkLabel({
  x,
  y,
  text,
  tone = 'muted',
  halo = 'var(--card)',
  size = 11,
}: {
  x: number;
  y: number;
  text: string;
  tone?: LabelTone;
  halo?: string;
  size?: number;
}) {
  const fill = {
    muted: 'var(--muted-foreground)',
    primary: 'var(--accent-foreground)',
    warn: 'var(--status-warn-text)',
    bad: 'var(--status-bad-text)',
    ok: 'var(--status-ok-text)',
  }[tone];
  // Monospace, so the advance is a fixed fraction of the size.
  const w = text.length * size * 0.6 + 8;
  const h = size + 5;
  return (
    <g>
      <rect x={x - w / 2} y={y - h / 2} width={w} height={h} rx={4} fill={halo} fillOpacity={0.92} />
      <text
        x={x}
        y={y}
        textAnchor="middle"
        dominantBaseline="central"
        className="font-mono tabular-nums"
        style={{ fontSize: size }}
        fill={fill}
      >
        {text}
      </text>
    </g>
  );
}
