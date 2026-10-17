import { ChevronLeft, ChevronRight } from 'lucide-react';
import { cn } from '@/lib/utils';

/** Which page numbers to offer: the ends, and two either side of the current. */
function pageList(page: number, pages: number): (number | '…')[] {
  const out: (number | '…')[] = [];
  for (let p = 1; p <= pages; p++) {
    if (p === 1 || p === pages || Math.abs(p - page) <= 1) {
      out.push(p);
    } else if (out[out.length - 1] !== '…') {
      out.push('…');
    }
  }
  return out;
}

/** "1–50 of 3,412" and the page buttons under a list. */
export function Pager({
  offset,
  limit,
  total,
  onOffset,
}: {
  offset: number;
  limit: number;
  total: number;
  onOffset: (o: number) => void;
}) {
  const pages = Math.max(Math.ceil(total / limit), 1);
  const page = Math.floor(offset / limit) + 1;
  const go = (p: number) => onOffset((Math.min(Math.max(p, 1), pages) - 1) * limit);
  const btn = 'flex h-[30px] min-w-[30px] items-center justify-center rounded-lg px-1.5 tabular-nums transition-colors';
  return (
    <div className="flex items-center justify-between gap-3 border-t border-border px-4 py-3 text-[12.5px] text-muted-foreground">
      <span className="tabular-nums">
        {total === 0
          ? 'None'
          : `${(offset + 1).toLocaleString('en-US')}–${Math.min(offset + limit, total).toLocaleString('en-US')} of ${total.toLocaleString('en-US')}`}
      </span>
      {pages > 1 && (
        <nav aria-label="Pages" className="flex items-center gap-1">
          <button type="button" aria-label="Previous page" disabled={page === 1} onClick={() => go(page - 1)} className={cn(btn, 'hover:bg-secondary disabled:opacity-40')}>
            <ChevronLeft className="size-4" />
          </button>
          {pageList(page, pages).map((p, i) =>
            p === '…' ? (
              <span key={`e${i}`} className="px-1">
                …
              </span>
            ) : (
              <button
                key={p}
                type="button"
                aria-current={p === page ? 'page' : undefined}
                onClick={() => go(p)}
                className={cn(btn, p === page ? 'bg-secondary font-medium text-foreground' : 'text-foreground/75 hover:bg-secondary')}
              >
                {p}
              </button>
            ),
          )}
          <button type="button" aria-label="Next page" disabled={page === pages} onClick={() => go(page + 1)} className={cn(btn, 'hover:bg-secondary disabled:opacity-40')}>
            <ChevronRight className="size-4" />
          </button>
        </nav>
      )}
    </div>
  );
}
