import { cn } from '@/lib/utils';
import { Skeleton } from '@/components/ui/skeleton';

export interface StatFilter<T extends string> {
  value: T;
  label: string;
  count?: number;
  /** A dot in the state's colour before the label. */
  color?: string;
}

/**
 * The counts across the top of a list, which are also its filter: each card is
 * a number the reader wants and a click that shows exactly those rows.
 */
export function StatFilterStrip<T extends string>({
  options,
  value,
  onChange,
  loading,
  'aria-label': ariaLabel,
}: {
  options: StatFilter<T>[];
  value: T;
  onChange: (v: T) => void;
  loading?: boolean;
  'aria-label': string;
}) {
  return (
    <div
      role="group"
      aria-label={ariaLabel}
      className="grid grid-cols-2 gap-3 @min-[560px]:grid-cols-3 @min-[780px]:grid-cols-[repeat(var(--n),minmax(0,1fr))]"
      style={{ '--n': options.length } as React.CSSProperties}
    >
      {options.map((o) => {
        const on = o.value === value;
        return (
          <button
            key={o.value}
            type="button"
            aria-pressed={on}
            onClick={() => onChange(o.value)}
            className={cn(
              'min-w-0 rounded-[14px] border bg-card px-4 py-3 text-left transition-colors outline-none focus-visible:ring-[3px] focus-visible:ring-ring/50',
              on ? 'border-foreground shadow-[inset_0_0_0_1px_var(--foreground)]' : 'border-border hover:border-ring/40',
            )}
          >
            <span className="flex items-center gap-1.5 truncate text-[12.5px] text-foreground/75">
              {o.color && <span className="size-[7px] shrink-0 rounded-full" style={{ background: o.color }} />}
              {o.label}
            </span>
            {loading ? (
              <Skeleton className="mt-1.5 h-[26px] w-14" />
            ) : (
              <span className="mt-1 block text-[26px] leading-tight font-semibold tracking-[-0.01em] tabular-nums">
                {(o.count ?? 0).toLocaleString('en-US')}
              </span>
            )}
          </button>
        );
      })}
    </div>
  );
}
