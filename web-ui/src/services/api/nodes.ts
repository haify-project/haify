import { type ApiResponse, type RequestFn } from './client';

export interface Node {
  name: string;
  address: string;
  hostname: string;
  state: string;
  lastSeen: string;
  version: string;
  labels?: Record<string, string>;
  /** Reached through its Haify CSI node plugin, not over SSH. */
  localAgent?: boolean;
  /** For a node in maintenance: who drained it, why, and when (Unix seconds). */
  maintenanceBy?: string;
  maintenanceReason?: string;
  maintenanceSince?: string;
  /** When the node stopped answering (Unix seconds); "0" or absent while it answers. */
  offlineSince?: string;
}

/**
 * Whether the controller can reach the node. A drained node keeps the state
 * "maintenance" whether it answers or not, so for it the answer is in
 * offlineSince.
 */
export function nodeReachable(node: Pick<Node, 'state' | 'offlineSince'>): boolean {
  if (node.state === 'online') return true;
  return node.state === 'maintenance' && (!node.offlineSince || node.offlineSince === '0');
}

export interface NodesResponse extends ApiResponse {
  nodes: Node[];
}

export interface HealthInfo {
  drbdInstalled: boolean;
  drbdVersion: string;
  drbdReactorInstalled: boolean;
  drbdReactorVersion: string;
  drbdReactorRunning: boolean;
  resourceAgentsInstalled: boolean;
  availableAgents: string[];
}

export const nodesApi = (request: RequestFn) => ({
  // ==================== Nodes ====================
  getNodes: () => request<NodesResponse>('/nodes'),

  getNode: (address: string) =>
    request<ApiResponse & { node: Node }>(`/nodes/${address}`),

  registerNode: (data: { name: string; address: string }) =>
    request<ApiResponse & { node: Node }>('/nodes', {
      method: 'POST',
      body: JSON.stringify(data),
    }),

  unregisterNode: (address: string) =>
    request<ApiResponse>(`/nodes/${address}`, { method: 'DELETE' }),

  healthCheck: (node: string) =>
    request<ApiResponse & { health: HealthInfo }>(`/nodes/${node}/health`),

  /** Merges labels into the node's; a key with an empty value is removed. */
  setNodeLabels: (node: string, labels: Record<string, string>) =>
    request<ApiResponse & { node: Node }>(`/nodes/${encodeURIComponent(node)}/labels`, {
      method: 'POST',
      body: JSON.stringify({ node, labels }),
    }),
});
