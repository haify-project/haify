# Planned switchover of an HA resource
Move the controller (haify-meta) or a gateway to another node, on purpose.
Needs: admin

1. `haify_ha_promoter_status` shows where the resource is active now. `haify_resource_status` for it: every replica must be UpToDate and connected. A takeover by a replica that is behind, or that lacks quorum, fails or costs the service.
   Its `standbys` say which nodes could take over now and, for the others, why not (a unit not installed there, drbd-reactor not running, a disk not UpToDate); move only to a ready one. Its `outside` lists services, and counts login sessions, using the resource's mount from outside the chain: they hold the unmount up, so stop them first.
2. What rides the resource: `haify_ha_status` for haify-meta and other HA resources, `haify_gateway_get` for a gateway. For haify-meta that is the controller, its VIP and whatever `[self_ha] extra_services` added, such as the AI Copilot and the remote MCP server.
3. `haify_ha_evict` with the resource name. It moves that one resource and nothing else on the node.
   - haify-meta: the call returns as soon as the switchover is launched. The controller is unreachable for several seconds, and if this MCP server rides haify-meta the connection drops too. That is expected; reconnect and go on.
   - any other resource: the call returns once another node has taken over, and fails if none did.
4. `haify_ha_promoter_status` again: the node changed and the status is active. After haify-meta, `haify_node_list` answering means the controller is up on the new node.

NFS and iSCSI clients see a pause of some seconds, not I/O errors.

Do not stop or restart a drbd-reactor-managed service by hand: the promoter reads it as a fault and fails the resource over, unplanned. Do not evict two resources back to back; wait for the first to land.
