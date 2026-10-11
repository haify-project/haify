import { SelfHaStatus } from '@/services/api';
import { StatusBadge } from '@/components/StatusBadge';
import { cn } from '@/lib/utils';
import { Trash2, Info, LogOut, Loader2 } from 'lucide-react';
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card';
import { Button } from '@/components/ui/button';
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
import { type PromoterView, hiddenFailure, startListFor, unmanagedUnits } from './promoter';
import { ChainList } from './ChainList';
import { OutsideServices, Standbys } from './Takeover';
import { QuorumPill } from './QuorumPill';
import { Fact } from './Fact';
import { SelfHaControls } from './SelfHaCard';
import { TomlEditorSection } from './TomlEditorSection';

// ==================== Promoter card ====================

export function PromoterCard({
  view,
  controlPlane,
  onShowDetails,
  onEvict,
  onDelete,
  isEvicting,
  isDeleting,
}: {
  view: PromoterView;
  /** Set when this promoter is the one the controller itself runs on. It is a
   *  role, not a separate card: the chip and the self-HA actions below are the
   *  whole of what used to be a second card for this same resource. */
  controlPlane?: SelfHaStatus;
  onShowDetails: (resource: string) => void;
  onEvict: (resource: string, fromNode?: string) => void;
  onDelete: (resource: string) => void;
  isEvicting: boolean;
  isDeleting: boolean;
}) {
  const { config, resource, status, promoter, primaryNode, members, activeMember } =
    view;
  // No primary anywhere means nothing is promoted, so there is nothing to evict.
  const isRunning = Boolean(primaryNode);
  const { units: startList, live: startListIsLive } = startListFor(config, promoter);
  const unmanaged = startListIsLive ? unmanagedUnits(config, startList) : [];
  const failed = hiddenFailure(promoter);
  const activeNode = promoter?.activeNode || activeMember || primaryNode;
  const standbys = promoter?.standbys ?? [];
  const outside = promoter?.outside ?? [];
  const outsideMount = config.mountPoint || outside[0]?.mountPoints?.[0] || '';

  return (
    <Card className="gap-4 py-5">
      <CardHeader className="px-5">
        <div className="flex items-start justify-between gap-4">
          <div className="min-w-0">
            <div className="flex flex-wrap items-center gap-2">
              <CardTitle className="font-mono text-[15px]">
                {config.resource}
              </CardTitle>
              {controlPlane ? (
                <span className="inline-flex items-center rounded-[5px] bg-accent px-1.5 py-0.5 text-[11px] font-medium text-accent-foreground">
                  control plane
                </span>
              ) : null}
            </div>
            <p className="mt-1 text-[12.5px] text-muted-foreground">
              {isRunning ? (
                <>
                  active on{' '}
                  <span className="font-mono text-foreground">{activeNode}</span>
                </>
              ) : (
                'not currently promoted'
              )}
            </p>
          </div>
          {/* One line, not a stack. Quorum and role answer one question
              together, and stacking them left the right edge ragged and cost a
              row of height on every card. */}
          <div className="flex shrink-0 flex-wrap items-center justify-end gap-2">
            <QuorumPill quorum={status?.quorum} />
            <span title={failed?.name}>
              <StatusBadge status={failed ? 'failed' : isRunning ? 'running' : 'stopped'} />
            </span>
          </div>
        </div>
      </CardHeader>

      <CardContent className="space-y-4 px-5">
        {/* A two-column grid with the label above its value, not a leader line
            from a left label to a right-flushed value across the whole card.
            Two cards side by side now line up row for row instead of drifting
            apart with the length of a mount path. */}
        <div className="grid grid-cols-2 gap-x-5 gap-y-3.5">
          <Fact label="Virtual IP" value={config.vip || '-'} mono />
          <Fact
            label="Replication"
            value={
              resource
                ? `tcp ${resource.port} · protocol ${resource.protocol}`
                : '-'
            }
            mono
          />
          <Fact
            label="Mount point"
            value={
              <>
                <span className="font-mono">{config.mountPoint || '-'}</span>
                {config.fsType ? (
                  <span className="ml-1.5 text-muted-foreground">
                    {config.fsType}
                  </span>
                ) : null}
              </>
            }
          />
          {/* Members used to appear only on the control plane's own card. That
              a resource has four replicas is the same kind of fact and was
              nowhere on screen. */}
          <div className="min-w-0">
            <div className="eyebrow">Members</div>
            <div className="mt-1 flex flex-wrap gap-1.5">
              {members.length > 0 ? (
                members.map((m) => (
                  <span
                    key={m}
                    className={cn(
                      'inline-flex items-center gap-1.5 rounded-[5px] px-1.5 py-0.5 font-mono text-[11.5px]',
                      m === activeMember
                        ? 'bg-accent text-accent-foreground'
                        : 'bg-muted text-secondary-foreground',
                    )}
                  >
                    {m}
                    {m === activeMember ? (
                      <span className="text-[10px] opacity-75">active</span>
                    ) : null}
                  </span>
                ))
              ) : (
                <span className="text-[13px] text-muted-foreground">-</span>
              )}
            </div>
          </div>
        </div>

        {startList.length > 0 ? (
          <div>
            <div className="flex flex-wrap items-baseline justify-between gap-x-3">
              <div className="eyebrow">
                Start list{' '}
                <span className="font-mono text-[11px] tracking-normal text-foreground normal-case tabular-nums">
                  {startList.length}
                </span>
              </div>
              <span className="text-[11.5px] text-muted-foreground">
                {startListIsLive ? 'as the promoter runs it' : 'from the configuration'}
              </span>
            </div>
            <ChainList units={startList} />
            {unmanaged.length > 0 ? (
              // Not a warning: a hand-added unit is a legitimate thing to have
              // done. What is not legitimate is a console that hides it, so the
              // card names the units it cannot manage and stops there.
              <p className="mt-2.5 text-[12px] leading-snug text-muted-foreground">
                {unmanaged.length === 1 ? 'One unit is' : `${unmanaged.length} units are`} in
                the promoter but not in this controller&rsquo;s config, so editing the
                configuration here would drop{' '}
                {unmanaged.length === 1 ? 'it' : 'them'}:{' '}
                <span className="font-mono text-[11.5px] text-foreground">
                  {unmanaged.join(', ')}
                </span>
              </p>
            ) : null}
          </div>
        ) : null}

        {standbys.length > 0 ? <Standbys standbys={standbys} /> : null}
        <OutsideServices
          mountPoint={outsideMount}
          outside={outside}
          sessions={promoter?.outsideSessions}
          sessionProcesses={promoter?.outsideSessionProcesses}
        />

        <div className="flex flex-wrap items-center gap-2 border-t border-border pt-4">
          <Button
            variant="outline"
            size="sm"
            onClick={() => onShowDetails(config.resource)}
          >
            <Info className="mr-1 h-3 w-3" />
            Details
          </Button>

          {/* The control plane gets its own eviction, not this one: moving the
              controller means the API goes away mid-request, so that path
              re-reads the active node first and then polls for where it landed.
              Delete is withheld there too — removing the controller's own HA
              config from a button beside Evict is not an action to offer in
              passing; the self-HA card does it deliberately when disabled. */}
          {controlPlane ? (
            <SelfHaControls status={controlPlane} />
          ) : (
          <AlertDialog>
            <AlertDialogTrigger asChild>
              <Button
                variant="outline"
                size="sm"
                disabled={isEvicting || !isRunning}
              >
                {isEvicting ? (
                  <Loader2 className="mr-1 h-3 w-3 animate-spin" />
                ) : (
                  <LogOut className="mr-1 h-3 w-3" />
                )}
                Evict
              </Button>
            </AlertDialogTrigger>
            <AlertDialogContent>
              <AlertDialogHeader>
                <AlertDialogTitle>Evict "{config.resource}"?</AlertDialogTitle>
                <AlertDialogDescription>
                  This triggers a failover to another node. The VIP moves and
                  clients will briefly lose connectivity until the resource is
                  promoted elsewhere.
                </AlertDialogDescription>
              </AlertDialogHeader>
              <AlertDialogFooter>
                <AlertDialogCancel>Cancel</AlertDialogCancel>
                <AlertDialogAction
                  onClick={() => onEvict(config.resource, primaryNode)}
                >
                  Evict
                </AlertDialogAction>
              </AlertDialogFooter>
            </AlertDialogContent>
          </AlertDialog>
          )}

          {controlPlane ? null : (
          <AlertDialog>
            <AlertDialogTrigger asChild>
              <Button variant="outline" size="sm" disabled={isDeleting}>
                <Trash2 className="mr-1 h-3 w-3 text-destructive" />
                Delete
              </Button>
            </AlertDialogTrigger>
            <AlertDialogContent>
              <AlertDialogHeader>
                <AlertDialogTitle>
                  Delete HA configuration?
                </AlertDialogTitle>
                <AlertDialogDescription>
                  This removes the drbd-reactor HA config for "{config.resource}
                  ". The DRBD resource and its data are not affected, but
                  automatic failover stops.
                </AlertDialogDescription>
              </AlertDialogHeader>
              <AlertDialogFooter>
                <AlertDialogCancel>Cancel</AlertDialogCancel>
                <AlertDialogAction onClick={() => onDelete(config.resource)}>
                  Delete
                </AlertDialogAction>
              </AlertDialogFooter>
            </AlertDialogContent>
          </AlertDialog>
          )}
        </div>

        <div className="border-t border-border pt-4">
          <TomlEditorSection resource={config.resource} />
        </div>
      </CardContent>
    </Card>
  );
}
