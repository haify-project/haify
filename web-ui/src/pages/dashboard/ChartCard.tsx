import type { ReactNode } from 'react';
import { cn } from '@/lib/utils';

/** A card holding one chart: its name, the figure it is about, a legend. */
export function ChartCard({
  title,
  value,
  unit,
  meta,
  legend,
  className,
  children,
}: {
  title: string;
  value?: ReactNode;
  unit?: ReactNode;
  meta?: ReactNode;
  legend?: { name: string; color: string }[];
  className?: string;
  children: ReactNode;
}) {
  return (
    <section className={cn('tile flex min-w-0 flex-col p-4 sm:p-5', className)}>
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <h3 className="text-[13px] font-medium text-foreground/75">{title}</h3>
          {value !== undefined && (
            <div className="mt-1 text-[24px] leading-tight font-semibold tracking-[-0.01em] tabular-nums">
              {value}
              {unit && <span className="ml-1.5 text-[13px] font-normal text-muted-foreground">{unit}</span>}
            </div>
          )}
          {meta && <div className="mt-0.5 text-[12px] text-muted-foreground">{meta}</div>}
        </div>
        {legend && (
          <div className="flex flex-wrap justify-end gap-x-3.5 gap-y-1 text-[12px] text-foreground/75">
            {legend.map((l) => (
              <span key={l.name} className="flex items-center gap-1.5">
                <span className="size-2 rounded-[2px]" style={{ background: l.color }} />
                {l.name}
              </span>
            ))}
          </div>
        )}
      </div>
      <div className="mt-3 min-w-0 flex-1">{children}</div>
    </section>
  );
}

/** What a chart shows while it has nothing to draw. */
export function ChartEmpty({ children, height = 150 }: { children: ReactNode; height?: number }) {
  return (
    <div className="flex items-center justify-center text-[12.5px] text-muted-foreground" style={{ height }}>
      {children}
    </div>
  );
}
