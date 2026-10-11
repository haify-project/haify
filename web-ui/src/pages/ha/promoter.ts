import {
  HaConfig,
  Resource,
  ResourceStatus,
  type HaPromoterStatus,
  type HaUnit,
} from '@/services/api';
import { mountUnitFor, vipUnitFor } from '@/lib/toml';

/** One HA config joined to the two payloads that describe it: the cluster-wide
 * resource record and its live per-resource status. Assembled once in the page
 * so the topology and the promoter card read the same numbers. */
export type PromoterView = {
  config: HaConfig;
  resource?: Resource;
  status?: ResourceStatus;
  // What drbd-reactor is actually running, as opposed to what the controller
  // has on file. See startListFor.
  promoter?: HaPromoterStatus;
  primaryNode?: string;
  /** Node names that carry a replica of this resource. */
  members: string[];
  /** The member currently promoted, resolved from primaryNode's host name. */
  activeMember?: string;
};

/**
 * The promoter's start[], preferring what is deployed over what is configured.
 *
 * Composing it from the config is what this used to do, and the comment said it
 * could not disagree with the TOML. It can: `haify-meta` on this cluster starts
 * four units and the controller's config lists one service, because haify-ai was
 * added to the promoter without going back through the controller. A card built
 * from the config alone told an operator that three things move on a failover
 * when four do — and the missing one was the Copilot.
 *
 * So the live `deps` win when the status call answered. The first dep is
 * drbd-promote@<resource>, which is the promotion itself rather than an entry
 * of start[], and it is dropped. Falling back to the config is right when the
 * resource is down: there is no promoter to ask, and the intended list is still
 * worth showing.
 */
export function startListFor(
  config: HaConfig,
  promoter?: HaPromoterStatus,
): { units: StartUnit[]; live: boolean } {
  const deps = (promoter?.deps ?? []).filter((d) => !HIDDEN_KINDS.has(unitKind(d)));
  if (deps.length > 0) return { units: deps, live: true };
  const units = [
    config.mountPoint ? mountUnitFor(config.mountPoint) : '',
    config.vip ? vipUnitFor(config.vip) : '',
    ...(config.services ?? []),
  ]
    .filter((u) => u !== '')
    .map((name) => ({ name }));
  return { units, live: false };
}

type StartUnit = Omit<HaUnit, 'status'> & { status?: string };

// The promotion itself and the target unit wrapping the chain are not services
// anyone runs; their state shows in the card header instead.
const HIDDEN_KINDS = new Set(['promote', 'target']);

/** A unit's kind, read off its name when the controller does not send one. */
export function unitKind(u: Pick<HaUnit, 'name' | 'kind'>): string {
  if (u.kind) return u.kind;
  if (u.name.startsWith('drbd-promote@')) return 'promote';
  if (u.name.startsWith('drbd-services@') || u.name.endsWith('.target')) return 'target';
  if (u.name.endsWith('.mount')) return 'mount';
  if (u.name.startsWith('service-ip@')) return 'service_ip';
  return 'service';
}

/** A failed promote or target unit, which the header reports in place of the hidden rows. */
export function hiddenFailure(promoter?: HaPromoterStatus): HaUnit | undefined {
  const units = [...(promoter?.deps ?? []), ...(promoter?.target ? [promoter.target] : [])];
  return units.find((u) => HIDDEN_KINDS.has(unitKind(u)) && u.status?.toLowerCase() === 'failed');
}

/** Units the promoter runs that the controller's own config does not mention.
 *  Naming them is the point: this is the gap between what the console can
 *  manage and what a failover will actually carry. */
export function unmanagedUnits(config: HaConfig, units: StartUnit[]): string[] {
  const known = new Set(
    [
      config.mountPoint ? mountUnitFor(config.mountPoint) : '',
      config.vip ? vipUnitFor(config.vip) : '',
      ...(config.services ?? []),
    ].filter((u) => u !== ''),
  );
  return units.map((u) => u.name).filter((n) => !known.has(n));
}
