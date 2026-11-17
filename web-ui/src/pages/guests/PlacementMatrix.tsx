import type { ResourceStatus } from '../../services/api';
import { TONE_BG } from '@/components/status';
import { cn } from '@/lib/utils';
import { ExternalLink, Play, TriangleAlert } from 'lucide-react';
import { replicationSummary, TONE_TEXT } from '../resources/replication';
import { Badge } from '@/components/ui/badge';
import { LiveMoveButton } from './LiveMove';
import {
  diskBytes,
  formatGiB,
  placementCell,
  runningOn,
  runsOnReplica,
  type Guest,
  type GuestDisk,
  type PlacementCell,
} from './placement';

// Guests down the side, nodes across the top: one row per disk, one cell per
// node saying what that node holds of it. The column of the node a guest runs
// on is shaded through the guest's rows, so whether its disks are local reads
// straight down the lane: solid squares are replicas on that node, a ring
// means it reads and writes over the network.
export function PlacementMatrix({
  guests,
  nodes,
  statusOf,
  addressOf,
}: {
  guests: Guest[];
  nodes: string[];
  statusOf: Map<string, ResourceStatus | undefined>;
  addressOf: Map<string, string>;
}) {
  return (
    <div className="overflow-hidden rounded-lg border border-border bg-card">
      <div className="overflow-x-auto">
        <table className="w-full border-collapse text-sm">
          <thead>
            <tr className="border-b border-border text-left text-xs text-muted-foreground">
              <th className="sticky left-0 z-10 bg-card px-3 py-2.5 font-medium sm:px-4">Disk</th>
              <th className="hidden px-3 py-2.5 text-right font-medium sm:table-cell">Size</th>
              {nodes.map((n) => (
                <th key={n} className="w-12 px-0.5 py-2.5 text-center font-mono font-medium whitespace-nowrap sm:w-[72px]">
                  {n}
                </th>
              ))}
              <th className="hidden px-4 py-2.5 font-medium sm:table-cell">Replication</th>
            </tr>
          </thead>
          {guests.map((g) => (
            <GuestRows key={g.key} guest={g} nodes={nodes} statusOf={statusOf} addressOf={addressOf} />
          ))}
        </table>
      </div>
      <Legend />
    </div>
  );
}

function GuestRows({
  guest,
  nodes,
  statusOf,
  addressOf,
}: {
  guest: Guest;
  nodes: string[];
  statusOf: Map<string, ResourceStatus | undefined>;
  addressOf: Map<string, string>;
}) {
  const node = runningOn(guest, statusOf);
  const local = runsOnReplica(guest, node);
  const linkNode = node ?? guest.disks[0]?.resource.nodes[0];
  const linkAddr = guest.link && linkNode ? addressOf.get(linkNode) : undefined;
  const href = guest.link && linkAddr ? guest.link.url(linkAddr) : undefined;
  const lane = (n: string) => (n === node ? 'bg-primary/[0.07]' : '');

  return (
    <tbody className="border-b border-border last:border-b-0">
      <tr>
        <td colSpan={2} className="sticky left-0 z-10 bg-card px-4 pt-3.5 pb-1.5 sm:static">
          <div className="flex flex-wrap items-baseline gap-x-2.5 gap-y-0.5">
            <span className="font-medium">{guest.title}</span>
            {guest.ha ? (
              <Badge variant="outline" className="h-5 px-1.5 text-[11px] font-normal" title="Restarted on another replica when its node fails">
                HA
              </Badge>
            ) : null}
            <GuestState guest={guest} node={node} local={local} />
            {guest.haResource ? (
              <LiveMoveButton resource={guest.haResource} guest={guest.title} runsOn={node} className="h-6 sm:hidden" />
            ) : null}
            {href ? (
              <a
                className="text-primary sm:hidden"
                href={href}
                target="_blank"
                rel="noreferrer"
                aria-label={guest.link?.label}
              >
                <ExternalLink className="h-3 w-3" />
              </a>
            ) : null}
          </div>
        </td>
        {nodes.map((n) => (
          <td key={n} className={cn('px-0.5 pt-3.5 pb-1.5 text-center align-bottom sm:px-1', lane(n))}>
            {n === node ? (
              <Play aria-label="Runs here" className="mx-auto h-3 w-3 fill-current text-primary" />
            ) : null}
          </td>
        ))}
        <td className="hidden px-4 pt-3.5 pb-1.5 text-right sm:table-cell">
          {guest.haResource ? (
            <LiveMoveButton resource={guest.haResource} guest={guest.title} runsOn={node} className="h-7" />
          ) : null}
          {href ? (
            <a
              className="inline-flex items-center gap-1 text-xs whitespace-nowrap text-primary hover:underline"
              href={href}
              target="_blank"
              rel="noreferrer"
            >
              {guest.link?.label}
              <ExternalLink className="h-3 w-3" />
            </a>
          ) : null}
        </td>
      </tr>
      {guest.disks.map((d, i) => (
        <DiskRow
          key={d.resource.name}
          disk={d}
          status={statusOf.get(d.resource.name)}
          nodes={nodes}
          lane={lane}
          last={i === guest.disks.length - 1}
        />
      ))}
    </tbody>
  );
}

function GuestState({ guest, node, local }: { guest: Guest; node?: string; local: boolean }) {
  if (!node) return guest.template ? null : <span className="text-xs text-muted-foreground">stopped</span>;
  if (local)
    return (
      <span className="text-xs text-muted-foreground">
        running on <span className="text-foreground">{node}</span>
      </span>
    );
  return (
    <span className="inline-flex items-center gap-1 text-xs text-status-warn-text">
      <TriangleAlert className="h-3 w-3" />
      running on {node}, which holds no replica of its disks
    </span>
  );
}

function DiskRow({
  disk,
  status,
  nodes,
  lane,
  last,
}: {
  disk: GuestDisk;
  status?: ResourceStatus;
  nodes: string[];
  lane: (n: string) => string;
  last: boolean;
}) {
  const replication = replicationSummary(status);
  const size = formatGiB(diskBytes(disk.resource));
  const inStep = replication.tone === 'ok' ? 'In step' : replication.label;
  const pad = last ? 'pb-3.5' : '';
  return (
    <tr>
      <td className={cn('sticky left-0 z-10 bg-card py-1.5 pr-2 pl-5 sm:static sm:pr-3 sm:pl-7', pad)}>
        <div className="font-mono text-[13px] whitespace-nowrap" title={disk.resource.name}>
          {disk.label}
        </div>
        <div className="text-xs whitespace-nowrap sm:hidden">
          <span className="font-mono text-muted-foreground tabular-nums">{size}</span>
          <span className={cn('ml-2', TONE_TEXT[replication.tone])}>{inStep}</span>
        </div>
      </td>
      <td className={cn('hidden px-3 py-1.5 text-right font-mono text-muted-foreground tabular-nums sm:table-cell', pad)}>
        {size}
      </td>
      {nodes.map((n) => (
        <td key={n} className={cn('px-0.5 py-1.5 text-center sm:px-1', lane(n), pad)}>
          <Glyph cell={placementCell(disk, status, n)} node={n} />
        </td>
      ))}
      <td
        className={cn('hidden px-4 py-1.5 whitespace-nowrap sm:table-cell', TONE_TEXT[replication.tone], pad)}
        title={replication.title}
      >
        {inStep}
      </td>
    </tr>
  );
}

function Glyph({ cell, node }: { cell: PlacementCell; node: string }) {
  const label = `${node}: ${cell.title}`;
  if (cell.kind === 'replica')
    return (
      <span
        role="img"
        aria-label={label}
        title={label}
        className={cn('mx-auto block h-3.5 w-3.5 rounded-[3px]', TONE_BG[cell.tone])}
      />
    );
  if (cell.kind === 'diskless')
    return (
      <span
        role="img"
        aria-label={label}
        title={label}
        className={cn(
          'mx-auto block h-3.5 w-3.5 rounded-full border-2',
          cell.runsHere ? 'border-status-warn' : 'border-muted-foreground/45',
        )}
      />
    );
  return <span aria-hidden className="mx-auto block h-1 w-1 rounded-full bg-border" />;
}

function Legend() {
  const item = 'inline-flex items-center gap-1.5';
  return (
    <div className="flex flex-wrap gap-x-5 gap-y-1.5 border-t border-border px-4 py-2.5 text-xs text-muted-foreground">
      <span className={item}>
        <span className="h-3 w-3 rounded-[3px] bg-status-ok" /> replica, in step
      </span>
      <span className={item}>
        <span className="h-3 w-3 rounded-[3px] bg-status-warn" /> syncing
      </span>
      <span className={item}>
        <span className="h-3 w-3 rounded-[3px] bg-status-bad" /> behind or failed
      </span>
      <span className={item}>
        <span className="h-3 w-3 rounded-full border-2 border-muted-foreground/45" /> diskless, no replica
      </span>
      <span className={item}>
        <span className="h-3 w-3 rounded-full border-2 border-status-warn" /> guest reads it over the network
      </span>
      <span className={item}>
        <span className="inline-flex h-3 w-5 items-center justify-center rounded-[3px] bg-primary/[0.12]">
          <Play className="h-2.5 w-2.5 fill-current text-primary" />
        </span>
        node the guest runs on
      </span>
    </div>
  );
}
