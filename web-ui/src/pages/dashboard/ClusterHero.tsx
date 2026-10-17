import { cn } from '@/lib/utils';
import { toneOf, TONE_TEXT, type StatusTone } from '@/components/status';

// The dashboard's picture of the cluster: one server per node, stacked, with
// its state on its light, the replication between them drawn as moving dots,
// and three readings pinned to the drawing the way a body chart pins vitals.

export interface HeroNode {
  name: string;
  state: string;
  active: boolean;
}

export interface HeroCallout {
  label: string;
  value: string;
  unit?: string;
  status: string;
  tone: StatusTone;
}

const W = 420;
const H = 520;
const SLAB_W = 214;
const SLAB_H = 58;
const GAP = 16;
// The servers sit right of centre: the left third holds two readings, the
// space above the stack the third.
const X0 = 150;
const SIDE = 22;

const LED: Record<StatusTone, string> = {
  ok: 'var(--status-ok)',
  warn: 'var(--status-warn)',
  bad: 'var(--status-bad)',
  idle: 'var(--status-idle)',
};

function Slab({ y, node }: { y: number; node: HeroNode }) {
  const tone = toneOf(node.state);
  return (
    <g>
      <path
        d={`M ${X0 + 10} ${y} L ${X0 + 10 + SIDE} ${y - SIDE} L ${X0 + SLAB_W + SIDE} ${y - SIDE} L ${X0 + SLAB_W} ${y} Z`}
        fill="url(#hero-top)"
      />
      <path
        d={`M ${X0 + SLAB_W} ${y + 2} L ${X0 + SLAB_W + SIDE} ${y - SIDE + 2} L ${X0 + SLAB_W + SIDE} ${y + SLAB_H - SIDE - 2} L ${X0 + SLAB_W} ${y + SLAB_H - 2} Z`}
        fill="url(#hero-side)"
      />
      <rect x={X0} y={y} width={SLAB_W} height={SLAB_H} rx={11} fill="url(#hero-face)" stroke="var(--border)" />
      {Array.from({ length: 6 }, (_, i) => (
        <rect key={i} x={X0 + 96 + i * 18} y={y + 17} width={12} height={24} rx={3} fill="url(#hero-bay)" />
      ))}
      <circle cx={X0 + 22} cy={y + SLAB_H / 2} r={9} fill={LED[tone]} opacity={0.18} />
      <circle cx={X0 + 22} cy={y + SLAB_H / 2} r={4.2} fill={LED[tone]} />
    </g>
  );
}

function Callout({
  c,
  side,
  top,
}: {
  c: HeroCallout;
  side: 'left' | 'right';
  top: number;
}) {
  return (
    <div
      className={cn('absolute w-[33%] min-w-[104px]', side === 'left' ? 'left-0' : 'right-0 text-right')}
      style={{ top: `${(top / H) * 100}%`, transform: 'translateY(-50%)' }}
    >
      <div className="text-[12px] text-muted-foreground">{c.label}</div>
      <div className="mt-0.5 flex items-baseline gap-1 whitespace-nowrap" style={{ justifyContent: side === 'right' ? 'flex-end' : undefined }}>
        <span className="truncate text-[20px] font-semibold tracking-tight tabular-nums">{c.value}</span>
        {c.unit && <span className="text-[12.5px] text-muted-foreground">{c.unit}</span>}
      </div>
      <div className={cn('text-[11.5px]', c.tone === 'ok' ? 'text-muted-foreground' : TONE_TEXT[c.tone])}>
        {c.status}
      </div>
    </div>
  );
}

/** Where on the drawing each callout points, and where its label sits. */
function anchors(count: number) {
  const top = H / 2 - (count * SLAB_H + (count - 1) * GAP) / 2 + 10;
  const slabY = (i: number) => top + i * (SLAB_H + GAP);
  return { top, slabY };
}

export function ClusterHero({
  nodes,
  callouts,
  reduced,
}: {
  nodes: HeroNode[];
  /** Three readings: upper left, above the stack on the right, lower left. */
  callouts: [HeroCallout, HeroCallout, HeroCallout];
  reduced: boolean;
}) {
  // Up to five servers are drawn; a larger cluster is still five on the
  // picture, the count is in the reading.
  const shown = nodes.length > 0 ? nodes.slice(0, 5) : [{ name: '—', state: 'unknown', active: false }];
  const { slabY } = anchors(shown.length);
  const last = shown.length - 1;
  const ledX = X0 + 22;
  const ledY = (i: number) => slabY(i) + SLAB_H / 2;
  const lowLabel = Math.max(ledY(last), ledY(0) + 96);

  // Pins: the first server's light, the top of the stack, the last server's
  // bays.
  const pins = [
    { x: ledX, y: ledY(0), side: 'left' as const, labelY: ledY(0) },
    { x: X0 + SLAB_W - 20, y: slabY(0) - SIDE / 2, side: 'right' as const, labelY: Math.max(36, slabY(0) - SIDE - 52) },
    { x: X0 + 132, y: ledY(last), side: 'left' as const, labelY: lowLabel },
  ];
  const labelEdge = (side: 'left' | 'right') => (side === 'left' ? W * 0.3 : W * 0.7);

  return (
    <div className="relative mx-auto w-full max-w-[440px]" style={{ aspectRatio: `${W} / ${H}` }}>
      <svg viewBox={`0 0 ${W} ${H}`} className="absolute inset-0 h-full w-full" aria-hidden>
        <defs>
          <radialGradient id="hero-glow" cx="55%" cy="48%" r="55%">
            <stop offset="0%" stopColor="var(--hero-glow)" stopOpacity="1" />
            <stop offset="100%" stopColor="var(--hero-glow)" stopOpacity="0" />
          </radialGradient>
          <linearGradient id="hero-face" x1="0" y1="0" x2="0" y2="1">
            <stop offset="0%" stopColor="var(--card)" />
            <stop offset="100%" stopColor="var(--muted)" />
          </linearGradient>
          <linearGradient id="hero-top" x1="0" y1="1" x2="0" y2="0">
            <stop offset="0%" stopColor="var(--card)" />
            <stop offset="100%" stopColor="var(--secondary)" />
          </linearGradient>
          <linearGradient id="hero-side" x1="0" y1="0" x2="1" y2="0">
            <stop offset="0%" stopColor="var(--secondary)" />
            <stop offset="100%" stopColor="var(--border)" />
          </linearGradient>
          <linearGradient id="hero-bay" x1="0" y1="0" x2="0" y2="1">
            <stop offset="0%" stopColor="var(--border)" />
            <stop offset="100%" stopColor="var(--secondary)" />
          </linearGradient>
        </defs>
        <ellipse cx={W * 0.55} cy={H * 0.48} rx={W * 0.5} ry={H * 0.46} fill="url(#hero-glow)" />
        <ellipse cx={X0 + SLAB_W / 2 + 10} cy={slabY(last) + SLAB_H + 26} rx={SLAB_W * 0.62} ry={14} fill="var(--foreground)" opacity={0.06} />

        {/* Bottom up, so each server hides the top of the one below it. */}
        {shown
          .map((n, i) => ({ n, i }))
          .reverse()
          .map(({ n, i }) => (
            <Slab key={`${n.name}-${i}`} y={slabY(i)} node={n} />
          ))}

        {pins.map((p, i) => {
          const ex = labelEdge(p.side);
          return (
            <g key={i}>
              <polyline
                points={
                  p.side === 'left'
                    ? `${p.x},${p.y} ${ex + 14},${p.labelY} ${ex},${p.labelY}`
                    : `${p.x},${p.y} ${p.x},${p.labelY + 30}`
                }
                fill="none"
                stroke="var(--muted-foreground)"
                strokeOpacity={0.45}
                strokeWidth={1}
              />
              <circle cx={p.x} cy={p.y} r={11} fill="var(--card)" fillOpacity={0.75} stroke="var(--border)" />
              <circle cx={p.x} cy={p.y} r={5.5} fill="var(--card)" stroke={LED[callouts[i].tone]} strokeWidth={3} />
            </g>
          );
        })}
      </svg>
      {pins.map((p, i) => (
        <Callout key={i} c={callouts[i]} side={p.side} top={p.labelY} />
      ))}
    </div>
  );
}
