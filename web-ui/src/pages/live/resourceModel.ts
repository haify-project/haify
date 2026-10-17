import type { Gateway, HaConfig, Resource, ResourceStatus } from '@/services/api';
import type { StatusTone } from '@/components/status';
import { nodeKinds, type FlowEdge, type LinkKind, type ResourceFlows } from '@/components/flow/model';
import { managerOf, replicationSummary, totalGbOf } from '../resources/replication';

// The live map's per-resource picture: for every resource, the node serving it
// with a line to each other member, and what uses it.

export interface ResourcePeer {
  name: string;
  kind: LinkKind;
  primary: boolean;
  /** A disk state worth saying: a replica that is not UpToDate. */
  disk?: string;
  /** The link from the centre node, when the status describes it. */
  edge?: FlowEdge;
  /** A link of this peer's that is not the centre's and is down or resyncing. */
  other?: FlowEdge;
}

export interface ResourceCard {
  name: string;
  tone: StatusTone;
  health: string;
  healthTitle?: string;
  usedBy: string[];
  /** The Primary, or without one the node whose view the status is. */
  centre?: { name: string; kind: LinkKind; primary: boolean; disk?: string };
  peers: ResourcePeer[];
  sizeGb: number;
  writeRate?: number;
  /** True while the first status read is still on its way. */
  pending: boolean;
}

const GATEWAY_WORD: Record<string, string> = {
  nfs: 'NFS',
  iscsi: 'iSCSI',
  nvmeof: 'NVMe-oF',
  nvme: 'NVMe-oF',
  smb: 'SMB',
};

/** What uses the resource, in the words an operator knows it by. */
export function usersOf(r: Resource, gateways: Gateway[], ha: HaConfig[]): string[] {
  const out: string[] = [];
  const m = managerOf(r);
  if (m) out.push(m.chip);
  for (const g of gateways.filter((g) => g.resource === r.name)) {
    const word = GATEWAY_WORD[g.type.toLowerCase()] ?? g.type;
    out.push(g.path ? `${word} ${g.path}` : `${word} gateway`);
  }
  const h = ha.find((c) => c.resource === r.name);
  if (h) out.push(h.services?.length ? `HA ${h.services.join(', ')}` : `HA ${h.mountPoint || h.vip}`);
  return out;
}

/** A disk state worth showing next to a member. */
function diskNote(kind: LinkKind, disk: string | undefined) {
  if (!disk || disk === 'UpToDate') return undefined;
  if (disk === 'Diskless' && (kind === 'tiebreaker' || kind === 'client')) return undefined;
  return disk;
}

export function buildResourceCard(
  r: Resource,
  status: ResourceStatus | undefined,
  flows: ResourceFlows | undefined,
  users: string[],
  loading: boolean,
): ResourceCard {
  const health = replicationSummary(status);
  const kindOf = nodeKinds(r, status);
  const stateOf = new Map<string, { role: string; disk: string }>();
  for (const [host, st] of Object.entries(status?.nodeStates ?? {}))
    stateOf.set(st.node || host, { role: st.role, disk: st.diskState });

  const members = [
    ...new Set([...(r.nodes ?? []), ...(r.disklessNodes ?? []), ...(r.disklessClients ?? [])]),
  ];
  const isPrimary = (n: string) => (stateOf.get(n)?.role ?? '').toLowerCase() === 'primary';
  const centreName =
    flows?.primary ?? members.find(isPrimary) ?? flows?.reporter ?? (r.nodes ?? [])[0] ?? members[0];
  const edges = flows?.edges ?? [];
  const between = (a: string, b: string) => edges.find((e) => (e.a === a && e.b === b) || (e.a === b && e.b === a));

  const rank: Record<LinkKind, number> = { data: 0, wan: 1, client: 2, tiebreaker: 3 };
  const peers: ResourcePeer[] = members
    .filter((n) => n !== centreName)
    .map((n) => {
      const kind = kindOf(n);
      const other = edges.find(
        (e) =>
          (e.a === n || e.b === n) &&
          e.a !== centreName &&
          e.b !== centreName &&
          (e.state === 'down' || e.state === 'resync' || e.state === 'paused'),
      );
      return {
        name: n,
        kind,
        primary: isPrimary(n),
        disk: diskNote(kind, stateOf.get(n)?.disk),
        edge: centreName ? between(centreName, n) : undefined,
        other,
      };
    })
    .sort((a, b) => rank[a.kind] - rank[b.kind] || a.name.localeCompare(b.name));

  const centreKind = centreName ? kindOf(centreName) : 'data';
  return {
    name: r.name,
    tone: health.tone,
    health: health.label,
    healthTitle: health.title,
    usedBy: users,
    centre: centreName
      ? {
          name: centreName,
          kind: centreKind,
          primary: isPrimary(centreName),
          disk: diskNote(centreKind, stateOf.get(centreName)?.disk),
        }
      : undefined,
    peers,
    sizeGb: totalGbOf(r),
    writeRate: flows?.writeRate,
    pending: !status && loading,
  };
}

// Unknown sits with healthy: every card starts unknown until its status
// answers, and a card should not jump when it does.
const TONE_RANK: Record<StatusTone, number> = { bad: 0, warn: 1, ok: 2, idle: 2 };

/**
 * Trouble first, then by name. Never by traffic: rates change every few
 * seconds, and cards that swap places under the pointer cannot be read.
 */
export function sortCards(cards: ResourceCard[]): ResourceCard[] {
  return [...cards].sort((a, b) => TONE_RANK[a.tone] - TONE_RANK[b.tone] || a.name.localeCompare(b.name));
}
