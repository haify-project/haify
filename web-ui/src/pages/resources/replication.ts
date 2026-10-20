import { Resource, ResourceStatus, NodeResourceState } from '../../services/api';
import { type StatusTone } from '@/components/status';

// DRBD replication states that mean an active (or paused) resync is underway.
const SYNC_REPLICATION_STATES = new Set([
  'SyncSource',
  'SyncTarget',
  'PausedSyncS',
  'PausedSyncT',
  'StartingSyncS',
  'StartingSyncT',
  'WFBitMapS',
  'WFBitMapT',
]);

/** The system whose volume a resource is, when one manages its lifecycle. */
export interface ResourceManager {
  /** Shown as a chip on the resource. */
  chip: string;
  /** What the chip's tooltip says. */
  about: string;
  /** Why deleting the resource here is wrong, and what to do instead. */
  deleteWarning: string;
}

// Each integration stamps its label on every resource it creates. Deleting
// such a resource here leaves that system holding a volume whose data is gone,
// so the UI marks it and warns before a manual delete.
const MANAGERS: { label: string; value: string; manager: ResourceManager }[] = [
  {
    label: 'haify.csi/managed-by',
    value: 'csi',
    manager: {
      chip: 'kubernetes',
      about: 'Provisioned by the Kubernetes CSI driver. Delete the PersistentVolumeClaim instead of removing it here.',
      deleteWarning:
        'This volume was provisioned by the Kubernetes CSI driver. Deleting it here leaves the PersistentVolume that still references it stranded, and Kubernetes will not recreate the data. Delete the PersistentVolumeClaim instead and let the driver clean up.',
    },
  },
  {
    label: 'haify.pve/managed-by',
    value: 'pve',
    manager: {
      chip: 'proxmox',
      about: 'A Proxmox VE disk. Remove it from its guest in Proxmox VE instead of here.',
      deleteWarning:
        'This is a Proxmox VE disk. Deleting it here leaves its guest pointing at a disk whose data is gone. Remove the disk from the guest in Proxmox VE instead.',
    },
  },
  {
    label: 'haify.openstack/managed-by',
    value: 'cinder',
    manager: {
      chip: 'openstack',
      about: 'An OpenStack Cinder volume. Delete it with Cinder instead of here.',
      deleteWarning:
        'This is an OpenStack Cinder volume. Deleting it here leaves Cinder, and any instance it is attached to, with a volume whose data is gone. Delete it with Cinder (openstack volume delete) instead.',
    },
  },
  {
    label: 'haify.opennebula/managed-by',
    value: 'opennebula',
    manager: {
      chip: 'opennebula',
      about: 'An OpenNebula image or VM disk. Delete it in OpenNebula instead of here.',
      deleteWarning:
        'This is an OpenNebula image or VM disk. Deleting it here leaves OpenNebula, and any VM using it, with a disk whose data is gone. Delete the image (oneimage delete) or the VM in OpenNebula instead.',
    },
  },
];

// The claim a CSI volume was provisioned for, as namespace/name. Volumes
// created before the driver recorded it do not have it.
const PVC_LABEL = 'haify.csi/pvc';

export function managerOf(resource: Resource): ResourceManager | null {
  for (const m of MANAGERS) {
    if (resource.labels?.[m.label] !== m.value) continue;
    const pvc = m.value === 'csi' ? resource.labels?.[PVC_LABEL] : undefined;
    // A pvc-<uid> name says nothing about which application it is; its claim does.
    return pvc ? { ...m.manager, chip: `PVC ${pvc}` } : m.manager;
  }
  return null;
}

// The text colour that pairs with each tone. `TONE_SOFT` carries a fill with it,
// which is too loud for a word sitting on the row's own background. Literal
// class strings so the Tailwind scanner still sees all four.
export const TONE_TEXT: Record<StatusTone, string> = {
  ok: 'text-status-ok-text',
  warn: 'text-status-warn-text',
  bad: 'text-status-bad-text',
  idle: 'text-muted-foreground',
};

// isPeerSyncing reports whether a peer node-state represents a resync in
// progress. The local node has no replication relationship (empty
// replicationState) so it never counts as syncing.
export function isPeerSyncing(state?: NodeResourceState): boolean {
  if (!state) return false;
  const rs = state.replicationState ?? '';
  if (SYNC_REPLICATION_STATES.has(rs)) return true;
  // Any non-idle replication state still short of 100% counts as syncing.
  if (rs && rs !== 'Established' && rs !== 'Off' && (state.syncPercent ?? 100) < 100)
    return true;
  return false;
}

// statusHasActiveSync is the adaptive-polling predicate: true while any peer of
// the resource is resyncing, false when everything is idle/UpToDate.
function statusHasActiveSync(status?: ResourceStatus): boolean {
  if (!status) return false;
  return Object.values(status.nodeStates || {}).some(isPeerSyncing);
}

// syncPollInterval returns 2000ms while a resource is resyncing and false
// (stop polling) once it settles, matching the design's smart-polling rule.
export function syncPollInterval(query: {
  state: { data?: unknown };
}): number | false {
  const status = (query.state.data as { status?: ResourceStatus } | undefined)
    ?.status;
  return statusHasActiveSync(status) ? 2000 : false;
}

/** What the Replication column says, and what the row's tick is coloured from. */
export type Replication = {
  tone: StatusTone;
  /** The word beside the bar. Tone never carries the meaning on its own. */
  label: string;
  percent: number;
  title?: string;
};

// replicationSummary condenses a resource's live status into the one line the
// table has room for.
//
// Every role, disk and replication state here comes from `resourceStatus`:
// ListResources deliberately does not run `drbdadm status` on every node, so a
// resource straight out of the list has no node states at all.
export function replicationSummary(status?: ResourceStatus): Replication {
  if (!status) return { tone: 'idle', label: 'unknown', percent: 0 };
  const states = Object.entries(status.nodeStates ?? {});

  const syncing = states.find(([, st]) => isPeerSyncing(st));
  if (syncing) {
    const [host, st] = syncing;
    const pct = Math.min(100, Math.max(0, st.syncPercent ?? 0));
    return {
      tone: 'warn',
      label: `syncing ${pct.toFixed(0)}%`,
      percent: pct,
      title: `${st.replicationState} with ${st.node || host}`,
    };
  }

  if (status.quorum && !status.quorum.hasQuorum)
    return { tone: 'bad', label: 'no quorum', percent: 100 };

  // A tiebreaker or data client is *meant* to be Diskless; only a replica that
  // is not UpToDate is a degradation, so Diskless is not read as one here.
  const degraded = states.find(
    ([, st]) => st.diskState && st.diskState !== 'UpToDate' && st.diskState !== 'Diskless',
  );
  if (degraded)
    return {
      tone: 'bad',
      label: degraded[1].diskState,
      percent: 100,
      title: `on ${degraded[1].node || degraded[0]}`,
    };

  if (states.length === 0) return { tone: 'idle', label: 'unknown', percent: 0 };
  return { tone: 'ok', label: 'UpToDate', percent: 100 };
}

/** What a resource reads as before its status query has answered. */
export const UNKNOWN_REPLICATION: Replication = { tone: 'idle', label: 'unknown', percent: 0 };

// sizeGb crosses the wire as a proto int64, i.e. a JSON *string*; summing it
// without Number() concatenates instead of adding.
export function totalGbOf(resource: Resource): number {
  return resource.volumes.reduce((n, v) => n + Number(v.sizeGb), 0);
}
