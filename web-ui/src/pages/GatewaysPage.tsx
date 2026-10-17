import { useMemo, useState } from 'react';
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { api, Gateway } from '@/services/api';
import { toast } from 'sonner';
import { Plus, Network } from 'lucide-react';
import { PageHeader } from '@/components/PageHeader';
import { StatFilterStrip } from '@/components/list/StatFilterStrip';
import { ListToolbar, SearchBox } from '@/components/list/ListToolbar';
import { StatusTickHead } from '@/components/StatusTick';
import { Button } from '@/components/ui/button';
import {
  Table,
  TableBody,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table';
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from '@/components/ui/alert-dialog';
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu';
import { Skeleton } from '@/components/ui/skeleton';
import { type GwKind, PROTOCOL, gwKind, isRunning } from './gateways/protocol';
import { GatewayRow } from './gateways/GatewayRow';
import { CreateGatewayDialog } from './gateways/CreateGatewayDialog';
import { ManageDialog } from './gateways/ManageDialog';

// ==================== Page ====================

export function GatewaysPage() {
  const queryClient = useQueryClient();
  const { data: gateways, isLoading } = useQuery({
    queryKey: ['gateways'],
    queryFn: () => api.getGateways(),
    // A freshly created gateway is briefly "failed"/transitional while the
    // drbd-reactor promoter starts its services; poll until every gateway
    // settles so the UI reflects the real state instead of a stale snapshot.
    refetchInterval: (query) => {
      const gws =
        (query.state.data as { gateways?: { state?: string }[] } | undefined)
          ?.gateways ?? [];
      return gws.some((g) => g.state !== 'started' && g.state !== 'stopped')
        ? 3000
        : false;
    },
  });
  const { data: resources } = useQuery({
    queryKey: ['resources'],
    queryFn: () => api.getResources(),
  });

  const [createType, setCreateType] = useState<GwKind | null>(null);
  const [manageGateway, setManageGateway] = useState<Gateway | null>(null);
  const [expanded, setExpanded] = useState<string | null>(null);
  // The delete confirmation lives at page level rather than inside each row's
  // dropdown: a Radix AlertDialog nested in a menu item is unmounted with the
  // menu the moment it would open.
  const [pendingDelete, setPendingDelete] = useState<Gateway | null>(null);
  const [filter, setFilter] = useState<GwKind | 'all'>('all');

  const invalidate = () =>
    queryClient.invalidateQueries({ queryKey: ['gateways'] });

  const startMutation = useMutation({
    mutationFn: (id: string) => api.startGateway(id),
    onSuccess: () => {
      toast.success('Gateway started');
      invalidate();
    },
    onError: (e: Error) => toast.error(e.message),
  });

  const stopMutation = useMutation({
    mutationFn: (id: string) => api.stopGateway(id),
    onSuccess: () => {
      toast.success('Gateway stopped');
      invalidate();
    },
    onError: (e: Error) => toast.error(e.message),
  });

  const deleteMutation = useMutation({
    mutationFn: (id: string) => api.deleteGateway(id),
    onSuccess: () => {
      toast.success('Gateway deleted');
      invalidate();
    },
    onError: (e: Error) => toast.error(e.message),
  });

  const list = useMemo(() => gateways?.gateways ?? [], [gateways]);
  const counts = useMemo(() => {
    const c: Record<GwKind, number> = { nfs: 0, iscsi: 0, nvme: 0, smb: 0 };
    for (const g of list) c[gwKind(g.type)] += 1;
    return c;
  }, [list]);
  const running = list.filter((g) => isRunning(g.state)).length;
  const [query, setQuery] = useState('');
  const needle = query.trim().toLowerCase();
  const shown = (filter === 'all' ? list : list.filter((g) => gwKind(g.type) === filter)).filter(
    (g) => !needle || `${g.name} ${g.id} ${g.resource} ${g.node} ${g.path} ${JSON.stringify(g.options ?? {})}`.toLowerCase().includes(needle),
  );

  const openCreate = (kind: GwKind) => {
    // Pull a fresh resource list so newly created resources show up in the
    // dropdown without a full page reload.
    queryClient.invalidateQueries({ queryKey: ['resources'] });
    setCreateType(kind);
  };

  return (
    <div className="@container mx-auto max-w-[1600px]">
      <PageHeader
        title="Gateways"
        description={
          list.length ? (
            <>
              <span className="tabular-nums text-foreground">{running}</span> of{' '}
              <span className="tabular-nums text-foreground">{list.length}</span> started
            </>
          ) : undefined
        }
        actions={
          <>
            <DropdownMenu>
              <DropdownMenuTrigger asChild>
                <Button>
                  <Plus />
                  New gateway
                </Button>
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end">
                <DropdownMenuItem onSelect={() => openCreate('nfs')}>
                  NFS export
                </DropdownMenuItem>
                <DropdownMenuItem onSelect={() => openCreate('iscsi')}>
                  iSCSI target
                </DropdownMenuItem>
                <DropdownMenuItem onSelect={() => openCreate('nvme')}>
                  NVMe-oF subsystem
                </DropdownMenuItem>
                <DropdownMenuItem onSelect={() => openCreate('smb')}>
                  SMB share
                </DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>
          </>
        }
      />

      <StatFilterStrip
        aria-label="Show gateways of one protocol"
        loading={isLoading}
        value={filter}
        onChange={setFilter}
        options={[
          { value: 'all', label: 'All', count: list.length },
          { value: 'nfs', label: 'NFS', count: counts.nfs },
          { value: 'iscsi', label: 'iSCSI', count: counts.iscsi },
          { value: 'nvme', label: 'NVMe-oF', count: counts.nvme },
          { value: 'smb', label: 'SMB', count: counts.smb },
        ]}
      />
      <div className="mt-4 mb-3">
        <ListToolbar>
          <SearchBox value={query} onChange={setQuery} placeholder="Name, resource, address or node" aria-label="Search gateways" />
        </ListToolbar>
      </div>

      <div className="tile overflow-hidden">
          {isLoading ? (
            <div className="space-y-2 px-5 py-2">
              {Array.from({ length: 4 }).map((_, i) => (
                <Skeleton key={i} className="h-12 w-full" />
              ))}
            </div>
          ) : shown.length === 0 ? (
            <div className="flex flex-col items-center justify-center gap-2 py-14 text-center">
              <Network className="h-7 w-7 text-muted-foreground" />
              <p className="text-sm text-muted-foreground">
                {list.length === 0
                  ? 'No gateways yet. Create one to export a resource over NFS, iSCSI, NVMe-oF or SMB.'
                  : needle
                    ? 'No gateway matches this search.'
                    : `No ${PROTOCOL[filter as GwKind].label} gateways configured.`}
              </p>
            </div>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <StatusTickHead />
                  <TableHead>Gateway</TableHead>
                  <TableHead>Resource</TableHead>
                  <TableHead>Service IP</TableHead>
                  <TableHead>Active node</TableHead>
                  <TableHead>Clients</TableHead>
                  <TableHead className="pr-5 text-right">Actions</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {shown.map((gateway) => (
                  <GatewayRow
                    key={gateway.id}
                    gateway={gateway}
                    expanded={expanded === gateway.id}
                    onToggle={() =>
                      setExpanded((cur) => (cur === gateway.id ? null : gateway.id))
                    }
                    onManage={() => setManageGateway(gateway)}
                    onDelete={() => setPendingDelete(gateway)}
                    onStart={() => startMutation.mutate(gateway.id)}
                    onStop={() => stopMutation.mutate(gateway.id)}
                    startPending={startMutation.isPending}
                    stopPending={stopMutation.isPending}
                  />
                ))}
              </TableBody>
            </Table>
          )}
      </div>

      <CreateGatewayDialog
        type={createType}
        onTypeChange={setCreateType}
        resources={resources?.resources ?? []}
        onCreated={() => {
          setCreateType(null);
          invalidate();
        }}
      />

      <ManageDialog
        gateway={manageGateway}
        onOpenChange={(open) => !open && setManageGateway(null)}
      />

      <AlertDialog
        open={!!pendingDelete}
        onOpenChange={(open) => !open && setPendingDelete(null)}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete gateway?</AlertDialogTitle>
            <AlertDialogDescription>
              This removes the drbd-reactor config for gateway "
              {pendingDelete?.name || pendingDelete?.id}". The underlying DRBD
              resource is not affected.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={() =>
                pendingDelete && deleteMutation.mutate(pendingDelete.id)
              }
            >
              Delete
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
