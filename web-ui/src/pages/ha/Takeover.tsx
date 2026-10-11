import { TriangleAlert } from 'lucide-react';
import { TONE_BG } from '@/components/status';
import { cn } from '@/lib/utils';
import type { HaOutsideService, HaStandby } from '@/services/api';

// Who could take the chain over, and what on the active node is using the
// mount without being part of the chain.

export function Standbys({ standbys }: { standbys: HaStandby[] }) {
  return (
    <div>
      <div className="eyebrow">Standbys</div>
      <ul className="mt-2 space-y-1.5">
        {standbys.map((s) => {
          const ready = Boolean(s.ready);
          const why = (s.reasons ?? []).join('; ');
          return (
            <li key={s.node} className="flex min-w-0 items-start gap-2 text-[12.5px]">
              <span
                aria-hidden
                className={cn('mt-[6px] size-[7px] shrink-0 rounded-full', TONE_BG[ready ? 'ok' : 'warn'])}
              />
              <span className={cn('min-w-0 break-words', !ready && 'text-status-warn-text')}>
                <span className="font-mono">{s.node}</span>
                {' · '}
                {ready ? 'ready' : why ? `can't take over: ${why}` : "can't take over"}
              </span>
              {s.diskState ? (
                <span className="ml-auto shrink-0 font-mono text-[11px] text-muted-foreground">{s.diskState}</span>
              ) : null}
            </li>
          );
        })}
      </ul>
    </div>
  );
}

const plural = (n: number, word: string) => `${n} ${word}${n === 1 ? '' : 'es'}`;

export function OutsideServices({
  mountPoint,
  outside,
  sessions = 0,
  sessionProcesses = 0,
}: {
  mountPoint: string;
  outside: HaOutsideService[];
  sessions?: number;
  sessionProcesses?: number;
}) {
  const sessionLine =
    sessions > 0
      ? `${sessions} login session${sessions === 1 ? '' : 's'}${
          sessionProcesses > 0 ? ` · ${plural(sessionProcesses, 'process')}` : ''
        }`
      : '';
  if (outside.length === 0)
    return sessionLine ? (
      <p className="text-[12px] text-muted-foreground">
        {sessionLine} using <span className="font-mono">{mountPoint}</span>
      </p>
    ) : null;
  return (
    <div className="rounded-lg border border-status-warn/40 bg-status-warn-soft px-3 py-2.5">
      <p className="flex items-center gap-1.5 text-[12.5px] font-medium text-status-warn-text">
        <TriangleAlert className="size-3.5 shrink-0" aria-hidden />
        <span className="min-w-0 break-words">
          Using <span className="font-mono">{mountPoint}</span> outside HA
        </span>
      </p>
      <ul className="mt-1.5 space-y-1">
        {outside.map((o) => (
          <li key={o.unit} className="flex min-w-0 items-baseline gap-2 text-[12px]">
            <span className="min-w-0 flex-1">
              <span className="font-mono break-all text-foreground">{o.unit}</span>
              {o.description ? <span className="text-muted-foreground"> · {o.description}</span> : null}
            </span>
            {o.processes ? (
              <span className="shrink-0 text-[11px] text-muted-foreground tabular-nums">
                {plural(o.processes, 'process')}
              </span>
            ) : null}
          </li>
        ))}
      </ul>
      {sessionLine ? <p className="mt-1.5 text-[11.5px] text-muted-foreground">{sessionLine}</p> : null}
    </div>
  );
}
