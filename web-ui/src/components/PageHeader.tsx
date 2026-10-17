import type { ReactNode } from 'react';
import { cn } from '@/lib/utils';

/**
 * The band at the top of every page: title, a one-line factual subtitle, and
 * the page's actions. `<main>` already supplies the horizontal padding, so the
 * header spans the full content width.
 *
 * `description` is a node rather than a string on purpose — the subtitles are
 * mostly counts and identifiers, and the caller wraps the identifiers in
 * `<span className="font-mono text-foreground">` so they read as machine names.
 */
export function PageHeader({
  title,
  description,
  actions,
  className,
}: {
  title: string;
  description?: ReactNode;
  actions?: ReactNode;
  className?: string;
}) {
  return (
    <div
      data-slot="page-header"
      className={cn(
        // Stacked on a phone: a non-wrapping row squeezed the subtitle to one
        // word per line and pushed the actions off-screen, taking the whole
        // page into horizontal scroll with them.
        'mb-5 flex flex-col gap-3 sm:flex-row sm:items-end sm:justify-between sm:gap-4',
        className,
      )}
    >
      <div className="min-w-0">
        <h1 className="text-[26px] leading-tight font-semibold tracking-[-0.015em]">{title}</h1>
        {description ? (
          <div className="mt-1.5 text-[13px] text-balance text-muted-foreground">{description}</div>
        ) : null}
      </div>
      {/* Actions wrap rather than overflow; on a phone they sit under the title
          and stay reachable instead of being clipped at the viewport edge. */}
      {actions ? (
        <div className="flex flex-wrap items-center gap-2.5 sm:shrink-0 sm:flex-nowrap">{actions}</div>
      ) : null}
    </div>
  );
}
