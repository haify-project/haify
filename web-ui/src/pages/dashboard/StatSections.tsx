import type { ReactNode } from 'react';
import { Link } from 'react-router';
import { ArrowUpRight } from 'lucide-react';
import { cn } from '@/lib/utils';
import { num, type ClusterSummary } from '@/services/api';
import { eventTypeLabel } from '@/services/events';
import { fmtCount, fmtRate, fmtSize } from '@/components/charts/chartKit';

// Six cards of counts under the charts. Every row is one number, so the page
// reads the same for five nodes or five hundred.

type Tone = 'ok' | 'warn' | 'bad' | undefined;
const TONE: Record<string, string> = { ok: 'text-status-ok-text', warn: 'text-status-warn-text', bad: 'text-status-bad-text' };

function Row({ k, v, tone }: { k: ReactNode; v: ReactNode; tone?: Tone }) {
  return (
    <div className="flex items-baseline justify-between gap-3 border-b border-border py-2.5 text-[13.5px] last:border-0 last:pb-0">
      <span className="min-w-0 truncate text-foreground/80">{k}</span>
      <span className={cn('shrink-0 text-[19px] font-semibold tabular-nums', tone && TONE[tone])}>{v}</span>
    </div>
  );
}

function Section({ title, href, children }: { title: string; href: string; children: ReactNode }) {
  return (
    <section className="tile p-4 sm:p-5">
      <Link to={href} className="group mb-1 flex items-center justify-between text-[13px] font-medium text-foreground/75">
        {title}
        <ArrowUpRight className="size-4 text-muted-foreground transition-colors group-hover:text-foreground" />
      </Link>
      {children}
    </section>
  );
}

/** A count coloured only when it is not zero. */
const when = (n: number, tone: Tone): Tone => (n > 0 ? tone : undefined);

export function StatSections({ s }: { s?: ClusterSummary }) {
  const r = s?.resources ?? {};
  const c = s?.capacity ?? {};
  const rep = s?.replication ?? {};
  const p = s?.protection ?? {};
  const t = s?.today ?? {};
  const free = Math.max(num(c.totalBytes) - num(c.usedBytes), 0);
  const covered = num(r.total) ? Math.round((num(p.covered) / num(r.total)) * 100) : 0;
  const attention = s?.attention ?? [];
  return (
    <div className="mt-4 grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
      <Section title="Resources" href="/resources">
        <Row k="Healthy" v={fmtCount(num(r.healthy))} tone="ok" />
        <Row k="Resyncing" v={fmtCount(num(r.resyncing))} />
        <Row k="Degraded" v={fmtCount(num(r.degraded))} tone={when(num(r.degraded), 'warn')} />
        <Row k="Without quorum" v={fmtCount(num(r.noQuorum))} tone={when(num(r.noQuorum), 'bad')} />
        <Row k="Volumes" v={fmtCount(num(r.volumes))} />
      </Section>
      <Section title="Nodes and storage" href="/pools">
        <Row k="Storage pools" v={fmtCount(num(c.pools))} />
        <Row k="Pools above 90%" v={fmtCount(num(c.poolsAbove90))} tone={when(num(c.poolsAbove90), 'warn')} />
        <Row k="Pools 80–90%" v={fmtCount(num(c.pools80To90))} />
        <Row k="Free" v={fmtSize(free)} />
        <Row k="Control plane" v={s?.controlPlane || '–'} />
      </Section>
      <Section title="Replication" href="/live">
        <Row k="Replicas" v={fmtCount(num(rep.replicas))} />
        <Row k="Diskless clients" v={fmtCount(num(rep.disklessClients))} />
        <Row k="Replicated off-site" v={fmtCount(num(rep.offsite))} />
        <Row k="Encrypted links" v={fmtCount(num(rep.encryptedLinks))} />
        <Row k="Writes now" v={rep.writeBytesPerSec !== undefined ? fmtRate(num(rep.writeBytesPerSec)) : '–'} />
      </Section>
      <Section title="Data protection" href="/resources">
        <Row k="Snapshot schedules" v={fmtCount(num(p.snapshotSchedules))} />
        <Row k="Resources on a schedule" v={`${covered}%`} />
        <Row k="Schedules run today" v={fmtCount(num(p.schedulesRunToday))} />
        <Row k="Backups today" v={fmtCount(num(p.backupsToday))} />
        <Row k="Backups failed" v={fmtCount(num(p.backupsFailedToday))} tone={when(num(p.backupsFailedToday), 'bad')} />
      </Section>
      <Section title="Today" href="/notifications">
        <Row k="Failovers" v={fmtCount(num(t.failovers))} />
        <Row k="Nodes lost" v={fmtCount(num(t.nodesLost))} tone={when(num(t.nodesLost), 'bad')} />
        <Row k="Replicas moved" v={fmtCount(num(t.replicasMoved))} />
        <Row k="Pools extended" v={fmtCount(num(t.poolsExtended))} />
        <Row
          k="Inspection passed"
          v={num(t.inspectionTotal) ? `${num(t.inspectionPassed)}/${num(t.inspectionTotal)}` : '–'}
          tone={num(t.inspectionTotal) && num(t.inspectionPassed) < num(t.inspectionTotal) ? 'warn' : undefined}
        />
      </Section>
      <Section title="Needs attention" href="/notifications">
        {attention.length === 0 ? (
          <div className="py-6 text-center text-[13px] text-muted-foreground">Nothing is firing.</div>
        ) : (
          attention.slice(0, 5).map((a) => (
            <Row
              key={a.type}
              k={
                <>
                  {eventTypeLabel(a.type)}
                  {a.subjects?.length ? (
                    <span className="text-muted-foreground">
                      {' '}
                      ({a.subjects.join(', ')}
                      {num(a.count) > a.subjects.length ? ', …' : ''})
                    </span>
                  ) : null}
                </>
              }
              v={fmtCount(num(a.count))}
              tone={a.severity === 'critical' ? 'bad' : 'warn'}
            />
          ))
        )}
      </Section>
    </div>
  );
}
