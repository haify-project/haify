import { type NodeResourceState, type Resource, type ResourceStatus } from '../../services/api';
import { type StatusTone } from '@/components/status';
import { isPeerSyncing } from '../resources/replication';

// Guests and the Haify resources their disks are on, whatever runs them:
// Proxmox VE (pve.ts) and libvirt (libvirt.ts) each turn the resource list into
// Guests, and the placement matrix draws them the same way.

export interface GuestDisk {
  resource: Resource;
  /** The name the hypervisor shows for the disk, e.g. vm-101-disk-0. */
  label: string;
  /** False for small volumes read once or rarely (cloud-init, VM state). */
  data: boolean;
}

export interface Guest {
  key: string;
  /** How the hypervisor names the guest: "VM 101", "Template 100", "web1". */
  title: string;
  template: boolean;
  disks: GuestDisk[];
  /** Restarted on another replica by drbd-reactor when its node fails. */
  ha?: boolean;
  /** The resource whose HA config runs the guest, when it can be moved live. */
  haResource?: string;
  /** The guest's page in the hypervisor's own interface, on a node's address. */
  link?: { label: string; url: (address: string) => string };
}

/** Bytes of a disk: the exact size when it has one, else its whole GiB. */
export function diskBytes(resource: Resource): number {
  const v = resource.volumes[0];
  if (!v) return 0;
  const exact = Number(v.sizeBytes ?? 0);
  return exact > 0 ? exact : Number(v.sizeGb) * 1024 ** 3;
}

export function formatGiB(bytes: number): string {
  const gib = bytes / 1024 ** 3;
  if (gib >= 100) return `${gib.toFixed(0)} GiB`;
  if (gib >= 1) return `${gib.toFixed(1).replace(/\.0$/, '')} GiB`;
  return `${(bytes / 1024 ** 2).toFixed(0)} MiB`;
}

// ---------------------------------------------------------------------------
// Placement: for each disk, what every node holds of it, and where the guest
// runs. The node holding a disk Primary is the node running the guest.
// ---------------------------------------------------------------------------

export type CellKind = 'replica' | 'diskless' | 'none';

export interface PlacementCell {
  kind: CellKind;
  /** For a replica: in step, syncing, behind or failed; idle until read. */
  tone: StatusTone;
  runsHere: boolean;
  /** What the cell means, in words, for its tooltip. */
  title: string;
}

/** The live state of `node` for a resource, matched by Haify node name. */
export function stateOn(status: ResourceStatus | undefined, node: string): NodeResourceState | undefined {
  return Object.entries(status?.nodeStates ?? {}).find(([host, st]) => (st.node || host) === node)?.[1];
}

export function placementCell(disk: GuestDisk, status: ResourceStatus | undefined, node: string): PlacementCell {
  const r = disk.resource;
  const st = stateOn(status, node);
  const runsHere = st?.role === 'Primary';
  if (r.nodes.includes(node)) {
    if (!st) return { kind: 'replica', tone: 'idle', runsHere, title: 'Replica, state not read yet' };
    if (isPeerSyncing(st))
      return { kind: 'replica', tone: 'warn', runsHere, title: `Replica, syncing ${Math.round(st.syncPercent ?? 0)}%` };
    if (st.diskState === 'UpToDate') return { kind: 'replica', tone: 'ok', runsHere, title: 'Replica, in step' };
    if (st.diskState === 'Inconsistent')
      return { kind: 'replica', tone: 'warn', runsHere, title: 'Replica, Inconsistent until its sync ends' };
    return { kind: 'replica', tone: 'bad', runsHere, title: `Replica, ${st.diskState || 'unreachable'}` };
  }
  if ((r.disklessNodes ?? []).includes(node) || (r.disklessClients ?? []).includes(node) || st)
    return {
      kind: 'diskless',
      tone: 'idle',
      runsHere,
      title: runsHere ? 'No replica: the guest reads and writes over the network' : 'Diskless: a quorum vote, no replica',
    };
  return { kind: 'none', tone: 'idle', runsHere: false, title: 'Not on this node' };
}

/** A guest's data disks; cloud-init and state volumes are read once or rarely. */
export function dataDisks(guest: Guest): GuestDisk[] {
  const data = guest.disks.filter((d) => d.data);
  return data.length ? data : guest.disks;
}

export function runningOn(guest: Guest, statusOf: Map<string, ResourceStatus | undefined>): string | undefined {
  for (const d of guest.disks) {
    const hit = Object.entries(statusOf.get(d.resource.name)?.nodeStates ?? {}).find(([, st]) => st.role === 'Primary');
    if (hit) return hit[1].node || hit[0];
  }
  return undefined;
}

/** Whether a running guest's node holds a replica of every one of its data disks. */
export function runsOnReplica(guest: Guest, node: string | undefined): boolean {
  return !!node && dataDisks(guest).every((d) => d.resource.nodes.includes(node));
}

/** Every node any of these disks is on, replica or diskless, by name. */
export function nodeColumns(guests: Guest[]): string[] {
  const all = new Set<string>();
  for (const g of guests)
    for (const d of g.disks)
      for (const n of [...d.resource.nodes, ...(d.resource.disklessNodes ?? []), ...(d.resource.disklessClients ?? [])])
        all.add(n);
  return [...all].sort((a, b) => a.localeCompare(b, undefined, { numeric: true }));
}
