import { useState } from 'react';
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { api, type PoolDisk } from '../../services/api';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog';
import { Label } from '@/components/ui/label';
import { Skeleton } from '@/components/ui/skeleton';
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table';
import { StatusBadge } from '@/components/StatusBadge';
import { toast } from 'sonner';
import { Loader2, RefreshCw } from 'lucide-react';
import { cn } from '@/lib/utils';
import { formatBytes } from './format';
import { DiskPicker, chosenToWipe, useNodeDisks } from './DiskPicker';

// The disks under the pools, with their SMART / NVMe health, and the two ways
// to take one out. Probing runs smartctl on every node over SSH, so it is
// cached for a few minutes rather than refetched on every visit.
export function DisksPanel() {
  const { data, isLoading, isFetching, error, refetch } = useQuery({
    queryKey: ['pool-disks'],
    queryFn: () => api.getPoolDisks(),
    staleTime: 5 * 60_000,
    refetchOnWindowFocus: false,
  });
  const [action, setAction] = useState<{ kind: 'replace' | 'remove'; disk: PoolDisk } | null>(null);

  const disks = data?.disks ?? [];
  const disksIn = (d: PoolDisk) => disks.filter((x) => x.pool === d.pool && x.node === d.node).length;

  // Why a disk cannot simply be removed; empty when it can.
  const removeBlocked = (d: PoolDisk): string => (disksIn(d) < 2 ? "It is the pool's only disk: replace it instead." : '');

  return (
    <Card>
      <CardHeader className="flex flex-row items-start justify-between gap-4">
        <div className="space-y-1">
          <CardTitle className="text-base">Disks</CardTitle>
          <CardDescription>
            Health from SMART or the NVMe health log. Replacing or removing a disk moves its data first; the pool stays in use.
          </CardDescription>
        </div>
        <Button variant="outline" size="sm" onClick={() => refetch()} disabled={isFetching}>
          <RefreshCw className={isFetching ? 'h-4 w-4 animate-spin' : 'h-4 w-4'} />
          Check again
        </Button>
      </CardHeader>
      <CardContent>
        {isLoading ? (
          <Skeleton className="h-24 w-full" />
        ) : error ? (
          <p className="text-sm text-destructive">{(error as Error).message}</p>
        ) : disks.length === 0 ? (
          <p className="text-sm text-muted-foreground">No disks reported.</p>
        ) : (
          // Six columns need about 760px. Where the card has less, each disk
          // is a stacked record instead of a table scrolled sideways.
          <div className="@container">
            <div className="space-y-2 @min-[760px]:hidden">
              {disks.map((d) => (
                <div key={`${d.node}-${d.device}`} className="rounded-lg border bg-muted/40 p-3">
                  <div className="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1">
                    <span className="font-mono text-[13px] font-medium break-all">{d.device}</span>
                    <span className="text-xs text-muted-foreground">
                      {d.node} · {d.pool}
                    </span>
                  </div>
                  {d.model && <div className="mt-0.5 text-xs text-muted-foreground">{d.model}</div>}
                  <div className="mt-2 flex flex-wrap items-center gap-x-3 gap-y-1.5 text-xs">
                    <StatusBadge status={d.health} />
                    <span className="tabular-nums text-muted-foreground">
                      {formatBytes(d.usedBytes)} / {formatBytes(d.sizeBytes)}
                    </span>
                  </div>
                  {d.healthDetail && d.health !== 'ok' && (
                    <div className="mt-1 text-xs text-muted-foreground">{d.healthDetail}</div>
                  )}
                  <DiskButtons
                    className="mt-2.5"
                    blocked={removeBlocked(d)}
                    onReplace={() => setAction({ kind: 'replace', disk: d })}
                    onRemove={() => setAction({ kind: 'remove', disk: d })}
                  />
                </div>
              ))}
            </div>
            <div className="hidden @min-[760px]:block">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Node</TableHead>
                    <TableHead>Pool</TableHead>
                    <TableHead>Disk</TableHead>
                    <TableHead className="text-right">Used / Size</TableHead>
                    <TableHead>Health</TableHead>
                    <TableHead className="text-right">Actions</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {disks.map((d) => {
                    const blocked = removeBlocked(d);
                    return (
                      <TableRow key={`${d.node}-${d.device}`}>
                        <TableCell>{d.node}</TableCell>
                        <TableCell>{d.pool}</TableCell>
                        <TableCell className="font-mono text-xs">
                          {d.device}
                          {d.model && <div className="font-sans text-muted-foreground">{d.model}</div>}
                        </TableCell>
                        <TableCell className="text-right tabular-nums">
                          {formatBytes(d.usedBytes)} / {formatBytes(d.sizeBytes)}
                        </TableCell>
                        <TableCell className="max-w-xs">
                          <StatusBadge status={d.health} />
                          {d.healthDetail && d.health !== 'ok' && (
                            <div className="mt-1 text-xs whitespace-normal text-muted-foreground">{d.healthDetail}</div>
                          )}
                        </TableCell>
                        <TableCell className="text-right">
                          <DiskButtons
                            className="justify-end"
                            blocked={blocked}
                            onReplace={() => setAction({ kind: 'replace', disk: d })}
                            onRemove={() => setAction({ kind: 'remove', disk: d })}
                          />
                        </TableCell>
                      </TableRow>
                    );
                  })}
                </TableBody>
              </Table>
            </div>
          </div>
        )}
      </CardContent>
      {action && <DiskActionDialog action={action} onClose={() => setAction(null)} />}
    </Card>
  );
}

/** Replace and Remove, shared by the table row and the stacked record. */
function DiskButtons({
  blocked,
  onReplace,
  onRemove,
  className,
}: {
  blocked: string;
  onReplace: () => void;
  onRemove: () => void;
  className?: string;
}) {
  return (
    <div className={cn('flex gap-1', className)}>
      <Button variant="outline" size="sm" onClick={onReplace}>
        Replace
      </Button>
      <Button
        variant="ghost"
        size="sm"
        disabled={!!blocked}
        title={blocked || 'Move its data to the other disks, then take it out'}
        onClick={onRemove}
      >
        Remove
      </Button>
    </div>
  );
}

function DiskActionDialog({
  action,
  onClose,
}: {
  action: { kind: 'replace' | 'remove'; disk: PoolDisk };
  onClose: () => void;
}) {
  const queryClient = useQueryClient();
  const { kind, disk } = action;
  const [newDisk, setNewDisk] = useState<string[]>([]);
  const [wipe, setWipe] = useState(false);
  const { byDevice } = useNodeDisks(kind === 'replace' ? disk.node : '');
  const toWipe = chosenToWipe(newDisk, byDevice).length > 0;

  const start = useMutation({
    mutationFn: () =>
      kind === 'replace'
        ? api.replacePoolDisk({
            pool: disk.pool,
            node: disk.node,
            oldDisk: disk.device,
            newDisk: newDisk[0] ?? '',
            wipe: toWipe && wipe,
          })
        : api.removePoolDisk({ pool: disk.pool, node: disk.node, disk: disk.device }),
    onSuccess: (res) => {
      toast.success(res.message);
      queryClient.invalidateQueries({ queryKey: ['storage-jobs'] });
      queryClient.invalidateQueries({ queryKey: ['node-disks'] });
      onClose();
    },
    onError: (e: Error) => toast.error(e.message),
  });

  const replace = kind === 'replace';
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent>
        <form
          onSubmit={(e) => {
            e.preventDefault();
            start.mutate();
          }}
        >
          <DialogHeader>
            <DialogTitle>
              {replace ? 'Replace' : 'Remove'} {disk.device} on {disk.node}
            </DialogTitle>
            <DialogDescription>
              {replace
                ? `The new disk joins ${disk.pool}, ${formatBytes(disk.usedBytes)} of data moves to it, then ${disk.device} leaves the pool. The pool stays in use; this takes about as long as reading the disk once.`
                : `${formatBytes(disk.usedBytes)} of data moves to the pool's other disks, then ${disk.device} leaves ${disk.pool}. The pool stays in use; this takes about as long as reading the disk once.`}
            </DialogDescription>
          </DialogHeader>
          {replace && (
            <div className="space-y-2 py-4">
              <Label>New disk on {disk.node}</Label>
              <DiskPicker node={disk.node} selected={newDisk} onSelect={setNewDisk} wipe={wipe} onWipe={setWipe} />
              <p className="text-xs text-muted-foreground">
                At least as large as the data on {disk.device} ({formatBytes(disk.usedBytes)}).
              </p>
            </div>
          )}
          <DialogFooter className={replace ? '' : 'pt-4'}>
            <Button type="button" variant="outline" onClick={onClose} disabled={start.isPending}>
              Cancel
            </Button>
            <Button type="submit" disabled={start.isPending || (replace && (!newDisk[0] || (toWipe && !wipe)))}>
              {start.isPending && <Loader2 className="h-4 w-4 animate-spin" />}
              {replace ? 'Replace disk' : 'Remove disk'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
