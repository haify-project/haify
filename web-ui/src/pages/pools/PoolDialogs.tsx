import { useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { api, type Pool } from '../../services/api';
import { Button } from '@/components/ui/button';
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
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
import { toast } from 'sonner';
import { HardDrive, Plus, PlusCircle, Trash2, Loader2 } from 'lucide-react';
import { POOL_TYPE_OPTIONS } from './poolTypes';
import { DiskPicker, chosenToWipe, useNodeDisks } from './DiskPicker';

export function CreatePoolDialog({
  nodes,
}: {
  nodes: Array<{ name: string; address: string }>;
}) {
  const queryClient = useQueryClient();
  const [open, setOpen] = useState(false);
  const [name, setName] = useState('');
  const [type, setType] = useState('vg');
  const [node, setNode] = useState('');
  const [disks, setDisks] = useState<string[]>([]);
  const [wipe, setWipe] = useState(false);
  const { byDevice } = useNodeDisks(node);
  const blockedByWipe = chosenToWipe(disks, byDevice).length > 0 && !wipe;

  const createMutation = useMutation({
    // ZFS pools go through their dedicated endpoint; sending type=zfs to
    // the LVM /pools flow would vgcreate over the device instead.
    mutationFn: async (data: {
      name: string;
      type: string;
      node: string;
      disks: string[];
      wipe: boolean;
    }) => {
      const res = await (data.type === 'zfs'
        ? api.createZFSPool({ name: data.name, node: data.node, vdevs: data.disks, wipe: data.wipe })
        : api.createPool(data));
      if (!res.success) throw new Error(res.message);
      return res;
    },
    onSuccess: () => {
      toast.success(`Pool "${name}" created`);
      queryClient.invalidateQueries({ queryKey: ['pools'] });
      setOpen(false);
      setName('');
      setType('vg');
      setNode('');
      setDisks([]);
      setWipe(false);
    },
    onError: (e: Error) => toast.error(e.message),
  });

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    createMutation.mutate({ name, type, node, disks, wipe: chosenToWipe(disks, byDevice).length > 0 && wipe });
  };

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        <Button>
          <Plus className="h-4 w-4" />
          Create Pool
        </Button>
      </DialogTrigger>
      <DialogContent>
        <form onSubmit={handleSubmit}>
          <DialogHeader>
            <DialogTitle>Create Storage Pool</DialogTitle>
            <DialogDescription>
              Create a new storage pool on a node from one or more disks.
            </DialogDescription>
          </DialogHeader>

          <div className="space-y-4 py-4">
            <div className="space-y-2">
              <Label htmlFor="pool-name">Pool Name</Label>
              <Input
                id="pool-name"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="e.g., data"
                required
              />
              <p className="text-xs text-muted-foreground">
                Will be prefixed with "haify_"
              </p>
            </div>

            <div className="space-y-2">
              <Label>Pool Type</Label>
              <Select value={type} onValueChange={setType}>
                <SelectTrigger>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {POOL_TYPE_OPTIONS.map((opt) => (
                    <SelectItem key={opt.value} value={opt.value}>
                      {opt.label}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>

            <div className="space-y-2">
              <Label>Node</Label>
              <Select
                value={node}
                onValueChange={(n) => {
                  setNode(n);
                  setDisks([]);
                  setWipe(false);
                }}
                required
              >
                <SelectTrigger>
                  <SelectValue placeholder="Select a node..." />
                </SelectTrigger>
                <SelectContent>
                  {nodes.map((n) => (
                    <SelectItem key={n.name} value={n.name}>
                      {n.name} ({n.address})
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>

            <div className="space-y-2">
              <Label>Disks</Label>
              <DiskPicker node={node} multiple selected={disks} onSelect={setDisks} wipe={wipe} onWipe={setWipe} />
            </div>
          </div>

          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              onClick={() => setOpen(false)}
              disabled={createMutation.isPending}
            >
              Cancel
            </Button>
            <Button
              type="submit"
              disabled={createMutation.isPending || !node || disks.length === 0 || blockedByWipe}
            >
              {createMutation.isPending && (
                <Loader2 className="h-4 w-4 animate-spin" />
              )}
              Create
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

export function AddDiskDialog({ pool, nodeName }: { pool: Pool; nodeName: string }) {
  const queryClient = useQueryClient();
  const [open, setOpen] = useState(false);
  const [disk, setDisk] = useState<string[]>([]);
  const [wipe, setWipe] = useState(false);
  const { byDevice } = useNodeDisks(open ? nodeName : '');
  const toWipe = chosenToWipe(disk, byDevice).length > 0;

  const addDiskMutation = useMutation({
    mutationFn: async (d: string) => {
      const res = await api.addDisk(pool.name, d, pool.node, toWipe && wipe);
      if (!res.success) throw new Error(res.message);
      return res;
    },
    onSuccess: () => {
      toast.success(`Disk added to "${pool.name}"`);
      queryClient.invalidateQueries({ queryKey: ['pools'] });
      queryClient.invalidateQueries({ queryKey: ['node-disks'] });
      setOpen(false);
      setDisk([]);
      setWipe(false);
    },
    onError: (e: Error) => toast.error(e.message),
  });

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    if (disk[0]) addDiskMutation.mutate(disk[0]);
  };

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        <Button
          variant="ghost"
          size="icon"
          className="h-7 w-7 text-muted-foreground hover:text-primary"
          title="Add disk"
        >
          <PlusCircle className="h-4 w-4" />
        </Button>
      </DialogTrigger>
      <DialogContent>
        <form onSubmit={handleSubmit}>
          <DialogHeader>
            <DialogTitle>Add Disk to Pool</DialogTitle>
            <DialogDescription>
              Pool "{pool.name}" on {nodeName} grows into the disk. Volumes already on it keep their size until
              they are resized.
            </DialogDescription>
          </DialogHeader>

          <div className="space-y-4 py-4">
            <div className="space-y-2">
              <Label>Disk</Label>
              {open && (
                <DiskPicker node={nodeName} selected={disk} onSelect={setDisk} wipe={wipe} onWipe={setWipe} />
              )}
            </div>
          </div>

          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              onClick={() => setOpen(false)}
              disabled={addDiskMutation.isPending}
            >
              Cancel
            </Button>
            <Button type="submit" disabled={addDiskMutation.isPending || !disk[0] || (toWipe && !wipe)}>
              {addDiskMutation.isPending && (
                <Loader2 className="h-4 w-4 animate-spin" />
              )}
              Add Disk
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

export function DeletePoolDialog({ pool }: { pool: Pool }) {
  const queryClient = useQueryClient();

  const deleteMutation = useMutation({
    mutationFn: () =>
      pool.type === 'zfs'
        ? api.deleteZFSPool(pool.name, pool.node)
        : api.deletePool(pool.name, pool.node),
    onSuccess: () => {
      toast.success(`Pool "${pool.name}" deleted`);
      queryClient.invalidateQueries({ queryKey: ['pools'] });
    },
    onError: (e: Error) => toast.error(e.message),
  });

  return (
    <AlertDialog>
      <AlertDialogTrigger asChild>
        <Button
          variant="ghost"
          size="icon"
          className="h-7 w-7 text-muted-foreground hover:text-destructive"
          title="Delete pool"
        >
          <Trash2 className="h-4 w-4" />
        </Button>
      </AlertDialogTrigger>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Delete pool "{pool.name}"?</AlertDialogTitle>
          <AlertDialogDescription>
            This removes the pool from node {pool.node}. Any storage capacity it
            provides will no longer be available for new resources. This action
            cannot be undone.
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction
            onClick={() => deleteMutation.mutate()}
            className="bg-destructive text-destructive-foreground hover:bg-destructive/90"
          >
            <HardDrive className="h-4 w-4" />
            Delete
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
