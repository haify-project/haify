import { useEffect, useMemo, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api, Resource } from '../../services/api';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Skeleton } from '@/components/ui/skeleton';
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog';
import { toast } from 'sonner';
import { Plus, Loader2, X } from 'lucide-react';
import { MANAGED_OPTIONS, optionChanges, rowsFrom, type OptionRow } from './optionChanges';

const blank = (): OptionRow => ({ key: '', value: '', loaded: '', secret: false });

export function EditOptionsDialog({
  open,
  onOpenChange,
  resource,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
  resource: Resource;
}) {
  const queryClient = useQueryClient();
  const current = useQuery({
    queryKey: ['resource-options', resource.name],
    queryFn: () => api.getResourceOptions(resource.name),
    enabled: open,
    // Always read afresh: the config may have changed from the CLI.
    staleTime: 0,
  });
  const loaded = useMemo(() => current.data?.options ?? [], [current.data]);
  const [rows, setRows] = useState<OptionRow[]>([]);
  useEffect(() => {
    if (open && current.data) setRows(rowsFrom(current.data.options ?? []));
  }, [open, current.data]);

  const changes = optionChanges(loaded, rows);
  const changed = Object.keys(changes).length;

  const mutation = useMutation({
    mutationFn: (opts: Record<string, string>) => api.updateResourceOptions(resource.name, opts),
    onSuccess: () => {
      toast.success(`Options applied to "${resource.name}"`);
      queryClient.invalidateQueries({ queryKey: ['resources'] });
      queryClient.invalidateQueries({ queryKey: ['resource-options', resource.name] });
      onOpenChange(false);
    },
    onError: (e: Error) => toast.error(e.message, { duration: 12_000 }),
  });

  const setRow = (i: number, field: 'key' | 'value', v: string) =>
    setRows((rs) => rs.map((r, idx) => (idx === i ? { ...r, [field]: v } : r)));
  const addRow = () => setRows((rs) => [...rs, blank()]);
  const removeRow = (i: number) => setRows((rs) => rs.filter((_, idx) => idx !== i));

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    if (changed === 0) {
      toast.error('Nothing changed');
      return;
    }
    mutation.mutate(changes);
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-2xl">
        <form onSubmit={submit}>
          <DialogHeader>
            <DialogTitle>DRBD options — {resource.name}</DialogTitle>
            <DialogDescription>
              Keys are <code className="font-mono text-xs">section/key</code>, e.g.{' '}
              <code className="font-mono text-xs">net/max-buffers</code>. Remove a row to go back to DRBD&apos;s
              default. A node&apos;s drbdadm checks the result before any node gets it, and it applies without
              taking the resource down.
            </DialogDescription>
          </DialogHeader>
          <div className="max-h-[55vh] space-y-2 overflow-y-auto py-4 pr-1">
            {current.isLoading ? (
              <Skeleton className="h-24 w-full" />
            ) : current.isError ? (
              <p className="text-sm text-status-bad-text">
                Could not read the current options: {(current.error as Error).message}
              </p>
            ) : (
              <>
                {rows.map((r, i) => {
                  const managed = r.loaded ? MANAGED_OPTIONS[r.loaded] : undefined;
                  return (
                    <div key={i} className="flex items-center gap-2" title={managed}>
                      <Input
                        placeholder="net/max-buffers"
                        value={r.key}
                        readOnly={Boolean(r.loaded)}
                        onChange={(e) => setRow(i, 'key', e.target.value)}
                        className="min-w-0 flex-[3] font-mono text-xs read-only:bg-muted/50"
                        aria-label="Option"
                      />
                      <span className="text-muted-foreground">=</span>
                      <Input
                        placeholder={r.secret ? 'set; type to replace' : '8000'}
                        type={r.secret ? 'password' : 'text'}
                        autoComplete="off"
                        value={r.value}
                        readOnly={Boolean(managed)}
                        onChange={(e) => setRow(i, 'value', e.target.value)}
                        className="min-w-0 flex-[2] font-mono text-xs read-only:bg-muted/50"
                        aria-label={`Value of ${r.key || 'new option'}`}
                      />
                      <Button
                        type="button"
                        variant="ghost"
                        size="icon"
                        className="h-8 w-8 shrink-0 text-muted-foreground hover:text-destructive"
                        onClick={() => removeRow(i)}
                        disabled={Boolean(managed)}
                        aria-label={`Remove ${r.key || 'option'}`}
                      >
                        <X className="h-4 w-4" />
                      </Button>
                    </div>
                  );
                })}
                <Button type="button" variant="outline" size="sm" onClick={addRow}>
                  <Plus className="h-4 w-4" />
                  Add option
                </Button>
              </>
            )}
          </div>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => onOpenChange(false)} disabled={mutation.isPending}>
              Cancel
            </Button>
            <Button type="submit" disabled={mutation.isPending || changed === 0 || !current.data}>
              {mutation.isPending && <Loader2 className="h-4 w-4 animate-spin" />}
              {changed === 0 ? 'Apply' : `Apply ${changed} change${changed === 1 ? '' : 's'}`}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
