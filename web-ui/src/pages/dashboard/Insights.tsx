import { Link } from 'react-router';
import {
  ArrowUpRight,
  BellRing,
  Boxes,
  Database,
  HardDrive,
  KeyRound,
  Network,
  ShieldCheck,
  Sparkles,
  type LucideIcon,
} from 'lucide-react';
import { cn } from '@/lib/utils';
import type { InspectionCheck, InspectionReport } from '@/services/api';
import { eventTime, eventTitle, type ClusterEvent } from '@/services/events';

// What deserves a look: the latest inspection's findings, most serious first;
// with no inspection stored, the latest warnings from the event history.

const AREA_ICON: Record<string, LucideIcon> = {
  resources: Boxes,
  gateways: Network,
  nodes: HardDrive,
  pools: Database,
  backups: Database,
  alerts: BellRing,
  selfha: ShieldCheck,
  tls: KeyRound,
  hygiene: Sparkles,
};

const RANK: Record<string, number> = { fail: 0, error: 1, warn: 2, pass: 3 };

interface Row {
  icon: LucideIcon;
  tone: 'ok' | 'warn' | 'bad';
  text: string;
  when: string;
}

function ago(now: number, at: number): string {
  const s = Math.max(0, Math.round((now - at) / 1000));
  if (s < 60) return 'just now';
  if (s < 3600) return `${Math.floor(s / 60)} min ago`;
  if (s < 86400) return `${Math.floor(s / 3600)} h ago`;
  const d = Math.floor(s / 86400);
  return d === 1 ? 'yesterday' : `${d} days ago`;
}

function fromInspection(report: InspectionReport, now: number): Row[] {
  const when = report.finishedAtUnixMs ? ago(now, Number(report.finishedAtUnixMs)) : '';
  const problems = (report.checks ?? [])
    .filter((c: InspectionCheck) => c.status !== 'pass')
    .sort((a, b) => (RANK[a.status] ?? 3) - (RANK[b.status] ?? 3));
  if (problems.length === 0) {
    const n = report.summary?.pass ?? report.checks?.length ?? 0;
    return [{ icon: ShieldCheck, tone: 'ok', text: `All ${n} inspection checks passed`, when }];
  }
  return problems.slice(0, 4).map((c) => ({
    icon: AREA_ICON[c.area] ?? Sparkles,
    tone: c.status === 'warn' ? 'warn' : 'bad',
    text: c.subject ? `${c.subject}: ${c.message}` : c.message,
    when,
  }));
}

function fromEvents(events: ClusterEvent[], now: number): Row[] {
  return events
    .filter((e) => e.severity !== 'info')
    .slice(-4)
    .reverse()
    .map((e) => ({
      icon: BellRing,
      tone: e.severity === 'critical' ? 'bad' : 'warn',
      text: `${eventTitle(e)}${e.resource || e.node ? `: ${e.resource || e.node}` : ''}`,
      when: (() => {
        const t = eventTime(e);
        return t ? ago(now, t) : '';
      })(),
    }));
}

const TONE: Record<Row['tone'], string> = {
  ok: 'bg-status-ok-soft text-status-ok-text',
  warn: 'bg-status-warn-soft text-status-warn-text',
  bad: 'bg-status-bad-soft text-status-bad-text',
};

export function Insights({
  now,
  report,
  events,
  loading,
}: {
  now: number;
  report?: InspectionReport;
  events: ClusterEvent[];
  loading: boolean;
}) {
  const rows = report ? fromInspection(report, now) : fromEvents(events, now);
  const sum = report?.summary;
  return (
    <div className="tile flex flex-col p-4 sm:p-5">
      <div className="flex items-center justify-between gap-2">
        <div className="min-w-0">
          <div className="text-[14px] font-semibold">Recent insights</div>
          {sum && (
            <div className="mt-0.5 text-[11.5px] text-muted-foreground tabular-nums">
              {sum.pass ?? 0} passed · {sum.warn ?? 0} warnings · {(sum.fail ?? 0) + (sum.error ?? 0)} failed
            </div>
          )}
        </div>
        <Link to="/notifications" className="rounded-md p-1 text-muted-foreground hover:bg-secondary hover:text-foreground" aria-label="All events">
          <ArrowUpRight className="size-4" />
        </Link>
      </div>
      <ul className="mt-3 flex flex-col divide-y divide-border/70">
        {loading ? (
          <li className="py-2 text-[12.5px] text-muted-foreground">Reading the latest inspection…</li>
        ) : rows.length === 0 ? (
          <li className="py-2 text-[12.5px] text-muted-foreground">No inspection has run and no warnings were raised.</li>
        ) : (
          rows.map((r, i) => (
            <li key={i} className="flex items-center gap-3 py-2.5">
              <span className={cn('flex size-8 shrink-0 items-center justify-center rounded-full', TONE[r.tone])}>
                <r.icon className="size-4" />
              </span>
              <div className="min-w-0 flex-1">
                <div className="line-clamp-2 text-[12.5px] leading-snug">{r.text}</div>
                {r.when && <div className="mt-0.5 text-[11px] text-muted-foreground">{r.when}</div>}
              </div>
            </li>
          ))
        )}
      </ul>
    </div>
  );
}
