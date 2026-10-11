import { type ApiResponse, type RequestFn } from './client';

export interface HaConfig {
  resource: string;
  vip: string;
  mountPoint: string;
  fsType: string;
  services: string[];
}

// A configured OCF resource agent appended to an HA config's promoter
// start[] array. Composed by the OCF agent builder and submitted to MakeHa.
export interface OcfAgentSpec {
  provider: string;
  name: string;
  instance: string;
  params: Record<string, string>;
}

// One entry in the ordered promoter start[] list. Exactly one field is set:
// systemd/mount units and OCF agents are peers in a single ordered sequence.
export interface HaStartItem {
  systemdUnit?: string;
  ocf?: OcfAgentSpec;
}

// Summary entry from GET /ha/resource-agents.
export interface ResourceAgentSummary {
  provider: string;
  name: string;
  shortdesc: string;
}

export interface ResourceAgentsResponse {
  agents: ResourceAgentSummary[];
}

// A single OCF meta-data parameter (parsed from the agent's meta-data XML).
export interface OcfAgentParameter {
  name: string;
  required: boolean;
  unique: boolean;
  type: string;
  default: string;
  shortdesc: string;
  longdesc: string;
}

// Full OCF agent metadata from GET /ha/resource-agents/{provider}/{name}.
export interface ResourceAgentMetadata {
  provider: string;
  name: string;
  version: string;
  shortdesc: string;
  longdesc: string;
  parameters: OcfAgentParameter[];
}

// What drbd-reactor is actually running on the active node, from
// GET /ha/{resource}/status.
//
// This is the promoter as deployed, not as configured. The two can differ — a
// unit added to the TOML by hand is in `deps` and in no config the controller
// holds — and a console that shows only the configuration tells an operator
// that fewer things move on failover than actually do.
/** What a unit is in the chain. */
export type HaUnitKind =
  | 'promote'
  | 'mount'
  | 'service_ip'
  | 'nfs'
  | 'iscsi'
  | 'nvmeof'
  | 'smb'
  | 'database'
  | 'vm'
  | 'haify'
  | 'service'
  | 'target';

export interface HaUnit {
  name: string;
  status: string;
  // The fields below are absent from controllers that predate them, and
  // proto3 leaves out zero values (an empty since, zero restarts).
  kind?: HaUnitKind | string;
  /** The unit's job for a person: mount path, service IP, export, or its description. */
  label?: string;
  /** The unit file's own Description=. */
  description?: string;
  /** RFC 3339; when the unit last became active. Empty while it is not. */
  since?: string;
  /** systemd's NRestarts. */
  restarts?: number;
  subState?: string;
  mainPid?: number;
}

/** A node holding an enabled copy of the promoter, not running the chain. */
export interface HaStandby {
  node: string;
  ready?: boolean;
  /** What would stop a takeover there. */
  reasons?: string[];
  diskState?: string;
}

/** A unit outside the chain with processes using the resource's mount. */
export interface HaOutsideService {
  unit: string;
  description?: string;
  processes?: number;
  mountPoints?: string[];
}

export interface HaPromoterStatus {
  drbdResource: string;
  primaryOn: string;
  status: string;
  target?: HaUnit;
  // The promoter's units, live. The first is drbd-promote@<res>, which is the
  // promotion itself rather than an entry of start[].
  deps?: HaUnit[];
  /** The Haify node running the chain; empty when none is. */
  activeNode?: string;
  standbys?: HaStandby[];
  outside?: HaOutsideService[];
  /** Login sessions using the mount on the active node, and their processes. */
  outsideSessions?: number;
  outsideSessionProcesses?: number;
}

export interface HaStatusResponse extends ApiResponse {
  promoters: HaPromoterStatus[];
}

// Promoter TOML for an HA config from GET /ha/{resource}/toml.
export interface HaTomlResponse {
  resource: string;
  path: string;
  content: string;
}

export interface HaConfigsResponse extends ApiResponse {
  configs: HaConfig[];
}

export const haApi = (request: RequestFn) => ({
  // ==================== HA ====================
  getHaConfigs: () => request<HaConfigsResponse>('/ha'),

  getHaConfig: (resource: string) =>
    request<ApiResponse & { config: HaConfig }>(`/resources/${resource}/ha`),

  makeHa: (resource: string, data: {
    vip?: string;
    mountPoint?: string;
    fstype?: string;
    services?: string[];
    // Extra OCF resource agents appended to the promoter start[] after the
    // built-in mount/vip items (order preserved). Composed by the OCF builder.
    ocfAgents?: OcfAgentSpec[];
    // Ordered start[] list where systemd/mount units and OCF agents are peers.
    // When set, it defines the promoter start[] verbatim; vip/mountPoint/fstype
    // above are still sent for provisioning side effects only.
    startItems?: HaStartItem[];
  }) =>
    request<ApiResponse & { configPath: string }>(`/resources/${resource}/ha`, {
      method: 'POST',
      body: JSON.stringify(data),
    }),

  deleteHa: (resource: string) =>
    request<ApiResponse>(`/resources/${resource}/ha`, { method: 'DELETE' }),

  evictHa: (resource: string) =>
    request<ApiResponse>(`/resources/${resource}/ha/evict`, {
      method: 'POST',
      body: JSON.stringify({}),
    }),

  // ==================== HA OCF agents + promoter TOML ====================
  // List OCF resource agents available on the nodes.
  getResourceAgents: () =>
    request<ResourceAgentsResponse>('/ha/resource-agents'),

  // Fetch a single OCF agent's metadata (parameter schema).
  getResourceAgentMetadata: (provider: string, name: string) =>
    request<ResourceAgentMetadata>(
      `/ha/resource-agents/${encodeURIComponent(provider)}/${encodeURIComponent(name)}`,
    ),

  // Read an HA config's drbd-reactor promoter TOML.
  getHaStatus: (resource: string) =>
    request<HaStatusResponse>(`/ha/${encodeURIComponent(resource)}/status`),

  getHaToml: (resource: string) =>
    request<HaTomlResponse>(`/ha/${encodeURIComponent(resource)}/toml`),

  // Write (sync) an HA config's promoter TOML to all nodes and reload
  // drbd-reactor.
  syncHaToml: (resource: string, content: string) =>
    request<ApiResponse>(`/ha/${encodeURIComponent(resource)}/toml`, {
      method: 'POST',
      body: JSON.stringify({ content }),
    }),
});
