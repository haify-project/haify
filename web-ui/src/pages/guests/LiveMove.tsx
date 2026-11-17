import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { toast } from 'sonner';
import { ArrowRightLeft, Check, Loader2, RefreshCw, TriangleAlert } from 'lucide-react';
import { api, type HaMigration, type VmDefinitionNode } from '../../services/api';
import { Button } from '@/components/ui/button';
import { Progress } from '@/components/ui/progress';
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from '@/components/ui/dialog';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select';
import { cn } from '@/lib/utils';

const AUTO = '__auto__';

// Moves an HA guest to another node while it keeps running, and follows the
// move to its end. Shown wherever an HA guest is: the KVM page and the guest's
// card on the HA page.
export function LiveMoveButton({
  resource,
  guest,
  runsOn,
  className,
}: {
  resource: string;
  guest: string;
  runsOn?: string;
  className?: string;
}) {
  const [open, setOpen] = useState(false);
  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        <Button variant="outline" size="sm" className={className} disabled={!runsOn}>
          <ArrowRightLeft className="mr-1 h-3 w-3" />
          Move live
        </Button>
      </DialogTrigger>
      <DialogContent className="sm:max-w-lg">
        {open ? <LiveMove resource={resource} guest={guest} runsOn={runsOn} onClose={() => setOpen(false)} /> : null}
      </DialogContent>
    </Dialog>
  );
}

function LiveMove({
  resource,
  guest,
  runsOn,
  onClose,
}: {
  resource: string;
  guest: string;
  runsOn?: string;
  onClose: () => void;
}) {
  const queryClient = useQueryClient();
  const [target, setTarget] = useState(AUTO);
  // Set once this dialog has started a move, to the start time of the move
  // before it ('' when there was none). The last move's record stays readable
  // after it ends, and an earlier one's result is not this one's.
  const [previous, setPrevious] = useState<string | null>(null);
  const started = previous !== null;

  const definition = useQuery({
    queryKey: ['vm-definition', resource],
    queryFn: () => api.getVmDefinition(resource),
    enabled: !started,
  });
  const migration = useQuery({
    queryKey: ['ha-migration', resource],
    queryFn: () => api.getHaMigration(resource),
    refetchInterval: (q) => {
      const m = q.state.data?.migration;
      const stale = previous !== null && (m?.startedUnix ?? '') === previous;
      return stale || (m && !m.done) ? 2000 : false;
    },
  });
  const last = migration.data?.migration;
  const running = last && !last.done ? last : undefined;
  const ours = last && previous !== null && (last.startedUnix ?? '') !== previous ? last : undefined;
  const shown = running ?? ours;

  const refreshAll = () => {
    for (const key of ['resources', 'resource-status', 'ha', 'ha-status'])
      queryClient.invalidateQueries({ queryKey: [key] });
  };

  const move = useMutation({
    mutationFn: async () => {
      const res = await api.evictHaLive(resource, target === AUTO ? undefined : target);
      if (!res.success) throw new Error(res.message);
      return res;
    },
    onSuccess: () => {
      setPrevious(last?.startedUnix ?? '');
      queryClient.invalidateQueries({ queryKey: ['ha-migration', resource] });
    },
    onError: (e: Error) => toast.error(e.message),
  });

  const sync = useMutation({
    mutationFn: async () => {
      const res = await api.syncVmDefinition(resource);
      if (!res.success) throw new Error(res.message);
      return res;
    },
    onSuccess: () => {
      toast.success(`${guest} is now defined the same on every node`);
      queryClient.invalidateQueries({ queryKey: ['vm-definition', resource] });
    },
    onError: (e: Error) => toast.error(e.message),
  });

  if (shown) {
    return (
      <MoveProgress
        guest={guest}
        migration={shown}
        onClose={() => {
          refreshAll();
          onClose();
        }}
      />
    );
  }

  const nodes = definition.data?.success ? (definition.data.nodes ?? []) : [];
  const others = nodes.filter((n) => n.node !== runsOn);
  const off = nodes.filter((n) => n.state !== 'ok' && n.state !== 'unreachable');

  return (
    <>
      <DialogHeader>
        <DialogTitle>Move {guest} to another node</DialogTitle>
        <DialogDescription>
          {guest} keeps running while it moves{runsOn ? ` off ${runsOn}` : ''}. If the move cannot finish, it stays
          where it is.
        </DialogDescription>
      </DialogHeader>

      <div className="space-y-4 text-sm">
        <div className="space-y-1.5">
          <label className="text-xs font-medium text-muted-foreground" htmlFor="live-move-target">
            Move to
          </label>
          <Select value={target} onValueChange={setTarget}>
            <SelectTrigger id="live-move-target" className="w-full">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value={AUTO}>Any node that can take it</SelectItem>
              {others.map((n) => (
                <SelectItem key={n.node} value={n.node} disabled={n.state !== 'ok'}>
                  {n.node}
                  {n.state !== 'ok' ? ` (${definitionText(n)})` : ''}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>

        {definition.isLoading ? (
          <p className="text-xs text-muted-foreground">Checking the nodes…</p>
        ) : definition.data && !definition.data.success ? (
          <p className="text-xs text-status-warn-text">{definition.data.message}</p>
        ) : off.length ? (
          <div className="rounded-md border border-status-warn/40 bg-status-warn/[0.06] p-3">
            <div className="flex items-start gap-2">
              <TriangleAlert className="mt-0.5 h-3.5 w-3.5 shrink-0 text-status-warn-text" />
              <div className="min-w-0 flex-1 space-y-1">
                <p className="font-medium">{guest} is not defined the same on every node</p>
                <ul className="text-xs text-muted-foreground">
                  {off.map((n) => (
                    <li key={n.node}>
                      <span className="font-mono text-foreground">{n.node}</span>: {definitionText(n)}
                    </li>
                  ))}
                </ul>
              </div>
              <Button variant="outline" size="sm" onClick={() => sync.mutate()} disabled={sync.isPending}>
                {sync.isPending ? (
                  <Loader2 className="mr-1 h-3 w-3 animate-spin" />
                ) : (
                  <RefreshCw className="mr-1 h-3 w-3" />
                )}
                Sync
              </Button>
            </div>
          </div>
        ) : null}
      </div>

      <DialogFooter>
        <Button variant="outline" onClick={onClose}>
          Cancel
        </Button>
        <Button onClick={() => move.mutate()} disabled={move.isPending || started || !runsOn}>
          {move.isPending || started ? <Loader2 className="mr-1 h-3.5 w-3.5 animate-spin" /> : null}
          Move live
        </Button>
      </DialogFooter>
    </>
  );
}

function MoveProgress({ guest, migration, onClose }: { guest: string; migration: HaMigration; onClose: () => void }) {
  const done = Boolean(migration.done);
  const moved = migration.outcome === 'moved';
  const steps = migration.steps ?? [];
  return (
    <>
      <DialogHeader>
        <DialogTitle>
          {done ? outcomeTitle(guest, migration) : `Moving ${guest} to ${migration.targetNode}`}
        </DialogTitle>
        <DialogDescription>
          {migration.sourceNode} → {migration.targetNode}
        </DialogDescription>
      </DialogHeader>

      <div className="space-y-3 text-sm">
        {!done ? (
          <div className="space-y-1.5">
            <Progress value={migration.phase === 'migrating' ? (migration.progressPercent ?? 0) : phasePercent(migration.phase)} />
            <p className="flex items-center gap-1.5 text-xs text-muted-foreground">
              <Loader2 className="h-3 w-3 animate-spin" />
              {migration.phase === 'migrating'
                ? `Copying memory, ${migration.progressPercent ?? 0}%`
                : 'Working on it'}
            </p>
          </div>
        ) : (
          <p
            className={cn(
              'flex items-start gap-2 rounded-md border p-3',
              moved ? 'border-status-ok/40 bg-status-ok/[0.06]' : 'border-status-warn/40 bg-status-warn/[0.06]',
            )}
          >
            {moved ? (
              <Check className="mt-0.5 h-3.5 w-3.5 shrink-0 text-status-ok-text" />
            ) : (
              <TriangleAlert className="mt-0.5 h-3.5 w-3.5 shrink-0 text-status-warn-text" />
            )}
            <span className="min-w-0 break-words">{migration.message}</span>
          </p>
        )}
        <ol className="max-h-48 space-y-1 overflow-y-auto text-xs text-muted-foreground">
          {(done ? steps.slice(0, -1) : steps).map((s, i) => (
            <li key={i} className="break-words">
              {s}
            </li>
          ))}
        </ol>
      </div>

      <DialogFooter>
        <Button variant={done ? 'default' : 'outline'} onClick={onClose}>
          {done ? 'Done' : 'Close'}
        </Button>
      </DialogFooter>
    </>
  );
}

function outcomeTitle(guest: string, m: HaMigration): string {
  switch (m.outcome) {
    case 'moved':
      return `${guest} now runs on ${m.activeNode}`;
    case 'stayed':
      return `${guest} stayed on ${m.activeNode}`;
    case 'restarted':
      return `${guest} was restarted on ${m.activeNode}`;
    default:
      return `${guest} needs attention`;
  }
}

// Where the bar stands outside the memory copy, which reports its own share.
function phasePercent(phase: string): number {
  return { checking: 3, releasing: 8, window: 12, settling: 96 }[phase] ?? 0;
}

function definitionText(n: VmDefinitionNode): string {
  switch (n.state) {
    case 'missing':
      return 'not defined here';
    case 'changed':
      return 'defined differently here';
    case 'uuid_mismatch':
      return 'defined under another UUID';
    case 'unreachable':
      return 'not answering';
    default:
      return n.state;
  }
}
