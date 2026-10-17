import { Link } from 'react-router';
import { ArrowUpRight } from 'lucide-react';
import { cn } from '@/lib/utils';
import type { InspectionReport, SnapshotSchedule } from '@/services/api';
import { eventTime, eventTitle, type ClusterEvent } from '@/services/events';

// Today on one axis: what the cluster went through (events), what it did on
// its own (scheduled snapshots, the inspection) and what it will do next.

type ItemState = 'done' | 'upcoming' | 'firing' | 'resolved';

interface TimelineItem {
  at: number;
  title: string;
  detail?: string;
  state: ItemState;
  color: string;
}

const STATE_WORD: Record<ItemState, string> = {
  done: 'Completed',
  upcoming: 'Upcoming',
  firing: 'Firing',
  resolved: 'Resolved',
};

const STATE_CLASS: Record<ItemState, string> = {
  done: 'text-muted-foreground',
  upcoming: 'text-status-warn-text',
  firing: 'text-status-bad-text',
  resolved: 'text-primary',
};

const startOfDay = (now: number) => {
  const d = new Date(now);
  d.setHours(0, 0, 0, 0);
  return d.getTime();
};

const hhmm = (ms: number) =>
  new Date(ms).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', hour12: false });

function itemsFor(
  now: number,
  events: ClusterEvent[],
  schedules: SnapshotSchedule[],
  inspection?: InspectionReport,
): TimelineItem[] {
  const day0 = startOfDay(now);
  const today = (t: number | null): t is number => t !== null && t >= day0 && t < day0 + 86_400_000;
  const out: TimelineItem[] = [];
  for (const e of events) {
    const t = eventTime(e);
    if (!today(t)) continue;
    const subject = e.resource || e.node;
    out.push({
      at: t,
      title: eventTitle(e),
      detail: subject,
      state: e.status === 'firing' ? 'firing' : e.status === 'resolved' ? 'resolved' : 'done',
      color:
        e.severity === 'critical'
          ? 'var(--status-bad)'
          : e.severity === 'warning'
            ? 'var(--status-warn)'
            : 'var(--tile-sky)',
    });
  }
  for (const s of schedules) {
    if (!s.enabled) continue;
    const last = s.lastRun ? Date.parse(s.lastRun) : null;
    const next = s.nextRun ? Date.parse(s.nextRun) : null;
    if (today(last)) out.push({ at: last, title: `Snapshot ${s.name}`, detail: s.resource, state: 'done', color: 'var(--status-ok)' });
    if (today(next) && next > now)
      out.push({ at: next, title: `Snapshot ${s.name}`, detail: s.resource, state: 'upcoming', color: 'var(--tile-peach)' });
  }
  const started = inspection?.startedAtUnixMs ? Number(inspection.startedAtUnixMs) : null;
  if (today(started)) {
    const sum = inspection?.summary ?? {};
    const problems = (sum.warn ?? 0) + (sum.fail ?? 0) + (sum.error ?? 0);
    out.push({
      at: started,
      title: 'Inspection',
      detail: problems ? `${problems} to look at` : 'all checks passed',
      state: 'done',
      color: 'var(--tile-lilac)',
    });
  }
  return out.sort((a, b) => a.at - b.at);
}

/** The rows under the axis: the last two things that happened and the next one. */
function rowsAround(items: TimelineItem[], now: number): TimelineItem[] {
  const past = items.filter((i) => i.at <= now);
  const next = items.filter((i) => i.at > now);
  return [...past.slice(-2), ...next.slice(0, Math.max(1, 3 - Math.min(past.length, 2)))].slice(0, 3);
}

export function TodayTimeline({
  now,
  events,
  schedules,
  inspection,
}: {
  now: number;
  events: ClusterEvent[];
  schedules: SnapshotSchedule[];
  inspection?: InspectionReport;
}) {
  const items = itemsFor(now, events, schedules, inspection);
  const day0 = startOfDay(now);
  const pos = (t: number) => ((t - day0) / 86_400_000) * 100;
  const rows = rowsAround(items, now);

  return (
    <div className="tile flex flex-col p-4 sm:p-5">
      <div className="flex items-center justify-between">
        <span className="text-[14px] font-semibold">Today&apos;s timeline</span>
        <Link to="/notifications" className="rounded-md p-1 text-muted-foreground hover:bg-secondary hover:text-foreground" aria-label="All events">
          <ArrowUpRight className="size-4" />
        </Link>
      </div>

      <div className="relative mt-9">
        <div className="absolute -top-7 z-10 -translate-x-1/2" style={{ left: `${pos(now)}%` }}>
          <span className="rounded-md bg-foreground px-1.5 py-0.5 font-mono text-[11px] text-background tabular-nums">{hhmm(now)}</span>
        </div>
        <div className="absolute -top-2 bottom-[-6px] z-10 w-px bg-foreground" style={{ left: `${pos(now)}%` }} />
        <div className="relative h-3.5 rounded-full bg-secondary">
          {items.map((i, k) => (
            <span
              key={k}
              title={`${hhmm(i.at)} ${i.title}${i.detail ? ` · ${i.detail}` : ''}`}
              className={cn('absolute top-0.5 h-2.5 min-w-2.5 rounded-full', i.state === 'upcoming' && 'opacity-70')}
              style={{ left: `calc(${pos(i.at)}% - 5px)`, width: '1.4%', background: i.color }}
            />
          ))}
        </div>
        <div className="mt-1.5 flex justify-between font-mono text-[10.5px] text-muted-foreground tabular-nums">
          {['00:00', '06:00', '12:00', '18:00', '24:00'].map((t) => (
            <span key={t}>{t}</span>
          ))}
        </div>
      </div>

      <ul className="mt-4 flex flex-col gap-2.5">
        {rows.length === 0 ? (
          <li className="text-[12.5px] text-muted-foreground">Nothing has happened today, and nothing is scheduled for the rest of it.</li>
        ) : (
          rows.map((i, k) => (
            <li key={k} className="flex items-start gap-2.5">
              <span className="mt-1 size-2.5 shrink-0 rounded-full" style={{ background: i.color }} />
              <div className="min-w-0 flex-1">
                <div className="truncate text-[13px]">
                  {i.title}
                  {i.detail && <span className="text-muted-foreground"> · {i.detail}</span>}
                </div>
                <div className="font-mono text-[11px] text-muted-foreground tabular-nums">{hhmm(i.at)}</div>
              </div>
              <span className={cn('shrink-0 text-[12px]', STATE_CLASS[i.state])}>{STATE_WORD[i.state]}</span>
            </li>
          ))
        )}
      </ul>
    </div>
  );
}
