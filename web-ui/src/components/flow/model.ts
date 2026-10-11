import type { NodeResourceState, Resource, ResourceStatus } from '@/services/api';
import { BUSY_KIBS, flowLevel, reporterHost, type StatusRates } from './rates';

// Which way a resource's data is moving between its nodes, and how fast.
//
// A status answer is one node's view: its own entry plus one entry per peer
// connection. So the links it can actually measure are the answering node's
// own. When that node is the Primary those are exactly the links writes travel
// on. When it is not, the Primary's link to it is still measured (what it
// receives from the Primary), and the Primary's links to the other replicas
// are drawn from that: DRBD sends every write to every connected replica, so
// they carry the same stream. Those derived links animate but carry no number.

export type LinkKind = 'data' | 'wan' | 'tiebreaker' | 'client';
export type LinkState = 'idle' | 'busy' | 'resync' | 'paused' | 'down' | 'unknown';
export type FlowKind = 'write' | 'read' | 'resync';

export interface Flow {
  from: string;
  to: string;
  kind: FlowKind;
  /** Measured KiB/s; undefined when this link's rate is not reported. */
  rate?: number;
  /** Animation intensity 0..1. */
  level: number;
}

export interface FlowEdge {
  key: string;
  resource: string;
  a: string;
  b: string;
  kind: LinkKind;
  state: LinkState;
  flows: Flow[];
  /** Resync progress, 0..100, while state is resync or paused. */
  syncPercent?: number;
  /** DRBD's word for a link that is down. */
  word?: string;
  /** False for a Primary→replica link inferred from another node's view. */
  measured: boolean;
}

export interface ResourceFlows {
  edges: FlowEdge[];
  primary?: string;
  reporter?: string;
  /** The Primary's write rate, KiB/s, when it can be read off the counters. */
  writeRate?: number;
  /** Whether this controller reports traffic counters. */
  counters: boolean;
}

/** Resync without counters still moves; this is how fast it is drawn. */
const RESYNC_FALLBACK_LEVEL = 0.45;
const RESYNC_MIN_LEVEL = 0.15;

const RESYNC_ACTIVE = /^(SyncSource|SyncTarget|StartingSync|WFBitMap)/;
const RESYNC_PAUSED = /^PausedSync/;

/** A peer link as the answering node sees it. */
function linkUp(st: NodeResourceState): boolean {
  if (st.connection !== undefined) return st.connection === 'Connected';
  const r = st.replicationState;
  return Boolean(r) && r !== 'Off' && r !== 'StandAlone';
}

/** Whether the answering node is the resync source toward this peer. */
const isSource = (rs: string) => /S$/.test(rs) || rs === 'SyncSource';

export function nodeKinds(resource: Resource, status?: ResourceStatus) {
  const dr = status?.wan ? status.drNode : resource.wanMode ? resource.drNode : undefined;
  const tiebreakers = new Set(resource.disklessNodes ?? []);
  const clients = new Set(resource.disklessClients ?? []);
  return (name: string): LinkKind => {
    if (name === dr) return 'wan';
    if (tiebreakers.has(name)) return 'tiebreaker';
    if (clients.has(name)) return 'client';
    return 'data';
  };
}

/** The kind of a link: the less ordinary end decides. */
function linkKind(ka: LinkKind, kb: LinkKind): LinkKind {
  for (const k of ['tiebreaker', 'client', 'wan'] as const) if (ka === k || kb === k) return k;
  return 'data';
}

export function resourceFlows(
  resource: Resource,
  status: ResourceStatus | undefined,
  rates: StatusRates,
): ResourceFlows {
  if (!status) return { edges: [], counters: false };
  const kindOf = nodeKinds(resource, status);
  const entries = Object.entries(status.nodeStates ?? {}).map(([host, st]) => ({
    host,
    name: st.node || host,
    st,
  }));
  const repHost = reporterHost(status);
  const rep = entries.find((e) => e.host === repHost);
  const primaryEntry = entries.find((e) => e.st.role === 'Primary');
  const primary = primaryEntry?.name;
  const counters = rates.available;

  const flowKind = (from: string, to: string): FlowKind =>
    kindOf(to) === 'client' && from !== primary ? 'read' : 'write';

  const measured: FlowEdge[] = [];
  if (rep) {
    for (const p of entries) {
      if (p === rep) continue;
      const kind = linkKind(kindOf(rep.name), kindOf(p.name));
      const r = rates.byHost[p.host] ?? {};
      const edge: FlowEdge = {
        key: `${resource.name}|${rep.name}|${p.name}`,
        resource: resource.name,
        a: rep.name,
        b: p.name,
        kind,
        state: 'idle',
        flows: [],
        measured: true,
      };
      const rs = p.st.replicationState ?? '';
      if (!linkUp(p.st)) {
        edge.state = 'down';
        edge.word = p.st.connection || rs || 'disconnected';
      } else if (RESYNC_PAUSED.test(rs)) {
        edge.state = 'paused';
        edge.syncPercent = p.st.syncPercent;
      } else if (RESYNC_ACTIVE.test(rs)) {
        edge.state = 'resync';
        edge.syncPercent = p.st.syncPercent;
        const out = isSource(rs);
        const rate = out ? r.sent : r.received;
        edge.flows.push({
          from: out ? rep.name : p.name,
          to: out ? p.name : rep.name,
          kind: 'resync',
          rate,
          level:
            rate === undefined ? RESYNC_FALLBACK_LEVEL : Math.max(RESYNC_MIN_LEVEL, flowLevel(rate)),
        });
      } else if (kind !== 'tiebreaker') {
        // Quorum votes carry no data; anything they exchange is bookkeeping.
        for (const [from, to, rate] of [
          [rep.name, p.name, r.sent],
          [p.name, rep.name, r.received],
        ] as const) {
          if (rate !== undefined && rate >= BUSY_KIBS)
            edge.flows.push({ from, to, kind: flowKind(from, to), rate, level: flowLevel(rate) });
        }
        if (edge.flows.length) edge.state = 'busy';
      }
      measured.push(edge);
    }
  }

  let edges = measured;
  if (rep && primaryEntry && primaryEntry !== rep) {
    // The Primary's stream as the answering node receives it.
    const fromPrimary = measured.find((e) => e.b === primary);
    const stream = fromPrimary?.flows.find((f) => f.from === primary && f.kind === 'write');
    edges = [];
    if (fromPrimary) edges.push(fromPrimary);
    for (const y of entries) {
      if (y === rep || y === primaryEntry) continue;
      const viaRep = measured.find((e) => e.b === y.name)!;
      const kind = linkKind(kindOf(primary!), kindOf(y.name));
      // What the answering node knows of y says nothing certain about y's link
      // to the Primary, except that a node nobody can reach is likely gone.
      const unknown = viaRep.state === 'down';
      const derived: FlowEdge = {
        key: `${resource.name}|${primary}|${y.name}`,
        resource: resource.name,
        a: primary!,
        b: y.name,
        kind,
        state: unknown ? 'unknown' : 'idle',
        flows: [],
        measured: false,
      };
      if (!unknown && stream && (kind === 'data' || kind === 'wan')) {
        derived.state = 'busy';
        derived.flows.push({ from: primary!, to: y.name, kind: 'write', level: stream.level });
      }
      edges.push(derived);
      // The answering node's own link to y is worth drawing only when it says
      // something the derived one cannot: it is down, or a resync runs on it.
      if (viaRep.state === 'down' || viaRep.state === 'resync' || viaRep.state === 'paused')
        edges.push(viaRep);
    }
  }

  // The Primary's write rate: its own counter when it answered; otherwise what
  // a replica receives from it, which is every write it makes — unless a
  // resync shares that link, or the answering node holds no copy.
  let writeRate: number | undefined;
  if (rep && rep === primaryEntry) writeRate = rates.byHost[rep.host]?.written;
  else if (rep && primaryEntry) {
    const link = measured.find((e) => e.b === primary);
    const replica = kindOf(rep.name) === 'data' || kindOf(rep.name) === 'wan';
    if (replica && link && (link.state === 'idle' || link.state === 'busy'))
      writeRate = rates.byHost[primaryEntry.host]?.received;
  }
  return { edges, primary, reporter: rep?.name, writeRate, counters };
}

/** The flows on an edge that run from `from` to `to`. */
export const flowsFrom = (edge: FlowEdge, from: string) => edge.flows.filter((f) => f.from === from);

/** Sum of the measured rates on an edge, or undefined when none is measured. */
export function measuredRate(flows: Flow[]): number | undefined {
  const known = flows.filter((f) => f.rate !== undefined);
  return known.length ? known.reduce((s, f) => s + (f.rate ?? 0), 0) : undefined;
}
