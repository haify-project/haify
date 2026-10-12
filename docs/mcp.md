# Haify MCP server (for AI agents)

`haify-mcp` (in `cmd/mcp`) is a
[Model Context Protocol](https://modelcontextprotocol.io) server for the Haify
controller. An AI agent connects to it and uses its tools to inspect and
operate the cluster. It calls the controller's gRPC API, the same API `haify`
uses. It runs in three ways:

| Command | Transport | Who may call what |
| ------- | --------- | ----------------- |
| `haify-mcp` | stdio, for a client on the same machine | every tool, or only reads with `--read-only` |
| `haify-mcp serve` | HTTP at `/mcp`, for remote clients | decided by the role of each client's token |
| `haify-mcp k8s` | stdio | the Kubernetes tools (`haify_k8s_*`), which talk to a Kubernetes API server, not to the controller |

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

To set it up for one project instead, put it in a `.mcp.json` file. Claude Code
loads that file from the directory it starts in:

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

Replace the address with your cluster's. Claude Code expands environment
variables in `.mcp.json`, so `"${HAIFY_CONTROLLER_ADDR:-127.0.0.1:3374}"` also
works there. The client reads the file only when it starts, so restart it after
you change the file.

Which address to use:

- With Self-HA enabled, use the VIP. On failover the controller moves to
  another node, and the VIP moves with it. `haify ha self status` prints it.
- Without Self-HA, use the controller node's own address.
- On the controller node itself, leave the flag out: the default
  `127.0.0.1:3374` reaches it.

Flags:

- `--controller, -c`: controller `host:port` (default `127.0.0.1:3374`).
- `--token`: the API token, needed when the controller has `[auth]` or
  `[rbac]` enabled. Without the flag, haify-mcp looks in `HAIFY_TOKEN`, then
  `~/.haify/token`, then `/etc/haify/token`.
- `--tls`, `--tls-ca`, `--tls-cert`, `--tls-key`, `--tls-server-name`,
  `--tls-insecure`: use these to reach a controller that has `[tls]` enabled.
  They mean the same as in the `haify` CLI and read the same `HAIFY_TLS*`
  environment variables ([user guide §17](user-guide.md#17-access-control)).
- `--read-only`: register only the read-only tools.
- `--allow NAME[,NAME]`: register these mutating tools as well, for example
  `--allow haify_ha_evict`. It turns on `--read-only`, so every other tool that
  changes the cluster stays off. The server refuses to start if a name is not
  a mutating tool, so a typo cannot silently leave a tool out.
- `--debug`: debug logging on stderr. stdout is kept for the protocol.

### Kubernetes tools

`haify-mcp k8s` serves three tools for databases on Kubernetes whose data
lives on a Haify volume: `haify_k8s_app_list`, `haify_k8s_app_create` and
`haify_k8s_app_delete`.

Delete removes the Deployment and the Service. It keeps the volume claim and
the password Secret, so creating the app again under the same name picks up
the same data. Set `delete_data` to delete those too. Delete only touches
objects that `haify_k8s_app_create` made.

It finds the cluster through `--kubeconfig` (or `HAIFY_KUBECONFIG`), or uses
the in-cluster config when it runs inside a pod. It also takes `--read-only`,
`--allow` and `--debug`, which work as in the main server.

## Remote server (`haify-mcp serve`)

`haify-mcp serve` serves the same tools over streamable HTTP at `/mcp`. Every
call there needs a token.

```bash
haify-mcp token create --name laptop --role read --url https://mcp.example.com/mcp
haify-mcp serve --listen 0.0.0.0:43871 --public-url https://mcp.example.com
```

`token create` prints the secret once, together with the `claude mcp add`
command that uses it. `--expires 720h` sets how long the token lasts. The
default, `0`, means it never expires.

### Roles

Each token has a role. The role decides which tools exist on that connection
at all. A tool outside the role is not registered, so it cannot be called,
whatever the client sends.

| Role | Tools |
| ---- | ----- |
| `read` | the read-only tools: lists, status, health, diagnose, inspection reports and runs (`haify_inspect_run` changes nothing on the cluster), events, logs, audit, gateway exports/LUNs/ACLs/CHAP settings, runbooks |
| `operate` | plus every mutating tool that is not marked destructive (the middle column below) |
| `admin` | plus the destructive ones: delete, remove, unregister, restore, stop, evict, drain, renumber, role change, unmount, filesystem creation, thin-pool conversion, moving a volume to another pool, promoter TOML sync, schedule delete, Self-HA enable/disable, DR failback, database app failover, and adding or removing gateway exports, SMB shares, LUNs, NVMe namespaces and allow-list entries |

A tool is `admin` if it can lose data, take a volume away from the node that
serves it (role change, unmount, gateway stop, evict, drain, app failover), or
stop future snapshots or backups.

`operate` tools can still interrupt replication for a moment.
`haify_resource_set_options`, `haify_resource_tls`, `haify_wan_repair` and
`haify_wan_set_endpoint` may take replication links down and bring them back,
one link at a time, while the Primary keeps serving. This role can also detach
a diskless client (`haify_resource_detach_diskless`), which ends that node's
access to the volume.

`--max-role read|operate|admin` caps every token on the main listener,
whatever role the token has. The default is admin, which caps nothing.

`--admin-listen ADDR` opens a second listener that `--max-role` does not cap.
It is for operators on the local network: an admin token there gets every
tool, while the listener that a reverse proxy points at stays capped. The
second listener accepts only tokens made with `token create`, sent as a
bearer header. It has no OAuth endpoints, and it refuses the access tokens
that ChatGPT or claude.ai got through OAuth on the main listener. Never put it
behind the proxy. Bind it to a port the proxy does not forward, for example
`--listen 0.0.0.0:43871 --max-role operate --admin-listen 0.0.0.0:43872`.

### Tokens

Tokens are stored as SHA-256 hashes in `/var/lib/haify/mcp/tokens.json`.
`--tokens` or `HAIFY_MCP_TOKENS` changes the path. With Self-HA,
`/var/lib/haify` is the haify-meta mount, so the token store moves with the
controller. Run `haify-mcp token` commands on the node where haify-meta is
mounted.

`haify-mcp token list` and `haify-mcp token revoke <name|id>` work while the
server runs. The server re-reads the file when it changes, so a revoked token
is refused on its next request, even inside a session that is already open.
Revoking a token also revokes every OAuth token issued under it.

If the store is empty, the server still starts. It logs a warning and refuses
every request until you create a token.

### Clients

Claude Code sends the token as a header:

```bash
claude mcp add --transport http haify https://mcp.example.com/mcp \
    --header "Authorization: Bearer haifymcp_..."
```

ChatGPT and claude.ai add a server by its URL and then run OAuth. The server
offers OAuth only when it is started with `--public-url`. The client finds the
endpoints and registers itself. On the authorization page you paste a token
made with `token create`. The client then gets an access token that is valid
for one hour and a refresh token that is valid for 30 days. They carry the
pasted token's role, capped by `--max-role`, or a lower role if the client
asked for a smaller scope. Only S256 PKCE is accepted. Redirect URIs must be
`https`, or `http` to a loopback address.

### Deployment

Serve it over HTTPS. Either give it `--tls-cert`/`--tls-key`, or let a
reverse proxy terminate TLS. With a proxy, bind the server to a private
address and pass `--trust-proxy`. The server then reads the real client
address from `X-Forwarded-For`. The failed-login limit needs that address: it
locks an address out after 10 failed attempts within 5 minutes.
`GET /healthz` answers `ok` without a token.

`serve` has its own flags for reaching the controller. `--controller`
(default `127.0.0.1:3374`) is the address. `--controller-token` is the token
and falls back to the same places as `--token` above. For a controller with
`[tls]` enabled, use `--controller-tls`, `--controller-tls-ca`,
`--controller-tls-cert`, `--controller-tls-key`,
`--controller-tls-server-name` and `--controller-tls-insecure`. These also
fall back to the `HAIFY_TLS*` variables. Do not mix them up with
`--tls-cert`/`--tls-key`, which are the HTTPS certificate of this server
itself.

Every tool call is logged with the token's name and role. The arguments are
not logged, because some of them carry secrets:

```bash
journalctl -u haify-mcp-http | grep 'tool call'
```

To make the server move with the controller on failover:

1. Copy `haify-mcp` to `/opt/haify/bin/` and install
   `configs/haify-mcp-http.service` on every node. Leave the unit disabled. It
   runs `/opt/haify/bin/haify-mcp serve --listen 0.0.0.0:43871` with the token
   store on the Self-HA mount, and reads your site's extra flags from
   `HAIFY_MCP_ARGS` in `/var/lib/haify/mcp/haify-mcp.env`.
2. Add `haify-mcp-http.service` to the haify-meta promoter's start list. If
   Self-HA is not enabled yet, list it in `[self_ha] extra_services` in
   `controller.toml` before you run `ha self enable`. On a cluster that
   already runs Self-HA, add it to
   `/etc/drbd-reactor.d/haify-ha-haify-meta.toml` on every node. List it in
   that same setting as well, so the controller's stored copy of the
   promoter matches and a later sync does not drop it.

## Runbooks

A runbook says in which order to call the tools for a task, and what breaks
halfway if you use another order. The runbooks are built into the binary from
`pkg/mcpserver/runbooks/*.md` and are served in two ways:

- as MCP prompts, which Claude Code offers as slash commands. Each takes an
  optional `target` argument (the resource or node).
- through the read-only tool `haify_runbook`. Without a name it lists the
  runbooks; with a name it returns that one. ChatGPT, claude.ai and the AI
  Copilot reach the runbooks this way.

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
| Cluster, nodes | `haify_node_list`, `haify_node_health_check`, `haify_diagnose`, `haify_inspect_report`, `haify_inspect_run`, `haify_event_list`, `haify_log_list`, `haify_audit_list`, `haify_ocf_agent_list`, `haify_ocf_agent_metadata`, `haify_notify_channel_list`, `haify_replication_tls_status`, `haify_runbook` | `haify_node_register`, `haify_node_set_labels`, `haify_node_set_replication_paths`, `haify_node_undrain`, `haify_notify_channel_test` | `haify_node_drain`, `haify_node_unregister`, `haify_node_set_address` |
| LVM pools | `haify_pool_list`, `haify_node_disks`, `haify_pool_disks`, `haify_storage_jobs` | `haify_pool_create`, `haify_pool_add_disk`, `haify_pool_add_cache`, `haify_pool_remove_disk`, `haify_pool_replace_disk`, `haify_pool_trim` | `haify_pool_delete`, `haify_pool_remove_cache`, `haify_pool_convert_thin` |
| ZFS | `haify_zfs_pool_list` | `haify_zfs_volume_create`, `haify_zfs_volume_resize`, `haify_zfs_dataset_create`, `haify_zfs_snapshot_clone` | `haify_zfs_pool_delete`, `haify_zfs_dataset_delete` |
| Resources, volumes | `haify_resource_list`, `haify_resource_status` | `haify_resource_create`, `haify_resource_adopt`, `haify_resource_add_volume`, `haify_resource_resize_volume`, `haify_resource_set_options`, `haify_resource_mount`, `haify_resource_dual_primary`, `haify_resource_repair`, `haify_resource_verify`, `haify_resource_tls`, `haify_resource_set_labels` | `haify_resource_delete`, `haify_resource_remove_volume`, `haify_resource_move_volume`, `haify_resource_create_filesystem`, `haify_resource_set_role`, `haify_resource_unmount` |
| Replicas, WAN | | `haify_resource_add_replica`, `haify_resource_attach_diskless`, `haify_resource_detach_diskless`, `haify_resource_set_tiebreaker`, `haify_resource_add_dr`, `haify_wan_repair`, `haify_wan_set_endpoint` | `haify_resource_remove_replica`, `haify_resource_dr_failback` |
| Profiles | `haify_resource_profile_list`, `haify_resource_profile_get`, `haify_resource_profile_max_size` | `haify_resource_profile_create`, `haify_resource_profile_set_options`, `haify_resource_profile_adjust`, `haify_resource_set_profile` | `haify_resource_profile_delete` |
| Snapshots | `haify_snapshot_list`, `haify_snapshot_schedule_list` | `haify_snapshot_create`, `haify_snapshot_schedule_create` | `haify_snapshot_delete`, `haify_snapshot_restore`, `haify_snapshot_schedule_delete` |
| Backups | `haify_backup_list`, `haify_backup_target_list`, `haify_backup_schedule_list` | `haify_backup_create`, `haify_backup_import`, `haify_backup_schedule_create`, `haify_resource_ship` | `haify_backup_delete`, `haify_backup_restore`, `haify_backup_target_delete`, `haify_backup_schedule_delete` |
| Gateways | `haify_gateway_list`, `haify_gateway_get`, `haify_nfs_export_list`, `haify_iscsi_lun_list`, `haify_iscsi_initiator_list`, `haify_iscsi_chap_get`, `haify_nvme_namespace_list`, `haify_nvme_host_list`, `haify_gateway_smb_users` | `haify_gateway_create_nfs`, `haify_gateway_create_iscsi`, `haify_gateway_create_nvme`, `haify_gateway_create_smb`, `haify_gateway_start`, `haify_iscsi_chap` | `haify_gateway_stop`, `haify_gateway_delete`, `haify_nfs_exports`, `haify_iscsi_luns`, `haify_iscsi_initiators`, `haify_nvme_namespaces`, `haify_nvme_hosts`, `haify_gateway_smb_shares` |
| Database apps | `haify_app_list`, `haify_app_status` | `haify_app_create`, `haify_app_snapshot` | `haify_app_failover`, `haify_app_delete` |
| HA, Self-HA | `haify_ha_list`, `haify_ha_status`, `haify_ha_promoter_status`, `haify_ha_get_toml`, `haify_self_ha_status` | `haify_ha_create` | `haify_ha_evict`, `haify_ha_delete`, `haify_ha_sync_toml`, `haify_self_ha_enable`, `haify_self_ha_disable` |

`haify_nfs_exports`, `haify_iscsi_luns`, `haify_iscsi_initiators`,
`haify_nvme_namespaces` and `haify_nvme_hosts` add and remove entries, so they
need `admin`. They still accept the `list` action, but the `*_list` tools give
the same answer at `read`.

`haify_iscsi_chap` sets one-way CHAP and needs `operate`. Mutual CHAP is
refused. It still accepts `get`, which `haify_iscsi_chap_get` also answers at
`read`. Neither tool returns the password.

### What has no tool

- Reading the password that `haify app create` generates. `haify_app_create`
  tells you where the password is kept (root-only, on the app's volume)
  instead of returning it, because whatever called the tool records its
  result.
- Commands that take a secret, because whatever calls a tool records its
  arguments: `backup target add` (storage credentials), `channel add` (webhook
  URLs and signing keys), `rbac`. The one exception is `haify_iscsi_chap`,
  which sets a CHAP password. The server does not log tool arguments and
  never returns the password.
- `resource dr-failover`. It force-promotes the DR node and loses the writes
  still in the WAN buffer. That decision is taken at the CLI, which asks for
  `--yes`.
- `replication-tls setup`. It installs a CA into each node's system trust
  store.
- `inspect list` (`haify_inspect_report` reads the newest report, or one by id),
  `backup schedule run`, `channel delete`, `event watch` (a stream;
  `haify_event_list` reads the history) and `gateway nfs mount` (it mounts on
  the machine running the CLI).
