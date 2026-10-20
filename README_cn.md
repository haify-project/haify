![Haify](docs/assets/images/favicon.svg)

# Haify - 软件定义存储

Haify 是一套坏一台服务器也照常工作的存储。它用 DRBD 9 把卷复制到多个节点，提供给 Kubernetes（CSI、Helm）、Harvester、Nomad、Docker、Podman、Proxmox VE、XCP-ng、KVM（libvirt）、OpenStack（Cinder、Horizon）和 OpenNebula 使用，也可以通过 NFS / SMB / iSCSI / NVMe-oF 网关对外提供。它还管理存储池和高可用，AI 助手可以通过 MCP 操作它。控制器是一个用 Go 编写的轻量程序。

[English](README.md) | 简体中文 | [文档站](https://haify-project.github.io/haify/)

![Haify 架构](docs/img/architecture-cn.png)

存储节点上不跑管理 agent。一个控制器通过 SSH 管理所有节点，状态存在内嵌的 BoltDB 里。控制器自己也能切换到别的节点，并始终通过浮动 VIP 访问（Self-HA）。

## 功能

- **存储池**：LVM、LVM-thin、ZFS，也可以在没有空盘的节点上用镜像文件建池；thin 池支持 SSD 缓存。新插的硬盘会显示为未使用，有数据的盘不经确认不会被覆盖。
- **资源**：DRBD 复制卷，支持自动放置、在线扩容、无盘客户端（可原地转成副本，不打断正在用它的东西）、仲裁 tiebreaker、LUKS2 静态加密，复制可走一条或多条网络并做 TLS 加密。
- **网关**：NFS、SMB、iSCSI、NVMe-oF，由 drbd-reactor 加浮动 IP 做故障转移。
- **应用**：PostgreSQL、MySQL / MariaDB、Redis 或 S3 对象存储（RustFS）跑在资源上，随资源一起故障转移（`haify app`）。
- **快照与备份**：LVM / ZFS 快照和保留策略；定时、增量备份到 S3、SMB 或 WebDAV，可在重建的或另一个集群上恢复；也可以把资源直接复制到另一个 Haify 集群。
- **跨站点**：通过能穿过 NAT 的 TCP 隧道做异步容灾副本。
- **数据完整性**：定时做 DRBD verify，发现不一致的块后可按需重同步；副本降级、Primary 丢失、数据不同步、存储池写满时发出告警。
- **Kubernetes**：CSI 驱动（清单或 Helm chart，也支持 OpenShift 和 MicroShift），支持快照、多个卷同一时刻的组快照、克隆、扩容、原始块设备和远程（无盘）访问。按 StorageClass 开启加密和复制链路 TLS。正在使用的卷可以通过 VolumeAttributesClass 改副本数和 I/O 限速。快照可以送到 S3、SMB 或 WebDAV 备份目标，在另一个集群里恢复；卷也可以在另一个机房保留一份异步副本。节点宕机后，上面的 Pod 大约 30 秒就在另一个节点上跑起来，不用等 Kubernetes 默认的 5 分钟。KubeVirt 和 OpenShift Virtualization 的虚拟机可以在 ReadWriteMany 块设备卷上热迁移。
- **Docker 和 Podman**：卷插件，卷跟着容器在主机间走。
- **Harvester**：虚拟机的系统盘、数据盘、镜像和快照放在 Haify 卷上，Harvester 节点以 local agent 方式接入。
- **Nomad**：同一个 CSI 驱动，以 Nomad 插件 job 运行。
- **虚拟机**：Proxmox VE 存储插件（PVE 网页里带 Haify 面板）；XCP-ng 存储驱动；普通 KVM 用 libvirt 钩子；OpenStack 有 Cinder 驱动和 Horizon 面板；OpenNebula 有 datastore 和传输驱动。五者都支持热迁移。
- **AI**：给 Claude Code、ChatGPT 等用的 MCP 服务（本地，或带角色令牌和 OAuth 的远程），以及 Web UI 里的 Copilot（作为单独的可选服务运行）。
- **运维**：令牌认证、RBAC、审计日志、TLS、Prometheus 指标，通知可发到飞书 / Slack / 企业微信 / 钉钉 / Webhook。

## 环境要求

存储节点：Linux，装有 DRBD 9（内核模块和 `drbd-utils`）、`drbd-reactor`、`resource-agents`，以及 LVM 和/或 ZFS。完整的节点清单，以及缺了每一项会出现什么现象，见 [docs/node-prerequisites.md](docs/node-prerequisites.md)。

控制器所在主机（存储节点或单独一台机器）需要能以 root 通过 SSH 登录每个节点。

## 快速上手

三个节点 `node1`..`node3`，地址 `10.0.0.11`..`10.0.0.13`，各有一块空盘 `/dev/sdb`。控制器跑在 `node1` 上。

**1. 获取二进制。** Release 提供 linux/amd64 压缩包，内含 `haify-controller`、`haify`、`haify-mcp`、`service-ip`、systemd unit 和 `controller.toml.example`：

```bash
curl -LO https://github.com/haify-project/haify/releases/latest/download/haify-linux-amd64.tar.gz
tar -xzf haify-linux-amd64.tar.gz
```

或从源码构建（Go 1.26+，Node.js 用于内嵌的 Web UI），`make build` 把二进制写到 `bin/`：

```bash
git clone https://github.com/haify-project/haify.git && cd haify
(cd web-ui && npm ci) && make build
```

`make build` 按当前主机的系统和架构构建。在别的机器上构建 Linux 二进制见 [docs/deployment-guide.md](docs/deployment-guide.md#1-build-the-binaries)。

**2. SSH 与 dispatch。** 控制器通过 [dispatch](https://github.com/liliang-cn/dispatch) 库经 SSH 在节点上执行命令。在 `node1` 上以 root 生成密钥，并把公钥追加到每个节点（包括 `node1` 自己）的 `/root/.ssh/authorized_keys`：

```bash
ssh-keygen -t ed25519 -N "" -f /root/.ssh/id_ed25519
```

然后写 `/root/.dispatch/config.toml`：

```toml
[ssh]
user = "root"
port = 22
key_path = "/root/.ssh/id_ed25519"
strict_host_key = false   # 首次连接时记录未知主机的 host key
timeout = "30s"
```

某个节点需要不同的用户、端口或密钥时，为它单独写一节，以 IP 地址为键（`[hosts."10.0.0.12"]`），见 [docs/deployment-guide.md](docs/deployment-guide.md#3-ssh-trust-and-dispatch-config)。

**3. 在 `node1` 上安装并启动控制器**，以下在解压后的目录里执行（源码构建时二进制在 `bin/`，unit 文件在 `configs/`）：

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

其余配置项都有默认值，完整列表见 [`configs/controller.toml.example`](configs/controller.toml.example)。

**4. 注册节点并检查：**

```bash
haify node register --name node1 --address 10.0.0.11
haify node register --name node2 --address 10.0.0.12
haify node register --name node3 --address 10.0.0.13
haify health-check
```

**5. 创建存储池**（默认是 LVM thin 池，VG 名为 `haify_pool0`）：

```bash
haify pool create --name pool0 --nodes node1,node2,node3 --devices /dev/sdb
```

**6. 创建复制资源**并查看同步状态：

```bash
haify resource create --name data --port 7001 --size 10G --nodes node1,node2 --pool pool0
haify resource status data
```

两个节点上的卷都是 `/dev/drbd/by-res/data/0`。要对外提供，加一个网关：

```bash
haify gateway nfs create --resource data --service-ip 10.0.0.200/24 --export-path /data
```

Web UI 地址是 `http://node1:3376`。控制器 Self-HA、WAN 容灾副本、备份等见 [docs/deployment-guide.md](docs/deployment-guide.md) 和 [docs/user-guide.md](docs/user-guide.md)。

## 集成

Kubernetes：把控制器地址填进 `deploy/k8s/00-haify-controller-endpoint.yaml`，`kubectl apply -f deploy/k8s/`，然后使用 `haify-drbd` StorageClass（见 [deploy/k8s/README.md](deploy/k8s/README.md)）。Proxmox VE 见 [deploy/proxmox/README.md](deploy/proxmox/README.md)；`deploy/proxmox/bootstrap.sh` 用一条命令在现有 PVE 集群上装好 DRBD、控制器、Self-HA、插件和存储条目。XCP-ng 见 [deploy/xcp-ng/README.md](deploy/xcp-ng/README.md)，KVM（libvirt）见 [deploy/libvirt/README.md](deploy/libvirt/README.md)，Docker 和 Podman 见 [deploy/docker/README.md](deploy/docker/README.md)，Nomad 见 [deploy/nomad/README.md](deploy/nomad/README.md)，Harvester 见 [docs/harvester.md](docs/harvester.md)，Helm 见 [deploy/helm/haify-csi/README.md](deploy/helm/haify-csi/README.md)，OpenStack（Cinder、Horizon）见 [deploy/openstack/README.md](deploy/openstack/README.md)，OpenNebula 见 [deploy/opennebula/README.md](deploy/opennebula/README.md)，Prometheus 和 Grafana 见 [deploy/monitoring/README.md](deploy/monitoring/README.md)。

AI 助手：

```bash
claude mcp add haify -- haify-mcp --controller node1:3374            # 本地
claude mcp add --transport http haify https://<host>/mcp \
    --header "Authorization: Bearer <token>"                     # 远程,haify-mcp serve
```

工具、角色和令牌见 [docs/mcp.md](docs/mcp.md)。

## 端口

| 端口 | 服务 |
| ---- | ---- |
| 3374 | gRPC（`haify`、CSI、MCP） |
| 3375 | REST |
| 3376 | Web UI |
| 9433 | Prometheus |

控制器配置：`/etc/haify/controller.toml`。

## 文档

| 文档 | 内容 |
| ---- | ---- |
| [docs/user-guide.md](docs/user-guide.md) | 日常使用和排障，从这里开始。 |
| [docs/deployment-guide.md](docs/deployment-guide.md) | 从零搭建集群。 |
| [docs/node-prerequisites.md](docs/node-prerequisites.md) | 每个节点需要装什么。 |
| [docs/mcp.md](docs/mcp.md) | 本地和远程 MCP、令牌、运维手册。 |

## 许可证

Apache License 2.0
