# Haify MCP server (for AI agents)

`haify-mcp` (in `cmd/mcp`) exposes the Haify controller as a
[Model Context Protocol](https://modelcontextprotocol.io) server, so an AI
agent can inspect and operate the cluster through tools. It talks to the
controller's gRPC API, the same one `haify` uses. It runs in three ways:

| Command | Transport | Who may call what |
| ------- | --------- | ----------------- |
| `haify-mcp` | stdio, for a client on the same machine | every tool, or only reads with `--read-only` |
| `haify-mcp serve` | HTTP at `/mcp`, for remote clients | decided by the role of each client's token |
| `haify-mcp k8s` | stdio | the Kubernetes tools (`haify_k8s_*`), against a Kubernetes API server instead of the controller |

## Install

```bash
make install-mcp        # runs `make build` (needs Node.js for the web UI), copies bin/haify-mcp to /usr/local/bin
# or just the binary:
go build -o ~/.local/bin/haify-mcp ./cmd/mcp
```

## Local server (stdio)

```bash
claude mcp add haify -- haify-mcp --controller 10.0.0.250:3374
```

The same server as a project-scoped `.mcp.json`, which Claude Code loads from
the directory it starts in:

```json
{
  "mcpServers": {
    "haify": {
      "type": "stdio",
      "command": "haify-mcp",
      "args": ["--controller", "10.0.0.250:3374"],
      "env": {}
    }
  }
}
```

Use your cluster's address. Claude Code expands environment variables in
`.mcp.json`, so `"${HAIFY_CONTROLLER_ADDR:-127.0.0.1:3374}"` also works there.
The client reads the file once at startup: restart it after a change.

Which address to use:

- With Self-HA enabled, the VIP. The controller moves between nodes on
  failover and the VIP follows it. `haify ha self status` prints it.
- Without Self-HA, the controller node's own address.
- On the controller node itself, nothing: the default `127.0.0.1:3374`
  reaches it.

Flags:

- `--controller, -c`: controller `host:port` (default `127.0.0.1:3374`).
- `--token`: API token when the controller has `[auth]` enabled. Without the
  flag: `HAIFY_TOKEN`, then `~/.haify/token`, then `/etc/haify/token`.
- `--tls`, `--tls-ca`, `--tls-cert`, `--tls-key`, `--tls-server-name`,
  `--tls-insecure`: connect to a controller with `[tls]` enabled. They have the
  same meaning and `HAIFY_TLS*` environment variables as in the `haify` CLI
  ([user guide §17](user-guide.md#17-access-control)).
- `--read-only`: register only the read-only tools.
- `--allow NAME[,NAME]`: register these mutating tools as well, e.g.
  `--allow haify_ha_evict`. Implies `--read-only`. The server refuses to start
  on a name that is not a tool.
- `--debug`: debug logging on stderr. stdout carries the protocol.

### Kubernetes tools

`haify-mcp k8s` serves `haify_k8s_app_list`, `haify_k8s_app_create` and
`haify_k8s_app_delete` (a database on Kubernetes whose data lives on a Haify
volume). Delete removes the Deployment and Service and keeps the volume claim
and password secret unless `delete_data` is set; it touches only objects
`haify_k8s_app_create` made. It uses `--kubeconfig`
(or `HAIFY_KUBECONFIG`), or the in-cluster config inside a pod, and takes
`--read-only`, `--allow` and `--debug` like the main server.

## Remote server (`haify-mcp serve`)

`haify-mcp serve` serves the same tools over streamable HTTP at `/mcp`. Every
request needs a token.

```bash
haify-mcp token create --name laptop --role read --url https://mcp.example.com/mcp
haify-mcp serve --listen 0.0.0.0:43871 --public-url https://mcp.example.com
```

`token create` prints the secret once, with the `claude mcp add` command for
it. `--expires 720h` limits its life; the default `0` never expires.

### Roles

A token has a role, and the role decides which tools exist on that
connection. A tool that is not registered cannot be called by name.

| Role | Tools |
| ---- | ----- |
| `read` | the read-only tools: lists, status, health, diagnose, inspection reports and runs (`haify_inspect_run` changes nothing on the cluster), events, logs, audit, gateway exports/LUNs/ACLs/CHAP settings, runbooks |
| `operate` | plus every mutating tool not marked destructive (the middle column below) |
| `admin` | plus the destructive ones: delete, remove, restore, stop, evict, drain, renumber, role change, unmount, filesystem creation, thin-pool conversion, moving a volume to another pool, promoter TOML sync, schedule delete, Self-HA enable/disable, DR failback, and adding or removing gateway exports, LUNs and ACL entries |

Anything that loses data, takes a volume away from the node serving it
(role change, unmount, gateway stop, evict, drain), or stops future snapshots
or backups is `admin`. `operate` can still briefly take replication links down
and back, one at a time, while the Primary keeps serving:
`haify_resource_set_options`, `haify_resource_tls`, `haify_wan_repair`,
`haify_wan_set_endpoint`. It can also detach a diskless client
(`haify_resource_detach_diskless`), which ends that node's access.

`--max-role read|operate|admin` caps every token on the main listener, whatever
the token says. `--admin-listen ADDR` opens a second listener that
`--max-role` does not cap, for operators on the local network: an admin token
there gets every tool while the listener a reverse proxy points at stays
capped. It takes bearer tokens only, no OAuth, and must never be proxied. Bind
it to a port the proxy does not forward, e.g.
`--listen 0.0.0.0:43871 --max-role operate --admin-listen 0.0.0.0:43872`.

### Tokens

Tokens are stored as SHA-256 hashes in `/var/lib/haify/mcp/tokens.json`
(`--tokens` or `HAIFY_MCP_TOKENS` to change it). With Self-HA, `/var/lib/haify` is
the haify-meta mount, so the store moves with the controller; run `haify-mcp token`
commands on the node where haify-meta is mounted. `haify-mcp token list` and
`haify-mcp token revoke <name|id>` work while the server runs: the server
re-reads the file when it changes, and a revoked token is refused on its next
request, including inside an open session. Revoking a token also revokes every
OAuth token issued under it. The server starts with an empty store, logs a
warning, and refuses every request until a token exists.

### Clients

Claude Code sends the token as a header:

```bash
claude mcp add --transport http haify https://mcp.example.com/mcp \
    --header "Authorization: Bearer haifymcp_..."
```

ChatGPT and claude.ai add a server by URL and run OAuth, which the server
offers only with `--public-url`. The client discovers the endpoints and
registers itself, and on the authorization page you paste a token made with
`token create`. The client gets an access token valid for one hour and a
refresh token valid for 30 days, with the pasted token's role, capped by
`--max-role`, or lower if it asked for a smaller scope. Only S256 PKCE is
accepted, and redirect URIs must be `https`, or `http` to a loopback address.

### Deployment

Serve it over HTTPS: either `--tls-cert`/`--tls-key`, or terminate TLS in a
reverse proxy, bind to a private address and pass `--trust-proxy` so the
failed-login limit (an address is locked out after 10 failed attempts within
5 minutes) sees the real client address from `X-Forwarded-For`.
`GET /healthz` answers `ok` without a token.

`serve` reaches the controller with `--controller` (default `127.0.0.1:3374`),
`--controller-token` (same fallbacks as `--token` above) and, for a controller
with `[tls]` enabled, `--controller-tls`, `--controller-tls-ca`,
`--controller-tls-cert`, `--controller-tls-key`,
`--controller-tls-server-name` and `--controller-tls-insecure`, which also
fall back to `HAIFY_TLS*`. `--tls-cert`/`--tls-key` are this server's own HTTPS
certificate.

Every tool call is logged with the token's name and role; arguments are not,
since some carry secrets:

```bash
journalctl -u haify-mcp-http | grep 'tool call'
```

To make the server follow the controller across failover, copy `haify-mcp` to
`/opt/haify/bin/` and install `configs/haify-mcp-http.service` on every node, left
disabled. It runs `/opt/haify/bin/haify-mcp serve --listen 0.0.0.0:43871` with the token store on the
Self-HA mount, and reads site flags from `HAIFY_MCP_ARGS` in
`/var/lib/haify/mcp/haify-mcp.env`. Then put `haify-mcp-http.service` in the haify-meta
promoter's start list: before `ha self enable`, through
`[self_ha] extra_services` in `controller.toml`; on a running cluster, in
`/etc/drbd-reactor.d/haify-ha-haify-meta.toml` on every node.

## Runbooks

A runbook gives the order in which to call the tools for a task, and what
fails halfway if the order is wrong. The runbooks are embedded from
`pkg/mcpserver/runbooks/*.md` and served two ways:

- as MCP prompts, which Claude Code offers as slash commands, with an optional
  `target` argument (the resource or node);
- through the read-only tool `haify_runbook`: no name lists them, a name returns
  one. ChatGPT, claude.ai and the AI Copilot reach them this way.

| Runbook | For | Needs |
| ------- | --- | ----- |
| `add-replica` | another full copy of a resource on another node | operate |
| `dr-failback` | a WAN resource back to its primary site after a DR failover | admin |
| `grow-volume` | a larger volume, and the filesystem on it | operate |
| `planned-switchover` | haify-meta or a gateway moved to another node on purpose | admin |
| `reboot-node` | a node taken out of service and brought back | admin |
| `renumber-nodes` | nodes whose IP addresses changed | admin |
| `verify-and-repair` | replicas compared block by block, and differences repaired | operate |

## Tools

| Area | read | operate adds | admin adds |
| ---- | ---- | ------------ | ---------- |
| Cluster, nodes | `haify_node_list`, `haify_node_health_check`, `haify_diagnose`, `haify_inspect_report`, `haify_inspect_run`, `haify_event_list`, `haify_log_list`, `haify_audit_list`, `haify_ocf_agent_list`, `haify_ocf_agent_metadata`, `haify_notify_channel_list`, `haify_replication_tls_status`, `haify_runbook` | `haify_node_register`, `haify_node_set_labels`, `haify_node_undrain`, `haify_notify_channel_test` | `haify_node_drain`, `haify_node_unregister`, `haify_node_set_address` |
| LVM pools | `haify_pool_list`, `haify_node_disks`, `haify_pool_disks`, `haify_storage_jobs` | `haify_pool_create`, `haify_pool_add_disk`, `haify_pool_add_cache`, `haify_pool_remove_disk`, `haify_pool_replace_disk`, `haify_pool_trim` | `haify_pool_delete`, `haify_pool_remove_cache`, `haify_pool_convert_thin` |
| ZFS | `haify_zfs_pool_list` | `haify_zfs_volume_create`, `haify_zfs_volume_resize`, `haify_zfs_dataset_create`, `haify_zfs_snapshot_clone` | `haify_zfs_pool_delete`, `haify_zfs_dataset_delete` |
| Resources, volumes | `haify_resource_list`, `haify_resource_status` | `haify_resource_create`, `haify_resource_adopt`, `haify_resource_add_volume`, `haify_resource_resize_volume`, `haify_resource_set_options`, `haify_resource_mount`, `haify_resource_dual_primary`, `haify_resource_repair`, `haify_resource_verify`, `haify_resource_tls`, `haify_resource_set_labels` | `haify_resource_delete`, `haify_resource_remove_volume`, `haify_resource_move_volume`, `haify_resource_create_filesystem`, `haify_resource_set_role`, `haify_resource_unmount` |
| Replicas, WAN | | `haify_resource_add_replica`, `haify_resource_attach_diskless`, `haify_resource_detach_diskless`, `haify_resource_set_tiebreaker`, `haify_resource_add_dr`, `haify_wan_repair`, `haify_wan_set_endpoint` | `haify_resource_remove_replica`, `haify_resource_dr_failback` |
| Profiles | `haify_resource_profile_list`, `haify_resource_profile_get`, `haify_resource_profile_max_size` | `haify_resource_profile_create`, `haify_resource_profile_set_options`, `haify_resource_profile_adjust`, `haify_resource_set_profile` | `haify_resource_profile_delete` |
| Snapshots | `haify_snapshot_list`, `haify_snapshot_schedule_list` | `haify_snapshot_create`, `haify_snapshot_schedule_create` | `haify_snapshot_delete`, `haify_snapshot_restore`, `haify_snapshot_schedule_delete` |
| Backups | `haify_backup_list`, `haify_backup_target_list`, `haify_backup_schedule_list` | `haify_backup_create`, `haify_backup_import`, `haify_backup_schedule_create` | `haify_backup_delete`, `haify_backup_restore`, `haify_backup_target_delete`, `haify_backup_schedule_delete` |
| Gateways | `haify_gateway_list`, `haify_gateway_get`, `haify_nfs_export_list`, `haify_iscsi_lun_list`, `haify_iscsi_initiator_list`, `haify_iscsi_chap_get`, `haify_nvme_namespace_list`, `haify_nvme_host_list`, `haify_gateway_smb_users` | `haify_gateway_create_nfs`, `haify_gateway_create_iscsi`, `haify_gateway_create_nvme`, `haify_gateway_create_smb`, `haify_gateway_start`, `haify_iscsi_chap` | `haify_gateway_stop`, `haify_gateway_delete`, `haify_nfs_exports`, `haify_iscsi_luns`, `haify_iscsi_initiators`, `haify_nvme_namespaces`, `haify_nvme_hosts`, `haify_gateway_smb_shares` |
| Database apps | `haify_app_list`, `haify_app_status` | `haify_app_create`, `haify_app_snapshot` | `haify_app_failover`, `haify_app_delete` |
| HA, Self-HA | `haify_ha_list`, `haify_ha_status`, `haify_ha_promoter_status`, `haify_ha_get_toml`, `haify_self_ha_status` | `haify_ha_create` | `haify_ha_evict`, `haify_ha_delete`, `haify_ha_sync_toml`, `haify_self_ha_enable`, `haify_self_ha_disable` |

`haify_nfs_exports`, `haify_iscsi_luns`, `haify_iscsi_initiators`,
`haify_nvme_namespaces` and `haify_nvme_hosts` take an action (add, remove) and
need `admin`; they still accept `list`, which the `*_list` tools answer at
`read`. `haify_iscsi_chap` sets one-way CHAP at `operate` (mutual CHAP is
refused) and still accepts `get`; `haify_iscsi_chap_get` answers it at `read`.
Neither returns the password.

### What has no tool

- The password `haify app create` generates: `haify_app_create` reports where it
  is kept (root-only, on the app's volume) instead of returning it, since a
  tool result is recorded by whatever called it.
- Commands that take a secret, since a tool argument is recorded by whatever
  called it: `backup target add` (storage credentials), `channel add` (webhook
  URLs and signing keys), `rbac`. The exception is `haify_iscsi_chap`, which sets
  a CHAP password; the server does not log arguments and never returns the
  password.
- `resource dr-failover`: it force-promotes the DR node and loses the writes
  still in the WAN buffer, a decision to take at the CLI with `--yes`.
- `replication-tls setup`: it installs a CA into each node's system trust
  store.
- `inspect list` (`haify_inspect_report` reads the newest report, or one by id),
  `backup schedule run`, `channel delete`, `event watch` (a stream;
  `haify_event_list` reads the history), `gateway nfs mount` (mounts on the
  machine running the CLI).
