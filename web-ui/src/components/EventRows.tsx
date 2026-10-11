import { cn } from '@/lib/utils';
import { TONE_BG, TONE_TEXT } from '@/components/status';
import { eventTime, eventTitle, type ClusterEvent } from '@/services/events';

/** Severity → the tone vocabulary the rest of the console uses. The controller
 *  sends "info" | "warning" | "critical"; `toneOf` does not know those words,
 *  so the mapping is stated here rather than guessed from the message text. */
const EVENT_TONE = {
  critical: 'bad',
  warning: 'warn',
  info: 'idle',
} as const;

// The clock on a recent-events row. Events read over REST carry epoch millis,
// not an RFC 3339 string; eventTime is what knows the difference.
function eventClock(event: ClusterEvent): string {
  const at = eventTime(event);
  return at === null ? '—' : new Date(at).toLocaleTimeString();
}

/** Recent events as rows: message, then what kind of event and when. Newest first. */
export function EventRows({ events }: { events: ClusterEvent[] }) {
  return (
    <div className="flex flex-col">
      {events.map((event, i) => {
        const tone = event.status === 'resolved' ? 'ok' : EVENT_TONE[event.severity] ?? 'idle';
        return (
          <div key={event.id} className={cn('flex gap-3 py-2.5', i > 0 && 'border-t border-border/70')}>
            {/* The rail repeats the severity; the word below carries it. */}
            <span aria-hidden className={cn('w-[3px] shrink-0 rounded-[2px]', TONE_BG[tone])} />
            <div className="min-w-0 flex-1">
              <p className="text-[13px] leading-snug break-words">{event.message}</p>
              <p className="mt-1 flex items-center gap-1.5 text-[11.5px] text-muted-foreground">
                <span className={TONE_TEXT[tone]}>{eventTitle(event)}</span>
                <span aria-hidden>·</span>
                <span className="font-mono tabular-nums">{eventClock(event)}</span>
              </p>
            </div>
          </div>
        );
      })}
    </div>
  );
}
