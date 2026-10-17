import { useState } from 'react';
import { fmtTick, niceMax, smoothPath, useWidth, type Pt } from './chartKit';

export interface LineSeries {
  name: string;
  values: number[];
  color: string;
  /** Fill the area under the line. */
  area?: boolean;
  width?: number;
  /** Draw as thin bars instead of a line (error counts). */
  bars?: boolean;
  dashed?: boolean;
}

/** The runs of finite values, as point lists; NaN breaks a line. */
function runs(values: number[], x: (i: number) => number, y: (v: number) => number): Pt[][] {
  const out: Pt[][] = [];
  let cur: Pt[] = [];
  values.forEach((v, i) => {
    if (Number.isFinite(v)) {
      cur.push([x(i), y(v)]);
    } else if (cur.length) {
      out.push(cur);
      cur = [];
    }
  });
  if (cur.length) out.push(cur);
  return out;
}

/**
 * Lines over time with a value axis on the right. `band` draws a shaded range
 * (lowest to highest node) behind the first series. Hovering shows each
 * series' value at that moment.
 */
export function LineChart({
  times,
  series,
  band,
  height = 180,
  format,
  max: fixedMax,
  floor = 0,
  threshold,
  xLabels,
  ariaLabel,
}: {
  times: number[];
  series: LineSeries[];
  band?: { lo: number[]; hi: number[]; color: string };
  height?: number;
  format: (v: number) => string;
  max?: number;
  /** The least the axis reaches, so a flat zero line does not get an axis of
   *  fractions of a byte. */
  floor?: number;
  threshold?: { value: number; color: string };
  /** Axis labels at chosen points; by default the start, middle and "now". */
  xLabels?: { i: number; text: string }[];
  ariaLabel: string;
}) {
  const [ref, width] = useWidth<HTMLDivElement>();
  const [hover, setHover] = useState<number | null>(null);
  const n = times.length;
  const axisW = 58;
  const W = Math.max(width - axisW, 10);
  const top = 6;
  const bottom = height - 20;
  const peak = Math.max(
    ...series.flatMap((s) => s.values.filter(Number.isFinite)),
    ...(band?.hi ?? []),
    threshold?.value ?? 0,
    0,
  );
  const max = fixedMax ?? niceMax(Math.max(peak, floor));
  const y = (v: number) => bottom - (Math.min(v, max) / max) * (bottom - top);
  const x = (i: number) => (n <= 1 ? 0 : (i / (n - 1)) * W);
  const ticks = [0, max / 2, max];
  const span = n > 1 ? times[n - 1] - times[0] : 0;
  const labels =
    xLabels ??
    (n > 1
      ? [...new Set([0, Math.floor((n - 1) / 2), n - 1])].map((i) => ({ i, text: i === n - 1 ? 'now' : fmtTick(times[i], span) }))
      : []);

  return (
    <div ref={ref} className="relative w-full" style={{ height }}>
      {width > 0 && (
        <svg width={width} height={height} role="img" aria-label={ariaLabel} className="block overflow-visible">
          {ticks.map((t, k) => (
            <g key={k}>
              <line x1={0} x2={W} y1={y(t)} y2={y(t)} className="stroke-border" />
              <text x={W + 8} y={y(t) + 3.5} className="fill-muted-foreground font-mono text-[10.5px]">
                {format(t)}
              </text>
            </g>
          ))}
          {threshold && (
            <line x1={0} x2={W} y1={y(threshold.value)} y2={y(threshold.value)} stroke={threshold.color} strokeDasharray="4 4" />
          )}
          {band && n > 1 && (
            <path
              d={`${smoothPath(band.hi.map((v, i) => [x(i), y(v)] as Pt))} L${band.lo
                .map((v, i) => `${x(i).toFixed(1)},${y(v).toFixed(1)}`)
                .reverse()
                .join(' L')} Z`}
              fill={band.color}
              opacity={0.13}
            />
          )}
          {series.map((s) => {
            if (s.bars) {
              const bw = Math.max(W / Math.max(n, 1) - 1.5, 1);
              return (
                <g key={s.name} fill={s.color}>
                  {s.values.map((v, i) =>
                    v > 0 ? <rect key={i} x={x(i) - bw / 2} y={y(v)} width={bw} height={bottom - y(v)} opacity={0.85} /> : null,
                  )}
                </g>
              );
            }
            return (
              <g key={s.name}>
                {runs(s.values, x, y).map((pts, k) => {
                  const d = smoothPath(pts);
                  const x0 = pts[0][0];
                  const x1 = pts[pts.length - 1][0];
                  return (
                    <g key={k}>
                      {s.area && pts.length > 1 && (
                        <path d={`${d} L${x1},${bottom} L${x0},${bottom} Z`} fill={s.color} opacity={0.09} />
                      )}
                      <path
                        d={d}
                        fill="none"
                        stroke={s.color}
                        strokeWidth={s.width ?? 2}
                        strokeLinejoin="round"
                        strokeDasharray={s.dashed ? '3 4' : undefined}
                      />
                    </g>
                  );
                })}
              </g>
            );
          })}
          {labels.map(({ i, text }, k) => (
            <text
              key={k}
              x={x(i)}
              y={height - 4}
              textAnchor={i === 0 ? 'start' : i === n - 1 ? 'end' : 'middle'}
              className="fill-muted-foreground font-mono text-[10.5px]"
            >
              {text}
            </text>
          ))}
          {hover !== null && (
            <g>
              <line x1={x(hover)} x2={x(hover)} y1={top} y2={bottom} className="stroke-foreground/30" />
              {series.map((s) =>
                s.bars || !Number.isFinite(s.values[hover]) ? null : (
                  <circle key={s.name} cx={x(hover)} cy={y(s.values[hover] ?? 0)} r={3.5} fill="var(--card)" stroke={s.color} strokeWidth={2} />
                ),
              )}
            </g>
          )}
          <rect
            x={0}
            y={0}
            width={W}
            height={bottom}
            fill="transparent"
            onMouseMove={(e) => {
              const box = (e.currentTarget as SVGRectElement).getBoundingClientRect();
              const i = Math.round(((e.clientX - box.left) / box.width) * (n - 1));
              setHover(n > 0 ? Math.max(0, Math.min(n - 1, i)) : null);
            }}
            onMouseLeave={() => setHover(null)}
          />
        </svg>
      )}
      {hover !== null && width > 0 && (
        <div
          className="pointer-events-none absolute top-0 z-10 rounded-md border border-border bg-popover px-2 py-1.5 text-[11.5px] shadow-sm"
          style={{ left: Math.min(x(hover) + 10, Math.max(W - 150, 0)) }}
        >
          <div className="font-mono text-muted-foreground">{fmtTick(times[hover], 0)}</div>
          {series.filter((s) => Number.isFinite(s.values[hover])).map((s) => (
            <div key={s.name} className="flex items-center gap-1.5 whitespace-nowrap">
              <span className="size-2 rounded-[2px]" style={{ background: s.color }} />
              {s.name} <span className="ml-auto pl-2 font-mono tabular-nums">{format(s.values[hover] ?? 0)}</span>
            </div>
          ))}
          {band && (
            <div className="whitespace-nowrap text-muted-foreground">
              range <span className="font-mono">{format(band.lo[hover] ?? 0)} – {format(band.hi[hover] ?? 0)}</span>
            </div>
          )}
        </div>
      )}
    </div>
  );
}
