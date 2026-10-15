![Haify](docs/assets/images/favicon.svg)

# Haify

Haify is storage that keeps working when a server fails. It replicates volumes across nodes with DRBD 9 and provides them to Kubernetes (CSI, Helm), Docker, Proxmox VE, XCP-ng, KVM (libvirt) and OpenStack (Cinder, Horizon), or exports them through NFS / SMB / iSCSI / NVMe-oF gateways. It also manages storage pools and high availability, and AI assistants can operate it over MCP. The controller is a lightweight program written in Go.

English | [简体中文](README_cn.md) | [Documentation site](https://haify-project.github.io/haify/)

![Haify architecture](docs/img/architecture-en.png)

There is no management agent on the storage nodes. One controller drives them over SSH and keeps its state in an embedded BoltDB. The controller itself can fail over to another node and stay reachable on a floating VIP (Self-HA).

## Features

- **Pools**: LVM, LVM-thin, ZFS, or an image file on a node without a spare disk. SSD caching for thin pools. A disk put into a server shows up as unused, and nothing that holds data is overwritten without being asked.
- **Resources**: replicated DRBD volumes with automatic placement, online resize, diskless clients (made into replicas in place, without stopping what uses them), quorum tiebreakers, LUKS2 encryption at rest, TLS-encrypted replication over one or more networks.
- **Gateways**: NFS, SMB, iSCSI, NVMe-oF, each failing over with drbd-reactor and a floating IP.
- **Applications**: PostgreSQL, MySQL / MariaDB, Redis or an S3 object store (RustFS) run on a resource and fail over with it (`haify app`).
- **Snapshots and backups**: LVM / ZFS snapshots with retention schedules; scheduled, incremental backups to S3, SMB or WebDAV, restorable on a rebuilt or different cluster; a resource copied straight into another Haify cluster.
- **Cross-site**: asynchronous DR replicas over a TCP tunnel that works through NAT.
- **Integrity**: scheduled DRBD verify, and on request a resync of the blocks it found different. Alerts on degraded replicas, lost Primaries, out-of-sync data and full pools.
- **Kubernetes**: CSI driver (manifests or a Helm chart; OpenShift and MicroShift too) with snapshots, group snapshots taken at one instant across several volumes, clones, expansion, raw block and remote (diskless) access. Encryption and TLS replication per StorageClass. Replicas and I/O limits of a volume in use change through a VolumeAttributesClass. Snapshots can go to an S3, SMB or WebDAV target and be restored in another cluster, and a volume can keep an asynchronous replica at another site. When a node dies, its Pods run again on another node in about 30 seconds instead of Kubernetes' five minutes. KubeVirt and OpenShift Virtualization VMs live-migrate on ReadWriteMany block volumes.
- **Docker**: a volume plugin whose volumes follow containers between hosts.
- **Virtual machines**: Proxmox VE storage plugin with a panel in the PVE web UI; an XCP-ng storage driver; a libvirt hook for plain KVM; an OpenStack Cinder driver with a Horizon panel. Live migration works on all four.
- **AI**: MCP server for Claude Code, ChatGPT and others (local or remote with role-scoped tokens and OAuth), plus a Copilot in the web UI, which runs as a separate, optional service.
- **Operations**: token auth, RBAC, audit log, TLS, Prometheus metrics, notifications to Feishu / Slack / WeCom / DingTalk / webhooks.

## Requirements

Storage nodes: Linux with DRBD 9 (kernel module and `drbd-utils`), `drbd-reactor`, `resource-agents`, and LVM and/or ZFS. The full per-node list, with what breaks when each piece is missing, is in [docs/node-prerequisites.md](docs/node-prerequisites.md).

The controller host (a storage node or a separate machine) needs root SSH to every node.

## Quickstart

Three nodes, `node1`..`node3` at `10.0.0.11`..`10.0.0.13`, each with an empty disk `/dev/sdb`. The controller runs on `node1`.

**1. Get the binaries.** Releases carry a linux/amd64 archive with `haify-controller`, `haify`, `haify-mcp`, `service-ip`, the systemd units and `controller.toml.example`:

```bash
curl -LO https://github.com/haify-project/haify/releases/latest/download/haify-linux-amd64.tar.gz
tar -xzf haify-linux-amd64.tar.gz
```

Or build from source (Go 1.26+, Node.js for the embedded web UI); `make build` writes the binaries to `bin/`:

```bash
git clone https://github.com/haify-project/haify.git && cd haify
(cd web-ui && npm ci) && make build
```

`make build` targets the host it runs on. To build Linux binaries elsewhere, see [docs/deployment-guide.md](docs/deployment-guide.md#1-build-the-binaries).

**2. SSH and dispatch.** The controller runs node commands over SSH with the [dispatch](https://github.com/liliang-cn/dispatch) library. On `node1`, as root, create a key and append its public half to `/root/.ssh/authorized_keys` on every node, `node1` included:

```bash
ssh-keygen -t ed25519 -N "" -f /root/.ssh/id_ed25519
```

Then write `/root/.dispatch/config.toml`:

```toml
[ssh]
user = "root"
port = 22
key_path = "/root/.ssh/id_ed25519"
strict_host_key = false   # record unknown host keys on first contact
timeout = "30s"
```

A node that needs a different user, port or key gets its own section keyed by its IP address (`[hosts."10.0.0.12"]`); see [docs/deployment-guide.md](docs/deployment-guide.md#3-ssh-trust-and-dispatch-config).

**3. Install and start the controller** on `node1`, from the unpacked archive (in a source checkout the binaries are in `bin/` and the units in `configs/`):

```bash
install -d /opt/haify/bin /etc/haify
install -m 755 haify-controller service-ip /opt/haify/bin/
install -m 755 haify /usr/local/bin/
cp haify-controller.service service-ip@.service /etc/systemd/system/
cat > /etc/haify/controller.toml <<'TOML'
[dispatch]
config_path = "/root/.dispatch/config.toml"
TOML
systemctl daemon-reload
systemctl enable --now haify-controller
```

Every other key has a default; all of them are listed in [`configs/controller.toml.example`](configs/controller.toml.example).

**4. Register the nodes and check them:**

```bash
haify node register --name node1 --address 10.0.0.11
haify node register --name node2 --address 10.0.0.12
haify node register --name node3 --address 10.0.0.13
haify health-check
```

**5. Create a pool** (an LVM thin pool by default; the VG is named `haify_pool0`):

```bash
haify pool create --name pool0 --nodes node1,node2,node3 --devices /dev/sdb
```

**6. Create a replicated resource** and watch it sync:

```bash
haify resource create --name data --port 7001 --size 10G --nodes node1,node2 --pool pool0
haify resource status data
```

The volume is `/dev/drbd/by-res/data/0` on both nodes. To export it, add a gateway:

```bash
haify gateway nfs create --resource data --service-ip 10.0.0.200/24 --export-path /data
```

The web UI is at `http://node1:3376`. Self-HA for the controller, WAN replicas, backups and the rest are in [docs/deployment-guide.md](docs/deployment-guide.md) and [docs/user-guide.md](docs/user-guide.md).

## Integrations

Kubernetes: put the controller address in `deploy/k8s/00-haify-controller-endpoint.yaml`, `kubectl apply -f deploy/k8s/`, then use the `haify-drbd` StorageClass ([deploy/k8s/README.md](deploy/k8s/README.md)). Proxmox VE: [deploy/proxmox/README.md](deploy/proxmox/README.md); `deploy/proxmox/bootstrap.sh` sets up an existing PVE cluster (DRBD, controller, Self-HA, plugin, storage entry) in one command. XCP-ng: [deploy/xcp-ng/README.md](deploy/xcp-ng/README.md). KVM (libvirt): [deploy/libvirt/README.md](deploy/libvirt/README.md). Docker: [deploy/docker/README.md](deploy/docker/README.md). Helm: [deploy/helm/haify-csi/README.md](deploy/helm/haify-csi/README.md). OpenStack (Cinder, Horizon): [deploy/openstack/README.md](deploy/openstack/README.md). Prometheus and Grafana: [deploy/monitoring/README.md](deploy/monitoring/README.md).

AI assistants:

```bash
claude mcp add haify -- haify-mcp --controller node1:3374            # local
claude mcp add --transport http haify https://<host>/mcp \
    --header "Authorization: Bearer <token>"                     # remote, haify-mcp serve
```

Tools, roles and tokens: [docs/mcp.md](docs/mcp.md).

## Ports

| Port | Service |
| ---- | ------- |
| 3374 | gRPC (`haify`, CSI, MCP) |
| 3375 | REST |
| 3376 | Web UI |
| 9433 | Prometheus |

Controller config: `/etc/haify/controller.toml`.

## Documentation

| Guide | For |
| ----- | --- |
| [docs/user-guide.md](docs/user-guide.md) | Day-to-day use and troubleshooting. Start here. |
| [docs/deployment-guide.md](docs/deployment-guide.md) | Building a cluster from nothing. |
| [docs/node-prerequisites.md](docs/node-prerequisites.md) | What each node needs installed. |
| [docs/mcp.md](docs/mcp.md) | Local and remote MCP, tokens, runbooks. |

## License

Apache License 2.0
