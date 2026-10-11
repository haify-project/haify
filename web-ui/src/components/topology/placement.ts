import type { Resource, ResourceStatus, NodeResourceState } from '@/services/api';
import { toneOf, type StatusTone } from '@/components/status';

export type NodeKind = 'replica' | 'tiebreaker' | 'client' | 'dr';

export interface PlacedNode {
  name: string;
  kind: NodeKind;
  state?: NodeResourceState;
}

/**
 * Tone → the SVG paint for it. `TONE_BG` is a Tailwind *background* class and a
 * `class` on a `<rect>` sets no fill, so the diagram needs the tokens
 * themselves. Same four names, same four variables — one vocabulary with the
 * tables' StatusTick, just spelled for an attribute.
 */
export const TONE_FILL: Record<StatusTone, string> = {
  ok: 'var(--status-ok)',
  warn: 'var(--status-warn)',
  bad: 'var(--status-bad)',
  idle: 'var(--status-idle)',
};

/** Where a resource's copies live and what each one is. */
export function placeNodes(resource: Resource, status: ResourceStatus) {
  // The state map is keyed by DRBD host name; each entry carries the Haify node
  // name so it can be paired back to the node list.
  const stateByNode = new Map<string, NodeResourceState>();
  for (const [host, st] of Object.entries(status.nodeStates ?? {})) {
    stateByNode.set(st.node || host, st);
  }

  const drNode = status.wan ? status.drNode : undefined;
  const tiebreakers = new Set(resource.disklessNodes ?? []);
  const clients = new Set(resource.disklessClients ?? []);

  const classify = (name: string): NodeKind => {
    if (name === drNode) return 'dr';
    if (tiebreakers.has(name)) return 'tiebreaker';
    if (clients.has(name)) return 'client';
    return 'replica';
  };

  const all = [
    ...(status.nodes ?? resource.nodes ?? []),
    ...(resource.disklessNodes ?? []),
    ...(resource.disklessClients ?? []),
  ];
  const seen = new Set<string>();
  const nodes: PlacedNode[] = [];
  for (const name of all) {
    if (seen.has(name)) continue;
    seen.add(name);
    nodes.push({ name, kind: classify(name), state: stateByNode.get(name) });
  }

  const local = nodes.filter((n) => n.kind !== 'dr');
  const remote = nodes.filter((n) => n.kind === 'dr');

  // Where the replica's bytes actually sit. The volume list is per-resource, so
  // it is the same sentence on every replica — which is the point: a node whose
  // box omits it has no local copy at all.
  const vols = resource.volumes ?? [];
  const pool =
    vols.length > 0 && vols.every((v) => v.pool && v.pool === vols[0].pool)
      ? vols[0].pool
      : undefined;
  // sizeGb is a proto int64, i.e. a JSON string: summed as-is it concatenates
  // ("0" + "2" read as "02 GB").
  const totalGb = vols.reduce((sum, v) => sum + (Number(v.sizeGb) || 0), 0);
  const backing = [pool, totalGb > 0 ? `${totalGb} GB` : undefined]
    .filter(Boolean)
    .join(' · ');

  // Two independent facts about a WAN leg, and they can disagree.
  //
  // `replicating` is DRBD's own verdict on the DR peer — Established means bytes
  // are crossing, whatever anything else claims. `proxy` is the haify-proxy
  // process probe at both ends; reporting the near end alone would call a tunnel
  // healthy while the far side is dead.
  //
  // Observed on the live cluster: every proxy leg reads "inactive" while the DR
  // peer is Established and UpToDate at 100%. So the probe is not a proxy for
  // link health, and drawing a red broken line over a link that is demonstrably
  // carrying replication would be the diagram asserting something the data does
  // not say. Replication decides how the leg is drawn; the probe is an
  // annotation beside it.
  const legState = (primary: string) => {
    const proxy = status.wanProxy ?? {};
    const near = proxy[primary];
    const far =
      remote.length > 0
        ? (proxy[`${remote[0].name} (leg ${primary})`] ?? proxy[remote[0].name])
        : undefined;
    const probed = near !== undefined || far !== undefined;
    const proxyUp = near === 'active' && (far === undefined || far === 'active');
    const drState = remote[0]?.state?.replicationState ?? '';
    return {
      replicating: /^(established|syncsource|synctarget|verif|pausedsync)/i.test(drState),
      drState,
      proxy: !probed ? ('unknown' as const) : proxyUp ? ('active' as const) : ('inactive' as const),
    };
  };

  return { local, remote, backing, legState };
}

/**
 * The rail repeats the disk state the box already writes out. A tiebreaker or
 * a diskless client has no disk *on purpose*, so reading its "Diskless" state
 * through `toneOf` would paint every healthy quorum vote red; for those the
 * link is the health.
 */
export function railTone(node: PlacedNode): StatusTone {
  const st = node.state;
  if (!st) return 'idle';
  if (node.kind === 'tiebreaker' || node.kind === 'client') {
    const r = st.replicationState;
    return !r || r === 'Established' ? 'ok' : 'warn';
  }
  return toneOf(st.diskState);
}
