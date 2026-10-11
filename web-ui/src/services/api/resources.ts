import { type ApiResponse, type RequestFn } from './client';

export interface Resource {
  name: string;
  port: number;
  protocol: string;
  nodes: string[];
  role: string;
  volumes: Volume[];
  nodeStates: Record<string, NodeState>;
  // Diskless nodes joined purely as quorum tiebreakers (never promoted/mounted).
  disklessNodes?: string[];
  // Diskless data clients: nodes with no local replica that mount the volume
  // over the DRBD network (e.g. a CSI Pod scheduled onto a non-replica node).
  disklessClients?: string[];
  quorumRisk?: boolean;
  labels?: Record<string, string>;
  profile?: string;
  // Off-site asynchronous replication. drNode names which entry of `nodes`
  // lives at the DR site — it replicates under protocol A and never takes over
  // automatically, so showing it as just another replica would mislead.
  wanMode?: boolean;
  drNode?: string;
}

export interface Volume {
  volumeId: number;
  device: string;
  sizeGb: number;
  // Backing location, used to build the "<pool>/<lv>" path snapshot APIs
  // operate on.
  pool?: string;
  backingVolume?: string;
  // The device's exact size when it was given one (a Proxmox disk); 0 or
  // absent means sizeGb whole GiB. A 64-bit integer, so it arrives as a string.
  sizeBytes?: string;
}

export interface NodeState {
  role: string;
  diskState: string;
  replication: string;
  // Resync completion (0..100). 100 / absent when not resyncing.
  syncPercent?: number;
}

export interface ResourceStatus {
  name: string;
  role: string;
  nodes: string[];
  nodeStates: Record<string, NodeResourceState>;
  volumes: Volume[];
  // Quorum arithmetic: how many members the resource has, how many votes it
  // needs, and how much margin is left. This is what decides whether the
  // resource keeps serving as nodes are lost.
  quorum?: QuorumInfo;
  // WAN replication. All absent/false for an ordinary LAN resource.
  wan?: boolean;
  drNode?: string;
  drEndpoint?: string;
  wanPort?: number;
  // Per-leg `haify-proxy@<instance>` unit state, keyed by a label that names both
  // ends of the leg. A multi-replica resource has one leg per replica.
  wanProxy?: Record<string, string>;
  wanReachable?: boolean;
  wanMetrics?: WANMetrics;
}

export interface QuorumInfo {
  members: number;
  required: number;
  online: number;
  hasQuorum: boolean;
  // How many further members can be lost before I/O suspends.
  tolerated: number;
}

export interface WANMetrics {
  // The un-replicated backlog: writes already acknowledged locally that have
  // not reached the DR. Under protocol A this IS the data-loss window of a DR
  // failover performed right now.
  bufferUsedBytes?: string;
  bufferCapBytes?: string;
  bufferFillPercent?: number;
  compressionRatio?: number;
  wanConnected?: boolean;
  reconnects?: string;
}

export interface NodeResourceState {
  role: string;
  diskState: string;
  replicationState: string;
  // The Haify node name this state belongs to. The map is keyed by DRBD host
  // name (the machine's real hostname), which is generally not the node name
  // the rest of the API uses.
  node?: string;
  // Resync completion (0..100). 100 / absent when not resyncing. Only
  // meaningful for a peer whose replicationState is a resync state.
  syncPercent?: number;
  // DRBD's connection state for a peer ("Connected", "Connecting", ...); empty
  // on the reporting node's own entry.
  connection?: string;
  tls?: boolean;
  // Cumulative KiB counters, uint64 and therefore JSON strings. On the
  // reporting node's own entry (the one with an empty replicationState):
  // written to and read from its local device since it came up. On each peer
  // entry: sent to and received from that peer since the connection came up.
  // Absent from controllers that predate them.
  writtenKib?: string;
  readKib?: string;
  sentKib?: string;
  receivedKib?: string;
}

export interface ResourcesResponse extends ApiResponse {
  resources: Resource[];
}

export const resourcesApi = (request: RequestFn) => ({
  // ==================== Resources ====================
  getResources: () => request<ResourcesResponse>('/resources'),

  getResource: (name: string) =>
    request<ApiResponse & { resource: Resource }>(`/resources/${name}`),

  createResource: (data: {
    name: string;
    port: number;
    // Omit (or pass []) to let the controller place the replicas itself, using
    // replicas + the fault-domain constraints — which is how a profile's
    // placement half is meant to be used.
    nodes?: string[];
    replicas?: number;
    replicasOnDifferent?: string[];
    replicasOnSame?: string[];
    protocol?: string;
    sizeGb?: number;
    pool?: string;
    storageType?: string;
    // Multiple DRBD volumes (volume 0..N). When omitted, sizeGb/pool create a
    // single volume. Each volume shares the resource's storageType.
    volumes?: { sizeGb: number; pool?: string }[];
    // DRBD options as "section/key" -> value (e.g. "net/max-buffers" -> "8000").
    drbdOptions?: Record<string, string>;
    labels?: Record<string, string>;
    profile?: string;
  }) =>
    request<ApiResponse>('/resources', {
      method: 'POST',
      body: JSON.stringify(data),
    }),

  // addDR attaches an off-site asynchronous replica to a resource that is
  // already running. wanPort 0 lets the controller allocate one.
  addDR: (
    resource: string,
    data: {
      drNode: string;
      drEndpoint: string;
      wanPort?: number;
      egressAddress?: string;
    },
  ) =>
    request<ApiResponse & { wanPort: number }>(`/resources/${resource}/dr`, {
      method: 'POST',
      body: JSON.stringify({ resource, ...data }),
    }),

  deleteResource: (name: string) =>
    request<ApiResponse>(`/resources/${name}`, { method: 'DELETE' }),

  resourceStatus: (name: string) =>
    request<ApiResponse & { status: ResourceStatus }>(`/resources/${name}/status`),

  updateResourceOptions: (name: string, options: Record<string, string>) =>
    request<ApiResponse>(
      `/resources/${encodeURIComponent(name)}/options`,
      { method: 'POST', body: JSON.stringify({ options }) }
    ),

  setPrimary: (resource: string, node: string, force = false) =>
    request<ApiResponse>(`/resources/${resource}/primary`, {
      method: 'POST',
      body: JSON.stringify({ node, force }),
    }),

  setSecondary: (resource: string, node: string) =>
    request<ApiResponse>(`/resources/${resource}/secondary`, {
      method: 'POST',
      body: JSON.stringify({ node }),
    }),

  // Volume operations
  addVolume: (resource: string, data: { volume: string; pool: string; sizeGb: number }) =>
    request<ApiResponse>(`/resources/${resource}/volumes`, {
      method: 'POST',
      body: JSON.stringify(data),
    }),

  removeVolume: (resource: string, volumeId: number) =>
    request<ApiResponse>(`/resources/${resource}/volumes/${volumeId}`, { method: 'DELETE' }),

  resizeVolume: (resource: string, volumeId: number, sizeGb: number) =>
    request<ApiResponse>(`/resources/${resource}/volumes/${volumeId}`, {
      method: 'PATCH',
      body: JSON.stringify({ sizeGb }),
    }),

  // Filesystem operations
  createFilesystem: (resource: string, volumeId: number, fstype: string, node?: string) =>
    request<ApiResponse>(`/resources/${resource}/volumes/${volumeId}/filesystem`, {
      method: 'POST',
      body: JSON.stringify({ fstype, node }),
    }),

  mountResource: (resource: string, volumeId: number, path: string, fstype?: string, node?: string) =>
    request<ApiResponse>(`/resources/${resource}/volumes/${volumeId}/mount`, {
      method: 'POST',
      body: JSON.stringify({ path, fstype, node }),
    }),

  unmountResource: (resource: string, volumeId: number, node?: string) =>
    request<ApiResponse>(`/resources/${resource}/volumes/${volumeId}/unmount`, {
      method: 'POST',
      body: JSON.stringify({ node }),
    }),

  /** Merges labels into the resource's; a key with an empty value is removed. */
  setResourceLabels: (resource: string, labels: Record<string, string>) =>
    request<ApiResponse & { labels?: Record<string, string> }>(
      `/resources/${encodeURIComponent(resource)}/labels`,
      { method: 'POST', body: JSON.stringify({ resource, labels }) },
    ),
});
