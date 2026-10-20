import { num, type ResourceListItem } from '@/services/api';

// Words and colours for the resource list, from what the controller returns.

export const STATES: Record<string, { label: string; color: string }> = {
  healthy: { label: 'Healthy', color: 'var(--status-ok)' },
  resyncing: { label: 'Resyncing', color: 'var(--primary)' },
  degraded: { label: 'Degraded', color: 'var(--status-warn)' },
  no_quorum: { label: 'Without quorum', color: 'var(--status-bad)' },
  unknown: { label: 'No status', color: 'var(--status-idle)' },
};

export const USED_BY: Record<string, string> = {
  pvc: 'PVC',
  proxmox: 'Proxmox',
  openstack: 'OpenStack',
  'xcp-ng': 'XCP-ng',
  opennebula: 'OpenNebula',
  gateway: 'Gateway',
  ha: 'HA service',
  app: 'App',
};

export const REPLICA_COLOR: Record<string, string> = {
  ok: 'var(--status-ok)',
  resyncing: 'var(--primary)',
  bad: 'var(--status-bad)',
  tiebreaker: 'var(--status-idle)',
};

/** The name an operator knows the resource by, and the internal one under it
 *  when they differ. */
export function displayName(r: ResourceListItem): { title: string; sub?: string } {
  if (r.owner?.startsWith('PVC ')) return { title: r.owner.slice(4), sub: r.name };
  return { title: r.name };
}

/** The resource's size in bytes across its volumes. */
export function sizeBytes(r: ResourceListItem): number {
  return r.volumes.reduce((n, v) => n + (num(v.sizeBytes) || Number(v.sizeGb) * 1024 ** 3), 0);
}
