import { useQuery } from '@tanstack/react-query';
import { api, type NodeDisk } from '../../services/api';
import { Input } from '@/components/ui/input';
import { Skeleton } from '@/components/ui/skeleton';
import { cn } from '@/lib/utils';
import { formatBytes } from './format';

// Choosing disks for a pool from what is actually on the node, rather than
// typing a device name: a mistyped letter used to be enough to erase the
// wrong disk. Only empty disks and disks holding old data are offered; the
// latter need an explicit "erase" before they can be used.

/** The disks on node a pool could take, and whether listing them worked. */
export function useNodeDisks(node: string) {
  const q = useQuery({
    queryKey: ['node-disks', node],
    queryFn: () => api.getNodeDisks(node),
    enabled: !!node,
    staleTime: 60_000,
    refetchOnWindowFocus: false,
  });
  const all = q.data?.disks ?? [];
  return {
    ...q,
    usable: all.filter((d) => d.state === 'free' || d.state === 'wipe'),
    byDevice: Object.fromEntries(all.map((d) => [d.device, d])) as Record<string, NodeDisk>,
  };
}

/** Whether any chosen disk holds old data that would be erased. */
export function chosenToWipe(selected: string[], byDevice: Record<string, NodeDisk>): string[] {
  return selected.filter((d) => byDevice[d]?.state === 'wipe');
}

function idName(byId?: string): string {
  return byId ? byId.slice(byId.lastIndexOf('/') + 1) : '';
}

export function DiskPicker({
  node,
  multiple,
  selected,
  onSelect,
  wipe,
  onWipe,
}: {
  node: string;
  multiple?: boolean;
  selected: string[];
  onSelect: (devices: string[]) => void;
  wipe: boolean;
  onWipe: (wipe: boolean) => void;
}) {
  const { usable, byDevice, isLoading, error } = useNodeDisks(node);

  if (!node) {
    return <p className="text-sm text-muted-foreground">Choose a node first.</p>;
  }
  if (isLoading) {
    return <Skeleton className="h-16 w-full" />;
  }
  if (error) {
    // Listing failed (an old node, SSH trouble): fall back to a path, which
    // the controller still checks before it touches the disk.
    return (
      <div className="space-y-1.5">
        <Input
          value={selected.join(', ')}
          onChange={(e) =>
            onSelect(
              e.target.value
                .split(',')
                .map((s) => s.trim())
                .filter(Boolean),
            )
          }
          placeholder="/dev/sdc"
        />
        <p className="text-xs text-muted-foreground">
          Could not list the disks on {node}: {(error as Error).message}
        </p>
      </div>
    );
  }
  if (usable.length === 0) {
    return (
      <p className="rounded-md border border-dashed p-3 text-sm text-muted-foreground">
        {node} has no empty disk. A disk put into the server shows up here.
      </p>
    );
  }

  const toggle = (dev: string) => {
    if (!multiple) {
      onSelect(selected[0] === dev ? [] : [dev]);
      return;
    }
    onSelect(selected.includes(dev) ? selected.filter((d) => d !== dev) : [...selected, dev]);
  };
  const toWipe = chosenToWipe(selected, byDevice);

  return (
    <div className="space-y-2">
      <div className="max-h-56 space-y-1.5 overflow-y-auto">
        {usable.map((d) => {
          const on = selected.includes(d.device);
          return (
            <label
              key={d.device}
              className={cn(
                'flex cursor-pointer items-start gap-3 rounded-md border p-2.5 text-sm transition-colors',
                on ? 'border-primary bg-primary/5' : 'hover:bg-muted/50',
              )}
            >
              <input
                type={multiple ? 'checkbox' : 'radio'}
                name={`disk-${node}`}
                className="mt-0.5 accent-primary"
                checked={on}
                onChange={() => toggle(d.device)}
              />
              <span className="min-w-0 flex-1">
                <span className="flex flex-wrap items-baseline justify-between gap-x-3">
                  <span className="font-mono text-[13px] font-medium">{d.device}</span>
                  <span className="tabular-nums text-muted-foreground">{formatBytes(d.sizeBytes)}</span>
                </span>
                <span className="block truncate text-xs text-muted-foreground" title={d.byId}>
                  {[d.model, d.rotational === false ? 'SSD' : '', idName(d.byId)].filter(Boolean).join(' · ')}
                </span>
                {d.state === 'wipe' && (
                  <span className="mt-0.5 block text-xs text-amber-600 dark:text-amber-400">Holds {d.detail}</span>
                )}
              </span>
            </label>
          );
        })}
      </div>
      {toWipe.length > 0 && (
        <label className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/5 p-2.5 text-sm">
          <input
            type="checkbox"
            className="mt-0.5 accent-destructive"
            checked={wipe}
            onChange={(e) => onWipe(e.target.checked)}
          />
          <span>Erase everything on {toWipe.join(', ')}. This cannot be undone.</span>
        </label>
      )}
    </div>
  );
}
