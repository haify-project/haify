# Installing and deploying Haify

This guide takes raw storage nodes to a working DRBD-backed cluster with
gateways, HA, Kubernetes CSI, the AI Copilot and optional WAN replication.

The per-node package list, with the symptom each missing piece produces, is
[`node-prerequisites.md`](./node-prerequisites.md). Day-to-day operation is
covered in [`user-guide.md`](./user-guide.md).

The commands are for Ubuntu 24.04 (amd64 and arm64). Package names below are
Ubuntu's; adjust them for other distributions.

---

## 0. Architecture

```
haify / web UI / haify-mcp / haify-ai
        │  gRPC 3374 · REST 3375 · UI 3376 · metrics 9433
        ▼
  haify-controller ──(dispatch over SSH)──►  storage nodes
        │                                   ├─ DRBD 9 (kernel) + drbd-utils
        └─ BBolt DB /var/lib/haify/haify.db     ├─ drbd-reactor (promoters, HA)
                                            ├─ LVM / ZFS pools
                                            └─ OCF agents (gateways)
```

- The control plane is one `haify-controller` process with a BBolt database
  (`[database] path`, default `/var/lib/haify/haify.db`). It drives every node over
  SSH through the `dispatch` library; there is no per-node Haify agent. Commands
  aimed at the controller's own address run locally without SSH.
- The controller listens for gRPC on `[server] port` (default `3374`), for the REST gateway on
  `[server] rest_port` (default `3375`, bound to `[server] listen_address`), for the web UI on `[ui] port`
  (default `3376`) and for Prometheus metrics on `[metrics] port` (default `9433`). The
  UI proxies `/v1/` to the REST gateway and `/ai/` to the AI Copilot on
  `127.0.0.1:7634`, so a browser needs only the UI port.
- In the data plane, DRBD 9 replicates block volumes, `drbd-reactor` promoters
  fail over mounts, VIPs and services, and gateways (NFS / iSCSI / NVMe-oF)
  export a resource behind a floating service IP.
- Optional components are the Kubernetes CSI driver, the `haify-ai` Copilot,
  controller Self-HA (the controller floats on a DRBD-backed VIP), WAN
  replication (DRBD over a per-resource `haify-proxy` pair), encrypted
  replication (kernel TLS) and off-cluster backups (rclone).

Without Self-HA the controller can run on a storage node or on a separate host.
Self-HA requires it to run on a registered node.

---

## 1. Build the binaries

Building needs Go 1.26 (`go.mod` pins toolchain `go1.26.8`) and Node.js for the
web UI (CI uses Node 22).

```bash
git clone https://github.com/haify-project/haify.git && cd haify
(cd web-ui && npm ci)
make build
```

`make build` builds the web UI, copies it into `ui/dist` (embedded into the
controller via `go:embed`), and produces `bin/haify-controller`, `bin/haify`,
`bin/haify-mcp`, `bin/service-ip` (always for Linux, on the build host's
architecture), `bin/csi-controller` and `bin/csi-node`. Everything except
`service-ip` is built for the build host's OS.

To build for Linux nodes from another OS or architecture, build the UI once and
cross-compile:

```bash
make ui-sync
for c in controller:haify-controller cli:haify mcp:haify-mcp service-ip:service-ip; do
  GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bin/${c#*:} ./cmd/${c%%:*}
done
# arm64 nodes: GOARCH=arm64
```

A controller built without `make ui-sync` serves a placeholder page instead of
the UI; the API and `haify` are unaffected. The same holds for `go install`, which
builds without the UI:

```bash
go install github.com/haify-project/haify/cmd/...@latest
```

`go install` names each binary after its directory: `controller`, `cli` and
`mcp` are `haify-controller`, `haify` and `haify-mcp`; rename them when installing.
When cross-compiling (`GOOS`/`GOARCH` set), it puts them in `$(go env GOPATH)/bin/linux_<arch>/`.

Tagged releases on GitHub carry a linux/amd64 archive with `haify-controller`
(UI included), `haify`, `haify-mcp`, `service-ip`, both unit files and
`controller.toml.example`. `make deb` builds Debian packages instead
(section 4).

Other binaries:
- `haify-ai` (`cmd/haify-ai`, its own Go module): `cd cmd/haify-ai && go build .`
- `haify-proxy` (WAN transport) is built from the separate `haify-proxy` repository.
- The CSI image is built from `Dockerfile.csi` (section 9).

---

## 2. Prepare the storage nodes

Do this on every node. `node-prerequisites.md` has the details and the failure
symptoms.

1. The DRBD 9 kernel module, `drbd-utils` and `drbd-reactor` (LINBIT packages or
   source builds):
   ```bash
   cat /proc/drbd            # version: 9.x
   drbdadm --version
   drbd-reactor --version
   ```
   Keep drbd-utils and drbd-reactor versions matched on all nodes. Reactor parses
   `drbdsetup status --json`. A drbd-utils too old for it produces JSON that
   reactor cannot parse, so reactor logs `IGNORING resource … expected ','` and
   never fails over (seen with drbd-utils 9.31 and reactor 1.11; drbd-utils 9.34
   fixed it).
   drbd-reactor must be running (`systemctl enable --now drbd-reactor`);
   `ha self enable` refuses otherwise.

2. LVM (`lvm2`) for LVM pools, `thin-provisioning-tools` for incremental
   backups (`thin_delta`), and `zfsutils-linux` only for ZFS pools.

3. OCF resource agents for gateways:
   ```bash
   apt-get install -y resource-agents-extra   # Filesystem, nfsserver, exportfs, nvmet-*
   ```
   `resource-agents-base` alone lacks `Filesystem`, which every gateway promoter
   starts with. `ha create` and Self-HA use systemd mount units and
   `service-ip@.service` instead of OCF agents.

4. Per gateway type, on the nodes of the exported resource:
   - iSCSI: `apt-get install -y targetcli-fb python3-rtslib-fb`
   - NFS: `apt-get install -y nfs-kernel-server`
   - NVMe-oF: `apt-get install -y nvme-cli linux-modules-extra-$(uname -r)`
     (Ubuntu cloud kernels ship `nvmet-tcp` only in `linux-modules-extra`).
     Gateway creation loads `nvmet` and `nvmet-tcp` (or `nvmet-rdma` for
     `--transport rdma`) and adds them to `/etc/modules-load.d/nvmet.conf`.

5. Optional features:
   - backups: `rclone` on every diskful node of a backed-up resource
   - encrypted replication: `ktls-utils` (tlshd), `openssl`, DRBD ≥ 9.2 with TLS
   - encryption at rest (`resource create --encrypt`): `cryptsetup`, `dm-crypt`

6. A data disk per diskful node: a raw block device (`/dev/sdb`, `/dev/vdb`)
   with no filesystem or mount on it.

Once the nodes are registered (section 5), `haify health-check` reports per
node whether DRBD, drbd-reactor (installed and running) and the resource agents
are present.

---

## 3. SSH trust and dispatch config

The controller reaches nodes over SSH. Node commands use `sudo`, so the SSH user
must be root or have passwordless sudo. Self-HA also requires passwordless
**root** SSH between every pair of nodes.

On the controller node:

```bash
sudo test -f /root/.ssh/id_ed25519 || \
  sudo ssh-keygen -t ed25519 -N "" -f /root/.ssh/id_ed25519 -C root@controller
sudo cat /root/.ssh/id_ed25519.pub
# append that key to /root/.ssh/authorized_keys on EVERY node, including this one
```

`/root/.dispatch/config.toml`:

```toml
[ssh]
user = "root"
port = 22
key_path = "/root/.ssh/id_ed25519"
strict_host_key = false
timeout = "30s"

# One section per node, keyed by the node's IP address.
[hosts."<node1-ip>"]
addresses = ["<node1-ip>"]
user = "root"
key_path = "/root/.ssh/id_ed25519"

[hosts."<node2-ip>"]
addresses = ["<node2-ip>"]
user = "root"
key_path = "/root/.ssh/id_ed25519"

[hosts."<node3-ip>"]
addresses = ["<node3-ip>"]
user = "root"
key_path = "/root/.ssh/id_ed25519"
```

- **Key `[hosts.*]` by IP address.** The controller asks dispatch for hosts by
  address. A section named after a node (`[hosts.node1]`) never matches, and
  settings then fall through to `~/.ssh/config`: any `Host` entry whose
  `HostName` is that IP supplies its user, port and key, which commonly yields
  `unable to authenticate, attempted methods [none]`. dispatch takes each
  setting from the host section first, then the group section, then
  `~/.ssh/config`, then the `[ssh]` defaults, so `user` and `key_path` set
  inside the IP-keyed section always win.
- Point the controller at this file with `[dispatch] config_path`. If that is
  unset, it uses `~/.dispatch/config.toml` of the user the controller runs as. A
  path that does not exist stops the controller at startup.
- dispatch records host keys in `known_hosts` (default
  `~/.ssh/known_hosts`, i.e. `/root/.ssh/known_hosts`); `strict_host_key = false`
  adds unknown hosts automatically. A key that *changed* (a rebuilt node, or a
  VM that regenerates host keys) is rejected with `host key changed: <ip>`
  regardless of that setting. Remove the stale entry with
  `ssh-keygen -R <ip> -f /root/.ssh/known_hosts`; dispatch rereads the file when
  it changes, so no restart is needed.

---

## 4. Install and start the controller

### From the Debian package (Debian, Ubuntu)

`make deb` builds `dist/haify-controller_<version>_amd64.deb` and `_arm64.deb`
(plus the Proxmox plugin package; see `deploy/proxmox/README.md`) with plain
`dpkg-deb`. It needs Go, `dpkg-deb` and the web UI's dependencies
(`cd web-ui && npm ci`); `SKIP_UI_BUILD=1` embeds the UI already in `ui/dist`
instead. The version comes from `git describe --tags`, or `VERSION=...`; a tree
with no tag gets `0.0~git<commits>.<sha>`. The script is
`scripts/build-deb.sh`.

```bash
sudo apt install ./haify-controller_*_amd64.deb
```

| Path | What |
| ---- | ---- |
| `/opt/haify/bin/haify-controller`, `/opt/haify/bin/service-ip` | where the unit runs the controller from, and where the controller finds the `service-ip` it installs on HA nodes |
| `/usr/bin/haify` (`haify-cli` links to it), `/usr/bin/haify-mcp` | the CLI and MCP server; `/opt/haify/bin/haify-mcp` links to the latter for `haify-mcp-http.service` and `haify-ai` |
| `/lib/systemd/system/` | `haify-controller.service`, `service-ip@.service`, `haify-mcp-http.service` |
| `/usr/share/doc/haify-controller/controller.toml.example` | the full example config |

The package differs from the manual install below in three places:

- `haify` is `/usr/bin/haify`, not `/usr/local/bin/haify`, because `/usr/local`
  belongs to the administrator. Remove copies left there by a manual install
  (`/usr/local/bin/haify`, `haify-cli`, `haify-mcp`); they come first in `PATH`.
  Unit files copied to `/etc/systemd/system/` by hand likewise override the
  packaged ones.
- The packaged `service-ip@.service` runs `/opt/haify/bin/service-ip`. The
  controller still installs `/usr/local/bin/service-ip` and its own
  `/etc/systemd/system/service-ip@.service` on any HA node that lacks them,
  this one included, the same as without the package.
- `/etc/haify/controller.toml` is created from the example only when no file
  exists at that path yet (mode 0600, because tokens go in it). It is not a
  conffile, so an upgrade never touches it.

Installing never enables or starts the controller, and an upgrade never
restarts it: one host per cluster runs it, or drbd-reactor does under
Self-HA. After an upgrade, restart it yourself (`systemctl restart
haify-controller`, or under Self-HA install the package on every node and then
`haify ha evict haify-meta`). `apt remove` stops nothing; `apt purge` leaves
`/etc/haify` and `/var/lib/haify` (the database) in place.

Self-HA copies the controller's unit to the standbys as
`/etc/systemd/system/haify-controller.service`. When there is no copy in
`/etc/systemd/system/`, it reads the packaged one in `/lib/systemd/system/`, so
you do not need to copy anything first.

Then configure and start the controller as described in "Configure and start"
below.

### By hand

```bash
sudo install -d /opt/haify/bin /etc/haify
sudo install -m 755 bin/haify-controller bin/service-ip /opt/haify/bin/
sudo install -m 755 bin/service-ip /usr/local/bin/service-ip
sudo install -m 755 bin/haify /usr/local/bin/haify
sudo ln -sf haify /usr/local/bin/haify-cli      # older scripts call it haify-cli
sudo cp configs/haify-controller.service configs/service-ip@.service /etc/systemd/system/
```

### Configure and start

A minimal `/etc/haify/controller.toml`:

```toml
[server]
listen_address = "0.0.0.0"
port = 3374

[dispatch]
config_path = "/root/.dispatch/config.toml"
parallel = 10

[log]
level = "info"
format = "json"

[storage]
default_pool_type = "thin_pool"
default_snapshot_suffix = "_snap"
```

Every other key is in `configs/controller.toml.example` with its default and a
comment: `[server] rest_port`, `[database]`, `[wan]`, `[tls]` (gRPC/REST/UI
transport), `[metrics]`, `[ui]`, `[resource]` (tiebreaker, fault domains),
`[gateway]`, `[schedule]` (switches off snapshot, backup, verify and inspection
schedules together), `[audit]`, `[self_ha]`, `[alert]`, `[inspect]`, and
commented `[auth]` / `[rbac]` blocks. API authentication uses either
`[auth] enabled` with a `token` (at least 16 characters) or `[rbac]` with
per-user tokens.

`configs/haify-controller.service` runs
`/opt/haify/bin/haify-controller --config /etc/haify/controller.toml` as root with
`Environment="HOME=/root"`. Keep the `HOME` line: systemd sets no `$HOME` for a
system unit without it, and dispatch's default lookups (`~/.dispatch`,
`~/.ssh/known_hosts`, default keys) then miss root's files; remote operations
fail with `unable to authenticate, attempted methods [none]` while operations
on the controller's own node still work.

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now haify-controller
ss -tlnp | grep -E ':(3374|3375|3376|9433)\b'
```

`haify` talks to `127.0.0.1:3374` by default; from another host, pass
`--controller <host>:3374` (or `-c`). With `[auth]` or `[rbac]` on, it reads the token from
`--token`, `HAIFY_TOKEN`, `~/.haify/token` or `/etc/haify/token`; with `[tls]` on,
pass `--tls-ca` (and `--tls-cert`/`--tls-key` for mutual TLS).

---

## 5. Register nodes and create pools

```bash
haify node register --name node1 --address <node1-ip>
haify node register --name node2 --address <node2-ip>
haify node register --name node3 --address <node3-ip>
haify node list
```

`--name` can be any name you choose. Registration also records the node's real
hostname, which generated `.res` files use for their `on <host>` sections.
`--replication-address` puts DRBD traffic on a separate NIC or subnet.

Label any node that must never be picked as an automatic diskless quorum
tiebreaker (typically a WAN/DR node whose public address is not configured on
its own interface):

```bash
haify node label <dr-node> haify.tiebreaker=false
```

Without the label, a 2-node LAN resource can pull the DR node into its mesh and
fail `drbdadm up` with `IP <addr> not found on this host`.

Create a pool on each diskful node's data disk. The name gets an `haify_` prefix
(`vg0` becomes VG `haify_vg0`); without `--type` the controller's
`storage.default_pool_type` applies (thin pool by default):

```bash
haify pool create --name vg0 --nodes node1,node2,node3 --devices /dev/sdb
haify pool list
```

`--type lvm` builds a thick VG, `--type zfs` a zpool. A diskless tiebreaker node
needs no pool.

---

## 6. Create a replicated resource

```bash
haify resource create --name data --port 7000 --nodes node1,node2 --size 10G --pool vg0
haify resource status data
```

`--port` and `--size` are required. Omit `--nodes` to let the controller place
`--replicas` (default 2) copies by free pool space. New resources get
`quorum majority` and `on-no-quorum io-error`. With `[resource] auto_tiebreaker`
(default on), a 2-node resource gains a diskless tiebreaker on a third
registered node, so it keeps quorum when one node is lost. The controller also
force-promotes once on create so the fresh resource has an UpToDate copy, and
installs `haify-drbd-up.service` on its nodes so resources come back after a
reboot.

To create a filesystem and mount it (the volume id is positional):

```bash
haify resource fs    data 0 ext4      --node node1
haify resource mount data 0 /mnt/data --node node1
```

---

## 7. Gateways

A gateway is a DRBD resource plus a drbd-reactor promoter that mounts it, brings
up a floating service IP (CIDR) and starts the export on whichever node holds
Primary. Give each gateway its own resource; do not also mount it by hand or put
it under `ha create`.

```bash
# NFS
haify gateway nfs create --resource share --service-ip 192.168.1.200/24 --export-path /share

# iSCSI (LIO)
haify gateway iscsi create --resource lun1 \
    --iqn iqn.2026-01.com.example:haify.lun1 --service-ip 192.168.1.100/24

# NVMe-oF (TCP, port 4420)
haify gateway nvme create --resource ns1 \
    --nqn nqn.2026-01.com.example:haify.ns1 --service-ip 192.168.1.150/24
```

Before writing anything, gateway creation checks the resource's diskful nodes
for the needed OCF agents, plus `rpc.nfsd`/`exportfs` for NFS and `targetcli`
for iSCSI. It then adds the cluster-private state volume
when the resource has only one volume (`[gateway] auto_state_volume`), formats
volumes that carry no filesystem, writes `/etc/drbd-reactor.d/haify-{nfs,iscsi,nvmeof}-<resource>.toml`
and reloads drbd-reactor. iSCSI runs on LIO only: `--implementation tgt` or
`iet` is refused.

To check a gateway, run `haify gateway status --resource <r>` and, on the
Primary, `drbd-reactorctl status` and `ss -tlnp`.

---

## 8. High availability

### Per-resource HA

```bash
haify ha create data --mount /mnt/data --fstype ext4 [--vip 192.168.1.210/24] [--services myapp.service]
```

drbd-reactor promotes the resource, mounts it, raises the VIP and starts the
services on a surviving node when the Primary fails. With `--vip`, the
`service-ip` helper and `service-ip@.service` are installed on nodes that lack
them (from the controller's own build; a node of another architecture is
refused).

### Controller Self-HA

Self-HA puts the controller database on a DRBD resource `haify-meta` mounted at
`/var/lib/haify` and makes drbd-reactor run the controller and its VIP on the
Primary.

Before enabling it, make sure that:
- the controller runs on a registered node, with `/etc/haify/controller.toml` and
  its unit in `/etc/systemd/system/` (or, from the Debian package,
  `/lib/systemd/system/`); the unit's `ExecStart` names the binary by
  absolute path;
- every other node has the controller's architecture, or a build for its
  architecture sits beside the running binary as `haify-controller-<goarch>`
  (e.g. `/opt/haify/bin/haify-controller-arm64`); otherwise `enable` refuses that
  node before changing anything;
- passwordless root SSH works between every pair of target nodes, and the
  dispatch key named in the dispatch config exists on each of them;
- drbd-reactor is active on all of them, and `haify-controller` is not running on
  any node but this one.

```bash
haify ha self enable --pool vg0 --vip 192.168.1.250/24 [--nodes a,b,c] [--port 7999] [--size 1]
haify -c 192.168.1.250:3374 ha self status
haify ha evict haify-meta          # move the controller to another node
```

`enable` copies the running controller binary (or the node's `-<goarch>` build)
to the path the unit's `ExecStart` names on the other nodes, along with
`controller.toml`, the dispatch config and the unit itself. It then disables
`haify-controller` autostart on the standbys and hands over to drbd-reactor; the
command's own connection drops during the handoff. Afterwards, use the VIP for
everything (`<vip>:3374`, `http://<vip>:3376/`).

To ship a new controller build, install it at the unit's `ExecStart` path on
**every** node (`mv` the running file aside first; overwriting it in place fails
with `Text file busy`), then run `haify ha evict haify-meta` to restart it on
another node. Restarting `haify-controller` on the active node also restarts the
promoter target, which fails over the same way.

`[self_ha] extra_services = ["haify-ai.service"]` makes extra units follow the
controller. `enable` reads it when it writes the promoter config.

---

## 9. Kubernetes CSI driver (optional)

The Kubernetes nodes must be the DRBD storage nodes (the node plugin promotes
and mounts DRBD on the host). The controller runs outside Kubernetes and is
reached at an IP or VIP on port 3374.

1. Build the image for the node architecture and import it on every node:
   ```bash
   docker build -f Dockerfile.csi --platform linux/<arch> -t haify-csi:latest .
   docker save haify-csi:latest -o haify-csi.tar
   sudo k3s ctr images import haify-csi.tar      # or ctr -n k8s.io images import
   ```
   Where `registry.k8s.io` is unreachable, pull the sidecars from a mirror,
   retag them to the names in `deploy/k8s/20-controller.yaml` and
   `30-node.yaml`, and import them the same way.

2. Edit `deploy/k8s/00-haify-controller-endpoint.yaml` (the controller IP/VIP in
   the manual EndpointSlice) and the StorageClass `pool` in `40-storageclass.yaml`,
   then run `kubectl apply -f deploy/k8s/`. `50-volumesnapshotclass.yaml` needs the
   snapshot CRDs (`deploy/k8s/README.md`).

3. For a smoke test, run `scripts/csi-e2e.sh`. It uses a PVC on StorageClass
   `haify-drbd` and a pod that writes to it, and prints the node the pod landed
   on.

Both plugin pods use `hostNetwork` with `dnsPolicy: ClusterFirstWithHostNet`, so
they resolve the `haify-controller` Service through cluster DNS. Pods move between
replica nodes with their data. To fail over faster than the 300 s default, use a
Deployment or StatefulSet and short `tolerationSeconds` for
`node.kubernetes.io/unreachable` and `not-ready`.

---

## 10. AI Copilot (`haify-ai`, optional)

`haify-ai` serves the web UI's Copilot. It runs `haify-mcp` as its tool backend
and needs an OpenAI-compatible LLM and embedder.

- Install the binaries on every node, since haify-ai follows the controller:
  `/opt/haify/bin/haify-ai` and `/opt/haify/bin/haify-mcp`.
- The configuration lives on the Self-HA mount: `/var/lib/haify/ai/haify-ai.env`
  and `domain.toml`. The environment variables are:
  - `STEWARD_LLM_API_KEY`, `STEWARD_LLM_BASE_URL`, `STEWARD_LLM_MODEL`,
    `STEWARD_EMB_*` (the older `OPSPILOT_*`, `OPSDOCTOR_*`, `OSS_*` names are
    still read)
  - `HAIFY_AI_KNOWLEDGE_DB` (required), `HAIFY_AI_EMB_DIM` (default 768)
  - `HAIFY_AI_CONTROLLER`: default `127.0.0.1:3374`, the controller on the same
    node
  - `HAIFY_AI_MCP_CMD=/opt/haify/bin/haify-mcp` (default: `haify-mcp` on `PATH`)
  - `HAIFY_AI_DOMAIN=/var/lib/haify/ai/domain.toml` (default `ai/domain.toml`,
    relative to the working directory; the repository's copy is
    `ai/domain.toml`)
  - `HAIFY_AI_KUBECONFIG` (optional): adds the `haify_k8s_*` tools
    (`deploy/k8s/README.md`)
  - `HAIFY_AI_ADDR`: default `127.0.0.1:7634`, where the UI proxies
    `/ai/`. On any non-loopback address haify-ai refuses to start without a token
    (`HAIFY_AI_TOKEN`, `HAIFY_TOKEN`, `~/.haify/token` or `/etc/haify/token`).
- `HAIFY_AI_EMB_DIM` must equal the dimension the index was built with. Switching
  to an embedder of a different width means rebuilding the knowledge base;
  otherwise searches return nothing, without an error. `GET /ai/kb/list` shows
  the width read from the index.
- Shared knowledge base: `make kb` (see `ai/kb/build.sh`) builds
  `dist/kb/haify-kb.db` and its manifest `haify-kb.json`. Install both at
  `/opt/haify/share/` on every node and set
  `HAIFY_AI_SHARED_KNOWLEDGE_DB=/opt/haify/share/haify-kb.db`; it is searched
  read-only next to `HAIFY_AI_KNOWLEDGE_DB`. Startup fails if the cluster's
  embedder model or dimension differs from the manifest's.
- Write a systemd unit `haify-ai.service` (the repository does not ship one)
  with `EnvironmentFile=/var/lib/haify/ai/haify-ai.env` and `WorkingDirectory`
  and `HOME` set to `/var/lib/haify/ai`. Install it on every node but leave it
  **disabled**, and list it in `[self_ha] extra_services` so the promoter starts
  it with the controller.
- HTTP endpoints: `GET /ai/health`, `POST /ai/chat/stream`, `POST /ai/chat/approve`,
  `/ai/config`, and under `/ai/kb/`: `list`, `doc`, `doctor`, `resolve`,
  `upload`, `ingest`, `refresh`, `purge`. Send the same `session_id` in each
  chat request to continue a conversation.

`haify-mcp serve` exposes the same tools to remote MCP clients over HTTP with
token auth (default `127.0.0.1:43871`); `configs/haify-mcp-http.service` is a
reactor-managed unit for it. See [`mcp.md`](./mcp.md).

---

## 11. WAN replication (optional)

WAN replication replicates a resource to a DR site over a per-resource
`haify-proxy` pair (protocol A, mTLS). LAN resources are unaffected.

- The controller pushes its own `/usr/local/bin/haify-proxy` to the WAN nodes;
  for nodes of another architecture place
  `/usr/local/bin/haify-proxy-<amd64|arm64>` beside it. Without a matching binary
  the controller assumes it is already installed on the node.
- The DR node's WAN port must be reachable over TCP from the primary site.
- The controller keeps the proxy CA under `[wan] pki_dir`
  (default `/var/lib/haify/wanproxy-pki`).

```bash
haify resource create --name data --port 7000 --nodes site1 --pool vg0 --size 10G \
    --wan --dr-node site2 --dr-endpoint <site2-public-ip> [--wan-port 37901]
haify resource status data
```

`--wan-port 0` (default) picks a random port above 3000. Failover to the DR site
is manual because replication is asynchronous:

```bash
haify resource dr-failover data --yes
```

`haify wan set-endpoint` and `haify wan repair` change or rebuild the
tunnels.

---

## 12. Verify

```bash
haify node list
haify health-check
haify pool list
haify resource list
haify resource status <name>
haify ha self status
haify inspect run          # read-only cluster checks; [inspect] runs them daily
grpcurl -plaintext <host>:3374 list
curl -s http://<host>:3375/v1/nodes
# UI: http://<host-or-VIP>:3376/
```

On a node:

```bash
drbdadm status
drbd-reactorctl status
systemctl is-enabled haify-drbd-up.service
journalctl -u haify-controller -f
```

---

## 13. Known failure modes

| Symptom | Cause / fix |
| --- | --- |
| Remote ops fail: `unable to authenticate, attempted methods [none]` | Unit lacks `Environment="HOME=/root"`, or the dispatch host section is not keyed by IP (section 3). |
| Remote ops fail with `host key changed: <ip>` | The node's SSH host key changed. `ssh-keygen -R <ip> -f /root/.ssh/known_hosts` on the controller. |
| Controller exits at start: `dispatch config "<path>": stat …: no such file or directory` | `[dispatch] config_path` points at a missing file. |
| Reactor logs `IGNORING resource … expected ','` and never fails over | drbd-utils too old for drbd-reactor; match versions on all nodes. |
| Promoter fails: `Unit ocf.rs@….service not found` | drbd-reactor installed without its `ocf.rs@.service` template and wrapper (`node-prerequisites.md` §1). |
| Reactor won't start: `Could not read config file: /etc/drbd-reactor.toml` | Create it: `snippets = "/etc/drbd-reactor.d"` plus a `[[log]]` table. |
| Pool create fails with `Device or resource busy` | The disk is mounted or formatted: `umount`, remove from `/etc/fstab`, `wipefs -a <dev>`. |
| One node loss stops I/O on a 2-node resource | No tiebreaker: register a third node (with `auto_tiebreaker` on) or add a replica. |
| `gateway nfs create` fails with `missing: rpc.nfsd exportfs` | `apt-get install nfs-kernel-server` (EL: `nfs-utils`) on the resource's diskful nodes. |
| `gateway nvme create` fails loading `nvmet`/`nvmet-tcp` | `apt-get install linux-modules-extra-$(uname -r)`. |
| `gateway nvme create --transport rdma` fails with `no RDMA device` | The node has no RDMA NIC (or soft-RoCE link) under `/sys/class/infiniband`. |
| `pool add-cache` refuses with "not a thin pool" | Convert first: `haify pool convert-thin --node <n> --pool <p>`. |
| Backup fails: `rclone is required on <node>` | Install rclone on that node. |
| Copilot cites documents unrelated to the question | The knowledge base is nearly empty; check `GET /ai/kb/list` and ingest content. |
