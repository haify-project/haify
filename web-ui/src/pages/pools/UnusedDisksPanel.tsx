import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api, type NodeDisk, type Pool } from '../../services/api';
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
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select';
import { toast } from 'sonner';
import { HardDrive, Loader2 } from 'lucide-react';
import { formatBytes } from './format';

// Disks on the nodes that no pool uses yet — typically one just put into a
// server. Shown only when there are some, with the one action that matters.
export function UnusedDisksPanel({ pools, nodeNames }: { pools: Pool[]; nodeNames: Record<string, string> }) {
  const { data } = useQuery({
    queryKey: ['node-disks', ''],
    queryFn: () => api.getNodeDisks(),
    staleTime: 5 * 60_000,
    refetchOnWindowFocus: false,
  });
  const [adding, setAdding] = useState<NodeDisk | null>(null);
  const unused = (data?.disks ?? []).filter((d) => d.state === 'free' || d.state === 'wipe');
  if (unused.length === 0) return null;

  // A pool's node is a name or an address depending on how it was created.
  const poolsOn = (node: string) => pools.filter((p) => p.node === node || nodeNames[p.node] === node);

  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">Unused disks</CardTitle>
        <CardDescription>Disks on the nodes that no pool uses yet.</CardDescription>
      </CardHeader>
      <CardContent className="space-y-1.5">
        {unused.map((d) => {
          const targets = poolsOn(d.node);
          return (
            <div
              key={`${d.node}-${d.device}`}
              className="flex flex-wrap items-center justify-between gap-x-4 gap-y-2 rounded-lg border bg-muted/40 px-3 py-2"
            >
              <div className="flex min-w-0 items-center gap-3">
                <HardDrive className="h-4 w-4 shrink-0 text-muted-foreground" />
                <div className="min-w-0">
                  <div className="text-sm">
                    <span className="font-medium">{d.node}</span>{' '}
                    <span className="font-mono text-[13px]">{d.device}</span>{' '}
                    <span className="tabular-nums text-muted-foreground">{formatBytes(d.sizeBytes)}</span>
                  </div>
                  <div className="truncate text-xs text-muted-foreground">
                    {d.state === 'wipe' ? <span className="text-amber-600 dark:text-amber-400">Holds {d.detail}</span> : d.model}
                  </div>
                </div>
              </div>
              {targets.length > 0 ? (
                <Button variant="outline" size="sm" onClick={() => setAdding(d)}>
                  Add to a pool
                </Button>
              ) : (
                <span className="text-xs text-muted-foreground">No pool on {d.node} yet: use Create Pool</span>
              )}
            </div>
          );
        })}
      </CardContent>
      {adding && <AddToPoolDialog disk={adding} pools={poolsOn(adding.node)} onClose={() => setAdding(null)} />}
    </Card>
  );
}

function AddToPoolDialog({ disk, pools, onClose }: { disk: NodeDisk; pools: Pool[]; onClose: () => void }) {
  const queryClient = useQueryClient();
  const [pool, setPool] = useState(pools.length === 1 ? pools[0].name : '');
  const [wipe, setWipe] = useState(false);
  const target = pools.find((p) => p.name === pool);

  const add = useMutation({
    mutationFn: async () => {
      const res = await api.addDisk(pool, disk.device, target?.node ?? disk.node, disk.state === 'wipe' && wipe);
      if (!res.success) throw new Error(res.message);
      return res;
    },
    onSuccess: () => {
      toast.success(`${disk.device} added to ${pool} on ${disk.node}`);
      queryClient.invalidateQueries({ queryKey: ['pools'] });
      queryClient.invalidateQueries({ queryKey: ['node-disks'] });
      onClose();
    },
    onError: (e: Error) => toast.error(e.message),
  });

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent>
        <form
          onSubmit={(e) => {
            e.preventDefault();
            add.mutate();
          }}
        >
          <DialogHeader>
            <DialogTitle>
              Add {disk.device} on {disk.node} to a pool
            </DialogTitle>
            <DialogDescription>
              The pool grows by {formatBytes(disk.sizeBytes)}. A volume with replicas on other nodes can only grow as far as
              the fullest of those nodes allows, so give them disks too.
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-3 py-4">
            <div className="space-y-2">
              <Label>Pool</Label>
              <Select value={pool} onValueChange={setPool}>
                <SelectTrigger>
                  <SelectValue placeholder="Choose a pool" />
                </SelectTrigger>
                <SelectContent>
                  {pools.map((p) => (
                    <SelectItem key={p.name} value={p.name}>
                      {p.name}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            {disk.state === 'wipe' && (
              <label className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/5 p-2.5 text-sm">
                <input
                  type="checkbox"
                  className="mt-0.5 accent-destructive"
                  checked={wipe}
                  onChange={(e) => setWipe(e.target.checked)}
                />
                <span>
                  {disk.device} holds {disk.detail}. Erase it. This cannot be undone.
                </span>
              </label>
            )}
          </div>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose} disabled={add.isPending}>
              Cancel
            </Button>
            <Button type="submit" disabled={add.isPending || !pool || (disk.state === 'wipe' && !wipe)}>
              {add.isPending && <Loader2 className="h-4 w-4 animate-spin" />}
              Add disk
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
