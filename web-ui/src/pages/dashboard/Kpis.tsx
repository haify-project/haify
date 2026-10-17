import type { ReactNode } from 'react';
import { Link } from 'react-router';
import { ArrowUpRight } from 'lucide-react';
import { cn } from '@/lib/utils';
import { Skeleton } from '@/components/ui/skeleton';
import { num, type ClusterSummary } from '@/services/api';
import { fmtCount, fmtSize } from '@/components/charts/chartKit';

// The five numbers across the top of the dashboard.

function Kpi({
  label,
  href,
  value,
  unit,
  note,
  tint,
  loading,
}: {
  label: string;
  href: string;
  value: ReactNode;
  unit?: string;
  note: ReactNode;
  tint?: 'mint' | 'peach';
  loading: boolean;
}) {
  return (
    <Link
      to={href}
      className={cn(
        'tile group block p-4 outline-none focus-visible:ring-[3px] focus-visible:ring-ring/50 sm:p-5',
        tint && `tile-${tint}`,
      )}
    >
      <div className="flex items-center justify-between text-[13px] text-foreground/75">
        {label}
        <ArrowUpRight className="size-4 text-muted-foreground transition-colors group-hover:text-foreground" />
      </div>
      {loading ? (
        <Skeleton className="mt-3 h-10 w-24" />
      ) : (
        <div className="mt-3 text-[40px] leading-none font-semibold tracking-[-0.02em] tabular-nums">
          {value}
          {unit && <span className="ml-0.5 text-[18px] font-medium text-muted-foreground">{unit}</span>}
        </div>
      )}
      <div className="mt-2 truncate text-[12.5px] text-muted-foreground">{note}</div>
    </Link>
  );
}

const Bad = ({ n }: { n: number }) => <b className="font-medium text-status-bad-text">{fmtCount(n)}</b>;
const Warn = ({ n }: { n: number }) => <b className="font-medium text-status-warn-text">{fmtCount(n)}</b>;

export function Kpis({ s, loading }: { s?: ClusterSummary; loading: boolean }) {
  const r = s?.resources ?? {};
  const attention = num(r.resyncing) + num(r.degraded) + num(r.noQuorum);
  const unread = num(r.unknown);
  const n = s?.nodes ?? {};
  const used = num(s?.capacity?.usedBytes);
  const total = num(s?.capacity?.totalBytes);
  const g = s?.gateways ?? {};
  const t = s?.today ?? {};
  const join = (parts: ReactNode[]) =>
    parts.flatMap((p, i) => (i ? [<span key={`s${i}`}> · </span>, p] : [p]));

  return (
    <div className="grid grid-cols-2 gap-4 md:grid-cols-3 xl:grid-cols-5">
      <Kpi
        label="Resources"
        href="/resources"
        tint="mint"
        loading={loading}
        value={fmtCount(num(r.total))}
        note={
          attention ? (
            <>
              <Bad n={attention} /> need attention
            </>
          ) : unread ? (
            <>
              <Warn n={unread} /> without status
            </>
          ) : (
            'all healthy'
          )
        }
      />
      <Kpi
        label="Nodes"
        href="/nodes"
        loading={loading}
        value={fmtCount(num(n.online))}
        unit={`/${fmtCount(num(n.total))}`}
        note={
          num(n.offline) || num(n.draining)
            ? join([
                ...(num(n.offline) ? [<span key="o"><Bad n={num(n.offline)} /> offline</span>] : []),
                ...(num(n.draining) ? [<span key="d"><Warn n={num(n.draining)} /> draining</span>] : []),
              ])
            : 'all online'
        }
      />
      <Kpi
        label="Capacity"
        href="/pools"
        tint="peach"
        loading={loading}
        value={total ? Math.round((used / total) * 100) : '–'}
        unit={total ? '%' : undefined}
        note={total ? `${fmtSize(used)} of ${fmtSize(total)} used` : 'no pools read yet'}
      />
      <Kpi
        label="Gateways"
        href="/gateways"
        loading={loading}
        value={fmtCount(num(g.serving))}
        unit={`/${fmtCount(num(g.total))}`}
        note={
          !num(g.total) ? 'none configured' : num(g.serving) === num(g.total) ? 'all serving' : (
            <>
              <Bad n={num(g.total) - num(g.serving)} /> not serving
            </>
          )
        }
      />
      <Kpi
        label="Events today"
        href="/notifications"
        loading={loading}
        value={fmtCount(num(t.events))}
        note={
          num(t.critical) || num(t.warning)
            ? join([
                <span key="c"><Bad n={num(t.critical)} /> critical</span>,
                <span key="w"><Warn n={num(t.warning)} /> warnings</span>,
              ])
            : 'nothing raised'
        }
      />
    </div>
  );
}
