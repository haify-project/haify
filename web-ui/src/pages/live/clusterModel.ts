import { poolRoomGiB, type Node, type Pool, type Resource } from '@/services/api';
import { toneOf, type StatusTone } from '@/components/status';
import { flowLevel } from '@/components/flow/rates';
import type { Flow, FlowEdge, LinkKind, LinkState, ResourceFlows } from '@/components/flow/model';

// The live map's picture of the cluster: one card per node and one link per
// pair of nodes that share a resource, with every resource's traffic between
// the two summed onto it.

export interface MapNode {
  name: string;
  state: string;
  tone: StatusTone;
  /** 0..100 of the node's pool capacity in use; undefined without pools. */
  poolPct?: number;
  primaries: number;
  replicas: number;
  tiebreakers: number;
  clients: number;
  /** KiB/s written by the resources this node is Primary for, where known. */
  writeRate?: number;
}

export interface MapLink extends FlowEdge {
  resources: string[];
}

/** A node's own state word as a tone: maintenance is not an outage. */
function nodeTone(state: string): StatusTone {
  const s = state.toLowerCase();
  if (s === 'online') return 'ok';
  if (s === 'maintenance' || s === 'draining') return 'warn';
  if (s === 'offline' || s === 'evicted' || s === 'lost') return 'bad';
  return toneOf(s);
}

const KIND_RANK: Record<LinkKind, number> = { tiebreaker: 0, client: 1, wan: 2, data: 3 };
const STATE_RANK: Record<LinkState, number> = { unknown: 0, idle: 1, busy: 2, paused: 3, resync: 4, down: 5 };

/** Flows on one pair, merged per direction and kind, with known rates summed. */
function mergeFlows(flows: Flow[]): Flow[] {
  const groups = new Map<string, Flow[]>();
  for (const f of flows) {
    const k = `${f.from}|${f.to}|${f.kind}`;
    groups.set(k, [...(groups.get(k) ?? []), f]);
  }
  return [...groups.values()].map((g) => {
    const known = g.filter((f) => f.rate !== undefined);
    const rate = known.length ? known.reduce((s, f) => s + (f.rate ?? 0), 0) : undefined;
    const level = Math.max(...g.map((f) => f.level), rate !== undefined ? flowLevel(rate) : 0);
    return { ...g[0], rate, level };
  });
}

export function buildClusterMap(
  nodes: Node[],
  pools: Pool[],
  resources: Resource[],
  flows: Map<string, ResourceFlows>,
): { nodes: MapNode[]; links: MapLink[]; counters: boolean } {
  const sorted = [...nodes].sort((a, b) => a.name.localeCompare(b.name));
  const nameOf = new Map<string, string>();
  for (const n of sorted) {
    nameOf.set(n.address, n.name);
    if (n.hostname) nameOf.set(n.hostname, n.name);
    nameOf.set(n.name, n.name);
  }

  const mapNodes = new Map<string, MapNode>(
    sorted.map((n) => {
      const own = pools.filter((p) => (nameOf.get(p.node) ?? p.node) === n.name);
      let total = 0;
      let free = 0;
      for (const p of own) {
        const room = poolRoomGiB(p);
        if (Number.isFinite(room.total) && room.total > 0) {
          total += room.total;
          free += Math.max(0, Math.min(room.total, room.free || 0));
        }
      }
      return [
        n.name,
        {
          name: n.name,
          state: n.state,
          tone: nodeTone(n.state),
          poolPct: total > 0 ? ((total - free) / total) * 100 : undefined,
          primaries: 0,
          replicas: 0,
          tiebreakers: 0,
          clients: 0,
        },
      ];
    }),
  );

  const pairs = new Map<string, MapLink>();
  let counters = false;
  for (const r of resources) {
    const tiebreakers = new Set(r.disklessNodes ?? []);
    const clients = new Set(r.disklessClients ?? []);
    for (const m of r.nodes ?? []) {
      const n = mapNodes.get(m);
      if (n && !tiebreakers.has(m) && !clients.has(m)) n.replicas++;
    }
    for (const t of tiebreakers) {
      const n = mapNodes.get(t);
      if (n) n.tiebreakers++;
    }
    for (const c of clients) {
      const n = mapNodes.get(c);
      if (n) n.clients++;
    }

    const f = flows.get(r.name);
    if (!f) continue;
    counters ||= f.counters;
    if (f.primary) {
      const p = mapNodes.get(f.primary);
      if (p) {
        p.primaries++;
        if (f.writeRate !== undefined) p.writeRate = (p.writeRate ?? 0) + f.writeRate;
      }
    }
    for (const e of f.edges) {
      if (!mapNodes.has(e.a) || !mapNodes.has(e.b)) continue;
      const [a, b] = [e.a, e.b].sort();
      const key = `${a}|${b}`;
      const link = pairs.get(key);
      if (!link) {
        pairs.set(key, { ...e, key, a, b, resource: r.name, resources: [r.name], flows: [...e.flows] });
        continue;
      }
      link.resources.push(r.name);
      if (KIND_RANK[e.kind] > KIND_RANK[link.kind]) link.kind = e.kind;
      if (STATE_RANK[e.state] > STATE_RANK[link.state]) {
        link.state = e.state;
        link.word = e.word;
      }
      if (e.syncPercent !== undefined)
        link.syncPercent = Math.min(link.syncPercent ?? 100, e.syncPercent);
      link.flows.push(...e.flows);
      link.measured ||= e.measured;
    }
  }
  for (const l of pairs.values()) l.flows = mergeFlows(l.flows);

  return { nodes: [...mapNodes.values()], links: [...pairs.values()], counters };
}
