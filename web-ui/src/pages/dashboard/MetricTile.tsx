import type { ReactNode } from 'react';
import { Link } from 'react-router';
import { ArrowUpRight } from 'lucide-react';
import { cn } from '@/lib/utils';
import { Skeleton } from '@/components/ui/skeleton';

export type TileTint = 'mint' | 'peach' | 'rose' | 'lilac' | 'sky' | 'plain';

// One reading on the dashboard: its name and a way to the page behind it, the
// figure, a small picture of it, and a footer line of context.

export function MetricTile({
  title,
  href,
  tint = 'plain',
  value,
  unit,
  chip,
  viz,
  footLeft,
  footRight,
  loading,
}: {
  title: string;
  href: string;
  tint?: TileTint;
  value: ReactNode;
  unit?: ReactNode;
  /** A short pill under the figure, e.g. "2 at quorum risk". */
  chip?: { text: string; tone: 'ok' | 'warn' | 'bad' | 'idle' };
  viz?: ReactNode;
  footLeft?: ReactNode;
  footRight?: ReactNode;
  loading?: boolean;
}) {
  return (
    <Link
      to={href}
      className={cn(
        'tile group flex min-h-[176px] flex-col p-4 outline-none transition-transform focus-visible:ring-[3px] focus-visible:ring-ring/50',
        tint !== 'plain' && `tile-${tint}`,
      )}
    >
      <div className="flex items-start justify-between gap-2">
        <span className="text-[13px] font-medium text-foreground/80">{title}</span>
        <ArrowUpRight className="size-4 text-muted-foreground transition-colors group-hover:text-foreground" />
      </div>
      <div className="mt-1 flex min-h-0 flex-1 gap-2">
        <div className="flex min-w-0 flex-1 flex-col">
          {loading ? (
            <Skeleton className="mt-1 h-9 w-16" />
          ) : (
            <span className="text-[34px] leading-[1.15] font-semibold tracking-tight tabular-nums">{value}</span>
          )}
          {unit && <span className="text-[12.5px] text-muted-foreground">{unit}</span>}
          {chip && (
            <span
              className={cn(
                'mt-2 w-fit rounded-full px-2 py-0.5 text-[11.5px] font-medium',
                chip.tone === 'ok' && 'bg-status-ok-soft text-status-ok-text',
                chip.tone === 'warn' && 'bg-status-warn-soft text-status-warn-text',
                chip.tone === 'bad' && 'bg-status-bad-soft text-status-bad-text',
                chip.tone === 'idle' && 'bg-secondary text-muted-foreground',
              )}
            >
              {chip.text}
            </span>
          )}
        </div>
        {viz && <div className="pointer-events-none flex w-[46%] max-w-[120px] shrink-0 items-end justify-end">{viz}</div>}
      </div>
      {(footLeft || footRight) && (
        <div className="relative z-10 mt-2 flex items-end justify-between gap-2 text-[12px]">
          <span className="min-w-0 truncate text-foreground/80">{footLeft}</span>
          <span className="shrink-0 text-muted-foreground tabular-nums">{footRight}</span>
        </div>
      )}
    </Link>
  );
}

/** Concentric rings filled to a fraction, the way a dial reads. */
export function RingsViz({ fraction, color = 'var(--status-ok)' }: { fraction: number; color?: string }) {
  const f = Math.max(0, Math.min(1, fraction));
  const rings = [44, 36, 28];
  return (
    <svg viewBox="0 0 110 110" className="aspect-square w-full" aria-hidden>
      {rings.map((r, i) => (
        <circle key={r} cx={55} cy={55} r={r} fill="none" stroke="var(--foreground)" strokeOpacity={0.08 + i * 0.02} strokeWidth={1} />
      ))}
      <circle
        cx={55}
        cy={55}
        r={44}
        fill="none"
        stroke={color}
        strokeWidth={3}
        strokeLinecap="round"
        strokeDasharray={`${f * 2 * Math.PI * 44} ${2 * Math.PI * 44}`}
        transform="rotate(-90 55 55)"
      />
      <circle cx={55 + 44 * Math.sin(f * 2 * Math.PI)} cy={55 - 44 * Math.cos(f * 2 * Math.PI)} r={4} fill="var(--card)" stroke={color} strokeWidth={2} />
    </svg>
  );
}

/** A half dial from low to high with a marker at value (0..1). */
export function GaugeViz({ fraction, color = 'var(--primary)' }: { fraction: number; color?: string }) {
  const f = Math.max(0, Math.min(1, fraction));
  const r = 40;
  const a = Math.PI * (1 - f);
  return (
    <svg viewBox="0 0 100 58" className="mb-4 w-full" aria-hidden>
      <path d="M 10 52 A 40 40 0 0 1 90 52" fill="none" stroke="var(--foreground)" strokeOpacity={0.1} strokeWidth={6} strokeLinecap="round" />
      <path
        d="M 10 52 A 40 40 0 0 1 90 52"
        fill="none"
        stroke={color}
        strokeWidth={6}
        strokeLinecap="round"
        strokeDasharray={`${f * Math.PI * r} ${Math.PI * r}`}
      />
      <circle cx={50 + r * Math.cos(a)} cy={52 - r * Math.sin(a)} r={4.5} fill="var(--card)" stroke={color} strokeWidth={2.5} />
    </svg>
  );
}

/** Vertical bars, one per value (0..1); `highlight` marks one of them. */
export function BarsViz({
  values,
  highlight,
  color = 'var(--foreground)',
}: {
  values: number[];
  highlight?: number;
  color?: string;
}) {
  const n = Math.max(values.length, 1);
  const w = 100 / n;
  return (
    <svg viewBox="0 0 100 50" preserveAspectRatio="none" className="mb-4 h-[54px] w-full" aria-hidden>
      {values.map((v, i) => {
        // A zero is a sliver, so an empty hour reads as empty, not as noise.
        const h = v > 0 ? Math.max(4, Math.min(1, v) * 48) : 1;
        return (
          <rect
            key={i}
            x={i * w + w * 0.22}
            y={50 - h}
            width={w * 0.56}
            height={h}
            rx={Math.min(1.6, w * 0.2)}
            fill={i === highlight ? 'var(--primary)' : color}
            fillOpacity={i === highlight ? 0.9 : v > 0 ? 0.28 : 0.1}
          />
        );
      })}
    </svg>
  );
}

/** A smooth line through values (0..1), left to right. */
export function WaveViz({ values, color = 'var(--primary)' }: { values: number[]; color?: string }) {
  if (values.length < 2) return null;
  const pts = values.map((v, i) => ({ x: (i / (values.length - 1)) * 100, y: 40 - Math.max(0, Math.min(1, v)) * 34 }));
  let d = `M ${pts[0].x} ${pts[0].y}`;
  for (let i = 1; i < pts.length; i++) {
    const p = pts[i - 1];
    const q = pts[i];
    const mx = (p.x + q.x) / 2;
    d += ` C ${mx} ${p.y}, ${mx} ${q.y}, ${q.x} ${q.y}`;
  }
  return (
    <svg viewBox="0 0 100 44" preserveAspectRatio="none" className="mb-4 h-[46px] w-full" aria-hidden>
      <path d={d} fill="none" stroke={color} strokeWidth={1.8} vectorEffect="non-scaling-stroke" />
    </svg>
  );
}
