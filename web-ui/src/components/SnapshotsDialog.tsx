import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Camera, Loader2, Plus, Trash2 } from 'lucide-react';
import { toast } from 'sonner';
import { api } from '@/services/api';
import { replyNote } from '@/services/api/client';
import { Button } from '@/components/ui/button';
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog';
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
} from '@/components/ui/alert-dialog';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select';
import { Skeleton } from '@/components/ui/skeleton';

/**
 * Snapshot management for a resource's backing volumes. Snapshots operate on
 * the "<pool>/<lv>" backing path, not the DRBD device, so the dialog derives
 * paths from the volume metadata the controller reports.
 *
 * Note: restore is intentionally NOT offered — merging a snapshot back into
 * a volume that DRBD replicates bypasses replication and diverges the peers.
 */
export function SnapshotsDialog({
  resource,
  open,
  onOpenChange,
}: {
  resource: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const queryClient = useQueryClient();
  const [volumeId, setVolumeId] = useState('0');
  const [snapName, setSnapName] = useState('');

  const { data: resData } = useQuery({
    queryKey: ['resource', resource],
    queryFn: () => api.getResource(resource),
    enabled: open,
  });
  const volumes = resData?.resource?.volumes ?? [];
  const node = resData?.resource?.nodes?.[0] ?? '';
  const selected = volumes.find((v) => String(v.volumeId) === volumeId);
  const volumePath =
    selected?.pool && selected?.backingVolume
      ? `${selected.pool}/${selected.backingVolume}`
      : '';

  const {
    data: snapData,
    isLoading: snapsLoading,
    isError: snapsError,
    error: snapsErrorObj,
  } = useQuery({
    queryKey: ['snapshots', volumePath, node],
    queryFn: () => api.listSnapshots(volumePath, node),
    enabled: open && volumePath !== '' && node !== '',
  });
  const snapshots = snapData?.snapshots ?? [];

  const invalidate = () =>
    queryClient.invalidateQueries({ queryKey: ['snapshots', volumePath, node] });

  const createMutation = useMutation({
    mutationFn: async () => {
      const res = await api.createSnapshot(volumePath, snapName, node);
      if (!res.success) throw new Error(res.message);
      return res;
    },
    onSuccess: (res) => {
      // For a guest's volume the reply says whether the snapshot is
      // filesystem- or crash-consistent, and why.
      toast.success(`Snapshot "${snapName}" created`, {
        description: replyNote(res.message, 'Snapshot created successfully'),
      });
      setSnapName('');
      invalidate();
    },
    onError: (e: Error) => toast.error(e.message),
  });

  const deleteMutation = useMutation({
    mutationFn: (name: string) => api.deleteSnapshot(volumePath, name, node),
    onSuccess: () => {
      toast.success('Snapshot deleted');
      invalidate();
    },
    onError: (e: Error) => toast.error(e.message),
  });

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[85vh] overflow-y-auto sm:max-w-lg">
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2">
            <Camera className="h-5 w-5" />
            Snapshots — {resource}
          </DialogTitle>
          <DialogDescription>
            Point-in-time snapshots of the backing volume (on {node || '...'}).
            Restore onto a replicated volume is not offered: it would bypass
            DRBD and diverge the peers.
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-4">
          {volumes.length > 1 && (
            <div className="space-y-2">
              <Label>Volume</Label>
              <Select value={volumeId} onValueChange={setVolumeId}>
                <SelectTrigger className="w-full">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {volumes.map((v) => (
                    <SelectItem key={v.volumeId} value={String(v.volumeId)}>
                      Volume {v.volumeId} — {v.pool}/{v.backingVolume} (
                      {v.sizeGb}GB)
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          )}

          {!volumePath && volumes.length > 0 && (
            <p className="text-sm text-amber-600 dark:text-amber-400">
              This volume does not expose backing metadata; snapshots need a
              controller upgrade.
            </p>
          )}

          {snapsLoading && <Skeleton className="h-16 w-full" />}
          {snapsError && (
            <p className="text-sm text-destructive">
              {(snapsErrorObj as Error).message}
            </p>
          )}

          {!snapsLoading && volumePath && (
            <div className="rounded-lg border">
              {snapshots.length === 0 ? (
                <p className="p-4 text-sm text-muted-foreground">
                  No snapshots yet.
                </p>
              ) : (
                <ul className="divide-y">
                  {snapshots.map((s) => (
                    <li
                      key={s.name}
                      className="flex items-center justify-between px-4 py-2 text-sm"
                    >
                      <div>
                        <span className="font-mono">{s.name}</span>
                        <span className="ml-2 text-muted-foreground">
                          {s.sizeGb}GB
                        </span>
                        {s.consistency ? (
                          <span className="ml-2 text-muted-foreground">
                            {s.consistency === 'filesystem'
                              ? 'Filesystem-consistent'
                              : 'Crash-consistent'}
                          </span>
                        ) : null}
                      </div>
                      <AlertDialog>
                        <AlertDialogTrigger asChild>
                          <Button
                            variant="ghost"
                            size="icon"
                            className="h-7 w-7 text-destructive"
                          >
                            <Trash2 className="h-4 w-4" />
                          </Button>
                        </AlertDialogTrigger>
                        <AlertDialogContent>
                          <AlertDialogHeader>
                            <AlertDialogTitle>
                              Delete snapshot "{s.name}"?
                            </AlertDialogTitle>
                            <AlertDialogDescription>
                              The snapshot is removed permanently; the volume
                              itself is not affected.
                            </AlertDialogDescription>
                          </AlertDialogHeader>
                          <AlertDialogFooter>
                            <AlertDialogCancel>Cancel</AlertDialogCancel>
                            <AlertDialogAction
                              onClick={() => deleteMutation.mutate(s.name)}
                              className="bg-destructive text-destructive-foreground hover:bg-destructive/90"
                            >
                              Delete
                            </AlertDialogAction>
                          </AlertDialogFooter>
                        </AlertDialogContent>
                      </AlertDialog>
                    </li>
                  ))}
                </ul>
              )}
            </div>
          )}

          <form
            className="flex items-end gap-2"
            onSubmit={(e) => {
              e.preventDefault();
              if (snapName.trim()) createMutation.mutate();
            }}
          >
            <div className="flex-1 space-y-2">
              <Label htmlFor="snap-name">New snapshot name</Label>
              <Input
                id="snap-name"
                value={snapName}
                onChange={(e) => setSnapName(e.target.value)}
                placeholder="e.g., before-upgrade"
                disabled={!volumePath}
              />
            </div>
            <Button type="submit" disabled={!snapName.trim() || !volumePath || createMutation.isPending}>
              {createMutation.isPending ? (
                <Loader2 className="h-4 w-4 animate-spin" />
              ) : (
                <Plus className="h-4 w-4" />
              )}
              Create
            </Button>
          </form>
        </div>
      </DialogContent>
    </Dialog>
  );
}
