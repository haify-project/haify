import { Loader2, Check, X } from 'lucide-react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { toast } from 'sonner';
import { api, type Node } from '@/services/api';
import { LabelsEditor } from '@/components/LabelsEditor';
import { cn } from '@/lib/utils';
import { TONE_BG, toneOf, type StatusTone } from '@/components/status';
import { formatLastSeen, type HealthResult } from './health';

/** What a health check found, or the offer to run one. Same body in the
 *  expanded row and the expanded card. */
export function NodeDetail({
  node,
  isOnline,
  isChecking,
  result,
  error,
  onCheck,
}: {
  node: Node;
  isOnline: boolean;
  isChecking: boolean;
  result?: HealthResult;
  error?: string;
  onCheck: () => void;
}) {
  // A container of its own: the same panel opens under a table row and inside
  // a card, and its grids should follow the room it was given there.
  return (
    <div className="@container flex flex-col gap-4">
              <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-1">
                <div className="eyebrow flex items-center gap-1.5">
                  Health check
                  {isChecking ? (
                    <Loader2 className="size-3 animate-spin text-muted-foreground" />
                  ) : null}
                  {result && !isChecking ? (
                    <>
                      <span aria-hidden>·</span>
                      <span className="font-mono text-[11px] tracking-normal text-foreground normal-case tabular-nums">
                        {new Date(result.at).toLocaleTimeString()}
                      </span>
                      {/* A re-check, not an invitation to run the first one:
                          opening the row already did that. Text-weight, beside
                          the timestamp it refreshes. */}
                      <button
                        type="button"
                        onClick={onCheck}
                        disabled={!isOnline}
                        className="rounded text-[11px] font-medium tracking-normal text-muted-foreground normal-case underline-offset-2 outline-none hover:text-foreground hover:underline focus-visible:ring-[3px] focus-visible:ring-ring/50 disabled:opacity-50"
                      >
                        re-check
                      </button>
                    </>
                  ) : null}
                </div>
                <div className="text-[12px] text-muted-foreground">
                  what the promoter's start chain needs on this node
                </div>
              </div>

              {error ? (
                <p className="text-[12.5px] text-status-bad-text">
                  Health check failed: {error}
                </p>
              ) : null}

              {result ? (
                <div className="grid grid-cols-1 gap-3 @xl:grid-cols-3">
                  <HealthTile label="DRBD" ok={result.info.drbdInstalled}>
                    {result.info.drbdInstalled
                      ? result.info.drbdVersion || 'installed'
                      : 'not installed'}
                  </HealthTile>
                  <HealthTile label="drbd-reactor" ok={result.info.drbdReactorInstalled}>
                    {result.info.drbdReactorInstalled
                      ? `${result.info.drbdReactorVersion || 'installed'} · ${
                          result.info.drbdReactorRunning ? 'running' : 'stopped'
                        }`
                      : 'not installed'}
                  </HealthTile>
                  <HealthTile
                    label="Resource agents"
                    ok={result.info.resourceAgentsInstalled}
                  >
                    <AgentChips agents={result.info.availableAgents} />
                  </HealthTile>
                </div>
              ) : (
                // Three tiles' worth of space, held. Without it the panel
                // jumps a hundred pixels the moment the check lands, under
                // whatever the reader had already moved on to.
                <div
                  className="grid grid-cols-1 gap-3 @xl:grid-cols-3"
                  aria-busy={isChecking}
                >
                  {['DRBD', 'drbd-reactor', 'Resource agents'].map((label) => (
                    <div
                      key={label}
                      className="rounded-lg border border-dashed border-border bg-card px-3.5 py-3"
                    >
                      <div className="text-[12.5px] font-medium text-muted-foreground">
                        {label}
                      </div>
                      <div className="mt-1 text-[12px] text-muted-foreground">
                        {isChecking
                          ? 'checking…'
                          : isOnline
                            ? 'no answer yet'
                            : 'node offline'}
                      </div>
                    </div>
                  ))}
                </div>
              )}

              {!result && !isChecking && !isOnline ? (
                <p className="text-[12.5px] text-muted-foreground">
                  A health check runs over SSH, so it needs a reachable node.
                </p>
              ) : null}

              <div className="grid grid-cols-2 gap-3 border-t border-border/70 pt-3.5 @md:grid-cols-3 @3xl:grid-cols-5">
                <Fact label="Hostname" value={node.hostname || '-'} />
                <Fact label="Address" value={node.address} />
                <Fact label="State" value={node.state} tone={toneOf(node.state)} />
                <Fact label="Version" value={node.version || (node.localAgent ? 'Local agent' : '-')} />
                <Fact label="Last seen" value={formatLastSeen(node.lastSeen)} />
              </div>

              <div className="flex flex-col gap-1.5">
                <div className="eyebrow">Labels</div>
                <NodeLabels node={node} />
              </div>
    </div>
  );
}

/** A node's labels; host=, rack= and the like decide where replicas may go. */
function NodeLabels({ node }: { node: Node }) {
  const queryClient = useQueryClient();
  const save = useMutation({
    mutationFn: (changes: Record<string, string>) => api.setNodeLabels(node.name, changes),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['nodes'] }),
    onError: (e: Error) => toast.error(`Labels of ${node.name} not saved: ${e.message}`),
  });
  return <LabelsEditor labels={node.labels} onSave={(c) => save.mutate(c)} saving={save.isPending} />;
}

/** One prerequisite: whether it is there, and what version answered. The tick
 *  and the cross are shapes, not two shades of the same dot. */
function HealthTile({
  label,
  ok,
  children,
}: {
  label: string;
  ok: boolean;
  children: React.ReactNode;
}) {
  return (
    <div className="rounded-lg border border-border bg-card px-3.5 py-3">
      <div className="flex items-center gap-2 text-[12.5px] font-medium">
        {ok ? (
          <Check className="size-[15px] text-status-ok" />
        ) : (
          <X className="size-[15px] text-status-bad" />
        )}
        {label}
        <span className="sr-only">{ok ? ' installed' : ' not installed'}</span>
      </div>
      <div className="mt-1.5 font-mono text-[11.5px] tabular-nums text-muted-foreground">
        {children}
      </div>
    </div>
  );
}

/** Five agent names is enough to recognise a working node; the rest are a
 *  count, with the full list on the overflow chip's title. */
function AgentChips({ agents }: { agents: string[] }) {
  const all = agents ?? [];
  if (!all.length) return <>no OCF agents detected</>;
  const shown = all.slice(0, 5);
  const rest = all.slice(5);
  return (
    <span className="flex flex-wrap items-center gap-1.5">
      {shown.map((agent) => (
        <span
          key={agent}
          className="rounded border border-border bg-secondary px-1.5 py-0.5 text-[11px] text-foreground"
        >
          {agent}
        </span>
      ))}
      {rest.length ? (
        <span className="px-1 py-0.5 text-[11px]" title={rest.join(', ')}>
          +{rest.length}
        </span>
      ) : null}
    </span>
  );
}

function Fact({
  label,
  value,
  tone,
}: {
  label: string;
  value: string;
  tone?: StatusTone;
}) {
  return (
    <div className="min-w-0">
      <div className="eyebrow">{label}</div>
      <div className="mt-1.5 flex items-center gap-1.5 truncate font-mono text-[13px] tabular-nums">
        {tone ? <span className={cn('size-1.5 rounded-full', TONE_BG[tone])} /> : null}
        {value}
      </div>
    </div>
  );
}
