import { useState } from 'react';
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { api, Resource, Volume } from '../../services/api';
import { replyNote } from '../../services/api/client';
import { Button } from '@/components/ui/button';
import { Badge } from '@/components/ui/badge';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import { Skeleton } from '@/components/ui/skeleton';
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
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select';
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs';
import { toast } from 'sonner';
import { Trash2, Loader2 } from 'lucide-react';
import { type PoolOpt } from './types';
import { MoveVolumeForm } from './MoveVolumeForm';

export function VolumesDialog({
  open,
  onOpenChange,
  resource,
  pools,
  defaultTab = 'volumes',
}: {
  open: boolean;
  onOpenChange: (v: boolean) => void;
  resource: Resource;
  pools: PoolOpt[];
  /** Which tab the dialog lands on — "Add volume" opens straight on the form. */
  defaultTab?: 'volumes' | 'add';
}) {
  const queryClient = useQueryClient();

  const { data, isLoading } = useQuery({
    queryKey: ['resource', resource.name],
    queryFn: () => api.getResource(resource.name),
    enabled: open,
  });

  const volumes = data?.resource?.volumes ?? resource.volumes;

  const invalidate = () => {
    queryClient.invalidateQueries({ queryKey: ['resource', resource.name] });
    queryClient.invalidateQueries({ queryKey: ['resources'] });
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[85vh] overflow-y-auto sm:max-w-xl">
        <DialogHeader>
          <DialogTitle>Volumes — {resource.name}</DialogTitle>
          <DialogDescription>
            Manage volumes backing this resource.
          </DialogDescription>
        </DialogHeader>

        {/* Keyed so reopening on a different tab actually lands there: Radix
            keeps its own state for an uncontrolled `defaultValue`. */}
        <Tabs key={defaultTab} defaultValue={defaultTab} className="py-2">
          <TabsList>
            <TabsTrigger value="volumes">Volumes</TabsTrigger>
            <TabsTrigger value="add">Add Volume</TabsTrigger>
          </TabsList>

          <TabsContent value="volumes" className="space-y-2">
            {isLoading ? (
              <Skeleton className="h-24 w-full" />
            ) : volumes.length === 0 ? (
              <p className="py-4 text-center text-sm text-muted-foreground">
                No volumes.
              </p>
            ) : (
              volumes.map((vol) => (
                <VolumeRow
                  key={vol.volumeId}
                  resourceName={resource.name}
                  replicaNodes={resource.nodes}
                  pools={pools}
                  volume={vol}
                  onChanged={invalidate}
                />
              ))
            )}
          </TabsContent>

          <TabsContent value="add">
            <AddVolumeForm
              resourceName={resource.name}
              pools={pools}
              onAdded={invalidate}
            />
          </TabsContent>
        </Tabs>
      </DialogContent>
    </Dialog>
  );
}

function VolumeRow({
  resourceName,
  replicaNodes,
  pools,
  volume,
  onChanged,
}: {
  resourceName: string;
  replicaNodes: string[];
  pools: PoolOpt[];
  volume: Volume;
  onChanged: () => void;
}) {
  const [resizing, setResizing] = useState(false);
  const [moving, setMoving] = useState(false);
  const [newSize, setNewSize] = useState(String(volume.sizeGb));

  const resizeMutation = useMutation({
    mutationFn: async (sizeGb: number) => {
      const res = await api.resizeVolume(resourceName, volume.volumeId, sizeGb);
      // A resize can grow the volume and still be unfinished, when a
      // gateway's filesystem could not be grown at that moment.
      if (!res.success) throw new Error(res.message);
      return res;
    },
    onSuccess: (res) => {
      // What happened to a gateway's filesystem or a running guest.
      toast.success(`Volume ${volume.volumeId} resized`, {
        description: replyNote(res.message, 'Volume resized successfully'),
      });
      onChanged();
      setResizing(false);
    },
    onError: (e: Error) => toast.error(e.message),
  });

  const removeMutation = useMutation({
    mutationFn: () => api.removeVolume(resourceName, volume.volumeId),
    onSuccess: () => {
      toast.success(`Volume ${volume.volumeId} removed`);
      onChanged();
    },
    onError: (e: Error) => toast.error(e.message),
  });

  return (
    <div className="rounded-lg border bg-muted/40 p-3">
      <div className="flex items-center justify-between">
        <div className="flex flex-col">
          <span className="text-sm font-medium">Volume {volume.volumeId}</span>
          <span className="font-mono text-xs text-muted-foreground">
            {volume.device}
            {volume.pool && ` · ${volume.pool}`}
          </span>
        </div>
        <div className="flex items-center gap-2">
          <Badge variant="secondary">{volume.sizeGb} GB</Badge>
          <Button
            variant="outline"
            size="sm"
            onClick={() => setResizing((v) => !v)}
          >
            Resize
          </Button>
          <Button
            variant="outline"
            size="sm"
            onClick={() => setMoving((v) => !v)}
          >
            Move
          </Button>
          <AlertDialog>
            <AlertDialogTrigger asChild>
              <Button
                variant="ghost"
                size="icon"
                className="h-8 w-8 text-muted-foreground hover:text-destructive"
              >
                <Trash2 className="h-4 w-4" />
              </Button>
            </AlertDialogTrigger>
            <AlertDialogContent>
              <AlertDialogHeader>
                <AlertDialogTitle>
                  Remove volume {volume.volumeId}?
                </AlertDialogTitle>
                <AlertDialogDescription>
                  This permanently destroys the backing storage for volume{' '}
                  {volume.volumeId} of {resourceName}. This cannot be undone.
                </AlertDialogDescription>
              </AlertDialogHeader>
              <AlertDialogFooter>
                <AlertDialogCancel>Cancel</AlertDialogCancel>
                <AlertDialogAction
                  onClick={() => removeMutation.mutate()}
                  className="bg-destructive text-destructive-foreground hover:bg-destructive/90"
                >
                  Remove
                </AlertDialogAction>
              </AlertDialogFooter>
            </AlertDialogContent>
          </AlertDialog>
        </div>
      </div>

      {moving && (
        <MoveVolumeForm
          resourceName={resourceName}
          replicaNodes={replicaNodes}
          volume={volume}
          pools={pools}
          onStarted={() => setMoving(false)}
        />
      )}

      {resizing && (
        <form
          className="mt-3 flex items-end gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            const size = parseInt(newSize, 10);
            if (Number.isFinite(size) && size > 0) resizeMutation.mutate(size);
          }}
        >
          <div className="flex-1 space-y-1">
            <Label htmlFor={`resize-${volume.volumeId}`}>New size (GB)</Label>
            <Input
              id={`resize-${volume.volumeId}`}
              type="number"
              min={1}
              value={newSize}
              onChange={(e) => setNewSize(e.target.value)}
            />
          </div>
          <Button type="submit" disabled={resizeMutation.isPending}>
            {resizeMutation.isPending && (
              <Loader2 className="h-4 w-4 animate-spin" />
            )}
            Apply
          </Button>
        </form>
      )}
    </div>
  );
}

function AddVolumeForm({
  resourceName,
  pools,
  onAdded,
}: {
  resourceName: string;
  pools: PoolOpt[];
  onAdded: () => void;
}) {
  const [volume, setVolume] = useState('');
  const [pool, setPool] = useState('');
  const [sizeGb, setSizeGb] = useState('10');

  const addMutation = useMutation({
    mutationFn: (data: { volume: string; pool: string; sizeGb: number }) =>
      api.addVolume(resourceName, data),
    onSuccess: () => {
      toast.success(`Volume "${volume}" added`);
      onAdded();
      setVolume('');
      setPool('');
      setSizeGb('10');
    },
    onError: (e: Error) => toast.error(e.message),
  });

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    addMutation.mutate({
      volume,
      pool,
      sizeGb: parseInt(sizeGb, 10),
    });
  };

  return (
    <form onSubmit={handleSubmit} className="space-y-4">
      <div className="space-y-2">
        <Label htmlFor="add-vol-name">Volume Name</Label>
        <Input
          id="add-vol-name"
          value={volume}
          onChange={(e) => setVolume(e.target.value)}
          placeholder="e.g., data2"
          required
        />
      </div>
      <div className="space-y-2">
        <Label>Pool</Label>
        <Select value={pool} onValueChange={setPool}>
          <SelectTrigger>
            <SelectValue placeholder="Select a pool..." />
          </SelectTrigger>
          <SelectContent>
            {Array.from(
              pools
                .reduce((m, p) => {
                  const cur = m.get(p.name);
                  // A pool name is shared across the diskful nodes; show one
                  // entry per name with the tightest (minimum) free space, so
                  // SelectItem values stay unique and the trigger doesn't
                  // concatenate duplicate labels.
                  if (!cur || p.freeGb < cur.freeGb) m.set(p.name, p);
                  return m;
                }, new Map<string, PoolOpt>())
                .values()
            ).map((p) => (
              <SelectItem key={p.name} value={p.name}>
                {p.name} - {p.freeGb}GB free
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>
      <div className="space-y-2">
        <Label htmlFor="add-vol-size">Size (GB)</Label>
        <Input
          id="add-vol-size"
          type="number"
          min={1}
          value={sizeGb}
          onChange={(e) => setSizeGb(e.target.value)}
          required
        />
      </div>
      <Button type="submit" disabled={addMutation.isPending || !pool}>
        {addMutation.isPending && <Loader2 className="h-4 w-4 animate-spin" />}
        Add Volume
      </Button>
    </form>
  );
}
