# Haify user guide

This guide explains how Haify is organised and how to do the day-to-day work
with it. It assumes a cluster that is already up. See
[deployment-guide.md](deployment-guide.md) to build one and
[node-prerequisites.md](node-prerequisites.md) for what each node needs
installed.

Every command here is `haify`, which talks to the controller over gRPC on
port 3374. The same operations are available in the web UI and, for AI
assistants, through [`haify-mcp`](mcp.md).

---

## Contents

1. [The mental model](#1-the-mental-model)
2. [Talking to the cluster](#2-talking-to-the-cluster)
3. [Nodes](#3-nodes)
4. [Pools](#4-pools)
5. [Resources: your storage](#5-resources--your-storage)
6. [Using a resource](#6-using-a-resource)
7. [Changing a resource's shape](#7-changing-a-resources-shape)
8. [Snapshots](#8-snapshots)
9. [Backups: the only copy that survives losing the cluster](#9-backups--the-only-copy-that-survives-losing-the-cluster)
10. [Gateways: exporting to clients](#10-gateways--exporting-to-clients)
11. [High availability](#11-high-availability)
12. [Database and object-store applications](#12-database-and-object-store-applications)
13. [Cross-site replication (WAN DR)](#13-cross-site-replication-wan-dr)
14. [Storage tiering](#14-storage-tiering)
15. [Encryption](#15-encryption)
16. [Alerts and notifications](#16-alerts-and-notifications)
17. [Access control](#17-access-control)
18. [Kubernetes and virtual machines](#18-kubernetes-and-virtual-machines)
19. [The AI Copilot](#19-the-ai-copilot)
20. [Routine operations](#20-routine-operations)
21. [When something is wrong](#21-when-something-is-wrong)

---

## 1. The mental model

Haify works with four layers, each inside the one above:

```
node          a Linux machine Haify reaches over SSH
 └─ pool      an LVM volume group or ZFS pool on that node's disks
     └─ resource   a replicated block device: the same bytes on several nodes
         └─ volume     one block device inside the resource (usually just one)
```

The **resource** is the unit you work with. It is a DRBD device: you write to it on
one node and the bytes land on every replica synchronously. One replica is
**Primary** (mounted, writable); the others are **Secondary** (receiving, not
mountable). Only one node may be Primary at a time, which stops two machines
from writing to the same filesystem and destroying it.

Three ideas explain most of Haify's behaviour:

**Quorum.** A replica set decides by majority who is allowed to serve I/O. Two
diskful replicas cannot form a majority when one is lost, so Haify adds a third
member with no disk, a **tiebreaker**, that votes but stores nothing. Without
it, losing either node suspends I/O on both.

**Promotion is automatic, by drbd-reactor.** You do not usually run `primary`
yourself. You declare what should run with the resource (a mount, a virtual IP,
some services) and drbd-reactor promotes a node and starts them, moving the
whole set elsewhere if that node dies. See [High availability](#11-high-availability).

**Snapshots, replicas and backups** protect against different failures, and only
one of them is a backup:

| | Protects against | Does not protect against |
| --- | --- | --- |
| Replica | Losing a machine | `rm -rf` (it replicates) |
| Snapshot | `rm -rf` | Losing the pool it lives in |
| Backup | Losing the whole cluster | Nothing else, if it is verified |

---

## 2. Talking to the cluster

```bash
haify node list                          # against 127.0.0.1:3374
haify -c 192.0.2.250:3374 node list      # against a remote controller
```

With Self-HA enabled the controller moves between nodes; point `-c` at the
floating VIP rather than at a node, so it never goes stale:

```bash
haify ha self status                     # prints the VIP and the active node
haify -c 192.0.2.250:3374 node list      # 192.0.2.250 = that VIP
```

If the cluster has RBAC or token auth on, supply a token with `--token`, the
`HAIFY_TOKEN` environment variable, or a file at `~/.haify/token` or
`/etc/haify/token` (checked in that order).

If the controller serves TLS, connect with `--tls`, adding `--tls-ca` when its
certificate is not signed by a CA in the system trust store, and
`--tls-cert`/`--tls-key` when it requires client certificates. Each flag has an
environment variable (`HAIFY_TLS`, `HAIFY_TLS_CA`, `HAIFY_TLS_CERT`, `HAIFY_TLS_KEY`,
`HAIFY_TLS_SERVER_NAME`, `HAIFY_TLS_INSECURE`).

Most commands that change something on the nodes wait up to 10 minutes for
the controller; backups and pool rebuilds wait longer. Most listings and status
commands give up after 30 seconds. When a command gives up, the controller may still finish the
work, so check with the matching `list` or `status` before you run it again.

Two commands to learn first:

```bash
haify health-check          # can the controller reach every node, and is the stack installed
haify resource status <name>  # everything about one resource: roles, disks, replication
```

---

## 3. Nodes

```bash
haify node register --name node1 --address 192.0.2.11
haify node list
haify node get node1
```

`--address` is the management IP Haify uses for SSH. If replication should run
over a different network, such as a dedicated 10G link, name it separately:

```bash
haify node register --name node1 --address 192.0.2.11 \
    --replication-address 10.0.0.11
```

**More than one replication network.** With two links between the nodes (two
NICs, two switches), give each node its address on the second network, in the
same order on every node:

```bash
haify node set-paths node1 10.1.0.11
haify node set-paths node2 10.1.0.12
haify node set-paths node3 10.1.0.13
haify node set-paths node1 --clear     # back to one network
```

Each pair of nodes then gets a DRBD path on every network they share. If the
link in use fails, DRBD reconnects over the other one within about half a
minute, and the resource stays UpToDate. Haify rewrites and adjusts the node's
resources in place, and their connections stay up while paths are added or
removed.

DRBD does not connect at all while one of a connection's paths has no local
address. A node whose second NIC is down at boot, or that lost its address
when the link went down, would then be cut off on the network that still
works too. To prevent this, every node with `set-paths` runs a timer
(`haify-paths-guard.timer`). Every ten seconds it drops paths that have no
local address, and it puts them back when the address returns.

**Labels** describe where a node physically is. Auto-placement uses them to keep
replicas apart, or together:

```bash
haify node label node1 rack=A zone=east
haify node label node1 rack=          # trailing = deletes the label
haify node label node1 zone=west --replace   # drop every other label
```

`haify.tiebreaker=false` keeps automatic selection from picking a node as a
resource's quorum tiebreaker (you can still name the node explicitly). Set it on an off-site DR node, which is not on the
replication network.

**Renumbering** a node (its IP changed, or it moved subnet) takes one command
once the node answers on the new address:

```bash
haify node set-address node1 192.0.2.21
haify node set-address node1 192.0.2.21 --replication-address 10.0.0.21
```

The command first checks that the new address reaches the same machine. It
then updates the node in the registry, rewrites the node's entry in
`/etc/hosts` on every node, and rewrites the DRBD config of each resource the
node is part of. Each of those resources reconnects on the new address.

When several nodes changed address, for example because a DHCP server handed
every node a new lease, renumber them in **one** command. One at a time does
not work here, for two reasons. The controller would rewrite each node's
resources through peers it still knows only by their old addresses. And when
two nodes swap addresses, the configs would pass through a state where both
nodes have the same address.

```bash
haify node set-address node1=192.0.2.21 node2=192.0.2.22 node3=192.0.2.23
```

If every node moved, the controller cannot start: its database is on a DRBD
resource whose peers can no longer find each other. Bring that resource back
by hand first. Replace all old addresses with the new ones in a single pass;
a chain of `sed` substitutions goes wrong when two nodes swap addresses:

```bash
# on every node, with each node's old → new address
perl -pi -e 'my %m = ("192.0.2.11" => "192.0.2.21", "192.0.2.12" => "192.0.2.22",
                      "192.0.2.13" => "192.0.2.23");
             s/\b(\d+\.\d+\.\d+\.\d+)(?=:)/exists $m{$1} ? $m{$1} : $1/ge' /etc/drbd.d/haify-meta.res
drbdadm adjust haify-meta
# once the controller is up on its VIP:
haify node set-address node1=192.0.2.21 node2=192.0.2.22 node3=192.0.2.23
```

`resource repair <resource>` also writes the registry's addresses into a
resource's config. If a renumbering could not reach a resource (because a node
was down), repair that resource afterwards.

**Draining** a node marks it `maintenance` and moves every resource that is
Primary there to another replica. Drain before maintenance, not after:

```bash
haify node drain node1
# ... reboot, replace a disk, upgrade ...
haify node undrain node1
```

A `maintenance` node gets no new replicas or tiebreakers (`resource create`,
CSI provisioning) and keeps that state through health checks and
re-registration until `undrain`. Undrain moves nothing back.

How each Primary moves:

- **HA resources, gateways, apps and haify-meta** go through drbd-reactor's eviction,
  the same as `haify ha evict`; drbd-reactor picks the new node.
- **Everything else** is demoted, then promoted on the first replica in the
  resource's node list that is diskful, `UpToDate`, connected, and neither
  drained, offline, nor a WAN resource's DR node. If that promote fails, the
  original node is promoted back.

A resource that cannot move (no qualifying replica, a mounted volume, a
failed eviction) stays Primary where it is; the drain moves the rest and
names each one it left, with the reason. The node stays drained either way.

Unregistering is for a node that is never coming back:

```bash
haify node unregister node4
```

It refuses while anything still uses the node, and names each resource and its
role there:

```
node node4 is still in use by: data (replica, NFS gateway); logs (tiebreaker); move or remove these first
```

Clear each role first: a replica with `resource remove-replica`, a tiebreaker
with `ha set-tiebreaker` (move it, or `--remove`), a diskless client with
`resource diskless detach`, a gateway with `gateway delete`. A WAN resource's
DR node can only be released by deleting that resource. Once nothing is left,
unregistering removes the node from the registry; it does not touch the node.

---

## 4. Pools

A pool is the storage a node contributes. There are three kinds, plus a fourth,
thin on VDO, described below:

```bash
haify pool create --name data-pool --type lvm      --nodes node1,node2 --devices /dev/sdb
haify pool create --name thin-pool --type lvm-thin --nodes node1,node2 --devices /dev/sdc
haify pool create --name tank      --type zfs      --nodes node1,node2 --devices /dev/sdd
haify pool list
haify pool get --name thin-pool --node node1
```

On the nodes the volume group (or zpool) is named with an `haify_` prefix:
`thin-pool` becomes `haify_thin-pool`. Commands accept either form except
`pool convert-thin`, which takes the prefixed name.

A ZFS pool compresses data; OpenZFS 2.2 and later use `lz4` by default.
`--compression <algorithm>` (`zstd`, `zstd-3`, `gzip-6`, `off`, ...) picks
another algorithm and `--dedup` turns on deduplication. Both are set on the
pool's root dataset at creation, so every volume inherits them. Dedup costs RAM
on every write and only pays off for data that is known to repeat. `pool get`
(and the MCP pool listing) show the algorithm and the ratio ZFS achieves
(`Compression: zstd, 1.85x achieved`). Encrypted resources can only use LVM
pools, so ciphertext, which does not compress, never lands on a ZFS pool.

**A pool on a file.** A node without a spare disk (a laptop, a test VM) can
build a pool on an image file:

```bash
haify pool create --name data --type lvm-thin --nodes node1,node2 \
    --devices file:/var/lib/haify-pools/data.img:50G
```

For a thin pool the file is sparse; for a thick pool it is allocated in full.
Haify attaches the file as a loop device and builds the pool on it as on a
disk, so thin volumes, snapshots and everything else work the same. A unit
attaches the file again at boot, before DRBD starts. Every write also passes
through the node's filesystem, so use a real disk for a pool that matters. The
file must be on the node's own disk. A path under `/var/lib/haify` (the
controller's own replicated volume) or on any DRBD device is refused.
`pool delete` removes the file.

Omitting `--type` gives the controller's `[storage] default_pool_type`, which
is `thin_pool` unless changed. **Choose thin unless you have a reason not to.**
A thick (plain `lvm`) pool cannot hold a useful snapshot history. LVM makes
every thick snapshot reserve its copy-on-write area up front, so a 10 GiB pool
backing a 6 GiB volume fits only two snapshots, even if nothing ever changes. A thin snapshot costs only
the blocks that diverge.

Converting later is possible but is a rebuild, one node at a time:

```bash
haify pool convert-thin --node node1 --pool haify_data-pool
```

It destroys that node's copy and resyncs it in full from the peers. The resource
keeps serving throughout, and the node is diskless until the resync is done. It
refuses to start if the node holds a Primary, if a peer is not `UpToDate`, if a resync
is already running, or if this is one of only two diskful copies.

**Growing a pool.** A new disk in a server shows up in `node disks` and on the
web UI's Pools page under "Unused disks":

```bash
haify node disks                     # every node; --node to narrow
haify pool add --pool data-pool --nodes node1 --devices /dev/sde
```

`node disks` says what each disk is: `free` (empty), `wipe` (holds only old
data: a filesystem, a partition table, the label of an array or pool that is not
running), `pool` (in a Haify pool) or `busy` (mounted, the system disk, another
volume group, a DRBD backing disk). `pool create`, `pool add` and
`pool replace-disk` accept `free` disks. They accept `wipe` disks only with
`--wipe`, which erases them. They always refuse a `busy` disk, so a mistyped
device name cannot destroy a disk in use. A device can also be named by its
`/dev/disk/by-id` link, which does not change when a reboot renumbers `/dev/sdX`.

The disk joins the volume group. If the group holds a thin pool, Haify then
extends that pool so thin volumes can use the new disk straight away. It first
grows the pool's metadata area in proportion, then extends the pool into the
same share of the group's free space that `pool create` uses: 95% minus
`[storage.thin] reserve_percent`, so 85% by default. A ZFS pool takes the disk as a new vdev (`zpool add`). If the
thin pool could not be extended, running the same `pool add` again finishes it.

Volumes already in the pool keep their size until you resize them. A
replicated volume can only grow as far as its fullest node allows, so add a
disk on every node that holds a replica.

**Disks.** `pool disks` lists the disks under the pools on every node, with
their health from SMART or the NVMe health log:

```bash
haify pool disks                     # every node; --pool, --node to narrow
```

The health is one of:

- `ok`;
- `warn`: an NVMe drive past 90% of its rated endurance or with media errors,
  or an ATA disk with reallocated or pending sectors;
- `fail`: the drive's own SMART verdict, an NVMe critical warning or worn-out
  drive, or uncorrectable sectors;
- `unknown`: no `smartctl` on the node, or a device that has no SMART, such as
  a virtual disk.

The check needs `smartmontools` on the nodes. The daily inspection runs
the same check (`disk.health`) and raises a `disk.health` event for every disk
at `warn` (warning) or `fail` (critical).

Taking a disk out moves its data to the group's other disks first (`pvmove`),
while the pool stays in use, and only then drops it from the pool:

```bash
haify pool remove-disk  --pool data-pool --node node1 --disk /dev/sdc
haify pool replace-disk --pool data-pool --node node1 --disk /dev/sdc --new-disk /dev/sdf
haify pool jobs                      # what is running; --all includes finished ones
```

`remove-disk` is refused when the other disks have no room for this disk's
data, and when it is the pool's only disk. `replace-disk` works in both cases:
the new disk joins, the old disk's data moves to it, then the old disk leaves.
Both run as a job on the node, so they survive a controller restart or
failover. When a job ends, it raises a `pool.disk_moved` event. A move takes
about as long as reading the whole disk once.

**Keeping thin pools from filling.** When a thin pool is full, writes to it
fail at once, DRBD drops the disk, and the resource carries on with one replica
fewer. Three things in `[storage.thin]` prevent that:

- **Trim.** Every day (`trim_schedule`, `30 2 * * *` by default) the controller
  runs `fstrim` on every mounted ext4 and XFS filesystem on DRBD, on the node
  serving it. The discards reach every replica, so each node's thin pool gets
  back the blocks the filesystem freed. To trim now, run `haify pool trim`
  (`--node` for one node). Each scheduled run raises `pool.trimmed` with how
  much was discarded.
- **No zeros from resyncs.** A resync onto a thin volume writes every block it
  copies, holes included, unless DRBD knows the device can discard. Resources
  on thin pools get `rs-discard-granularity` set automatically, and the
  controller adds it to older resources that lack it when it starts.
- **Autoextend.** A new thin pool leaves `reserve_percent` (10) of its volume
  group free. When a pool's data or metadata use reaches `autoextend_threshold`
  (80%), the controller grows that part into the free room. Metadata is
  doubled. Data grows by `autoextend_percent` (20) of the pool's size, or by
  what the group has left if that is less. When both cross the threshold,
  metadata grows first. Each extension raises `pool.extended`. When the group has no
  room left, `pool.extended` warns once: that is the time to `pool add` a
  disk. `autoextend_threshold = 0` turns it off.

```toml
[storage.thin]
trim_schedule = "30 2 * * *"   # "" turns it off
reserve_percent = 10           # of a new pool's volume group, left for autoextend
autoextend_threshold = 80      # 0 turns it off; otherwise 50-95
autoextend_percent = 20        # 5-100
```

Deleting a pool is per node, and an LVM pool that still holds any volume is
refused; the freed disks have their PV labels wiped:

```bash
haify pool delete --name data-pool --node node1
```

**Thin pools on VDO (`--type lvm-thin-vdo`).** The thin pool's data area sits
on a VDO volume, so everything written to it is deduplicated and compressed;
thin volumes, thin snapshots and DRBD above it behave as on a plain thin pool.
It suits data that repeats, such as VM images built from the same template,
backups and logs.

```bash
haify pool create --name dedup --type lvm-thin-vdo --nodes node1,node2 --devices /dev/sdf
```

Each node needs the dm-vdo kernel module (kernel 6.9 or later, or kmod-kvdo on
EL), `vdoformat` from the `vdo` package, and lvm2 2.03.24 or later
(`--pooldatavdo`); `pool create` checks for all three before it touches the
disk. Ubuntu 24.04's GA kernel does not ship dm-vdo; use the HWE kernel.

What to know before choosing it:

- **Physical space is the figure to watch.** The thin pool's usage shows the
  logical space handed out. The VDO pool under it fills at whatever rate is
  left after dedup and compression. When VDO runs out of physical space, writes
  fail with I/O errors although the thin pool still shows room, and DRBD
  drops the disk. `pool get` shows both
  (`VDO: 41.00% physical used, 63.00% saved by dedup/compression`),
  and `pool.vdo_physical_near_full` / `pool.vdo_physical_full` fire at the
  same thresholds as the thin pool alerts.
- **It deduplicates per node.** VDO is under DRBD, so DRBD replicates every
  logical block in full; it saves disk on each node, not network.
- **No encryption.** An encrypted resource is refused on a VDO pool:
  ciphertext neither deduplicates nor compresses.
- **Deleted snapshots do not give space back to VDO** on current lvm2 (the thin
  pool does not pass the discard down); the space is reused for new writes
  inside the pool, but VDO's physical usage does not drop.
- **Growing is by hand.** `pool add` adds the disk to the group but does not
  grow the pool: extend the VDO pool's physical size first, then the thin
  pool's logical size, in the ratio you expect dedup to achieve.
- VDO costs memory (roughly 1 GB per TB of physical space for its index and
  block map, more with a larger index) and CPU on every write.

This is new in Haify; validate it on your hardware and kernel before production.

An SSD or NVMe cache in front of a thin pool (`pool add-cache`,
`pool remove-cache`) is covered in [Storage tiering](#14-storage-tiering).

**Quotas.** A thin pool accepts volumes as long as it has room for the data
already written, so it can promise many times its real size. `[quota]
max_overcommit_ratio` caps that. A new resource, replica, volume or resize is
refused on a node when the sum of the thin volumes in that node's thin pool
(snapshots not counted) would pass that many times the pool's real size, and
auto-placement skips the node. The default, 0, means no limit.

A project is a set of resources that share a label (`project=<name>` by
default, set by `[quota] project_label`). A project's quota caps the number of
its resources and their total size. Each resource counts once, at the size
asked for, not once per replica:

```toml
[quota]
max_overcommit_ratio = 3.0
[[quota.projects]]
name = "team-a"
max_gb = 2000
max_resources = 50
```

```bash
haify resource create --name a1 --size 100G --label project=team-a ...
```

---

## 5. Resources — your storage

The simplest form:

```bash
haify resource create --name db --size 100G --port 7000 --nodes node1,node2
```

That creates a 100 GiB replicated device on two nodes, adds a diskless
tiebreaker on a third if one is available, and brings it up, Secondary on every
node. Each resource needs its own **port** (7000+ by convention; it must be free
on every node). Without `--pool` the volume goes into a pool named `data-pool`.

The tiebreaker is added only to a resource with exactly two diskful replicas,
never to a WAN resource, and only while `[resource] auto_tiebreaker` is on (the
default). With no spare node the resource is created without one and
`resource list` marks it `⚠quorum-risk`.

**Placement.** Omit `--nodes` and Haify picks by free space:

```bash
haify resource create --name db --size 100G --port 7000 --replicas 2
haify resource create --name db --size 100G --port 7000 --replicas 3 \
    --replicas-on-same zone --do-not-place-with web
```

`--replicas-on-same zone` keeps every replica in one zone (latency);
`--do-not-place-with web` keeps this resource off the nodes `web` uses
(anti-affinity); `--replicas-on-different rack` gives each replica a different
`rack` value. All three take a label key, are repeatable, and apply only when
`--nodes` is omitted. Placement considers only online nodes that host the
requested pool and have room for the volume in it.

**Fault domains.** Nodes that fail together (VMs on one physical host, servers
in one rack) should not hold two copies of the same data. Tell Haify which
nodes share a machine with a `host` label:

```bash
haify node label node1 host=hv1
haify node label node2 host=hv1
haify node label node3 host=hv2
```

Automatic placement, `resource profile adjust` and the CSI driver then put
replicas on different hosts first, and only then look at free space. The quorum
tiebreaker goes to a host that holds none of the replicas: a tiebreaker on the
same host as a replica fails together with it, and the surviving replica loses
quorum. `add-replica` uses
the node you name and does not check the label. When the cluster cannot spread
(all VMs on one machine), the resource is still created and the CLI prints a
warning. `resource list` flags every resource where losing one host would lose
all copies or the quorum majority: `⚠one-failure-domain(host=hv1)`.

The label key is `[resource] fault_domain_label` in `controller.toml` (default
`host`; a StorageClass sets its own with `faultDomainLabel`). A node without
the label counts as its own domain, so a cluster without the label places
replicas exactly as it would without this feature. `--replicas-on-different host` makes the spread a hard requirement.

**Profiles** group resources that should be alike. A resource created with
`--profile`, or attached to it later, is a member, and what is set on the profile
reaches every member:

```bash
haify resource profile create --name db-tier --pool thin-pool --protocol C --replicas 2 \
    --replicas-on-different host --drbd-options net/max-buffers=4000
haify resource create --name db --size 100G --port 7000 --profile db-tier
haify resource set-profile legacy-db db-tier      # attach an existing resource
haify resource list --profile db-tier             # the members
haify resource profile get db-tier                # settings and members

# one change, every member: saved on the profile, applied to each resource
haify resource profile set-options db-tier --drbd-options net/max-buffers=8000

# after raising --replicas, or for a member attached with fewer copies:
haify resource profile adjust db-tier --dry-run   # what would change
haify resource profile adjust db-tier             # apply options, add missing replicas

haify resource profile max-size db-tier           # largest volume a new member could get
haify resource profile list
haify resource profile delete db-tier
```

A flag given to `resource create` overrides the profile's value for that
resource. `adjust` applies the profile's DRBD options to every member and adds
replicas where the profile asks for more, placed by its pool and label
constraints. It never removes one: a member with more copies than the
profile is reported and left alone. A profile with members cannot be deleted;
take them out first with `resource set-profile <resource> --none`.

**DRBD options** can be set at creation or changed later:

```bash
haify resource create --name db ... --drbd-options on-no-quorum=suspend-io
haify resource set-options db --drbd-options disk/c-max-rate=200M,net/max-buffers=8000
```

Keys take the form `section/key`; a bare key goes to the resource-level
`options` section, so `on-no-quorum` needs no prefix but a `net` or `disk`
option does. `disk/` options go into every volume, both at creation and with
`set-options`. `set-options` rewrites the config on every node and runs
`drbdadm adjust`.

**Labels** on a resource (`--label app=postgres`, repeatable) are free-form tags
shown by `resource list`; a profile can carry default labels. Change them later
the way node labels are changed, or in the web UI under the resource's detail:

```bash
haify resource label db                 # show them
haify resource label db app=postgres    # set one
haify resource label db app=            # trailing = deletes the label
```

Integrations set labels of their own: `haify.pve/managed-by` (Proxmox VE),
`haify.csi/managed-by` (Kubernetes), `haify.openstack/managed-by` (Cinder) and
`haify.libvirt/domain` (KVM). The web UI uses them to find each integration's
guests.
A Kubernetes volume also carries `haify.csi/pvc=<namespace>/<name>`, naming its
claim. The web UI shows it in place of the `kubernetes` tag, alerts show it
after the resource name, and the resource search finds the volume by it.

**Storage type** follows the pool automatically for LVM: a thin pool gets a
thin volume without you asking for it. ZFS is the exception and must be named:
`--storage-type zfs`.

---

## 6. Using a resource

A fresh resource is a raw block device, Secondary everywhere. Promote it on one
node, put a filesystem on it there and mount it:

```bash
haify resource primary db node1                       # <resource> <node>
haify resource fs db 0 ext4 --node node1              # <resource> <volume-id> <fstype>
haify resource mount db 0 /mnt/db --node node1        # <resource> <volume-id> <mount-path>
```

Volume ids start at 0. A single-volume resource is always volume `0`. `fs` runs
`mkfs.<fstype>` on `/dev/drbd/by-res/<resource>/<volume-id>` (without `--node`,
on the current Primary). `mount` creates the mount path and mounts the device;
it does not add an fstab entry, so the mount does not survive a reboot.

To give it back:

```bash
haify resource unmount db 0 --node node1
haify resource secondary db node1
```

`promote`/`demote` are the same as `primary`/`secondary`; `primary --force`
promotes a node whose data DRBD does not consider up to date.

For anything that should survive a node dying, do **not** mount it by hand.
Declare it as HA instead and let drbd-reactor do the mounting. See
[High availability](#11-high-availability).

To see what you have:

```bash
haify resource list
haify resource get db         # port, protocol, nodes, tiebreaker, volumes, profile, labels
haify resource status db      # roles, disk states, replication, per node
```

Read `status` like this: exactly one node should be `Primary`, every node's disk
should be `UpToDate`, and replication should be `Established`. Anything else is
covered in [When something is wrong](#21-when-something-is-wrong).

**Diskless clients** let a node mount a resource without storing a copy; it
reads and writes over the DRBD network:

```bash
haify resource diskless attach db node3
haify resource primary db node3        # after unmounting and demoting elsewhere
haify resource mount db 0 /mnt/db --node node3
# ... later: unmount and demote on node3, then
haify resource diskless detach db node3
```

This suits a compute node that needs the data but has no disks to spare. The
one-Primary-at-a-time rule still applies. If you attach the node that is the
resource's tiebreaker, it becomes a client and keeps its quorum vote.

**Dual-primary** exists for one job: live-migrating a VM, when the source and
target hypervisor both hold the disk open during the hand-off.

```bash
haify resource dual-primary db on
haify resource dual-primary db off
```

Do not use it to share one volume between two machines: an ordinary filesystem
mounted twice is corrupted. WAN resources are refused. The setting is
runtime-only: a reboot or `drbdadm adjust` returns the resource to
single-primary.

**Deleting** a resource first removes its HA configuration and any app or
gateway on it, then unmounts it, takes it down on every node, and deletes its backing
volumes. It does not ask for confirmation:

```bash
haify resource delete db
```

There is no force option, because errors do not stop the delete. If a node
fails to take the resource down, or a backing volume cannot be removed, the
delete carries on and logs a warning in the controller log. Remove a volume
left behind by hand on its node (`lvremove` or `zfs destroy`). The delete is
refused while any snapshot of the resource is locked (see Snapshots).

---

## 7. Changing a resource's shape

All of these run on a live resource.

```bash
# more capacity: <resource> <volume-id> <size>. Refused when a replica's pool
# cannot hold the growth: the new area is written to every replica, and a full
# thin pool drops the disk (--ignore-free-space overrides).
haify resource resize-volume db 0 200G

# another local replica (refused if the node's pool has less free space than
# the volume: the sync writes all of it; --ignore-free-space overrides).
# On a node that is already a diskless client or the tiebreaker, the node gets
# a disk in place: whatever it has open keeps running while the disk attaches
# and resyncs from the peers.
haify resource add-replica db --node node3

# take one out; the node's volume for it is deleted
haify resource remove-replica db --node node3 --yes

# more volumes in the same resource (--volume is the backing volume's name in
# the pool); remove-volume takes the volume id
haify resource add-volume db --volume db_logs --size 50G --pool thin-pool
haify resource remove-volume db 1

# move a volume to another pool, e.g. onto new disks or from thick to thin
haify resource move-volume db --volume 0 --pool fast
```

`move-volume` rebuilds the volume in the target pool one node at a time,
secondaries first. On each node, the controller waits until the peers are
UpToDate and creates the new backing volume. It then detaches the old one,
attaches the new one, and lets DRBD resync it in full from the peers. Only then does it delete the
old backing volume and its snapshots. The resource keeps serving throughout:
while its disk is detached, a Primary reads and writes over the network. The pool must exist on every node holding a replica, and the resource
needs at least two diskful replicas. It is refused for an encrypted resource,
while one of its replicas is being moved to another node, and while a snapshot
is locked. It runs as a
job (`haify pool jobs`) that resumes after a controller restart. A failed job can
be started again and continues from the nodes not yet moved. The volume's
snapshots on the old pool are deleted with it, so take a backup first if you
need them.

A resource's promoters (its `ha create` config and its gateway) follow its
replicas. `add-replica` first checks that the new node could run them: the OCF
agents and services the chain starts, and the gateway's tools. If it could not,
the command refuses. Once the replica is in, the node gets a copy of each
promoter, plus the mount unit an HA config starts, so it can take over as soon
as it is UpToDate. `remove-replica` removes them from the node that leaves.
Nodes that already hold a promoter are never rewritten, because on the node
running the service a rewrite and reload would restart it. A WAN resource's DR
node never gets a promoter.

`remove-replica` is refused when the node is Primary, when it is the
tiebreaker or the off-site DR node, or when fewer than two diskful copies would
remain. It first stops the resource on the leaving node, then rewrites the
config on every node and adjusts the survivors. So every node must be
reachable; for a node that never will be, see `--lost` below. If a step fails,
the command stops with an error and the registry still lists the replica.
After a failed removal, check that every node's
`/etc/drbd.d/<resource>.res` agrees:

```bash
# on each node
md5sum /etc/drbd.d/db.res
```

Mismatched files mean one node has a stale view: copy the correct one over and
run `drbdadm adjust db`. Removal also frees the leaver's bitmap slot on the
survivors (`drbdsetup forget-peer`), which an add-replica later needs.

**A node that is gone for good.** A normal removal has to reach the leaving
node. When it never will, use `--lost`:

```bash
haify resource remove-replica db --node node3 --lost --yes
```

Nothing runs on node3. The survivors, the tiebreaker and any clients get a
config without node3, are adjusted, and forget its slot; the registry drops
it. The command is refused while node3 answers over SSH, and while any survivor
is still connected to it over DRBD (a node cut off from the controller is not
necessarily gone). It is also refused unless the survivors still hold quorum
and an UpToDate copy without node3. One remaining diskful copy is enough,
since node3's copy is already lost; add a replica afterwards. node3 keeps its volume and its old config: if it ever
comes back, run `drbdadm down db` there and delete
`/etc/drbd.d/db.res`, its `haify-*-db.toml` promoters and its volume before it
rejoins anything.

If the survivors lost quorum with it (two replicas and no tiebreaker), give
them one first: `haify ha set-tiebreaker db <node>` works with a member that is
gone (no SSH, and no survivor connected to it), skipping it. Then remove it
with `--lost`.

`resource repair <resource>` rewrites the config on every participant
(replicas, tiebreaker, diskless clients) so they agree on the volumes and the
registry's node addresses, then runs `drbdadm adjust`. Use it when a tiebreaker
or client of a multi-volume resource stays `Connecting` and its kernel log says
a packet arrived "for volume N, which is not configured locally". Repair also
puts the resource's promoters on exactly its primary-site replicas, as
`add-replica` does. Use it after an older version added or removed a replica,
or gave a DR node a promoter.

**Adopting** an existing DRBD resource that Haify did not create:

```bash
haify resource adopt legacy-vol --nodes node1,node2
```

Adopting reads the live `/etc/drbd.d/<name>.res` and records what it finds; it
never changes the resource or its data. `--nodes` and `--port` are read from
the config when omitted; `--protocol` defaults to C.

**Tiebreakers** can be moved if the node holding one is going away. The change
is config-only: nothing resyncs and a promoted resource keeps serving.

```bash
haify ha set-tiebreaker db --node node4
haify ha set-tiebreaker db --remove       # drop it, accepting the quorum risk
```

**Adding a replica while a member is away.** `add-replica` rewrites every
member's config, so it is refused when a member does not answer. With
`--allow-unreachable` it goes ahead on the members that answer, as long as
they are the majority. The members that were away are recorded, and they get
the new config (a `resource repair`) as soon as they answer again.

**Moving a replica** adds the new one, waits until it is UpToDate and only then
removes the old one, so the resource is never a copy short. The command returns
once the new replica is added. The removal follows by itself, and resumes after
a controller restart or failover. A Primary is not moved: drain the node first.

```bash
haify resource move-replica db --from node2 --to node4
```

**Rebalancing** never happens on its own, because each move is a full sync
(1 TB takes tens of minutes on 10GbE, hours on 1GbE). You get a plan and apply
it. The plan moves replicas from the node holding the most allocated capacity
to nodes that hold less, until the spread is within a tenth of the mean. It
leaves out the controller's metadata, WAN resources and CSI volumes (a
PersistentVolume's node affinity is fixed when it is created), and says so.

```bash
haify rebalance                  # the plan; nothing changes
haify rebalance --apply          # run it, one move at a time
```

**Preferred nodes** set the order in which drbd-reactor tries nodes for an HA
resource: the further down the list a node is, the longer it waits. With
`--policy start-only` (drbd-reactor 1.9+) the order only decides where the
resource starts. With `always`, the resource also moves back when a more
preferred node returns, and that move is a failover in itself. The order is
only a preference; DRBD quorum is what prevents split brain.

```bash
haify ha set-preferred db --nodes node1,node2 --policy start-only
```

### Self-healing

A node that dies leaves every resource it held a copy of one replica short.
With `[self_heal] auto_evict = "on"` the controller replaces those replicas
itself. With `"dry-run"` it makes the same decisions but only reports what it
would do (`node.evicted` events). It acts only when all of these hold:

- the node has been offline for `after_minutes` (default 60; the time is kept
  across controller failovers, and `haify node list` shows it);
- at most `max_offline_percent` (default 34) of the nodes are offline, and the
  controller reaches a majority (otherwise it may be the one cut off);
- for each resource, the `--lost` guard above: the node does not answer over
  SSH, no surviving member is connected to it over DRBD, and the survivors
  hold quorum and an UpToDate copy;
- the node is not drained and not labelled `haify.io/auto-evict=false`, and the
  resource is not the controller's own metadata, not WAN-replicated and not
  labelled `haify.io/auto-evict=false`.

It replaces one replica at a time and waits for each sync before the next.
The new replica goes where placement would put it. Once the node holds no
replica, it is **evicted**: it gets no new replicas, and if it comes back it
still holds its old configs and volumes. Then:

```bash
haify node restore node3 --dry-run   # what would be deleted on it
haify node restore node3             # delete it, and let it take replicas again
haify node lost node3                # or: it is not coming back
```

`node restore` works on that node only. It takes down every resource the node
is no longer a member of, and deletes the resource's config, its promoter
configs and the volumes Haify named after it. `node lost` removes every replica the node still holds the `--lost`
way and keeps it evicted. Both are on the two-person approval list.

---

## 8. Snapshots

Snapshots live in the same pool as the resource. They are instant and cheap on a
thin pool, and they are **not a backup**: losing the pool loses both.

```bash
haify resource snapshot create --resource db --name before-upgrade
haify resource snapshot list --resource db
haify resource snapshot restore --resource db --name before-upgrade
haify resource snapshot delete --resource db --name before-upgrade
```

A manual snapshot covers volume 0's backing volume on **one node** (`--node`,
by default the resource's first replica), in the resource's own pool (`--pool`
overrides). Use the same `--node` for `list`, `restore` and
`delete`. On a thin volume it is a thin snapshot; on a thick one it reserves a
copy-on-write area of `--size` (default `1G`). A ZFS resource needs
`--storage-type zfs` on each of these commands.

`restore` is refused while the resource is Primary on any node, so unmount and
demote it first. Stop a gateway with `gateway stop`. For an `ha create`
resource, run `ha delete` before the unmount, or drbd-reactor promotes it
again. The restore takes the resource down on every node and merges the
snapshot back on its node. Every other replica then resyncs that volume from
it, and until the resync finishes there is only one complete copy.

**Snapshots of the whole resource.** `snapshot create` snapshots one node.
A replicated snapshot covers every volume on every diskful replica at once.
I/O is suspended on all of them for the moment it takes. A systemd timer on
each node resumes I/O after a minute, whatever happens to the controller:

```bash
haify resource snapshot replicated create   --resource db --name before-upgrade
haify resource snapshot replicated list     --resource db
haify resource snapshot replicated rollback --resource db --name before-upgrade
haify resource snapshot replicated delete   --resource db --name before-upgrade
```

Each copy carries its replica's DRBD metadata, so a rollback restores every
replica together and resyncs nothing. A rollback needs the resource to be
Secondary everywhere. The backing snapshots are named `<backing>_snap_<name>`
(on ZFS, `<dataset>@snap_<name>`). The Proxmox plugin takes VM snapshots this
way. A replicated snapshot refuses an encrypted resource: snapshot its volumes
one by one instead.

**Exact sizes and renaming.** `resource create --size 10737418752 --exact-size`
makes the device exactly that many bytes (rounded up to a 512-byte sector)
instead of whole GiB; the backing volume is still allocated in GiB and the
DRBD device capped. A resize keeps it exact. `haify resource rename <old> <new>`
renames a resource that is not Primary anywhere and that no HA config,
gateway, schedule, backup or snapshot refers to. WAN-replicated and encrypted
resources cannot be renamed.

**Scheduled snapshots** with grandfather-father-son retention:

```bash
haify resource snapshot schedule create --resource db \
    --cron "0 * * * *" \
    --keep-hourly 24 --keep-daily 7 --keep-weekly 4 --keep-monthly 6

haify resource snapshot schedule list
haify resource snapshot schedule delete --resource db
```

A resource has at most one schedule. Running `create` on a resource that
already has one replaces it (`--disabled` saves it without running it). Each run snapshots
every volume of the resource on every diskful node, named
`<backing-volume>_sched_<UTC timestamp>`.

The cron field is standard 5-field syntax. Retention is applied after each run,
and only to that node's scheduled snapshots of that volume. Manual snapshots
are never touched. Each tier keeps the newest snapshot in each of its most recent
periods: `--keep-hourly 24` keeps one per hour for the last 24 hours that have
one, `--keep-daily 7` one per day, and so on through `--keep-yearly`. A
snapshot any tier keeps survives. A policy of all zeros keeps everything.
Deleting a schedule keeps the snapshots it already made.

Retention counts snapshots; it does not look at the pool. On a thin pool each
snapshot holds the blocks written since it was taken, so a busy volume's
history can fill the pool while staying inside its policy. A full pool makes
the replica's own writes fail. So after retention, if the pool is still past
85% (data or metadata), the scheduler removes that volume's oldest scheduled
snapshots on that node, one at a time, until the pool is below 85%. It always
keeps the newest two. It logs each one it removes and raises a `pool.snapshots_removed`
event.

**Locked snapshots.** The near-full rule removes the *oldest* snapshots. A
volume being encrypted by ransomware rewrites every block, so it fills a thin
pool fast, and the oldest snapshots are the clean ones from before the attack. A schedule can lock what it takes:

```bash
haify resource snapshot schedule create --resource db \
    --cron "0 * * * *" --keep-hourly 24 --keep-daily 7 --lock-days 14
```

Until a scheduled snapshot is `--lock-days` old (counted from the time in its
name), Haify does not delete it. That covers retention, the near-full rule,
`snapshot delete`, and a ZFS `restore` that would roll back past it. While
any snapshot of the resource is locked, the schedule cannot be deleted, the
resource and its volumes cannot be deleted, a replica cannot be removed (except
with `--lost`) or moved to another node, and the lock can be raised but not
lowered. Replacing the schedule without
`--lock-days` keeps its lock. Restoring a locked LVM thin snapshot merges it
and takes it again under the same name, so its lock is unchanged; a locked
thick snapshot cannot be restored until its lock passes. The limit is 365 days.

On ZFS, each locked snapshot also carries a `haify-lock` hold, so `zfs destroy`
on the node (a cleanup script, a `zfs destroy -r` of the dataset) fails too
until Haify releases the hold when the lock has passed. Root can `zfs release`
it, so the hold guards against mistakes, not against root.

Locks use the time the controller has counted since it started, not the
system clock. Moving the clock forward (a `date -s`, a spoofed NTP answer) to
end the locks early has no effect, and raises a
`controller.clock_jumped` event.

The cost is space: size the pool for `--lock-days` of change. If a pool is past
the near-full line and only locked snapshots are left, nothing is removed. A
critical `pool.snapshots_locked` event is raised, and if the pool fills, that
replica's writes fail. The lock also binds only Haify and its API. Root on a
storage node can still `lvremove` anything. To protect against root, back up to
a target with S3 Object Lock (see [Backups](#9-backups--the-only-copy-that-survives-losing-the-cluster)).

**Freezing a schedule.** A frozen schedule keeps taking snapshots but removes
none, either by retention or to relieve a full pool. Until the freeze ends,
every scheduled snapshot of the resource is locked: through Haify, neither the
snapshots nor the schedule nor the resource can be deleted. A freeze can be
extended but not shortened. Only `unfreeze` ends it early, and that needs a
second person under [two-person approval](#17-access-control).

```bash
haify resource snapshot schedule freeze --resource db --hours 72 --reason "investigating"
haify resource snapshot schedule unfreeze --resource db
```

**Write anomalies.** Ransomware that encrypts a volume rewrites it as fast as
the disks allow. With `[alert] enabled`, the health poll reads how much DRBD
wrote to the answering node's disk (also exported as
`haify_drbd_written_bytes`). Each resource learns its usual write rate, overall
and by hour of the week; resync traffic is not counted. After 30 normal polls
it has learned enough. From then on, if the write rate is at least
`[alert.write_anomaly] factor` times the usual (default 5) and at least
`min_mbps` (default 20 MB/s) on two polls in a row, Haify raises a critical
`resource.write_anomaly`, freezes the resource's schedule for `freeze_hours`
(default a week) and takes a snapshot right away. The event resolves after
three normal polls. The freeze stays until it ends or is lifted.

It reports a rate, not an attack: a bulk import, a reindex or a restore look
the same, and `unfreeze` is there for those cases. Encryption slowed down to
look normal does not trip it. The resource needs a snapshot schedule for there
to be anything to freeze.

Schedules live in the controller database and survive a restart or a failover;
the node that becomes active picks them up.

---

## 9. Backups — the only copy that survives losing the cluster

A backup is a compressed image of a resource, stored outside the cluster. Targets are S3-compatible object stores, SMB shares, or WebDAV.
The node that reads a backup uses `rclone` to talk to the target, so install it
on every storage node (`apt install rclone`). A node without it is refused
before anything is snapshotted.

**Define a target.** The secret is never a command-line flag, because it would
land in your shell history:

```bash
export HAIFY_BACKUP_SECRET='...'
haify backup target add --name offsite --kind s3 \
    --bucket haify-backups --endpoint https://s3.example.com --user AKIAEXAMPLE

haify backup target add --name nas --kind smb \
    --host nas.example.com --share backups --user backupuser --secret-file -

haify backup target list        # secrets are never returned
```

An SMB host may carry a non-standard port (`--host nas.example.com:4450`).

**Take and restore a backup:**

```bash
haify backup create --resource db --target offsite
haify backup list
haify backup restore <backup-id> --node node1
haify backup delete <backup-id>
```

**Incremental after the first.** The first backup of a resource to a target is
a full image. On thin pools every later one carries only the blocks that changed
since the previous one: the thin pool's own metadata says which (`thin_delta`),
so nothing is read or hashed to find out. A 1 GiB volume, for example, takes
20 s and 161 MB as a full backup, and 4.5 s and 26 MB for the next one after
26 MB of changes.

```bash
haify backup create --resource db --target offsite          # incremental when it can be
haify backup create --resource db --target offsite --full   # start a new chain
```

A backup is full instead, and the controller log says why, when:

- there is no earlier completed backup on that target;
- the backup is read on a different node than the last one;
- a volume was added, resized or moved to another backing volume since then;
- the base snapshot is gone;
- the volume is thick; or
- 30 incrementals already follow the last full one.

What an incremental costs:

- **A base snapshot stays on the node.** The snapshot of the last backup is kept
  (named `<volume>_bk_<time>`) and holds whatever the volume has overwritten
  since, the same space a scheduled snapshot of that age would hold. It moves
  forward with each backup and goes when its backup is deleted.
- **A chain restores as a whole.** Restoring an incremental writes its full
  backup and then every incremental after it, oldest first. None of them can be
  deleted while a later one exists; delete newest first.

**Which replica is read.** Without `--node`, Haify picks the node holding the
last backup's base snapshot, if it is UpToDate. Otherwise it picks an UpToDate
Secondary, so the cost of the snapshot lands away from the workload. It picks
the Primary only when no Secondary is UpToDate. It never picks the DR replica
of a WAN resource, which may be behind. A thick volume's snapshot reserves free
space in its volume group up front: 20% of the volume, at least 256 MiB. A
replica whose group lacks that room is passed over for one that has it.

**Scheduled backups.** Each resource can have one schedule per target. The
active controller runs it, and it carries on after a controller failover. Each
backup records the schedule that took it (`schedule=` in `backup list`, and in
its manifest, so the link survives an import). Retention counts only the
schedule's own backups, as snapshot schedules do, and never deletes a backup
taken by hand. One more rule: an incremental that is kept also keeps every
backup back to its full one, whoever took it. So a daily schedule that keeps 7
can hold up to a month of backups before the chain restarts with a full one. A
schedule must keep at least one tier; `--disabled` saves it without running
it.

```bash
haify backup schedule create --resource db --target offsite \
    --cron "30 18 * * *" --keep-daily 7 --keep-weekly 4 --keep-monthly 3
haify backup schedule list          # last run, the backup it made or why it failed, next run
haify backup schedule run db@offsite   # run now, retention included, and wait
haify backup schedule delete db@offsite   # its backups stay
```

The cron is in the controller's time zone, usually UTC on a server. If a run
is still going when the next one is due, that next run is skipped. A failed run raises a
`backup.failed` event, delivered like any alert (it needs `[alert] enabled`),
and resolves with the next completed run. Failed records older than the newest
completed backup are removed by the schedule. Backup schedules run on the
snapshot scheduler, so `[schedule] enabled = false` in `controller.toml` stops
them too.

**When the controller's database is gone, or the backups belong to another
cluster.** Each backup leaves a `manifest.json` next to its images on the
target. `backup import` reads them and rebuilds the records, incremental chains
included:

```bash
haify node register ...                       # a rebuilt cluster: nodes first
haify backup target add --name offsite ...    # same bucket and --prefix as before
haify backup import --target offsite
haify resource create --name db --size 20G ...   # at least as large as the backup
haify backup restore <backup-id> --resource db
```

Importing twice does not create duplicate records. A backup whose images are
not all on the target is skipped, and the import names it. The next backup after an import is full unless the
newest backup's base snapshot is still on that node of this cluster. The
images are raw block data, so a backup restores onto another architecture or
pool type: a chain taken from an arm64 node's thin pool restores onto a thick
LVM volume on x86.

**Immutable backups: S3 Object Lock.** Whoever controls the cluster (with a
stolen Haify token, or root on a storage node) can delete ordinary backups, and
ransomware does that first. A locked target stores every object under S3
Object Lock. Until the lock expires, neither Haify nor anyone using Haify's
keys can delete or overwrite it:

```bash
haify backup target add --name vault --kind s3 --bucket haify-vault \
    --endpoint https://s3.example.com --user AKIAEXAMPLE \
    --lock-mode compliance --lock-days 30
```

- **The bucket** must be created with Object Lock enabled. This turns on
  versioning, and on most servers it cannot be added later. AWS S3, MinIO, Ceph
  RGW, Backblaze B2 and Wasabi support it. Some S3-compatible servers accept
  the lock headers and ignore them, so every backup reads each object's lock
  back and is **failed** if the server stored it unlocked.
- **rclone 1.74.0 or later** is needed on the nodes. Older versions upload
  objects unlocked, so with them the backup is refused before anything is
  snapshotted.
- **Modes.** `governance` can be lifted by a principal holding
  `s3:BypassGovernanceRetention`; `compliance` by no one, the bucket owner
  included, until it expires.
- **Chains.** A lock cannot be extended once written, and an incremental needs
  every backup back to its full one. So every backup in a chain is locked until
  chain start + `--full-every-days` (default 7) + `--lock-days`. A backup taken
  more than `--full-every-days` after its chain started is a full one. Each
  backup is locked for at least `--lock-days`. Plan storage for up to
  `--full-every-days` + `--lock-days` of backups.
- **Deleting and retention.** `backup delete` refuses a locked backup;
  `--force` drops only the record and leaves the objects. A schedule's
  retention leaves locked backups for a later run. After the lock has expired,
  a delete only puts a delete marker over the object. To get the space back,
  add a lifecycle rule that expires noncurrent versions (say 7 days after they
  become noncurrent).
- **Restores read what was written.** A restore reads a locked backup as it
  was one hour after the backup finished (`--s3-version-at`). A version written
  over it later, or a delete marker on top, does not change what the restore
  sees.
- **After the backups were deleted anyway.** On a versioned bucket a delete
  only hides an object. `backup import --as-of <RFC3339>` reads the target as
  it was at that time and records what it finds. Restores then read those
  versions:

  ```bash
  haify backup import --target vault --as-of 2026-10-01T00:00:00Z
  ```

**Object Lock holds only if the keys are restricted.** Haify's S3 credentials are on the
storage nodes, so assume an attacker has them. They need only:

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Action": ["s3:PutObject", "s3:PutObjectRetention", "s3:GetObject",
               "s3:GetObjectVersion", "s3:GetObjectRetention", "s3:DeleteObject",
               "s3:ListBucket", "s3:ListBucketVersions"],
    "Resource": ["arn:aws:s3:::haify-vault", "arn:aws:s3:::haify-vault/*"]
  }]
}
```

The keys must **not** hold `s3:BypassGovernanceRetention`, `s3:DeleteObjectVersion`,
`s3:PutBucketObjectLockConfiguration`, `s3:PutBucketVersioning` or
`s3:PutLifecycleConfiguration`: any one of them lets the key holder get around the lock.
`s3:DeleteObject` only adds delete markers on a versioned bucket. What each
layer stops:

| Attacker holds | Object Lock (compliance) | Object Lock (governance) |
| --- | --- | --- |
| a Haify token or admin account | stopped | stopped |
| root on a storage node (and so Haify's S3 keys) | stopped | stopped, if the keys lack the bypass permission |
| root on the controller | stopped | same as above |
| the object store account itself | stopped | not stopped |

Know these limits before you build a backup policy:

- Only a backup listed as `completed` can be restored. `running` means it is
  still uploading. `failed` means it is not a usable copy, and its objects were
  removed. Haify checks completion against the size the target reports, not
  just the exit code. When the controller restarts, it marks any `running`
  backup `failed`.
- **LVM-backed volumes only.** A resource on a ZFS pool is refused.
- **Restore overwrites from byte zero.** It refuses when the resource is Primary
  anywhere, when it has a drbd-reactor promoter (a gateway, an HA
  config or an app), or when the destination is smaller than the image. It promotes the resource on the node it writes through, and
  demotes it again when done.
- A target that backups still reference cannot be deleted without `--force`;
  `backup delete --force` drops a record whose objects can no longer be
  removed.

### Copying a resource to another cluster

`resource ship` copies a resource straight into another Haify cluster, with no
backup target in between: to seed a second site, move a workload, or keep a
copy somewhere the first cluster's failure cannot reach.

```bash
haify resource ship db --to 10.1.0.10:3374 --token-file ~/.haify/dr-token
haify resource ship db --to 10.1.0.10:3374 --as db-copy --pool vg0 --replicas 2
haify pool jobs                     # progress, then done or failed
```

Every volume is snapshotted at the same moment on one replica, so the copy is
crash-consistent. The other cluster creates the resource. Each volume then
streams from a node here to a node there over TLS, with keys made for that one
transfer. Both ends checksum the bytes. If the checksums differ, or anything
fails, the copy on the other cluster is deleted. The resource here keeps serving throughout.

- The other cluster's nodes must accept connections from this cluster's nodes,
  on a port chosen for each transfer.
- The nodes on both sides need a current `haify` (`/usr/local/bin/haify`);
  `scripts/deploy-all.sh` installs it.
- LVM-backed, unencrypted resources only. Encrypted resources hold ciphertext
  that the other cluster's keys cannot read; back them up and restore them
  there instead.
- The name must be free on the other cluster. A job interrupted by a
  controller restart is marked failed; delete the partial copy there and ship
  again.

---

## 10. Gateways — exporting to clients

A gateway turns a resource into something a non-Haify machine can mount: NFS,
SMB, iSCSI or NVMe-oF. It is a drbd-reactor promoter config, so it fails over with
the resource while clients keep talking to a floating service IP.

```bash
# NFS
haify gateway nfs create --resource data --service-ip 192.0.2.200/24 \
    --export-path /data --allowed-ips 192.0.2.0/24

# iSCSI
haify gateway iscsi create --resource blk \
    --iqn iqn.2026-01.com.example:haify.blk --service-ip 192.0.2.201/24

# NVMe-oF
haify gateway nvme create --resource fast \
    --nqn nqn.2026-01.com.example:haify.fast --service-ip 192.0.2.202/24
```

Creating a gateway does four things. It checks that the OCF agents and tools
the chain needs are installed on the resource's diskful nodes (see below). It
formats the state volume (and, for NFS, the exported volume) if it has no
filesystem. It writes the promoter config
(`/etc/drbd-reactor.d/haify-<type>-<resource>.toml`) to the resource's diskful
nodes only. Then it reloads drbd-reactor there. A gateway needs a small
cluster-private state volume (volume 0) next to the exported data. A
single-volume resource gets one added automatically (`[gateway]
auto_state_volume`, default on, `state_volume_size_gb` default 1).

Managing a live gateway:

```bash
haify gateway list                       # or: gateway nfs|iscsi|nvme list; read from the nodes
haify gateway get --resource data
haify gateway status --resource data
haify gateway stop  --resource data      # demote and stop, config kept
haify gateway start --resource data
haify gateway delete --resource data
```

Per-protocol details:

```bash
haify gateway nfs export add|list|remove ...      # extra exports on an NFS gateway
haify gateway nfs export quota --resource data --path team-a --size 500G
                                                    # cap an export directory (0 removes)
haify gateway nfs mount --resource data --target /mnt/data --mkdir --sudo
                                                    # mount it on this machine
haify gateway iscsi lun add|list|remove ...       # LUNs
haify gateway iscsi chap get|set ...              # one-way CHAP only; mutual CHAP is not supported
haify gateway iscsi initiator add|list|remove ... # initiator allow-list
haify gateway nvme namespace add|list|remove ...  # namespaces
haify gateway nvme host add|list|remove ...       # host allow-list
```

An export directory's quota is an ext4 project quota on the gateway's
filesystem, so one export cannot fill the volume for the others. New gateways
support it: the filesystem is made with the project feature and mounted with
`prjquota`. For an older gateway, add the feature while the filesystem is
unmounted (`tune2fs -O quota,project <device>`) and recreate the gateway. The gateway nodes need the `quota` package (`setquota`).

Gateways cannot limit a client's IOPS or bandwidth. Their I/O is done by kernel
threads in the root cgroup, which has no I/O limits, and neither LIO nor nvmet
has per-LUN limits. If you need a limit, shape traffic to the service IP with
`tc` on the gateway nodes. DRBD's `c-max-rate` limits resync traffic only; it is not QoS.

These edits read the gateway's promoter config from the resource's diskful
nodes (not from the machine the controller runs on) and write the result back
to all of them. If the nodes hold different copies, Haify uses the copy most
of them hold; a tie goes to the first host in sorted order. The controller log
names the nodes that differ, and the write makes all copies the same again.

**When an edit takes effect.**

- *Stopped gateway*: the edit goes into the `.toml.disabled` copy and takes
  effect at the next `gateway start`. Nothing on the nodes is touched and the
  gateway stays stopped.
- *Running gateway*: the edit takes effect **immediately on the node that runs
  it**, without restarting the gateway. The other diskful nodes are ready to
  run the edited gateway on failover. Per edit:

  | Edit | Applied on the running node by | Clients |
  | ---- | ------------------------------ | ------- |
  | `iscsi initiator add` | `targetcli … acls create <iqn> add_mapped_luns=true` (all LUNs mapped) | new initiator can log in at once |
  | `iscsi initiator remove` | `targetcli … acls delete <iqn>` | that initiator's sessions are closed; others unaffected |
  | `iscsi chap set` | `targetcli … set auth` on every ACL (or the TPG when all initiators are allowed) | applies at the next login; existing sessions stay |
  | `iscsi lun add` / `remove` | starts / stops just that LUN's `ocf.rs@lu<N>_<res>` unit | other LUNs unaffected |
  | `nvme host add` / `remove` | links / unlinks the host in the subsystem's `allowed_hosts` (configfs) | a removed host keeps an existing connection until it reconnects |
  | `nvme namespace add` / `remove` | starts / stops just that namespace's unit | other namespaces unaffected |
  | `nfs export add` / `remove` | starts / stops just that export's `exportfs` unit | other exports unaffected |

  Removing the last initiator or host makes the target accept every initiator
  again, also live: an empty allow-list means "allow all". A change that cannot
  be applied without restarting the gateway (for example, removing CHAP) is
  refused before anything is written. Stop the gateway, make the edit, and
  start it again.
- *Running nowhere* (enabled, but no node has it up): the config is written and
  drbd-reactor is reloaded, which starts the gateway with the edit.

Why the running node is handled this way: drbd-reactor cannot apply a changed
promoter config to a running gateway. On `systemctl reload drbd-reactor` it
stops the old promoter and starts a new one. Haify writes its promoters with
`stop-services-on-exit = true`, so a reload with a changed config stops the
whole gateway on that node, and every node then races to promote it again.

So Haify never reloads drbd-reactor for an edit on the node running the
gateway. That node keeps the `.toml` its drbd-reactor loaded, and gets the
edited config as `haify-<type>-<res>.toml.pending`, which drbd-reactor ignores.
Haify rewrites the unit drop-ins in `/run/systemd/system` to the edited chain,
so a unit restart or a later failback uses it. A drop-in on
`drbd-reactor.service` (`50-haify-pending-gateway-config.conf`) moves the
`.pending` file into place before drbd-reactor next starts. The next edit made
while another node runs the gateway also replaces it. **Do not run
`systemctl reload drbd-reactor` on the running node by hand to "pick up" a
gateway edit**: it restarts the gateway. If the live step fails, the command
says so. The config is then saved everywhere and a failover uses it. To apply
it now, run `haify gateway stop` then `haify gateway start` (this interrupts
clients).

Haify checks target and initiator IQNs (at `iscsi create` and `initiator add`)
and host NQNs before anything is written, using the rules LIO and nvmet
apply. An iSCSI name must be `iqn.<yyyy-mm>.<domain with at
least two labels>[:<name>]` without spaces or `_` (`iqn.2026-10.test:probe` is
refused, `iqn.2026-10.lab.test:probe` is accepted), or `eui.` + 16 hex digits,
or `naa.` + 16 hex digits starting with 1, 2 or 5. A host NQN must be
`nqn.<yyyy-mm>.<reversed domain>[:<name>]` or
`nqn.2014-08.org.nvmexpress:uuid:<uuid>`. Without this check, a name LIO
rejects would be accepted here and then fail the target's next start, leaving
the gateway down.

`iscsi create` also takes `--allowed-initiators`, `--username`/`--password` for
CHAP, and `--implementation lio` (the default and the only one supported; `tgt`
and `iet` are refused). `nvme create` takes `--transport tcp` (default) or
`rdma`; `nvmet` and the transport's module (`nvmet-tcp` or `nvmet-rdma`) are
loaded on the nodes at creation and added to
`/etc/modules-load.d/nvmet.conf` so they load at boot. `rdma` also requires an RDMA device under
`/sys/class/infiniband`.

**What clients need.** Windows has a built-in iSCSI initiator and a limited
NFSv3 client. macOS has a built-in NFS client and no iSCSI initiator. Both
have SMB built in (see [SMB](#smb-gateways) below). Pick the protocol by what
the client can mount.

**What creation checks.** Every gateway needs the OCF agents from
`resource-agents-extra` (Ubuntu) or `resource-agents` (Debian, EL). Each type
also needs:

| Gateway | Checked | Install |
| ------- | ------- | ------- |
| NFS | `rpc.nfsd`, `exportfs` | `nfs-kernel-server` (Debian/Ubuntu), `nfs-utils` (EL) |
| iSCSI | `targetcli` | `targetcli-fb` (Debian/Ubuntu), `targetcli` (EL) |
| NVMe-oF | kernel modules, loaded at creation | `linux-modules-extra` on Ubuntu cloud kernels |
| SMB | `smbd`, `smbpasswd`, `smbcontrol`, `pdbedit`; the distribution's `smbd` not running | `samba` |

If anything is missing, creation fails and lists what is missing and what to
install. No config is written.

### SMB gateways

A workgroup SMB server per resource: standalone Samba, local users, no domain.

```bash
haify gateway smb create --resource files --service-ip 192.0.2.210/24 [--workgroup OFFICE]
haify gateway smb user set alice --resource files          # prompts; or --password-stdin
haify gateway smb share add public --resource files --path public
haify gateway smb share list --resource files
haify gateway smb user list --resource files
```

Clients connect to `\\192.0.2.210\files` (the first share is named after the
resource unless `--share` says otherwise, and covers the whole data volume).

**Shares inside shares.** Samba checks `--valid-users` per share, not per
directory. If a share restricted to alice sits inside a share that lets bob in,
bob can reach it through the outer share. `share add` refuses that. The first
share covers the whole volume and lets in every user unless it was created
with `--valid-users`. So to give a group a share of its own, either create the
gateway with `--valid-users` on the first share, or remove the first share and
add shares side by side:

```bash
haify gateway smb share remove files --resource files
haify gateway smb share add public   --resource files --path public
haify gateway smb share add projects --resource files --path projects --valid-users alice,bob
```

`share add` checks only the share being added. Shares that were nested before
this check existed are left as they are, so compare the paths and users in
`share list`.

How it is built, and what that means:

- **Everything Samba keeps is on the gateway's state volume**: `smb.conf`,
  the shares (`shares.conf`), the user database and secrets, the lock and
  state directories. So after a failover the next node serves the same users
  and shares. Share and user changes go to the node serving the gateway and
  apply immediately (`smbcontrol reload-config`). A stopped gateway refuses
  them until it is started.
- **Sessions do not survive a failover.** Without CTDB, Samba has no
  transparent failover. Clients reconnect to the service IP (Windows and macOS
  do so on their own). An application that holds a file open during the switch
  sees the same error as after a server restart. Writes acknowledged
  before the switch are on both replicas (protocol C).
- **One smbd per gateway**, `haify-smbd@<resource>.service`, bound to the service
  IP only, so several SMB gateways can run on one node. The distribution's own
  `smbd` must not run on gateway nodes, because it holds port 445 on every
  address. Creation refuses while it runs; stop it with
  `systemctl disable --now smbd nmbd`.
- **Files are owned by one account, `haify-smb`,** on every share. Access is set per
  share (`--valid-users`, `--read-only`), not by per-user Unix permissions. Each
  SMB user also gets a local account, because Samba requires one. The unit
  creates it with the same uid on every node before smbd starts. A node where
  that uid is taken refuses to start the gateway and says why.
- Users cannot be added through MCP, because a password typed into a model
  conversation is stored in its transcript. Use the CLI or the web UI. The
  password crosses the API the way a CHAP secret does, so use `[tls]`.
- Removing a share keeps its data; deleting the gateway keeps the volume.


---

## 11. High availability

`ha create` declares what should run wherever the resource is Primary.
drbd-reactor then picks a node, mounts the filesystem, starts the services,
raises the VIP, and moves all of it if that node dies. The VIP comes up last,
so clients only reach a node whose services are up, and it is the first thing
taken down. `ha create` sets `net.ipv4.ip_nonlocal_bind` on the nodes, so a
service that binds to the VIP itself can start before the VIP exists. (Older
configs put the VIP ahead of the services, and keep that order until they are
recreated.)

```bash
haify ha create db \
    --vip 192.0.2.210/24 \
    --mount /var/lib/postgresql \
    --fstype ext4 \
    --services postgresql.service

haify ha list
haify ha status db
haify ha delete db
```

`ha status` shows the chain on the node running it, one line per unit. Each
line gives what the unit is (promote, mount, service IP, NFS/iSCSI/NVMe-oF/SMB,
database, VM, service), its state, since when, and how often systemd restarted
it. Next it lists every other node holding the promoter and whether that node
could take over now. If it could not, it gives the reason: a unit or OCF agent
not installed there, drbd-reactor not running, or a local disk that is not
UpToDate. Last come the services outside the chain that have processes on the
chain's mounts, and a count of login sessions doing the same. On a failover,
these hold up the unmount. It works for gateway and app resources too.

`ha create` makes a filesystem of `--fstype` on the device when it finds none,
and refuses when a `--services` unit is missing on any of the resource's nodes.

`--vm <guest>` runs a libvirt guest instead of services. Define the guest
under that name, with the same UUID, on every diskful replica, with autostart
off and its disks on `/dev/drbd/by-res/<resource>/<volume>`. When the node
running it fails, another replica restarts it. When you start, stop or
live-migrate guests by hand with `virsh`, the libvirt hook handles the DRBD
side. Both are described in `deploy/libvirt/README.md`.

```bash
haify ha create web1 --vm web1
```

`ha create` and the gateways put a drbd-reactor promoter on the resource. They
refuse a resource whose Primary is decided by something else, because the
promoter would fight it for the role. That means a Proxmox VM disk (labelled `haify.pve/managed-by=pve`
by the plugin, or named `pve-<vmid>-...` from before it labelled them), a CSI
volume (`haify.csi/managed-by=csi`), an OpenStack Cinder volume, an XCP-ng
disk, and a resource with diskless clients.

To move it deliberately, for maintenance or to test that failover works:

```bash
haify ha evict db
```

**Test your failover before you need it.** Evicting moves the resource
gracefully. Pulling the power on the active node shows what a real failure
does.

### The controller's own HA

The controller is a single process. Self-HA puts it on the same machinery as
everything else: its database lives on a replicated resource, and a VIP follows
whichever node is running it.

```bash
haify ha self enable --vip 192.0.2.250/24 --pool thin-pool
haify ha self status
haify ha self disable --node node1     # back to a plain service on node1
```

`enable` creates the `haify-meta` resource (1 GB on DRBD port 7999 by default;
`--size`, `--port`, `--nodes` change that). It copies the running controller
binary, its config and its systemd unit to the other nodes, and hands the
controller to drbd-reactor. The binary goes to the path the unit's `ExecStart`
names. A node with a different architecture gets `haify-controller-<goarch>`
from the same directory as the running binary instead. If that file is
missing, `enable` refuses that node before changing anything. The controller
restarts during the handoff, so the command's connection drops; check the
result with `ha self status` against the VIP. `disable` copies the database
back to the named node and leaves `haify-meta` in place for you to delete. `haify ha evict haify-meta` moves the controller to
another node.

Point clients at the VIP afterwards. Other services can move with the
controller: `[self_ha] extra_services = ["haify-ai.service"]` in `controller.toml`
starts and stops the AI Copilot with it.

---

## 12. Database and object-store applications

`haify app` runs one database instance (PostgreSQL, optionally with pgvector;
MySQL or MariaDB; or Redis) or an S3-compatible object store (RustFS) on a
resource's DRBD volume, the way `ha create`
runs a service and a gateway runs an export. drbd-reactor mounts the volume,
starts the database and raises a service IP on the node where the resource is
Primary; when that node fails, another replica does the same. It is storage
failover, not database replication: there is one database, its files are on
DRBD, and clients find it at the service IP wherever it runs.

```bash
# A resource with two diskful replicas (give it quorum: a third replica or a tiebreaker)
haify resource create --name orders --port 7010 --size 20G --nodes node1,node2 --pool pool0

haify app create --name orders --engine postgres --service-ip 192.0.2.220/24
haify app create --name embeddings --engine postgres --vector --resource emb --service-ip 192.0.2.221/24
haify app create --name sessions --engine redis --port 6380 --service-ip 192.0.2.222/24
haify app create --name shop --engine mysql --service-ip 192.0.2.223/24
haify app create --name objects --engine rustfs --service-ip 192.0.2.224/24

haify app list
haify app status orders
haify app failover orders                            # planned switchover
haify app snapshot orders --snapshot before-upgrade
haify app delete orders                              # stops it; the resource and data stay
haify app delete orders --delete-data --yes          # and deletes the resource
```

`--resource` defaults to the app's name; `--port` to 5432, 3306, 6379 or 9000.
`create` prints the generated password once.

### What `create` does

1. **Refuses** before touching any node when the resource does not exist, has
   fewer than two diskful replicas, or already has a promoter: an HA config, a
   gateway or another app (on the nodes or in the controller's records). One
   resource runs one promoter; `ha create` and the gateways likewise refuse a
   resource that runs an app.
2. **Checks every diskful replica**: the OCF `Filesystem` and `IPaddr2` agents,
   the engine's programs, `fsfreeze`, `runuser`, `setpriv` and `systemd-run`, and
   that the port is free. It then compares the replicas: the engine must be
   installed in the same place and the same version (PostgreSQL major; MySQL,
   MariaDB and Redis major.minor), and **the daemon user must have the same uid
   and gid on every node**. The data files belong to numeric ids, so on a node
   where `postgres` has another uid the database would not start after a
   failover. Every problem is reported at once, naming the nodes and ids.
3. **Initializes once** on the node already Primary, or the first replica,
   promoted for the purpose: formats the volume ext4 if `blkid` finds nothing
   on it (and refuses one carrying another filesystem or a partition table),
   mounts it, runs `initdb` / `mariadb-install-db` / `mysqld
   --initialize-insecure` or writes `redis.conf`, sets the password, starts the
   database once without network access to prove it runs, stops it and
   unmounts.
4. **Hands it to drbd-reactor**: demotes the resource, writes
   `haify-app-<name>.service` and the promoter `/etc/drbd-reactor.d/haify-app-<name>.toml`
   to every diskful replica, and reloads drbd-reactor, which starts it. The
   chain starts the mount, then the database, and the service IP **last**. The
   database unit counts as started only once the database answers its health
   probe (it waits up to 300 seconds, crash recovery included). So clients
   reach a node only when the database is up there, and on the way out they
   lose the address first.

A volume that already holds the same engine's app (one deleted without
`--delete-data`) is kept as it is, data and credentials; `create` says so and
prints no password. Anything else on the volume is refused.

| Engine | Install on every diskful node | Runs as | Health probe | Freeze for a snapshot |
| ------ | ----------------------------- | ------- | ------------ | --------------------- |
| `postgres` | `postgresql` (same major); with `--vector` also `postgresql-<major>-pgvector` (Debian/Ubuntu) or `pgvector_<major>` (PGDG) | `postgres` | `pg_isready` on the socket | `CHECKPOINT`, then `fsfreeze` |
| `mysql` | `mariadb-server` or `mysql-server` (same version) | `mysql` | `mysqladmin ping` | `FLUSH TABLES WITH READ LOCK` held while `fsfreeze` runs |
| `redis` | `redis-server` (Debian/Ubuntu) or `redis` (EL) | `redis` | `redis-cli PING` | `BGSAVE`, wait, then `fsfreeze` |

The OCF agents come from `resource-agents-extra` (Ubuntu) or
`resource-agents` (Debian, EL). The distributions start their own instance of the
engine on install: stop and disable it (`postgresql`, `mariadb`/`mysql`,
`redis-server`), or `create` refuses the busy port.

### Where things live

Everything the database needs to start lives on the volume, so it fails over
with the data:

```
/var/lib/haify-app/<name>/          the volume, mounted on the node running the app
  data/                           the data directory
  conf/                           postgresql.haify.conf + pg_hba.conf, my.cnf, or redis.conf
  haify/password                    the generated password (root only, 0600)
  haify/client.cnf                  mysql: credentials for the health probe and the snapshot lock
/etc/systemd/system/haify-app-<name>.service   on every replica; never enable or start it by hand
/etc/drbd-reactor.d/haify-app-<name>.toml      the promoter, on every replica
/run/haify-app-<name>/              sockets and pid files, while it runs
```

Tune the engine in its config on the volume, on the node running it. For
PostgreSQL, edit `data/postgresql.conf`; the few settings Haify relies on (port,
listen address, socket directory, `hba_file`) are in `conf/postgresql.haify.conf`,
included last. Apply a change the engine's own way (`SELECT pg_reload_conf()`,
`SET GLOBAL`, `CONFIG SET`) or with `haify app failover`, which restarts it on
another replica. Do not restart the unit by hand: drbd-reactor treats a stopped
unit as a failure and fails the app over.

**Credentials.** PostgreSQL's `postgres` superuser, MySQL's `root` (local and
`'root'@'%'`) and Redis's `requirepass` get a generated 32-character password.
`haify app create` prints it once; afterwards it exists only in
`/var/lib/haify-app/<name>/haify/password`, readable by root on the node running the
app. It never appears in a command line or a log: it reaches the node over the
SSH stream as a 0600 file. The MCP tool `haify_app_create` does not return it.

### S3 object store (RustFS)

`--engine rustfs` runs [RustFS](https://github.com/rustfs/rustfs) on the volume
as a single-node, single-disk store. Its redundancy comes from the resource's
DRBD replicas, not from erasure coding. Use it for an S3 endpoint that has to
stay up (application uploads, backups of other systems, artifacts), not for
capacity that grows past one volume: the store is as large as the resource and
as fast as the node serving it. To grow it, grow the volume with `haify
resource resize-volume`, then, on the node serving it, `resize2fs /dev/drbd/by-res/<resource>/0` (ext4
grows while mounted).

- **On every diskful replica:** the `rustfs` binary on the `PATH` in the same
  version (the musl build from the RustFS releases runs on any distribution),
  `curl`, and a system user `rustfs` with the same uid and gid everywhere:
  `useradd --system -u <uid> -U -M -s /usr/sbin/nologin rustfs`.
- **Credentials:** the access key is `admin`; the generated password is the
  secret key, printed once by `create` and kept as described above. RustFS reads
  both from files on the volume, owned by `rustfs`, so neither appears in a
  command line. Users, policies and buckets created through the S3 or admin API
  live in the data directory (`.rustfs.sys`) and fail over with the objects.
- **Endpoints:** S3 at `http://<service IP>:<port>`, path-style (set
  `force_path_style` / `s3_use_path_style` in the client); the web console at
  `http://<service IP>:<port+1>/rustfs/console/`. `create` checks that both
  ports are free. Traffic is plain HTTP: put a TLS proxy in front when clients
  are outside a trusted network.
- **Snapshots** freeze the filesystem for their few seconds, as for the other
  engines; objects being uploaded at that moment wait.

When the app moves to another node (`drbd-reactorctl evict`, or a failed
node), the service IP answers again within about 3 seconds, and objects
written before the move read back unchanged.

### Failover

drbd-reactor fails the app over when its node is lost, and when the database
itself dies: the unit is bound to the promoter's target and has no
`Restart=`, so a crash moves the app rather than restarting it in place. The
replica taking over promotes the resource, mounts it, starts the database and
raises the service IP.

- **Crash-consistent.** The new node starts from what was on disk, exactly as
  after a power cut, and the engine recovers (WAL replay, InnoDB redo, AOF).
- **RPO 0 with protocol C.** DRBD acknowledges a write only once it is on both
  replicas, so every transaction the database committed with an fsync is
  there: PostgreSQL by default, MySQL/MariaDB with
  `innodb_flush_log_at_trx_commit = 1` and `sync_binlog = 1` (set by `create`).
  Redis is configured with `appendfsync everysec` and can lose up to a second
  of acknowledged writes; set `appendfsync always` in its `redis.conf` for none.
- **Recovery time** is detection, promotion, the engine's crash recovery and the
  service IP, usually seconds to tens of seconds; a large uncheckpointed log
  takes longer.
- **Clients** lose their connections and must reconnect to the service IP; use
  a client or pool that retries.
- **Quorum** is what makes this safe. Give the resource three votes (a third
  replica or a diskless tiebreaker) so a node cut off from the others loses
  quorum and stops writing instead of running a second copy.

`haify app failover` is the planned version, through `drbd-reactorctl evict`; it
fails when no other replica took over. `haify node drain` and `haify ha evict
<resource>` move an app the same way.

A replica added later (`haify resource add-replica`) gets the unit and promoter
only if it passes the same checks against what the app was created with (engine
path, version, uid and gid). If it does not, `add-replica` fails and says what
differs; once that is fixed, `haify resource repair <resource>` places them. A
removed replica loses them.

### Snapshots

`haify app snapshot` freezes the database on its node (see the table), takes a
[replicated snapshot](#8-snapshots) of every volume on every replica, and thaws.
Before freezing anything, the node arms a transient systemd timer
(`haify-app-thaw-<name>`) that thaws the database after 60 seconds by itself, so a
controller that dies mid-snapshot cannot leave it frozen. Writes stall for the
seconds the snapshot takes; reads continue. An app that is not running is
snapshotted without a freeze.

To go back to a snapshot: `haify app delete <name>` (the data stays), `haify
resource snapshot replicated rollback --resource <res> --name <snap>`, then
`haify app create` again with the same name, engine and resource; it finds the
data and keeps it.

---

## 13. Cross-site replication (WAN DR)

WAN replication keeps an asynchronous replica at another site, over the public
internet, through a TCP proxy with mTLS. It is opt-in per resource because it
uses protocol A (asynchronous): the primary does not wait for the DR site.

A resync across the WAN compares SHA-256 checksums before sending a block
(`csums-alg sha256`), so blocks both sites already hold cross the link as a
checksum, not as data. Resync speed follows DRBD's dynamic controller; to cap
it for a group of resources, set it on their profile:
`haify resource profile set-options <profile> --drbd-options disk/c-max-rate=20M`.
WAN resources created before this default can get it with
`haify resource set-options <name> --drbd-options net/csums-alg=sha256`.

```bash
haify resource create --name db --size 100G --port 7000 \
    --nodes node1,node2 \
    --wan --dr-node dr1 --dr-endpoint dr.example.com
```

`--dr-endpoint` is an address or host name, without a port: the WAN port is
`--wan-port`, and 0 (the default) picks a random port above 3000.
`--wan-egress-address` pins the outbound side to one interface when the primary
has several.

A resource that is already running gets its DR replica in place, while it keeps
serving:

```bash
haify resource add-dr db --dr-node dr1 --dr-endpoint dr.example.com
```

The DR node joins over one `haify-proxy` leg per primary-site replica, with
protocol A and pull-ahead, and does not vote: quorum stays with the primary
site.

When the DR site's address changes, or the primary should dial out from
another interface:

```bash
haify wan set-endpoint db --dr-endpoint dr2.example.com
haify wan set-endpoint db --egress-address 203.0.113.20     # or --clear-egress
```

The tunnels are rebuilt on the new address at once. A new endpoint that does
not answer is refused and the old one kept, since the old tunnel was working. For a
DR site whose firewall is not open yet, `--skip-check` saves it anyway; run
`wan repair` once it answers. Renumbering the DR node with `node set-address`
moves an endpoint that was that node's address by itself.

DR is manual on purpose: an automatic cross-site promotion during a network
partition would produce two live copies:

```bash
haify resource dr-failover db          # prints what it will do
haify resource dr-failover db --yes    # force-promotes the DR node
```

Writes still buffered in the WAN link when the primary site died are lost.
Mount the volumes on the DR node and resume there.

To come back, run `dr-failback` repeatedly until it says done:

```bash
haify resource dr-failback db --wait 30m
```

It reconnects the primary-site nodes, **discarding what they wrote after the
failover** in favour of the DR copy, waits for them to resync from the DR, then
makes a primary-site node (`--node`, default the first) Primary again. Each
primary-site node must be unmounted for the first step and the DR node for the
last.

Each primary-site node gets its own tunnel ("leg"). If a node is renumbered or
removed, its leg can be left behind:

```bash
haify wan repair db --dry-run     # show the plan, touch nothing
haify wan repair db
```

It converges, so running it on a healthy resource reports nothing to do. It
restarts tunnels, so use `--dry-run` first.

---

## 14. Storage tiering

Put an SSD in front of a thin pool, and every volume in the pool reads and
writes through it (lvmcache).

```bash
haify pool add-cache --node node1 --pool thin-pool --device /dev/nvme0n1
haify pool remove-cache --node node1 --pool thin-pool
```

The device is consumed whole and must be free (no filesystem signature, no
partitions in use, not already a PV) and at least 4 GiB. A pool takes one
cache. `haify pool get` shows the cache and how much of a writeback cache is
dirty.

**Writethrough is the default. `--mode writeback` must be asked for by name**,
because it puts the SSD in the durability path: a node that dies with a dirty
cache takes acknowledged writes with it, and the surviving replica may never
have seen them. `remove-cache` flushes first and reports failure if the flush
cannot be confirmed, rather than letting you pull a device with data on it.

The cache is per node. DRBD replicates volumes, not the block layer beneath
them, so it is fine to cache one replica and not another.

Caching requires a thin pool: a thick pool has no single LV that every volume passes
through.

---

## 15. Encryption

`--encrypt` wraps each replica's backing volume in its own LUKS2 container, so
the stack is DRBD → LUKS → LVM.

```bash
haify resource create --name secrets --size 50G --port 7010 \
    --nodes node1,node2 --encrypt
```

What it does and does not cover:

- **At rest only.** DRBD sits above the crypt layer, so replication traffic
  between nodes is plaintext. Encrypt it with replication TLS, below.
- **Each node generates and keeps its own key** under `/etc/haify/luks`, root-only.
  Nothing is sent anywhere and there is no central escrow. Lose a node's key and
  that replica is gone; the others are unaffected.
- **It cannot be enabled later.** Decide at creation.
- LVM pools only.
- **Backups are plaintext.** `haify backup` reads each snapshot through a
  temporary read-only LUKS mapping on the node it backs up from, so the image
  holds the volume's data and restores onto any replica. The key stays on the
  node, which means the target receives plaintext: protect it with the target's
  own encryption and access policy. Backups taken by older versions
  hold ciphertext and cannot be restored; `backup restore` refuses them. The
  first backup after upgrading is a full one.

**Encrypted replication.** DRBD 9.2 and later can run a connection over kernel
TLS: the kernel asks `tlshd` (package `ktls-utils`) to do the handshake, then
encrypts in place. Install `ktls-utils` on every node, then:

```bash
haify replication-tls setup            # every node: key, certificate, tlshd
haify replication-tls status           # READY per node, or what is missing
haify resource tls db on               # encrypt every connection of db
haify resource status db               # each peer shows "tls"
haify resource tls db off
```

`setup` has each node make its own key. It then signs a certificate for each
node with the controller's replication CA (kept next to the database), installs
that CA in the node's system trust store, points `tlshd` at the certificate,
and loads the `tls` module at boot. A peer whose certificate this CA did not sign is refused
at the handshake. Run `setup` again to renew certificates; `status` warns 30
days before one expires.

- **Live switch.** DRBD cannot change a connection's transport while it is up,
  so `resource tls` takes one link down at a time and brings it back while the
  others keep quorum. The Primary keeps serving; the reconnected peer catches up
  with a short resync. Switching a resource's three links takes about 15 s,
  and a continuous write load sees no failed write.
- **A failed handshake is not retried.** DRBD leaves that link StandAlone and
  the switch stops, naming it. Fix the node (`journalctl -u tlshd`), then
  `haify resource repair <resource>`.
- **New members must be ready.** Adding a replica, a diskless client or a
  tiebreaker to an encrypted resource is refused on a node that is not.
- **The CA joins the system trust store**, because `tlshd` before ktls-utils
  0.10 has no setting for a private one. Anything on a node that validates
  against that store will accept a certificate it issued; it only ever issues
  replication certificates.
- **Not for WAN resources.** Their off-site leg already runs mutual TLS through
  `haify-proxy`.

---

## 16. Alerts and notifications

Detection is on by default. Its settings are in `controller.toml`:

```toml
[alert]
enabled = true                # default; false stops the health poll
check_interval_sec = 30       # default
check_nodes = true            # default; SSH-probe each node, produces node.unreachable
check_pools = true            # default; thin pool data/metadata use
pool_near_full_percent = 85   # default; warning
pool_full_percent = 95        # default; critical
history_size = 500            # default; kept in the database across failovers
watch_drbd_events = true      # default; see below
idle_interval_sec = 300       # default
warning_hold_sec = 30         # default; see below
```

A **warning** is raised only once its condition has lasted `warning_hold_sec`:
a replica link that drops and reconnects within it is not reported. Critical
conditions (a full pool, a lost Primary, an unreachable node) are never held.

With `watch_drbd_events` the controller keeps one `drbdsetup events2` stream
open to each node. A DRBD state change is then checked within a few seconds
instead of at the next poll. While every node's stream is up and nothing is
resyncing or verifying, the cluster is polled every `idle_interval_sec`
instead of every `check_interval_sec`. That longer interval still applies to
what DRBD does not report, such as a thin pool filling up.

Events: `resource.degraded`, `resource.failover`, `resource.no_primary`,
`resource.promoted`, `node.unreachable`, `wan.degraded`, `resource.out_of_sync`,
`pool.data_near_full`, `pool.data_full`, `pool.metadata_near_full`,
`pool.metadata_full`, `pool.out_of_space` (LVM already refused writes),
`pool.vdo_physical_near_full`, `pool.vdo_physical_full` (the physical space
under an `lvm-thin-vdo` pool),
`pool.snapshots_removed` (a near-full pool gave up a scheduled snapshot),
`pool.snapshots_locked` (a near-full pool with only locked snapshots left),
`audit.shipping_failed`, `audit.truncated`, `approval.requested` (see [Access control](#17-access-control)),
`controller.clock_jumped`, `resource.write_anomaly`, `node.evicted`,
`resource.replica_moved`, `resource.volume_moved`, `pool.trimmed`,
`pool.extended`, `pool.disk_moved`, `disk.health`,
`channel.test` (sent by a channel test, so receivers can ignore it),
`backup.failed` (a scheduled backup) and `inspection.completed` (see
[Inspection](#inspection)). Each carries a severity
(`info`/`warning`/`critical`) and a status (`firing` when a condition starts,
`resolved` when it clears, `info` for a one-off such as an inspection), so a
receiver can pair an alert with its recovery instead of reading the recovery as
a new fault.

```bash
haify event list --resource db --min-severity warning
haify event watch --min-severity critical
haify event watch --type resource.failover --json
haify event watch --replay          # retained history first, then live
```

`event list` and `event watch` take `--resource`, `--type`, `--min-severity`
and `--since-id` (resume after a disconnect).

### Sending them somewhere

A chat service will not accept an arbitrary JSON document, so a channel has a
**kind**. Feishu, WeCom and DingTalk also report a refusal *inside an HTTP
200*, so a misconfigured channel looks like it is working until an outage passes
unnoticed.

```bash
haify channel add --name oncall --kind feishu \
    --url https://open.feishu.cn/open-apis/bot/v2/hook/xxxx --min-severity warning

haify channel add --name pager --kind slack \
    --url https://hooks.slack.com/services/T00/B00/xxxx --min-severity critical

export HAIFY_NOTIFY_SECRET=SECxxxx        # DingTalk 加签, never a flag
haify channel add --name ops --kind dingtalk \
    --url 'https://oapi.dingtalk.com/robot/send?access_token=xxxx'

haify channel test oncall             # reports what the service itself said
haify channel list
haify channel delete ops
```

`--type` limits a channel to some event types
(`--type resource.failover,node.unreachable`); `--header Name=Value` adds a
request header, for a receiver that wants a token. Re-running `add` without a
secret keeps the stored DingTalk secret; `--clear-secret` removes it.

Kinds: `generic` (the event JSON unchanged, for a receiver you wrote), `feishu`,
`slack`, `wecom`, `dingtalk`. A well-known bot URL saved with the wrong kind is
refused up front rather than at delivery time.

**Always run `channel test` after adding one.** It is the only way to tell
a working channel from a silent one.

Channels live in the controller database, so adding or muting one takes effect
immediately, without a restart. `--muted` stores a channel without delivering to it,
which is how you silence a noisy pager during an incident without having to find
the bot URL again afterwards.

Receivers can also be set in `controller.toml`: `webhook_url` (with
`webhook_min_severity`) or `[[alert.webhooks]]` entries with `url`,
`min_severity` and `headers` under `[alert]`. They post the generic event JSON,
need a restart to change, and work alongside channels.

The same events are also readable on the REST API (`[server] rest_port`, default 3375) at
`GET /v1/events`, streamable as newline-delimited JSON at `/v1/events/watch`,
and pushed to the web UI's bell over `/v1/events/stream`.

### Metrics

The controller serves Prometheus metrics at `http://<controller>:9433/metrics`
(`[metrics]`: `enabled` default true, `listen_address` default `0.0.0.0`,
`port` default 9433). The cluster gauges below are fed by the health poll, so
they stay empty unless `[alert] enabled = true`; the controller logs a warning
at startup when it is not. Besides the API's own request counts and latencies
(`haify_controller_grpc_requests_total`, `haify_controller_grpc_request_duration_seconds`)
and the counts `haify_controller_resources{state}`, `haify_controller_nodes{state}`
and `haify_controller_gateways{type,state}`:

| Metric | Labels | Meaning |
| --- | --- | --- |
| `haify_drbd_role`, `haify_drbd_disk_state`, `haify_drbd_replication_state` | resource, node, role/state | 1 for the state each replica holds now |
| `haify_drbd_quorum` | resource, node | 0 when the node that answered has lost quorum |
| `haify_drbd_resource_up` | resource | 0 when no node answered for the resource |
| `haify_drbd_resync_completed_ratio` | resource, node | 1 when in sync |
| `haify_drbd_out_of_sync_bytes` | resource, node | data DRBD has marked as differing |
| `haify_drbd_connection_tls` | resource, node | 1 when that connection is encrypted |
| `haify_controller_node_reachable` | node | 0 when the controller cannot reach it |
| `haify_controller_pool_thin_used_percent` | pool, node, kind | thin pool data/metadata use |
| `haify_controller_storage_capacity_bytes` | pool, node, state | total/used/free |
| `haify_controller_alerts_firing` | type, severity | conditions raised now |
| `haify_controller_backup_last_success_timestamp_seconds` | resource, target | newest completed backup |
| `haify_controller_backup_last_shipped_bytes` | resource, target, kind | what it carried |
| `haify_controller_resource_fault_domain_risk` | resource, domain | 1 when one domain's loss takes it down |
| `haify_controller_last_observation_timestamp_seconds` | source | when each source last answered |

The controller also exports `haify_controller_up`,
`haify_controller_operations_total{operation,result}`,
`haify_controller_operation_duration_seconds{operation}` and
`haify_drbd_written_bytes{resource,node}` (bytes DRBD wrote to a node's disk).

A series nobody observed is absent rather than zero: a replica that stopped
answering has no `haify_drbd_disk_state`, and `haify_drbd_resource_up` says why.
`deploy/monitoring/prometheus-rules.yml` has alerting rules on these
(controller down, stale observations, unreachable node, lost quorum, unreadable
resource, replica not UpToDate, out of sync, thin pool near full and full,
backup older than two days, one-failure-domain risk). `deploy/monitoring`
also has a Docker Compose stack that uses them: Prometheus, plus Grafana with
a "Haify" dashboard.

### Inspection

Alerts fire when a condition starts. An inspection asks what they do not: did
the alert reach anyone, is a replica that says Connected actually replicating,
is the node still at the address it is registered at. It runs a fixed set of
checks, changes nothing, and stores a report in which every finding has a
status (`pass`, `warn`, `fail`, or `error` when the check itself could not
run), the evidence, and the command that fixes it.

```bash
haify inspect run                         # now; waits for the report
haify inspect run --area alerts,nodes     # only some areas
haify inspect list
haify inspect show                        # newest; or: haify inspect show 42
haify inspect show latest --json
```

Each node is probed once over SSH per run; a node that does not answer is a
`nodes.ssh` failure and `error` for its other checks, not a failed run.

| Area | Checks |
| ---- | ------ |
| resources | a resource under `haify ha create` has exactly one Primary; on every node: replica Outdated, Inconsistent with no resync, Diskless where it should hold data, not up, quorum lost; a peer StandAlone; a peer no node can reach, reported once per peer: fail for a diskful peer, warn for a tiebreaker or diskless client (the data is still fully redundant); a Connected peer stuck in WFBitMapS/WFBitMapT/WFSyncUUID (or Off between two diskful nodes), judged from every node's own view, because a handshake can be stuck on one side only; two-node quorum risk (warn); single-failure-domain risk (one warn per domain, listing its resources); an HA promoter config missing on a primary-site diskful node (warn), present on a diskless one (warn: it works as a diskless Primary, over the network) or present on a WAN resource's DR node (warn: it could fail over to the asynchronous copy unasked) |
| gateways | every gateway not `stopped` has exactly one Primary; its promoter config on every diskful node (warn when missing, and when present on a diskless node) |
| nodes | SSH reachable; clock skew against the controller (warn > 2 s, fail > 30 s; SSH latency is not counted), NTP synchronised; root filesystem (warn ≥ 85 %, fail ≥ 95 %); the registered address present on an interface (a public address answering as the registered host is taken as NAT, not drift), and the address answering as the registered host; drbd module loaded; drbd-reactor running; DRBD module, drbd-utils, drbd-reactor and `haify-controller` binary the same on every node, the binary compared only between nodes of one architecture (a differing binary fails under Self-HA); `/etc/hosts` mapping a node name to an address it is not registered at |
| pools | thin pool data and metadata against `[alert] pool_near_full_percent` / `pool_full_percent`; growth since the previous report, warn when full within 14 days, fail within 3; on a thick pool, free space below the copy-on-write area a snapshot of a volume reserves (20 % of it, at least 256 MiB), naming the volumes whose snapshots and backups will fail (warn); each disk under a pool, by SMART or the NVMe health log: warn on wear past 90 %, media errors, reallocated or pending sectors, fail on a failed SMART verdict, an NVMe critical warning, a worn-out NVMe drive (100 %) or uncorrectable sectors (`disk.health`; no check without `smartctl`) |
| backups | each enabled backup schedule: last run failed, target missing, last success older than 1.5 cron intervals (warn) or 3 (fail); snapshot schedules not run for 1.5 / 3 intervals; schedules enabled while `[schedule] enabled = false`; `_bk_` snapshots no backup record refers to |
| alerts | `[alert]` enabled; at least one enabled channel; each channel's last deliveries succeeded; every warning or critical raised in the last 24 h was accepted by a channel that delivered it. No test message is sent |
| selfha | at least two UpToDate copies of `haify-meta`; its promoter config active on every diskful candidate (and noted on a diskless one); exactly one `haify-controller` active, on the `haify-meta` Primary; a controller binary on every candidate |
| tls | API server certificate, replication CA and every node's replication certificate: warn under 30 days, fail under 7 or expired |
| hygiene | `/etc/drbd.d/*.res` and Haify-named volumes (`<res>_data`, `<res>_volN`, `<res>_state*`, `_sched_` snapshots) of resources the controller no longer has. Listed, never deleted |

```toml
[inspect]
enabled = true            # default
schedule = "0 1 * * *"    # default; cron, controller time zone
keep = 30                 # default; reports stored
notify_min = "warn"       # default; pass | warn | fail
```

The schedule runs on the snapshot scheduler, so it runs on the active controller
only and only while `[schedule] enabled = true`; `haify inspect run` works either
way. Each run publishes one `inspection.completed` event when its worst finding
is at least `notify_min`. The severity is `critical` for a fail, `warning` for
a warn or error, and `info` otherwise. The message holds the counts and the
first failing items. The event goes to notification channels like any alert,
so it needs `[alert] enabled = true`. A channel filtered to `warning` hears
from the inspection only when it found something. `haify-mcp` exposes the same as
`haify_inspect_run` and `haify_inspect_report`.

Channel delivery results (last success, last failure and its error, events
given up on) are recorded per channel as alerts are delivered; that record is
what the `alerts` area reads.

---

## 17. Access control

With nothing configured, the API accepts every caller. There are two models,
set in `controller.toml`:

```toml
[auth]                  # one shared token for every caller
enabled = true
token = "..."

[rbac]                  # per-user tokens with roles; replaces [auth]
enabled = true
[[rbac.users]]
name = "admin"
token = "..."
role = "admin"
```

Roles are `admin`, `operator`, `viewer` and `security-officer`. Every API call is classified by
object (pool, resource, gateway, snapshot, backup, node, ha, app, approval, system) and action
(read, write, approve). An operator may write pools, resources, gateways, snapshots,
backups, HA and database apps, and only read nodes and system settings; a viewer can only
read; an admin can do everything. `[[rbac.policies]]` entries (`role`,
`object`, `action`, either may be `*`) add grants or define further roles.

```bash
haify rbac whoami                          # your identity and role
haify rbac policies                        # effective roles and assignments (admin only)
haify rbac user add --name alice --role operator   # prints a generated token once
haify rbac user add --name ci --role viewer --user-token <16+ chars>   # set the token yourself
haify rbac user set-role alice viewer
haify rbac user remove alice
```

Users added this way are kept in the controller database. Users declared in
`controller.toml` are re-applied at every start and cannot be removed through
the API, so an admin always remains. The `rbac` commands use the gRPC API
like every other command, so `--token` and the TLS options below apply to them.

Tokens come from `--token`, `HAIFY_TOKEN`, `~/.haify/token` or `/etc/haify/token`.

**Two-person approval.** A role says what a user may do, and an admin may do
everything, so one stolen admin token could delete the backup target, the
backups and the snapshots. With approval on, the calls that destroy data or
weaken what protects it run only after a *different* user approved that exact
call:

```toml
[rbac.approval]
enabled = true
ttl_minutes = 60     # how long a request waits, and how long an approved call may then be made
# methods = [...]    # default: the list below
```

```bash
$ haify backup target delete offsite                 # alice
Error: DeleteBackupTarget needs a second person's approval: request 3f9a1c2b7d10 is pending ...
$ haify approval list                                # bob
$ haify approval approve 3f9a1c2b7d10                # bob; alice cannot approve her own
$ haify backup target delete offsite                 # alice again: runs, once
```

The approval covers the method and its exact arguments, so approving the
removal of one target cannot be spent removing another. Who may approve: an
admin, or a `security-officer`, who can read everything and approve, and change
nothing. Each new request raises an `approval.requested` event, so approvers
hear of it; an unexpected one may mean a token has been stolen.

The default list:

- deleting pools, ZFS pools and datasets, resources, volumes and snapshots,
  including whole-resource snapshots;
- restoring snapshots and backups, and rolling back a whole-resource snapshot
  (all of these overwrite the volume);
- moving a volume to another pool (this deletes its snapshots in the old pool);
- adding, replacing or removing a backup target; deleting backups and snapshot
  or backup schedules; unfreezing a snapshot schedule;
- replacing a snapshot or backup schedule with one that keeps fewer of any
  kind, because its next run would prune the rest (creating one, or keeping as
  many, runs at once);
- deleting a database app (whose `--delete-data` deletes its resource from
  inside the controller);
- marking a node lost and restoring a node;
- adding users, removing them or changing roles, so a stolen token cannot
  create its own second approver. The web UI's user management is held back
  the same way.

Approval needs `[rbac]`: the controller refuses to start with approval on and
RBAC off.

Automation is held back too. The CSI driver deleting a PVC and the Proxmox
plugin removing a VM disk call `DeleteResource`. The Proxmox plugin rolling
back a VM disk calls `RollbackResourceSnapshot`, or `RestoreSnapshot` for a
snapshot taken before whole-resource snapshots existed. These calls then wait
for an approver like anyone else. Where that
is not wanted, list `methods` yourself without them; the snapshot and backup
locks still protect what they lock.

**TLS.** `[tls] enabled = true` with `cert_file` and `key_file` puts the gRPC
API (port 3374) on TLS; adding `client_ca_file` requires a client certificate
signed by that CA. The REST API on `[server] rest_port` (default 3375) stays
plain HTTP unless `[tls] rest = true` is also set. Then it is served over TLS
with the same certificate, but without client certificates: REST callers carry
a bearer token, and the web UI proxies to it over a pinned loopback
connection.
REST clients such as the Proxmox plugin then use `https://`. Clients:

```bash
haify --tls-ca /etc/haify/ca.crt node list                 # verify against this CA
haify --tls-cert me.crt --tls-key me.key --tls-ca ca.crt node list   # mutual TLS
```

`--tls` alone verifies against the system trust store; `--tls-server-name`
overrides the name checked; `--tls-insecure` encrypts without verifying. Each
has an environment variable (`HAIFY_TLS`, `HAIFY_TLS_CA`, `HAIFY_TLS_CERT`,
`HAIFY_TLS_KEY`, `HAIFY_TLS_SERVER_NAME`, `HAIFY_TLS_INSECURE`).

**Audit.** Every state-changing API call is recorded with caller, target,
outcome and latency in the controller database (`[audit] enabled`, default
true; `include_reads = true` records reads too). Read it at `GET /v1/audit`
on the REST API, or in the web UI. Entries are kept for `retention_days`
(default 180), up to `max_entries` (default 200000) in total, so a flood of
calls cannot fill the metadata volume. If the cap drops an entry before its
retention period is over, a critical `audit.truncated` event is raised.

Whoever controls the controller host can rewrite the database trail. To keep
a copy they cannot rewrite, send the trail off the cluster as it is written:

```toml
[audit]
syslog = "tls://logs.example.com:6514"   # or tcp://host:514, udp://host:514
syslog_ca = "/etc/haify/logs-ca.pem"       # tls only; empty = system roots
webhook_url = "https://audit.example.com/haify"
webhook_token = "..."                    # sent as Authorization: Bearer
```

Syslog gets one RFC 5424 message per entry, facility 13 ("log audit"),
newline-framed over TCP. The webhook gets each batch as a JSON POST,
`{"source": "haify-controller", "records": [{"seq": N, "event": {...}}]}`. Either
is sent in order, starting from where it last stopped. Each destination's
position is stored with the trail. So a destination that was down gets
everything when it is back, and a controller that takes over after a failover
carries on where the last one stopped. A resend after a failure can repeat an
entry but never skips one; `seq` tells a duplicate from a gap. A destination failing for five
minutes raises `audit.shipping_failed`. UDP can lose messages unnoticed; use
TCP or TLS.

Point it at storage the cluster's credentials cannot rewrite: a log server
you run elsewhere, or a collector writing to an object store with Object Lock.

---

## 18. Kubernetes and virtual machines

Haify ships a CSI driver. Volumes are DRBD resources; a pod moving between nodes
gets its storage promoted on the new one.

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: haify
provisioner: haify.csi.liliang-cn.com
parameters:
  pool: thin-pool
  replicas: "2"
```

Other parameters: `storageType` (`lvm`, the default, or `zfs`),
`resourceProfile` (take pool, replicas and storage type from a profile; `pool`
may then be omitted), `resourceLabels` (`key=value,...`), `faultDomainLabel`
(default `host`), and `allowRemoteVolumeAccess: "true"`, which opts a volume
into diskless attachment so a pod can run on a node that holds no replica.

**I/O limits.** `readBytesPerSecond`, `writeBytesPerSecond` (sizes such as
`200Mi`), `readIOPS` and `writeIOPS` cap a pod's I/O to its volume. When the
volume is published, the node plugin writes these limits for the volume's
device into the pod's cgroup (`io.max`). This needs cgroup v2 with the `io`
controller enabled for pod cgroups, and the node plugin's `/sys/fs/cgroup`
mount from `deploy/k8s/30-node.yaml`. Without them, the volume is still
published, but without limits, and the node plugin logs why. The limits are
fixed per StorageClass; to change them, move the volume to another class.

The manifests are in `deploy/k8s` (see its README); the CSI section of
[deployment-guide.md](deployment-guide.md) covers installation.

A database on Kubernetes with its data on a Haify volume, the counterpart of
[`haify app`](#12-database-and-object-store-applications), is one MCP call: `haify-mcp k8s` serves
`haify_k8s_app_create`, `haify_k8s_app_list` and `haify_k8s_app_delete`
([mcp.md](mcp.md#kubernetes-tools)). Deleting keeps the volume claim and the
password secret unless `delete_data` is set, and creating the app again with
the same name and template runs it on them.

**Proxmox VE** has its own integration: a storage plugin (type `haify`) that backs VM
disks with Haify resources over the controller's REST API. It is in
`deploy/proxmox`, with its requirements and install steps in that README.
`deploy/proxmox/bootstrap.sh`, run on one PVE node, does those steps for an
existing PVE cluster: DRBD from LINBIT's repository on every node, the
controller, node registration, the pool, Self-HA, the plugin and the
`storage.cfg` entry.

**KVM with libvirt** needs no plugin: a guest's disks are Haify devices
(`/dev/drbd/by-res/<resource>/0`), and the qemu hook in `deploy/libvirt` makes
them Primary when the guest starts and handles live migration.
`haify ha create --vm <guest>` restarts the guest on another host when its host
fails ([High availability](#11-high-availability)). See
[deploy/libvirt/README.md](../deploy/libvirt/README.md).

**OpenStack** uses the Cinder driver in `deploy/openstack`: each Cinder volume is
a Haify resource, attached to the compute host as a local block device, or as a
diskless client where the host holds no replica. The same package adds a Haify
panel and a Replication tab to Horizon. See
[deploy/openstack/README.md](../deploy/openstack/README.md).

---

## 19. The AI Copilot

`haify-ai` is an optional service that answers questions about the cluster in the
web UI's Copilot sidebar. It reaches the cluster through `haify-mcp`: every
read-only tool, plus a fixed list of day-to-day writes. Each write waits in
the chat panel until the operator approves that call, with its arguments
shown. The writes are: registering nodes and setting node labels; creating
pools, resources, profiles, NFS, iSCSI and NVMe-oF gateways, HA configs,
snapshots, snapshot schedules, backups, ZFS datasets and volumes, ZFS
snapshot clones and Kubernetes apps; adding disks, caches, volumes, replicas
and DR; adopting a resource; attaching and detaching diskless clients;
mounting; resizing volumes; setting resource options, profile options, a
resource's profile and its tiebreaker; adjusting a profile; dual-primary;
iSCSI CHAP; starting a gateway; undrain; verify; WAN repair and endpoint
changes; and testing a notification channel. Deleting, restoring, unmounting,
other role changes, evicting, draining and stopping are not available to it
at all; it proposes them and the operator runs
them from the UI. It listens on `127.0.0.1:7634` by default (`HAIFY_AI_ADDR`;
any non-loopback address requires a token, taken from `HAIFY_AI_TOKEN`,
`HAIFY_TOKEN`, `~/.haify/token` or `/etc/haify/token`, in that order); the web UI proxies `/ai/*` to port 7634 on its own node.

Two things determine how useful it is:

- **Its knowledge base.** Check what it has: `GET /ai/kb/list`. A near-empty one
  makes it fall back on the model's own memory, and for a specific question
  about your cluster that produces confident wrong answers. Feed it with
  `POST /ai/kb/ingest` (a directory on the node running `haify-ai`) or
  `POST /ai/kb/doc` (one document). `GET /ai/kb/doctor` checks that retrieval
  over the index still returns results.
- **The embedder matching the index.** `HAIFY_AI_EMB_DIM` (default 768) must
  equal the width the index was built at. If you switch to an embedder of a different width,
  every search returns nothing and no error is reported. `/ai/kb/list`
  reports the width read back from the index, so check it there.

Send the same `session_id` across turns and it follows a conversation; omit it
and every question starts from nothing.

---

## 20. Routine operations

**Before a node reboot**

```bash
haify node drain node1
# ... work ...
haify node undrain node1
haify resource status <each affected resource>   # wait for UpToDate everywhere
```

**Growing a volume**

```bash
haify resource resize-volume db 0 200G
# then grow the filesystem on the Primary
ssh <primary> sudo resize2fs /dev/drbd<minor>
```

**Replacing a disk**

Drain the node, remove its replica, replace the disk, recreate the pool, add the
replica back, and wait for the resync to finish before touching the next node.

**Upgrading the controller**

From a checkout, `./scripts/deploy-all.sh node1,node2,node3` builds for each
node's architecture and installs `haify-controller` and `haify` on each one,
with Self-HA taken into account. By hand:

```bash
# build for the nodes, not for the machine you build on
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 make build
scp bin/haify-controller <node>:/tmp/
ssh <node> "sudo systemctl stop haify-controller && \
  sudo install -m 0755 /tmp/haify-controller /opt/haify/bin/haify-controller && \
  sudo systemctl start haify-controller"
```

`/opt/haify/bin` is where the shipped unit file runs it from; copy to whatever
path your unit's `ExecStart` names (`systemctl cat haify-controller`). Set
`GOARCH` to the nodes' architecture (`arm64` for aarch64).

With Self-HA on, do not stop or restart the controller by hand: drbd-reactor manages it, and stopping it is an unplanned failover.
Replace the binary on every standby node first, then
`haify ha evict haify-meta` to move the controller onto one of them, then
replace it on the node it left.

**Checking that replicas really hold the same data**

Every replica can say `UpToDate` and still differ, for example after a disk
returned the wrong block or a write never reached one copy. Only reading both
finds that:

```bash
haify resource verify db --wait 30m     # compare every replica with the Primary
haify resource verify db                # still running? run it again to follow it
haify resource verify db --resync       # make the copies identical
```

DRBD records blocks that may differ in an out-of-sync bitmap. A verify **adds**
to the bitmap and never clears it. Marks also stay after whatever set them is
over (an earlier verify, an interrupted resync, a reconnect at equal
generation). So a peer can show most of its volume as "marked out of sync"
while its data is in fact identical to the source. That is why `verify`
reports two things separately: what it found itself, and what was already
marked. `--resync` copies the source's data over every marked block. This is
harmless when the copies are identical, and it is the only thing that clears
the marks. It copies from `--node` (default: the Primary). It refuses a source
that is not Primary unless another replica agrees with it, and it never
overwrites a Primary.

The controller verifies every resource on its own, one at a time, on
`storage.verify_schedule` (cron, default `0 3 1 * *`: monthly; `""` turns it
off). Marked blocks raise `resource.out_of_sync`; clearing them is left to you.

Resources on thin pools resync with `rs-discard-granularity`, so a full resync
(a new replica, a failback) keeps the target thin instead of allocating every
block. Resources created before this was a default can get it with
`haify resource set-options <name> --drbd-options disk/rs-discard-granularity=65536`.

**Checking a cluster you have not looked at in a while**

```bash
haify health-check
haify resource list
haify event list --min-severity warning
haify backup schedule list # did the last scheduled backup fail?
haify backup list          # when was the newest 'completed' one?
haify channel test <each>  # would you actually be told?
```

---

## 21. When something is wrong

Start here:

```bash
haify resource status <name>
haify event list --resource <name>
ssh <node> sudo drbdadm status <name>
ssh <node> sudo journalctl -u drbd-reactor -n 50
```

### Reading the status

`haify resource status` prints one line per node, `role=… disk=… repl=…`;
`drbdadm status` on a node prints the same states as `role:`, `disk:`,
`peer-disk:` and `replication:`, plus `quorum:no` when the node has lost it.

| What you see | What it means |
| --- | --- |
| `role=Primary` on exactly one node | Normal. |
| `role=Secondary` everywhere | Nobody is serving I/O. Normal for an idle resource; a problem if something should be mounted. |
| `disk=UpToDate` | This replica has every byte. |
| `disk=Inconsistent` | Mid-resync, or never synced. Wait. |
| `disk=Outdated` | This copy is known stale; it will not be promoted, which is correct. |
| `disk=Diskless` | No local copy. Expected on a tiebreaker or a diskless client; a fault anywhere else. |
| `peer-disk:DUnknown` (drbdadm) | Cannot see that peer at all. Network, or the peer is down. |
| `repl=Established` | Healthy. |
| `repl=StandAlone` | The connection was dropped, usually after a split brain. It will not reconnect by itself. |
| `quorum:no` (drbdadm) | This replica may not serve I/O. See below. |

### Common situations

**I/O suspended on a resource, `quorum:no`.** The replica set cannot form a
majority. Usually a two-diskful resource with no tiebreaker after losing one
node. Register a third node so a tiebreaker can be added. As an emergency
measure on the surviving diskful node only:

```bash
sudo drbdsetup resource-options <res> --quorum=1     # runtime only, not persisted
# ... once the peer is back and UpToDate ...
sudo drbdsetup resource-options <res> --quorum=majority
```

Change it on one node only. The peers keep `majority`, so a peer starting alone
still cannot be promoted, which stops this from becoming a split brain.

**A resource is stuck `Inconsistent` after creation.** A fresh DRBD device needs
one copy declared authoritative. `resource create` does this itself; if it was
interrupted before that step, promote once by hand with
`haify resource primary <res> <node> --force`, then
`haify resource secondary <res> <node>`.

**`StandAlone` after a split brain.** Two copies diverged. Decide which one is
authoritative (Haify will not guess), then discard the other's changes and
reconnect it. Anything written to the discarded side is lost, so look at both
before choosing.

**A gateway will not start.** Look at the OCF agent, not at drbd-reactor:

```bash
ssh <node> sudo drbd-reactorctl status
ssh <node> sudo journalctl -u "ocf.rs@*<resource>*" -n 50
```

The usual causes are a missing package (`nfs-kernel-server`, `targetcli-fb`, the
`nvmet-tcp` module) and a service IP already in use.

**A node is unreachable but the machine is up.** Haify reaches nodes over SSH
from the controller node. After a node is rebuilt its host key changes and
every operation fails with an empty error; clear the stale key and restart the
controller.

**Nothing is alerting.** Check three things in order: `[alert] enabled = true`,
at least one channel in `haify channel list` that is not muted (or a webhook
under `[alert]`), and `haify channel test` on each of them.

### Getting more detail

```bash
haify event watch                # follow live
haify rbac whoami                # "permission denied" that should not be
journalctl -u haify-controller -f    # on the controller node
```

On the REST API (`[server] rest_port`, default 3375, with the same token):

- `GET /v1/logs` returns the controller's recent log lines from memory.
- `GET /v1/audit` returns who changed what.
- `POST /v1/diagnostics/collect` runs a fixed set of named, read-only
  collectors on the nodes, so nobody has to log in
  (`{"nodes": [...], "collectors": [...]}`, both defaulting to all). They cover
  DRBD status, config and kernel state, kernel errors, the drbd-reactor,
  promoter and Haify journals, failed units, storage and mounts.

The web UI's Logs page shows the first two;
`haify-mcp` has all three (`haify_log_list`, `haify_audit_list`, `haify_diagnose`).

[Known failure modes](deployment-guide.md#13-known-failure-modes) in the
deployment guide lists the failures that are hardest to diagnose.
