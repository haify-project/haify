import { niceMax, useWidth } from './chartKit';

/** Stacked bars, one per slot, each a stack of coloured parts. */
export function StackedBars({
  slots,
  colors,
  labels,
  height = 150,
  emptyFrom,
  ariaLabel,
}: {
  slots: number[][];
  colors: string[];
  /** Axis labels at evenly spaced slots. */
  labels: string[];
  height?: number;
  /** Slots from this index on have not happened yet and draw as a stub. */
  emptyFrom?: number;
  ariaLabel: string;
}) {
  const [ref, width] = useWidth<HTMLDivElement>();
  const axisW = 30;
  const W = Math.max(width - axisW, 10);
  const top = 6;
  const bottom = height - 20;
  const max = niceMax(Math.max(...slots.map((s) => s.reduce((a, b) => a + b, 0)), 1));
  const bw = W / Math.max(slots.length, 1);
  const y = (v: number) => (v / max) * (bottom - top);
  return (
    <div ref={ref} className="w-full" style={{ height }}>
      {width > 0 && (
        <svg width={width} height={height} role="img" aria-label={ariaLabel} className="block">
          {[0, max / 2, max].map((t, k) => (
            <g key={k}>
              <line x1={0} x2={W} y1={bottom - y(t)} y2={bottom - y(t)} className="stroke-border" />
              <text x={W + 6} y={bottom - y(t) + 3.5} className="fill-muted-foreground font-mono text-[10.5px]">
                {Math.round(t)}
              </text>
            </g>
          ))}
          {slots.map((parts, i) => {
            if (emptyFrom !== undefined && i >= emptyFrom) {
              return <rect key={i} x={i * bw + 2} y={bottom - 2} width={Math.max(bw - 4, 1)} height={2} rx={1} className="fill-border" />;
            }
            let acc = 0;
            return (
              <g key={i}>
                <title>{`${parts.reduce((a, b) => a + b, 0)}`}</title>
                {parts.map((v, k) => {
                  if (!v) return null;
                  const h = y(v);
                  acc += h;
                  return <rect key={k} x={i * bw + 2} y={bottom - acc} width={Math.max(bw - 4, 1)} height={h} fill={colors[k]} />;
                })}
              </g>
            );
          })}
          {labels.map((l, k) => (
            <text
              key={l}
              x={(k / (labels.length - 1)) * W}
              y={height - 4}
              textAnchor={k === 0 ? 'start' : k === labels.length - 1 ? 'end' : 'middle'}
              className="fill-muted-foreground font-mono text-[10.5px]"
            >
              {l}
            </text>
          ))}
        </svg>
      )}
    </div>
  );
}

/** Pools by how full they are: ten buckets, the top two marked. */
export function FullnessHistogram({ buckets, height = 150 }: { buckets: number[]; height?: number }) {
  const [ref, width] = useWidth<HTMLDivElement>();
  const top = 18;
  const bottom = height - 20;
  const max = Math.max(...buckets, 1);
  const bw = width / 10;
  const color = (i: number) => (i === 9 ? 'var(--status-bad)' : i === 8 ? 'var(--status-warn)' : 'var(--status-idle)');
  return (
    <div ref={ref} className="w-full" style={{ height }}>
      {width > 0 && (
        <svg width={width} height={height} role="img" aria-label="Pools by how full they are" className="block">
          <line x1={0} x2={width} y1={bottom} y2={bottom} className="stroke-border" />
          {buckets.map((n, i) => {
            const h = n ? Math.max((n / max) * (bottom - top), 3) : 0;
            return (
              <g key={i}>
                <rect x={i * bw + 3} y={bottom - h} width={Math.max(bw - 6, 1)} height={h} rx={3} fill={color(i)} />
                {n > 0 && (
                  <text x={i * bw + bw / 2} y={bottom - h - 4} textAnchor="middle" className="fill-muted-foreground font-mono text-[10.5px]">
                    {n}
                  </text>
                )}
              </g>
            );
          })}
          {['0%', '50%', '100%'].map((t, k) => (
            <text
              key={t}
              x={(k / 2) * width}
              y={height - 4}
              textAnchor={k === 0 ? 'start' : k === 2 ? 'end' : 'middle'}
              className="fill-muted-foreground font-mono text-[10.5px]"
            >
              {t}
            </text>
          ))}
        </svg>
      )}
    </div>
  );
}

/** A ring split into parts; a part too small to see is drawn at a sliver. */
export function Donut({
  parts,
  center,
  caption,
  size = 128,
}: {
  parts: { value: number; color: string }[];
  center: string;
  caption: string;
  size?: number;
}) {
  const c = size / 2;
  const sw = 15;
  const r = c - sw / 2 - 1;
  const C = 2 * Math.PI * r;
  const total = parts.reduce((a, p) => a + p.value, 0);
  let off = 0;
  return (
    <svg width={size} height={size} viewBox={`0 0 ${size} ${size}`} role="img" aria-label={`${center} ${caption}`} className="shrink-0">
      <circle cx={c} cy={c} r={r} fill="none" strokeWidth={sw} className="stroke-border" />
      {total > 0 &&
        parts.map((p, i) => {
          if (!p.value) return null;
          const len = Math.max((p.value / total) * C, 3);
          const el = (
            <circle
              key={i}
              cx={c}
              cy={c}
              r={r}
              fill="none"
              stroke={p.color}
              strokeWidth={sw}
              strokeDasharray={`${Math.max(len - 1.5, 1)} ${C}`}
              strokeDashoffset={-off}
              transform={`rotate(-90 ${c} ${c})`}
            />
          );
          off += len;
          return el;
        })}
      <text x={c} y={c} textAnchor="middle" className="fill-foreground text-[18px] font-semibold">
        {center}
      </text>
      <text x={c} y={c + 16} textAnchor="middle" className="fill-muted-foreground text-[11px]">
        {caption}
      </text>
    </svg>
  );
}
