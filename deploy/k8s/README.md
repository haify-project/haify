# Haify CSI driver

The quickest way to install is the Helm chart in `deploy/helm/haify-csi` (see
its README). The manifests here create the same objects, for clusters you
manage without Helm.

Driver name: `haify.csi.liliang-cn.com`. Everything is deployed into
`kube-system` except the optional Copilot RBAC (`60-haify-ai-apps-rbac.yaml`,
namespace `haify-ai`).

| File | What it creates |
| ---- | --------------- |
| `00-csidriver.yaml` | `CSIDriver` (`attachRequired: false`, `storageCapacity: true`, `fsGroupPolicy: File`) |
| `00-haify-controller-endpoint.yaml` | Selectorless `Service` + manual `EndpointSlice` pointing at the external haify-controller |
| `10-rbac.yaml` | ServiceAccounts `haify-csi-controller`, `haify-csi-node`; ClusterRoles for provisioner, snapshotter, resizer and the health reporter |
| `20-controller.yaml` | Deployment `haify-csi-controller`: csi-provisioner, csi-resizer, csi-snapshotter, plugin (`csi-controller`), livenessprobe |
| `30-node.yaml` | DaemonSet `haify-csi-node`: node-driver-registrar, plugin (`csi-node`, privileged) |
| `40-storageclass.yaml` | StorageClass `haify-drbd` |
| `50-volumesnapshotclass.yaml` | VolumeSnapshotClass `haify-drbd-snapshot` (needs the snapshot CRDs, see below) |
| `examples/shared-filesystem.yaml` | StorageClass `haify-shared`, a `ReadWriteMany` PVC and a Deployment whose three Pods share it; not applied with the rest (set `nfsServiceIPs` first, see [Shared filesystems](#shared-filesystems-readwritemany)) |
| `examples/volumegroupsnapshotclass.yaml` | VolumeGroupSnapshotClass `haify-group-snapshot`; not applied with the rest (needs the group snapshot CRDs, see below) |
| `60-haify-ai-apps-rbac.yaml` | ServiceAccount + token Secret for the Haify Copilot's `haify_k8s_app_create` tool |

## Prerequisites

- Every Kubernetes node that runs volumes is a registered Haify node, with the
  DRBD 9 kernel module, `drbd-utils`, and LVM or ZFS, or a local-agent node
  that has only the DRBD 9 module (see [Nodes without SSH](#nodes-without-ssh-local-agent)).
  Instead of installing
  `drbd-dkms` on each node, the module can be loaded from Kubernetes:
  [DRBD on Kubernetes nodes](../../docs/kubernetes-drbd.md)
  (`optional/drbd-loader.yaml`, or the chart's `drbdModuleLoader`).
- **The Kubernetes node name must be the same as the Haify node name.** The
  node plugin reports `spec.nodeName` as its location, and the controller
  plugin looks that name up in `haify node list`.
- When the node plugin starts, it registers its node with the controller under
  that name and the node's `status.hostIP` (`--node-name`/`--node-ip`, filled
  from `NODE_NAME`/`NODE_IP`). Registering a node that is already registered
  changes nothing. The controller still needs SSH access to that address,
  unless the node is a local-agent node.
- The Haify pool named in the StorageClass exists on at least as many of those
  nodes as the StorageClass asks for `replicas` (`haify pool create --name vg0 ...`).

## Build the image

Both plugin containers use `haify-csi:latest` with `imagePullPolicy: IfNotPresent`,
so the image must already be on every node. Or push it to a registry and use
that image name in `20-controller.yaml` / `30-node.yaml`:

    docker build -f Dockerfile.csi -t haify-csi:latest .
    docker save haify-csi:latest | sudo k3s ctr images import -    # on each k3s node

`Dockerfile.csi` cross-compiles for `--platform`, so `docker buildx build
--platform linux/amd64,linux/arm64 ...` works from either architecture.

## Point the CSI at your haify-controller VIP

The haify-controller runs outside Kubernetes, on the storage hosts. With
Self-HA it moves between hosts and keeps one floating VIP.
`00-haify-controller-endpoint.yaml` lets pods reach it by the in-cluster name
`haify-controller:3374`. The file holds a `Service` named `haify-controller`
with no selector (port `3374`, named `grpc`) and an `EndpointSlice` that
contains the VIP.

If you are upgrading from an older manifest that used a v1 `Endpoints` object,
delete that object after applying the new file
(`kubectl -n kube-system delete endpoints haify-controller`). Otherwise
Kubernetes keeps copying it into a second slice.

**You must set the VIP.** Replace the `192.0.2.10` placeholder (marked
`# CHANGE ME`) with your real VIP, which appears as `VIP:` in the output of:

    haify ha self status

The plugins connect with `--haify-controller=haify-controller:3374`, which is
also the binaries' default, so the Deployment and DaemonSet need no edits. An
`ExternalName` Service would not work here: it can only point at a DNS name,
and the VIP is an IP address.

The controller Deployment and the node DaemonSet both run with
`hostNetwork: true` and `dnsPolicy: ClusterFirstWithHostNet`. Host networking
lets a pod reach the VIP even when the VIP is on its own node. The DNS policy
keeps the Service name working on the host network.

The controller Deployment runs one replica. Raise `replicas` in
`20-controller.yaml` for a standby: only one replica works at a time, and when
it stops another takes over within about half a minute. The replicas agree on
which one works through Leases in `kube-system`. Each replica needs a node of
its own, because they share the host network; a replica with no free node
stays Pending.

If the controller has `[auth]` or `[rbac]` on, the plugins need its API token.
Before applying, create a Secret from the token file on a Haify node:

    kubectl -n kube-system create secret generic haify-api-token --from-file=token=/etc/haify/token

Without that Secret the plugins connect without a token. That works only when
the controller has authentication off.

Then:

    kubectl apply -f deploy/k8s/

## StorageClass parameters

| Parameter | Default | Meaning |
| --------- | ------- | ------- |
| `pool` | required unless `resourceProfile` sets it | Haify pool name as given to `haify pool create --name` (`vg0` and `haify_vg0` both match) |
| `replicas` | `2` | Number of replicas that hold the data (diskful) |
| `storageType` | `lvm` | `lvm` or `zfs` |
| `allowRemoteVolumeAccess` | `false` | `true` lets a Pod run on a node with no replica. The node plugin attaches a diskless DRBD client there when the volume is staged, and detaches it when the volume is unstaged. Otherwise Pods run only on nodes that hold a replica |
| `faultDomainLabel` | `host` | Haify node label used to spread replicas: replicas go to nodes with different values of it when possible (`haify node label <node> host=<name>`). A node without the label counts as its own domain. `zone` spreads across Kubernetes zones without labelling anything by hand, see [Zones](#zones) |
| `resourceProfile` | none | Haify resource profile. Its pool, replica count and storage type are used for any of the three the StorageClass does not set |
| `resourceLabels` | none | Labels put on the Haify resource, written `key=value,key=value`. The driver always adds `haify.csi/managed-by=csi`, and `haify.csi/pvc=<namespace>/<name>` to name the claim (external-provisioner passes the claim with `--extra-create-metadata`, which `20-controller.yaml` sets) |
| `readBytesPerSecond`, `writeBytesPerSecond` | none | Read and write bandwidth limit for the Pod on this volume, as a size per second (`200Mi`, `1G`) |
| `readIOPS`, `writeIOPS` | none | Read and write operations per second limit for the Pod on this volume |
| `encryption` | `false` | `true` encrypts the volume on the disks: each replica's backing volume is a LUKS2 container beneath DRBD. LVM pools only. See [Encrypted volumes](#encrypted-volumes) |
| `replicationTLS` | `false` | `true` encrypts the DRBD connections between the volume's nodes with kernel TLS. The nodes must be prepared with `haify replication-tls setup`. See [Encrypted volumes](#encrypted-volumes) |
| `drNode` | none | Haify node at another site that gets an asynchronous DR replica of every volume. See [Cross-site disaster recovery](#cross-site-disaster-recovery) |
| `drEndpoint` | required with `drNode` | The DR site's public address or host name, without a port |
| `drEgressAddress` | none | Source address the primary site's proxy uses to reach the DR site |
| `sharedFilesystem` | none | `nfs` makes every volume of the class a filesystem that many nodes mount at once (`ReadWriteMany`), served by a Haify NFS gateway. See [Shared filesystems](#shared-filesystems-readwritemany) |
| `nfsServiceIPs` | required with `sharedFilesystem` | The floating addresses the driver may give to shared volumes, one per volume: a comma-separated list of addresses and ranges, each with the prefix length of its network, such as `192.168.1.200-192.168.1.220/24,192.168.1.240/24` |
| `nfsAllowedClients` | every client | Networks or addresses allowed to mount the shared volumes, comma-separated (`10.42.0.0/16,192.168.1.0/24`) |

The four limits are written into the Pod's cgroup `io.max` when the volume is
published. They need cgroup v2 with the `io` controller enabled for pod
cgroups. Where that is missing, the volume is still published without limits,
and the node plugin logs why.

The first replica goes on the node the scheduler picked for the Pod
(`WaitForFirstConsumer` with `--strict-topology`). The others go to the nodes
whose pool has the most free space, and nodes in a fault domain that has no
replica yet come first. If the volume cannot be placed, provisioning fails with
`ResourceExhausted`, and external-provisioner lets the scheduler pick another
node for the Pod.

The filesystem is `ext4` unless the StorageClass sets
`csi.storage.k8s.io/fstype` (the image ships `e2fsprogs` and `xfsprogs`).

## Zones

On a cluster whose nodes carry `topology.kubernetes.io/zone` (and
`topology.kubernetes.io/region`), the node plugin publishes both as CSI
topology beside its own per-node key, and copies them onto the Haify node as
labels. A cluster without those labels behaves as before.

**Replicas in different zones.** Set `faultDomainLabel: zone` on the
StorageClass. Nothing has to be labelled by hand:

```yaml
parameters:
  pool: "vg0"
  replicas: "3"
  faultDomainLabel: "zone"
```

The first replica still goes on the node the scheduler picked for the Pod. The
others go to nodes in zones that have no replica yet, the ones with the most
free pool space first. When there are fewer zones than replicas, the rest share
a zone rather than fail.

**The labels on the Haify node.** Each Haify node gets two labels per value
(`haify node list` shows them):

| Haify node label | Value |
| ---------------- | ----- |
| `topology.kubernetes.io/zone`, `topology.kubernetes.io/region` | Always what the Kubernetes node says. The driver owns these two: a value set by hand is overwritten at the next sync |
| `zone`, `region` | The same value, unless you set the label yourself |

A `zone` or `region` label that you set with `haify node label` to something
other than the Kubernetes value is yours: the driver never changes or removes
it, and the node plugin logs a warning that names both values. Placement with
`faultDomainLabel: zone` then uses your value for that node. To spread by the
Kubernetes zone no matter what `zone` says, use
`faultDomainLabel: topology.kubernetes.io/zone`. To hand `zone` back to the
driver, remove your label (`haify node label <node> zone=`).

**When a Kubernetes label changes.** The node plugin compares the labels every
`--zone-sync-interval` (default `5m`) and when it starts, so a changed or
removed zone reaches the Haify node within that time, and a Haify controller
that was unreachable is caught up with later. Volumes that already exist keep
their replicas where they are. The topology Kubernetes holds for the node is
read when the kubelet registers the plugin, so after changing a node's zone
or region label, restart the plugin's pod on that node:

    kubectl -n kube-system delete pod -l app=haify-csi-node --field-selector spec.nodeName=<node>

**Where Pods can run.** A volume's PersistentVolume names the nodes that hold a
replica, not their zones, so a Pod runs on exactly those nodes, in whichever
zones they are. A volume with replicas in two zones can be used in both. With
`allowRemoteVolumeAccess: "true"` the volume has no node restriction and a Pod
can use it from any node in any zone.

**`allowedTopologies` with zones.** A StorageClass may list zones:

```yaml
allowedTopologies:
  - matchLabelExpressions:
      - key: topology.kubernetes.io/zone
        values: ["east-1a", "east-1b"]
```

With `volumeBindingMode: WaitForFirstConsumer` the scheduler keeps the Pod, and
with it the first replica, in those zones. With `Immediate` binding the driver
puts one replica in a listed zone, and the others on nodes in the listed zones
before any outside them. If no node in the listed zones hosts the pool with
enough room, provisioning fails with `ResourceExhausted`.

**Capacity.** The `CSIStorageCapacity` objects stay one per node; each now
also carries the node's zone and region.

**Turning it off.** `--zone-topology=false` on the node plugin
(`30-node.yaml`), or `nodePlugin.zoneTopology: false` in the chart. The plugin
then publishes the node alone and leaves the Haify node's labels as they are.
Reading the Node needs the `haify-csi-node` ClusterRole in `10-rbac.yaml`;
without it the plugin logs a warning and works as on a cluster with no zones.

## Encrypted volumes

Two StorageClass parameters cover two different things:

- `encryption: "true"` encrypts the volume **on the disks**. Each replica's
  backing volume is a LUKS2 container, and DRBD runs on top of it (DRBD → LUKS
  → LVM), so the pool disks hold only ciphertext. DRBD sits above the
  encryption, so what it sends between nodes is not encrypted by this.
- `replicationTLS: "true"` encrypts the DRBD connections **between nodes**
  with kernel TLS. It does not encrypt the disks.

Set both to have both:

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: haify-encrypted
provisioner: haify.csi.liliang-cn.com
parameters:
  pool: "vg0"
  replicas: "2"
  encryption: "true"
  replicationTLS: "true"
reclaimPolicy: Delete
allowVolumeExpansion: true
volumeBindingMode: WaitForFirstConsumer
```

`40-storageclass.yaml` has the same class, commented out. The PV records what
the volume was created with: `kubectl get pv <name> -o yaml` shows
`encryption: "true"` and `replicationTLS: "true"` under
`spec.csi.volumeAttributes`. On the Haify side, `haify resource status
<resource>` shows the resource as encrypted and each connection with `tls`.

### Where the keys are

Each node makes the key for its own replica, from `/dev/urandom` on the node,
and keeps it in `/etc/haify/luks`, readable by root only. The key is never sent
anywhere: not to the Haify controller, and not to Kubernetes. There is no
Kubernetes Secret to create and none to protect.

This is deliberate. The controller reaches the nodes only through SSH, where a
command line can be read in the node's process list and in the controller's
debug log. A passphrase kept in a Secret would have to travel that way to
every node. A key made on the node that never leaves it cannot leak in
transit.

What that means in practice:

- Encryption protects disks that leave the server: sent back for repair,
  decommissioned or stolen. It does not protect against someone who has the
  running server, because the key is on the same machine.
- There is no central copy of the keys. If a node loses its root disk, it
  loses its keys, and its replicas can no longer be read. The other replicas
  hold the same data under their own keys: remove the lost replica
  (`haify resource remove-replica`) and add a new one (`haify resource
  add-replica`), which gets a new key on its node and is copied back from the
  others. A volume with `replicas: "1"` whose node loses its root disk is
  lost.
- Deleting the volume overwrites and removes each node's key before the
  backing volume is removed.
- Encryption is fixed when the volume is created. A volume cannot be encrypted
  or decrypted later; copy it into a volume of another StorageClass instead.

### Requirements

- `encryption`: an LVM pool (ZFS has its own encryption) that is not VDO-backed.
  Every node the volume is placed on needs `cryptsetup` and the dm-crypt kernel
  module; a node without them is refused before anything is created. Each
  replica's logical volume is 32 MiB larger than the volume, for the LUKS
  header. A StorageClass that asks for encryption on a ZFS or VDO pool fails
  provisioning with `InvalidArgument`, and the PVC's events say why.
- `replicationTLS`: DRBD 9.2 or later, the kernel `tls` module and `tlshd`
  (package `ktls-utils`) on every node, prepared once with
  `haify replication-tls setup`. `haify replication-tls status` shows which
  nodes are ready and what the others lack.
  - Replicas are placed only on ready nodes. If fewer ready nodes have the pool
    than `replicas` asks for, provisioning fails with `FailedPrecondition`, and
    the PVC's events name each node that is not ready and why.
  - If the node the scheduler picked for the Pod is not ready, provisioning
    fails with `ResourceExhausted` and the scheduler picks another node.
  - The connections are switched to TLS right after the volume is created, one
    link at a time, before any data is copied into it. This is part of
    provisioning, so it counts against the provisioner's `--timeout`. If the
    switch fails, for example because the quorum tiebreaker Haify adds to a
    two-replica volume is on a node that is not prepared, the volume is
    deleted again and the PVC's events carry the error.
  - With `allowRemoteVolumeAccess: "true"`, a diskless client on a node that
    is not ready is refused when the volume is staged.

### Snapshots, restores and clones

- A snapshot of an encrypted volume is a snapshot of its ciphertext, on the
  node that took it.
- Restoring or cloning an encrypted volume needs a StorageClass with
  `encryption: "true"`. Any other class fails provisioning with
  `InvalidArgument`, so a plaintext copy of an encrypted volume is never made.
  The node holding the source opens the snapshot through a temporary read-only
  LUKS mapping with its own key, and the data is written into the new volume,
  which encrypts it under its own nodes' keys.
- A volume that is not encrypted can be restored or cloned into an encrypted
  class.
- `haify backup` of an encrypted volume stores plaintext on the backup target;
  protect the target with its own encryption and access policy.

## Changing a volume after provisioning (VolumeAttributesClass)

A StorageClass is read once, when the volume is created. To change the number
of replicas, the resource profile or the I/O limits of a volume that already
exists, while it stays in use, name a VolumeAttributesClass in its PVC.

Requirements: Kubernetes 1.34 or later (VolumeAttributesClass is GA there)
and csi-resizer v2.0.0 or later, which `20-controller.yaml` and the Helm chart
run. The resizer's RBAC already reads `volumeattributesclasses`.

```yaml
apiVersion: storage.k8s.io/v1
kind: VolumeAttributesClass
metadata: {name: haify-three-copies}
driverName: haify.csi.liliang-cn.com
parameters:
  replicas: "3"
  writeIOPS: "2000"
```

```bash
kubectl patch pvc data -p '{"spec":{"volumeAttributesClassName":"haify-three-copies"}}'
kubectl get pvc data -o jsonpath='{.status.currentVolumeAttributesClassName}{"\n"}{.status.modifyVolumeStatus}{"\n"}'
```

`currentVolumeAttributesClassName` shows the class once the change is done.
While it is under way, or when it cannot be done, `modifyVolumeStatus` says so
and the PVC's events carry the driver's message. A class Kubernetes reports as
`Infeasible` (a key the driver does not take, a value out of range, a shrink
that cannot be done) is not retried: name another class, such as the one the
volume had before. Other failures are retried.

A VolumeAttributesClass can only change these keys. Any other key is refused,
and nothing is changed:

| Parameter | Meaning |
| --------- | ------- |
| `replicas` | Number of diskful replicas. Growing places the new replicas the way provisioning does: on nodes that host the volume's pool and have room for it, nodes in a fault domain that has no replica yet first (by the `faultDomainLabel` the volume was created with), emptiest pool first. A node where the volume runs as a diskless client gets the first new replica when it has room, so its Pod then reads locally. The new replica syncs in the background. Shrinking removes replicas through the controller's `RemoveReplica` |
| `resourceProfile` | Makes the Haify resource a member of this profile, which applies the profile's DRBD options to it. An empty value takes it out of its profile. The profile's replica count is not applied; set `replicas` for that |
| `readBytesPerSecond`, `writeBytesPerSecond`, `readIOPS`, `writeIOPS` | New I/O limits, in the same form as the StorageClass parameters. `max` lifts a limit. A key the class leaves out keeps its current value |

What shrinking will not do:

- go below two diskful replicas; Haify keeps at least two copies of a
  replicated volume;
- remove the replica on a node where the volume is Primary (in use), or one
  whose state the controller cannot see; the driver retries until it can;
- leave no `UpToDate` copy, for example while a recently added replica is
  still syncing; the driver retries until the sync is done;
- remove a node named in the PV's node affinity (below).

A replica that is not `UpToDate` goes first, then one that shares its fault
domain with another replica, then the one on the fullest pool.
A volume shrunk to two replicas gets a diskless quorum tiebreaker again, on
the node whose replica was removed, so losing either remaining node does not
suspend its I/O.

**Node affinity.** Kubernetes writes a PV's node affinity once and never
changes it. A volume with `allowRemoteVolumeAccess: "true"` has none, so its
Pods run anywhere and replicas can be added and removed freely. Any other
volume pins its Pods to the nodes it was first created on. Adding replicas to
such a volume makes the data safer, but its Pods still run only on the
original nodes. Its original nodes are never removed, because a Pod scheduled
to one could no longer use the volume there; only replicas added later can
go. The driver records this in the resource labels `haify.csi/topology`
(`any`, or the original nodes joined by `+`) and `haify.csi/fault-domain-label`
when it provisions a volume. A volume provisioned by an older driver has no
such label: the first change records its current replicas as its original
nodes. For such a volume that allows remote access, set the label yourself
(`haify resource label <resource> haify.csi/topology=any`) to let every
replica be removed.

**I/O limits of a running Pod.** A volume's context, where the StorageClass's
limits are kept, is fixed at provisioning, so the driver records a changed
limit in the resource label `haify.csi/qos-<parameter>`, which wins over the
StorageClass's value. Each node plugin reads the labels of the volumes it has
published every `--qos-interval` (default `30s`, one `GetResource` per
volume) and rewrites the Pod's `io.max` when the limits changed; a newly
published volume gets them at once. The plugin keeps its list of published
volumes in `/csi/qos` (on the host,
`/var/lib/kubelet/plugins/haify.csi.liliang-cn.com/qos`), so a restarted
plugin still keeps them current.

**At creation.** When external-provisioner passes a PVC's class to
CreateVolume, its parameters override the StorageClass's. external-provisioner
v6 and later do this (they need Kubernetes 1.34); the v5 provisioner these
manifests run does not read VolumeAttributesClasses, so name the class on the
PVC after it is bound, or run a v6 provisioner.

## Live migration of virtual machines (KubeVirt, OpenShift Virtualization)

KubeVirt live-migrates a VM only when its disks are `ReadWriteMany`. Give the
VM's disk PVC `accessModes: [ReadWriteMany]` and `volumeMode: Block`:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: vm-disk}
spec:
  accessModes: [ReadWriteMany]
  volumeMode: Block
  resources: {requests: {storage: 20Gi}}
```

The VM then reports `LiveMigratable=True`. During a migration the target
node stages the volume while the source still runs the guest: the node plugin
allows two DRBD Primaries for that window (the resource's
`allow-two-primaries`), the guest's memory moves, the source unstages, and
two Primaries are disallowed again as soon as one is left. The hypervisor
writes from one side at a time; Haify only lets both sides open the device.
A node without a replica takes part as a diskless client when the
StorageClass sets `allowRemoteVolumeAccess: "true"`. Resources replicated
over WAN (protocol A) are refused two Primaries.

## Shared filesystems (ReadWriteMany)

A StorageClass with `sharedFilesystem: nfs` provisions filesystems that Pods on
several nodes mount at the same time:

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: haify-shared
provisioner: haify.csi.liliang-cn.com
parameters:
  pool: "vg0"
  replicas: "2"
  sharedFilesystem: "nfs"
  nfsServiceIPs: "192.168.1.200-192.168.1.220/24"
reclaimPolicy: Delete
allowVolumeExpansion: true
volumeBindingMode: Immediate
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: shared-data}
spec:
  storageClassName: haify-shared
  accessModes: [ReadWriteMany]
  resources: {requests: {storage: 20Gi}}
```

Each volume is a replicated Haify resource with an NFS gateway of its own in
front of it. The gateway runs on one of the volume's replica nodes and serves
the filesystem on a floating address; every node with a Pod using the volume
mounts it from that address. When the node serving it fails, the gateway and
its address move to another replica, and the Pods keep their mounts: their I/O
waits for the few seconds the move takes and then continues. `haify gateway
list` shows the gateways, named after their volumes.

### Service addresses

Every shared volume takes one address from `nfsServiceIPs` for as long as it
exists, so the pool sets how many shared volumes the class can hold. A PVC
that finds the pool used up stays `Pending` with `ResourceExhausted` until an
address is freed by deleting a volume or the pool is widened.

- Write each entry with the prefix length of the network the address is in
  (`/24` for a node network of `192.168.1.0/24`), not the size of the range.
- The addresses must be in a network the Haify storage nodes are attached to,
  reachable from every Kubernetes node, and used by nothing else: keep them out
  of DHCP ranges, MetalLB pools and kube-vip.
- Addresses already held by another shared volume, by a gateway created with
  `haify gateway`, or by a Haify node are skipped.
- Several StorageClasses may name the same pool.
- Editing the pool of a StorageClass affects new volumes only. A volume keeps
  its address for life; it is recorded on the Haify resource as the label
  `haify.csi/nfs-service-ip`.

### Requirements

- The storage nodes need what any Haify NFS gateway needs: the NFS server and
  the OCF resource agents (`nfs-kernel-server` on Debian and Ubuntu,
  `nfs-utils` on RHEL and Rocky; `resource-agents-extra` on Ubuntu,
  `resource-agents` on Debian, RHEL and Rocky). Provisioning fails with the
  name of the missing piece otherwise.
- Kubernetes nodes need the NFS client. The driver's image carries it. When it
  is missing, the Pod's event says so: `the NFS client is missing: mount.nfs
  was not found`.
- Kubernetes nodes need not be Haify nodes to mount a shared volume, and Pods
  are not tied to the nodes that hold the replicas.

### Mount options

Volumes are mounted with NFSv4. The StorageClass's `mountOptions` are passed to
the mount, and a version given there (`vers=4.1`, `nfsvers=4.2`) is used
instead:

```yaml
mountOptions: ["nfsvers=4.2", "noatime"]
```

The mount is a hard mount unless `soft` is given; with `soft`, I/O in flight
during a failover returns an error to the application instead of waiting.

### What differs from other volumes

| | Shared volume |
| --- | --- |
| `volumeMode: Block` | Refused: the volume is a filesystem. |
| `csi.storage.k8s.io/fstype` | The filesystem is `ext4`, formatted on the storage node. Another value is refused. |
| Expansion | With `allowVolumeExpansion: true`, raising the PVC's request grows the volume and its filesystem while the Pods keep their mounts; see [Expanding a shared volume](#expanding-a-shared-volume). |
| Snapshots, group snapshots | Work as for other volumes. |
| Restore and clone | A shared volume can be created from a snapshot or a clone of another shared volume, at the same size; expand it afterwards for a larger one. Other sources, other sizes and snapshots kept on a backup target are refused. |
| Restore and clone into another class | A snapshot or clone of a shared volume can become a `ReadWriteOnce` volume of an `ext4` class. |
| I/O limits (`readIOPS`, ...) | Refused in the StorageClass and in a VolumeAttributesClass. |
| `replicas`, `resourceProfile` (VolumeAttributesClass) | Work as for other volumes. |
| `encryption`, `replicationTLS` | Work as for other volumes. The NFS traffic between the Pods' nodes and the service address is not encrypted by either. |
| `drNode` | Refused: the NFS service does not move to the DR site with the data. |
| `allowRemoteVolumeAccess` | Not needed: every node can mount a shared volume. |
| File ownership | Every client writes as `root`; the files of all Pods are readable and writable by all of them. |
| Node failover ("When a node dies") | Pods are not moved when a replica node drops out: the volume stays available to them through the gateway. |

Deleting the PVC removes the gateway, frees its address and deletes the
resource.

### Expanding a shared volume

Raise the PVC's `spec.resources.requests.storage`, as for any other volume.
The volume grows on every replica, and the filesystem is grown on the storage
node that is serving the volume's NFS gateway. The Pods see the new size on
their existing mounts and are not restarted; no node of the cluster has
anything to do.

```bash
kubectl patch pvc shared-data -p '{"spec":{"resources":{"requests":{"storage":"40Gi"}}}}'
kubectl get pvc shared-data          # CAPACITY shows 40Gi once it is done
```

The filesystem can only be grown where it is mounted. If the gateway is
moving to another node at that moment, the PVC keeps its old capacity, shows
a `VolumeResizeFailed` event saying the filesystem is mounted nowhere, and
the resize is repeated by itself until it goes through, usually within the
minute. Nothing has to be done by hand, and the PVC never reports a size its
Pods do not see.

`haify resource resize-volume <volume> 0 <size>` on the Haify side grows the
filesystem in the same way, but does not change what the PVC reports; resize
through the PVC.

## OpenShift and MicroShift

The Helm chart works on OpenShift and MicroShift as it is. Where the cluster
has the SecurityContextConstraints API, the chart lets the two plugins'
service accounts use the built-in `privileged` SCC: the node plugin runs
privileged on the host network and the controller plugin on the host network,
which the default `restricted-v2` SCC refuses. `--set openshift.enabled=true`
renders the same objects for an offline `helm template`.

Applications need nothing special. A Pod admitted under `restricted-v2`, with
the random user ID OpenShift assigns, owns what it writes on the volume:
kubelet applies the Pod's `fsGroup` to the filesystem.

On MicroShift running on RHEL, CentOS Stream or Ubuntu, the nodes are Haify
nodes like any other: the DRBD 9 kernel module and `drbd-utils` installed, and
root SSH from the controller.

OpenShift Container Platform nodes run RHCOS, which takes no packages and no
root SSH. They mount volumes as local-agent nodes, below, with the DRBD module
loaded through the Kernel Module Management operator
([DRBD on Kubernetes nodes](../../docs/kubernetes-drbd.md#openshift-kernel-module-management)).
The data lives on Haify storage nodes outside the cluster, or on cluster nodes
that run RHEL.

## Nodes without SSH (local agent)

A node the controller cannot SSH into can still run Pods with Haify volumes.
With

```yaml
nodePlugin:
  localAgent: true
```

the node plugin registers its node itself and becomes its agent: it writes the
DRBD configuration of the node's volumes to the host's `/etc/drbd.d`, brings
them up, and makes the node Primary when a Pod starts there and Secondary when
it leaves. The image carries `drbd-utils`; the node needs only the DRBD 9
kernel module.

Such a node holds no replica. It reads and writes its volumes over the network
from the nodes that hold them, so its volumes need
`allowRemoteVolumeAccess: "true"` on the StorageClass. It is never chosen as a
tiebreaker either.

The setting can be on for every node. A node the controller already reaches
over SSH stays managed that way, and its plugin works as before; `haify node
list` shows `local agent` in the VERSION column for the others. Registering a
local-agent node with `haify node register` later makes it an SSH node; restart
its node plugin afterwards.

The controller counts a local-agent node offline when its plugin has not
reported for a minute. Resources that replicate over kernel TLS cannot be
attached to one, because the controller cannot install its certificate there.

## What the driver supports

- Expansion: with `allowVolumeExpansion: true` and the `csi-resizer` sidecar,
  raising a PVC's request grows the LV, the DRBD device and the filesystem
  while the volume stays in use. That includes shared (`ReadWriteMany`)
  volumes, whose filesystem is grown on the storage node serving them.
- Changing a volume in place: replicas, resource profile and I/O limits through
  a VolumeAttributesClass (Kubernetes 1.34+), see above.
- Capacity-aware scheduling: with `storageCapacity: true` and the provisioner's
  `--enable-capacity`, each node's free pool space is published as a
  `CSIStorageCapacity` object. The scheduler then skips nodes that cannot hold
  the volume.
- Usage metrics: `NodeGetVolumeStats` feeds `kubelet_volume_stats_*` for
  filesystem volumes. Kubelet does not collect usage for raw block volumes.
- Raw block: `volumeMode: Block` hands the Pod the DRBD device itself.
- Access modes: `ReadWriteOnce` and `ReadWriteOncePod`, and `ReadWriteMany`
  with `volumeMode: Block` for live migration of virtual machines (see
  above). A StorageClass with `sharedFilesystem: nfs` adds `ReadWriteMany`,
  `ReadOnlyMany` and `ReadWriteOnce` filesystems that Pods on any number of
  nodes mount at once (see
  [Shared filesystems](#shared-filesystems-readwritemany)). From any other
  class, `ReadWriteMany` filesystems and the other multi-node modes are
  refused at provisioning: a DRBD resource has one Primary, and a filesystem
  on it cannot be written from two nodes.
- Replica health on the PVC and its Pods: the controller plugin checks each
  volume's replicas every `--health-interval` (default `1m`). When a replica
  disconnects, loses its disk, falls out of date or resyncs, it posts a
  `Warning VolumeDegraded` event on the PVC, and `Normal VolumeRecovered` when
  the replica is healthy again. The same event goes on every Pod using the PVC
  at that moment, with the claim's name in front of the message
  (`PVC data: ...`), so `kubectl describe pod` shows it too. Events are posted
  only when the state changes, not every check. The first sync of a new volume
  is not reported. Neither is the sync of a replica that is being added to a
  volume which keeps all the replicas it had: one that moves to the node
  where the volume is used (`[self_heal] auto_diskful`), a replica move or
  rebalance, or a VolumeAttributesClass that raises `replicas`. Such a volume
  is reported as soon as fewer replicas are up to date and connected than it
  had before the addition, or any replica disconnects, loses its disk or
  falls out of date. Kubernetes keeps events for an hour by default.
- Zones: each node's `topology.kubernetes.io/zone` and `region` are published
  as topology and copied onto the Haify node, so `faultDomainLabel: zone`
  spreads replicas across zones (see [Zones](#zones)).
- Node maintenance: a node taken down for maintenance is drained in Haify
  first and returned to service afterwards (see
  [Draining a node for maintenance](#draining-a-node-for-maintenance)).
- Node failover: when a node dies, the controller plugin moves the pods using
  its volumes to a node with a replica in about half a minute, rather than
  after Kubernetes' 5 minutes (see "When a node dies").

## Volume snapshots

The driver implements `CREATE_DELETE_SNAPSHOT` and `LIST_SNAPSHOTS`. A
`VolumeSnapshot` of a Haify volume becomes an LVM/ZFS snapshot of that volume's
backing store. Kubernetes backup tools such as Velero and Kasten can drive
these snapshots.

Each snapshot sits on one storage node, and there is no cluster-wide list of
them. So `ListSnapshots` goes through the driver's volumes and asks each replica
node. It lists only snapshots the driver created (`haifysnap_*`), not the
controller's own scheduled snapshots of the same LV.

### Cluster prerequisites (install once)

k3s and most distributions do not ship the snapshot machinery. Install the
CRDs and the snapshot-controller from
[kubernetes-csi/external-snapshotter](https://github.com/kubernetes-csi/external-snapshotter)
before applying `50-volumesnapshotclass.yaml`:

```bash
V=v8.2.0
B=https://raw.githubusercontent.com/kubernetes-csi/external-snapshotter/$V
kubectl apply -f $B/client/config/crd/snapshot.storage.k8s.io_volumesnapshotclasses.yaml \
              -f $B/client/config/crd/snapshot.storage.k8s.io_volumesnapshotcontents.yaml \
              -f $B/client/config/crd/snapshot.storage.k8s.io_volumesnapshots.yaml
kubectl apply -f $B/deploy/kubernetes/snapshot-controller/rbac-snapshot-controller.yaml \
              -f $B/deploy/kubernetes/snapshot-controller/setup-snapshot-controller.yaml
```

### Taking a snapshot

```yaml
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata: { name: data-snap }
spec:
  volumeSnapshotClassName: haify-drbd-snapshot
  source:
    persistentVolumeClaimName: data
```

With `deletionPolicy: Delete`, as in the shipped class, deleting the
`VolumeSnapshot` also deletes the LVM/ZFS snapshot. Use `Retain` if the
snapshot must stay after the Kubernetes objects are gone.

### What a snapshot is (and is not)

A snapshot is taken on one node's local backing volume, and DRBD does not
copy it to other nodes. It protects against bad writes, accidental deletion or
a failed migration, but not against losing that node. DRBD copies every write,
including the destructive ones, so replication is not a backup either. To
survive losing the whole cluster, ship snapshots off-site: see
[Snapshots that leave the cluster](#snapshots-that-leave-the-cluster).

### Restoring and cloning

Both work through a PVC `dataSource`:

```yaml
# restore a snapshot into a new volume
spec:
  dataSource: { name: my-snap, kind: VolumeSnapshot, apiGroup: snapshot.storage.k8s.io }

# clone an existing volume
spec:
  dataSource: { name: my-pvc, kind: PersistentVolumeClaim }
```

The new volume is created empty. Then the source data is written into the new
volume's DRBD device on the node that holds the source, and DRBD copies it to
the other replicas like any other write. So that node always gets a replica of
the new volume. If it cannot hold one, provisioning fails with
`ResourceExhausted`.

- A clone snapshots the source first. Reading a mounted volume's disk directly
  could copy it in the middle of a write, so the clone takes an internal
  snapshot, copies from that, and then deletes it.
- Both work on LVM and ZFS pools. A ZFS snapshot has no block device of its
  own, so the controller reads it through a temporary read-only clone,
  `<pool>/haify-populate-<new volume>-0`, and destroys the clone after the copy.
- A copy of an encrypted volume has to be encrypted too; see
  [Encrypted volumes](#snapshots-restores-and-clones).
- `CreateVolume` waits until the copy is done. The shipped provisioner
  `--timeout` is `120s`; raise it for large volumes. If the copy fails, the
  half-written volume is deleted, so a retry starts clean. Only a controller
  crash in the middle of the copy can leave an empty volume behind.

A snapshot is crash-consistent: it holds what was on disk at that moment. If a
database needs more than that, snapshot it after a `CHECKPOINT`/`FSYNC`, or
pause its writes first.

## Volume group snapshots

An application that keeps its data on more than one PVC, such as a database
with its write-ahead log on a volume of its own, can only be recovered from
snapshots that are all from the same moment. A `VolumeGroupSnapshot` takes
them that way. The driver suspends I/O on every volume in the group (each
volume's Primary first), takes one snapshot of each, and resumes I/O. The
snapshots then hold what a power cut at that moment would have left on all
the volumes together.

- Writes to every volume in the group wait while the snapshots are taken:
  one `lvcreate` or `zfs snapshot` per volume, run over SSH on the storage
  nodes.
- A timer on each storage node resumes I/O after one minute whatever happens
  to the Haify controller. A group that takes nearly that long fails, and its
  snapshots are deleted, because I/O may have resumed in the middle of it.
  Snapshot fewer volumes at once if that happens.
- It is all or nothing: if one snapshot fails, the ones already taken are
  deleted.
- Each member is an ordinary snapshot of this driver, on one node's backing
  volume. It appears as a `VolumeSnapshot` and is restored through a PVC
  `dataSource` like any other.
- Deleting the `VolumeGroupSnapshot` with `deletionPolicy: Delete` deletes
  every member's LVM/ZFS snapshot.
- Every PVC in the group must be a volume this driver provisioned. A PV made
  by hand for some other Haify resource is refused.

### Cluster prerequisites

Group snapshots need, from one external-snapshotter release (v8.2.0 or later):

1. The three group snapshot CRDs, as well as the snapshot CRDs above.
2. The snapshot-controller started with `--feature-gates=CSIVolumeGroupSnapshot=true`.
3. The same gate on the `csi-snapshotter` sidecar. In `20-controller.yaml` it
   is commented out; uncomment it. Turn it on only after the CRDs are
   installed: with the gate on and the CRDs missing, the sidecar stops handling
   ordinary `VolumeSnapshot`s too.

```bash
V=v8.2.0
B=https://raw.githubusercontent.com/kubernetes-csi/external-snapshotter/$V
kubectl apply -f $B/client/config/crd/groupsnapshot.storage.k8s.io_volumegroupsnapshotclasses.yaml \
              -f $B/client/config/crd/groupsnapshot.storage.k8s.io_volumegroupsnapshotcontents.yaml \
              -f $B/client/config/crd/groupsnapshot.storage.k8s.io_volumegroupsnapshots.yaml
kubectl -n kube-system patch deployment snapshot-controller --type=json \
  -p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--feature-gates=CSIVolumeGroupSnapshot=true"}]'
kubectl apply -f deploy/k8s/examples/volumegroupsnapshotclass.yaml
```

The API version of the group snapshot objects depends on the release:
`groupsnapshot.storage.k8s.io/v1beta1` for v8.2 and v8.3 (the shipped
`csi-snapshotter:v8.2.0`), `v1beta2` for v8.4 and v8.5, and `v1` from v8.6.
Use the version your CRDs serve in the class and in every `VolumeGroupSnapshot`,
and keep the snapshot-controller and the `csi-snapshotter` image on the same
release.

### Taking a group snapshot

Label the PVCs that belong together, then select them:

```yaml
apiVersion: groupsnapshot.storage.k8s.io/v1beta1
kind: VolumeGroupSnapshot
metadata: { name: db-before-upgrade }
spec:
  volumeGroupSnapshotClassName: haify-group-snapshot
  source:
    selector:
      matchLabels: { app: db }
```

`kubectl get volumesnapshot` then lists one `VolumeSnapshot` per PVC.

## Snapshots that leave the cluster

A VolumeSnapshotClass with `backupTarget` also uploads every snapshot to a
Haify backup target (S3, SMB or WebDAV; see `haify backup target add` in the
user guide, section 9). The copy on the target survives losing the node, and
the whole cluster.

```yaml
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshotClass
metadata:
  name: haify-snapshot-offsite
driver: haify.csi.liliang-cn.com
deletionPolicy: Delete
parameters:
  backupTarget: offsite          # a name from `haify backup target list`
  keepLocalSnapshot: "true"      # "false" deletes the local snapshot once uploaded
```

- The local snapshot is taken first, as with any snapshot, and that moment is
  the snapshot's point in time. Haify then uploads that same snapshot, read on
  the node that holds it, not a newer one. The `VolumeSnapshot` reports
  `readyToUse: false` until the upload has finished.
- Each upload is a full, compressed image of the volume. The images are not
  chained as incrementals, so each one can be deleted on its own, in any order.
- The backup appears in `haify backup list` with the id
  `<volume>_<snapshot>`, for example
  `pvc_3f2a..._haifysnap_snapshot_8c1d...`. The snapshot handle is
  `<volume>/<node>/<snapshot>/backup`.
- With `keepLocalSnapshot: "false"` the local snapshot is deleted once the
  upload has finished and takes no pool space. A restore then reads the backup.
- Deleting the `VolumeSnapshot` (with `deletionPolicy: Delete`) deletes the
  backup and the local snapshot. An upload that is still running is stopped
  first. A backup under S3 Object Lock cannot be deleted before its lock
  expires: until then the delete fails, is retried, and the reason is in the
  `VolumeSnapshotContent`'s events.
- A failed upload shows as an error on the `VolumeSnapshotContent` and starts
  again. So does an upload cut off by a restart of the controller plugin.
- LVM-backed volumes only: Haify does not ship backups of ZFS-backed volumes.
  `rclone` must be installed on the storage nodes, as for every backup.
- Give these snapshots a target of their own. A backup of a snapshot keeps no
  base for an incremental, so on a target that a `haify backup schedule` also
  writes to, the scheduled backup after it is full.

Restoring works through the same `dataSource` as any snapshot. When the local
snapshot is still on its node, the new volume is copied from it. Otherwise the
volume is created and written from the backup. That takes as long as reading
the image from the target, so the provisioner is answered `Aborted` until it
is done, and retries. The PVC stays `Pending` meanwhile, and its events say
the volume is being restored. Until the restore has finished, the volume
carries the Haify label `haify.csi/restoring-from=<backup id>`. If the
controller plugin restarts during the restore, the half-written volume is
deleted and the restore starts again.

### Restoring in another Kubernetes cluster

Another cluster can restore these snapshots once its Haify knows the backups.

1. In the other cluster's Haify, add the same target (same bucket and
   `--prefix`) and import its backups:

   ```bash
   haify backup target add --name offsite --kind s3 --bucket haify-backups \
       --endpoint https://s3.example.com --user AKIAEXAMPLE
   haify backup import --target offsite
   haify backup list
   ```

2. Find the snapshot handle. In the first cluster it is
   `kubectl get volumesnapshotcontent <name> -o jsonpath='{.status.snapshotHandle}'`.
   Without that cluster, build it from `haify backup list`: a backup with id
   `<volume>_<snapshot>` and node `<node>` has the handle
   `<volume>/<node>/<snapshot>/backup`.

3. Create a pre-provisioned `VolumeSnapshotContent` and a `VolumeSnapshot`
   bound to it. Use `deletionPolicy: Retain`: the backup is on a target the
   first cluster still uses, and `Delete` would remove it from there.

   ```yaml
   apiVersion: snapshot.storage.k8s.io/v1
   kind: VolumeSnapshotContent
   metadata:
     name: data-snap-imported
   spec:
     driver: haify.csi.liliang-cn.com
     deletionPolicy: Retain
     source:
       snapshotHandle: pvc_3f2a.../node1/haifysnap_snapshot_8c1d.../backup
     sourceVolumeMode: Filesystem
     volumeSnapshotRef:
       name: data-snap
       namespace: app
   ---
   apiVersion: snapshot.storage.k8s.io/v1
   kind: VolumeSnapshot
   metadata:
     name: data-snap
     namespace: app
   spec:
     source:
       volumeSnapshotContentName: data-snap-imported
   ```

4. Create a PVC from it, at least as large as the snapshot's `restoreSize`:

   ```yaml
   apiVersion: v1
   kind: PersistentVolumeClaim
   metadata: {name: data, namespace: app}
   spec:
     storageClassName: haify-drbd
     accessModes: [ReadWriteOnce]
     resources: {requests: {storage: 10Gi}}
     dataSource: {name: data-snap, kind: VolumeSnapshot, apiGroup: snapshot.storage.k8s.io}
   ```

The source volume and its node do not exist in that cluster, so the volume is
written from the backup, as described above.

### Velero

- Velero's CSI snapshot data mover works with Haify as with any CSI driver
  that supports snapshots: Velero takes a `VolumeSnapshot`, reads a volume
  restored from it, and uploads the files to Velero's own storage. That is a
  separate copy from the one `backupTarget` makes.
- A Velero backup without the data mover keeps only the `VolumeSnapshot`
  objects and leaves the data to the driver. With a `backupTarget` class the
  data is on the Haify target, so such a backup can be restored in a cluster
  whose Haify has imported that target's backups.

## Cross-site disaster recovery

A StorageClass with `drNode` gives every volume an asynchronous replica at
another site. Each volume is a Haify WAN resource (user guide, section 13):
replicas at this site as usual, plus one on `drNode`, reached through a
`haify-proxy` pair over mTLS.

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: haify-dr
provisioner: haify.csi.liliang-cn.com
parameters:
  pool: vg0
  replicas: "2"
  drNode: dr1                    # a Haify node at the other site
  drEndpoint: dr.example.com     # the DR site's public address, no port
  # drEgressAddress: 203.0.113.20
reclaimPolicy: Delete
allowVolumeExpansion: true
volumeBindingMode: WaitForFirstConsumer
```

- `drNode` must be a registered Haify node that hosts the pool. It never
  holds one of the local replicas and is not in the volume's topology, so no
  Pod of this cluster runs on it. A node that does not exist or lacks the pool
  is refused with `InvalidArgument`. When the scheduler picks the DR node for
  the Pod, provisioning fails with `ResourceExhausted` and the scheduler picks
  another node.
- Each volume gets its own WAN port, a random one above 3000. The DR site
  must accept TCP from the primary site on those ports;
  `haify resource status <volume>` shows a volume's port and whether it is
  reachable.
- Replication to the DR site is asynchronous (DRBD protocol A). The
  application does not wait for the DR site, and the DR copy is behind by
  whatever is still in flight or in the proxy's buffer: up to the buffer size
  (4 GiB per connection by default, `haify wan tune`). While the link is down,
  the primary keeps writing and the DR copy falls further behind, until the
  link returns and DRBD resyncs. A failover loses everything that had not
  reached the DR site. `haify resource status` shows how much is buffered, and
  `haify wan sizing <volume>` says whether the link keeps up with the writes.

### Failing over to the DR site

Failover is manual, because the DR copy can be behind. The steps need a Haify
controller that is reachable from the DR site and knows the volumes. A
controller that ran only at the lost site is gone with it, so decide where the
controller runs before you need it.

1. Find the Haify name of each volume: the PV's `spec.csi.volumeHandle`. Keep
   the list before you need it:

   ```bash
   kubectl get pv -o custom-columns=PV:.metadata.name,CLAIM:.spec.claimRef.name,NS:.spec.claimRef.namespace,VOLUME:.spec.csi.volumeHandle
   ```

   Haify also labels each volume `haify.csi/pvc=<namespace>/<name>`.

2. Promote the DR replica of each volume:

   ```bash
   haify resource dr-failover pvc_3f2a...          # prints what it will do
   haify resource dr-failover pvc_3f2a... --yes
   ```

   It refuses while a primary-site node still answers and is Primary for the
   volume: then the site is not lost, and promoting the DR node would leave
   two Primaries. Stop the workload there first.

3. In the DR site's Kubernetes cluster, install the Haify CSI driver pointing
   at that controller. The DR node must be a Kubernetes node there, with the
   same name as the Haify node. Then bind the volume with a static PV and a
   PVC:

   ```yaml
   apiVersion: v1
   kind: PersistentVolume
   metadata:
     name: data-dr
   spec:
     capacity: {storage: 10Gi}
     accessModes: [ReadWriteOnce]
     persistentVolumeReclaimPolicy: Retain   # deleting the PV must not delete the volume
     storageClassName: ""
     volumeMode: Filesystem                  # Block for a raw block volume
     csi:
       driver: haify.csi.liliang-cn.com
       volumeHandle: pvc_3f2a...             # the Haify volume name
       fsType: ext4
     nodeAffinity:
       required:
         nodeSelectorTerms:
         - matchExpressions:
           - {key: haify.csi.liliang-cn.com/node, operator: In, values: [dr1]}
   ---
   apiVersion: v1
   kind: PersistentVolumeClaim
   metadata: {name: data, namespace: app}
   spec:
     storageClassName: ""
     volumeName: data-dr
     accessModes: [ReadWriteOnce]
     resources: {requests: {storage: 10Gi}}
   ```

4. Start the workload. The node plugin on the DR node mounts the filesystem
   that is already there.

### Failing back

When the primary site is back:

1. Keep the workload at the primary site stopped: its nodes must not have the
   volume mounted.
2. Run `haify resource dr-failback <volume> --wait 30m` while the workload at
   the DR site keeps running. It reconnects the primary-site nodes,
   **discarding what they wrote after they were cut off**, and resyncs them
   from the DR replica. Run it again until it reports done. Its last step needs
   the DR node to let go of the volume: before that, stop the workload at the
   DR site (scale it to 0), so the node plugin unmounts and demotes the volume.
3. `dr-failback` leaves a primary-site node Primary (`--node`, default the
   first). Demote it, so the node plugin can promote the volume wherever the
   Pod starts: `haify resource secondary <volume> <node>`.
4. Start the workload at the primary site, and delete the static PV and PVC
   in the DR cluster (`Retain` keeps the volume).

## Draining a node for maintenance

`kubectl drain` moves the Pods off a node, and Haify needs to know about the
maintenance too: otherwise the node is still offered new replicas, and a
gateway, an HA service or the controller's own metadata that is Primary on it
stays there until the node is switched off and only then fails over. The
controller plugin's drain controller drains the node in Haify as well, which
is what `haify node drain <node>` does, and returns it to service afterwards.
It is on by default.

**Which nodes it drains.** A cordon alone does not say that a node is going
down, so by default the controller acts on a node only when it is told:

- the node has the annotation or label `haify.csi/drain: "true"`;
- or the node carries a maintenance taint: `medik8s.io/drain` (Node Maintenance
  Operator) or `kubevirt.io/drain`. `--node-drain-taints` changes the list.

```bash
kubectl annotate node worker-2 haify.csi/drain=true
kubectl drain worker-2 --ignore-daemonsets --delete-emptydir-data
# ... maintenance ...
kubectl uncordon worker-2
kubectl annotate node worker-2 haify.csi/drain-
```

With `--node-drain-on-cordon` every cordoned node is treated the same way, and
`kubectl drain` and `kubectl uncordon` are all it takes. Use it when nodes in
your cluster are cordoned only to take them down. `haify.csi/drain: "false"` on
a node keeps the controller away from it in either mode.

**When it acts.** The node must have asked for `--node-drain-delay` (default
`30s`) without a break. From then on the drain has two steps.

**What it does first.** As soon as the delay is over, while Pods may still be
leaving, the node is put in maintenance in Haify: it takes no new replicas
from that moment. Every gateway, HA service, database app and the
controller's metadata that is Primary there moves to another node. The
volumes of the Pods still on the node are not touched: such a volume is
Primary where its Pod runs and moves with the Pod. The node gets a
`Normal HaifyNodeDrainWaiting` event saying that it takes no new replicas,
what moved, and which Pods the drain still waits for. Pods on
[shared filesystems](#shared-filesystems-readwritemany) are not waited for:
they mount NFS, and the volume's gateway is among what moved.

**What it does when the Pods are gone.** Once no Pod on the node uses a Haify
volume any more, the drain is finished: whatever is still Primary on the node
moves to another node, and the node gets a `Normal HaifyNodeDrained` event
listing what moved. The replicas on the node stay and resync by themselves
once the node is back.

If something could not be moved in either step, the event is
`Warning HaifyNodeDrainFailed` with the controller's reason, and the step is
tried again every minute.

`haify node list` shows the node in maintenance with who drained it and why,
for example `maintenance (kubernetes drain: taint medik8s.io/drain)`.

**Afterwards.** When the node stops asking (the annotation or taint is gone,
or with `--node-drain-on-cordon` the node is uncordoned), the controller
returns it to service in Haify and posts `HaifyNodeUndrained`. Nothing is
moved back; `haify ha evict` moves a service if you want it elsewhere.

**One node at a time.** While one node is drained in Haify, another that asks
waits (`--node-drain-max-nodes`, default `1`), with an event saying which node
it waits for. A node drained with `haify node drain` counts.

**What it leaves alone.**

- A node that was drained in Haify by hand (`haify node drain`). It stays
  drained after an uncordon until `haify node undrain`.
- A node Kubernetes reports `NotReady`, or that still has the
  `haify.csi/lost-quorum` taint: it is down, its services have failed over, and
  the HA controller moves its Pods (see [When a node dies](#when-a-node-dies)).
  If it comes back still asking to be drained, it is drained then.
- A Kubernetes node that is not a Haify node.

**Maintenance and node failure.** A node whose drain has finished and that is
still cordoned is switched off on purpose, so the HA controller does not taint
it or delete Pods on it. A node that goes down before its drain finished,
with Pods still on it, is treated as any dead node.

The controller records what it did in the node's `haify.csi/drain-state`
annotation (`draining` from the first step, `drained` after the second) and
removes it when the node is back in service. A restarted plugin, or another replica taking over, continues from it
and repeats nothing. Do not edit it by hand.

**Tuning and turning it off.** The flags are on the controller plugin
(`20-controller.yaml`): `--node-drain=false` turns it off;
`--node-drain-on-cordon`, `--node-drain-taints`, `--node-drain-delay`,
`--node-drain-interval` and `--node-drain-max-nodes` tune it. With the Helm
chart, set `nodeDrain.enabled`, `nodeDrain.onCordon`, `nodeDrain.taints`,
`nodeDrain.delay`, `nodeDrain.interval` and `nodeDrain.maxNodes`. With more
than one controller plugin replica, only the one holding the
`haify-csi-node-drain` Lease acts. Its permissions are the
`haify-csi-node-drain` ClusterRole in `10-rbac.yaml`.

## When a node dies

When a node loses power, the replacement pod starts on a surviving replica
node with its data intact. Nothing needs cleaning up on the Haify side, and the
old node's replica resyncs by itself when the node comes back.

On its own, Kubernetes evicts pods from an unreachable node only after 5
minutes (the 300-second tolerations it adds to every pod that sets none), and
it never replaces a StatefulSet pod until someone force-deletes it. The
controller plugin's HA controller does not wait for that. It is on by default.

**How it decides.** Every `--ha-failover-interval` (default `5s`) it reads the
Haify controller's view of each volume a pod is using. A node counts as lost
for a volume when all of these hold:

- the surviving replica that answered has no DRBD connection to the pod's node
  (a `StandAlone` connection, which DRBD leaves after a split brain, does not
  count);
- the surviving replicas, diskless tiebreakers included, are more than half
  of the volume's nodes, which is DRBD quorum, and one of them is `UpToDate`;
- Kubernetes reports the node `NotReady` or `Unknown`, or the node has looked
  lost for `--ha-failover-grace` (default `10s`) in a row.

Quorum is what makes moving the pod safe: a node cut off from the majority can
no longer write to the volume, so the old pod cannot either.

**What it does.** It taints the node `haify.csi/lost-quorum:NoSchedule`, so the
replacement cannot be scheduled back onto it. Then it force-deletes (grace
period 0) each pod on that node that mounts an affected volume, and deletes the
volume's VolumeAttachments on that node, if there are any. The pod's
StatefulSet or Deployment creates a new pod, the scheduler puts it on a node
with a replica, and the node plugin promotes that replica. It posts a
`Warning HaifyFailoverPodDeleted` event on the pod and on its PVC, and
`HaifyFailoverNodeTainted` on the node, each saying which volume lost which
node and which replicas still hold quorum. The plugin log has one line per
action (`HA failover: tainted lost node`, `HA failover: force-deleted pod`).

**Timing.** The surviving replicas notice the dead peer after DRBD's ping
timeout, about 10 seconds. The pods are deleted one grace period after the
next check sees it; when Kubernetes already reports the node `NotReady`, the
grace period is skipped. The new pod then starts on a node with a replica and
the volume is promoted there. From power loss to the pod running again takes
about 30 seconds, well before Kubernetes marks the node `NotReady` (40
seconds) or evicts its pods (5 minutes).

The HA controller runs in the controller plugin's pod. If that pod was on the
dead node too, another replica of it takes over first, once the dead one's
Lease runs out after 15 seconds, and the pod is running again after about 40
seconds. That is why `20-controller.yaml` and the
chart run two replicas, each on its own node, and let the pod leave a dead
node after 10 seconds. With a single replica, a failure of its node delays
everything above until Kubernetes moves the pod, five minutes by default.

**When it does nothing.**

- The surviving replicas do not have quorum, or none of them is `UpToDate`.
  This is the case for a volume with 2 replicas and no tiebreaker.
- The plugin cannot reach the Haify controller, or cannot read a volume's
  status. No view of a volume is never taken as evidence that a node died.
- More than `--ha-failover-max-lost-fraction` (default `0.5`) of the cluster's
  nodes look lost at once, counting nodes Kubernetes reports not `Ready`. That
  is more likely a network partition than that many machines failing.
- The pod has no controller (a bare `Pod`): deleting it would only throw it
  away. Kubernetes evicts it after its tolerations as before; its volume is
  kept.
- The pod opted out (below).

**Taint removal.** The taint comes off once Kubernetes reports the node
`Ready` and DRBD has reconnected it to every Haify volume it holds a replica
of. The node gets a `HaifyFailoverNodeReturned` event. To take it off by hand:
`kubectl taint nodes <node> haify.csi/lost-quorum-`.

**Opting a pod out.** Set the annotation or label `haify.csi/ha-failover:
"false"` on the pod (in a StatefulSet or Deployment, on the pod template). Such
a pod is left for Kubernetes to evict after its `not-ready` and `unreachable`
tolerations.

```yaml
spec:
  template:
    metadata:
      annotations:
        haify.csi/ha-failover: "false"
```

**Tuning and turning it off.** The flags are on the controller plugin
(`20-controller.yaml`): `--ha-failover=false` turns it off,
`--ha-failover-interval`, `--ha-failover-grace` and
`--ha-failover-max-lost-fraction` tune it. With the Helm chart, set
`haFailover.enabled`, `haFailover.interval`, `haFailover.grace` and
`haFailover.maxLostFraction`. With more than one controller plugin replica,
only the one holding the `haify-csi-ha-failover` Lease acts. Its permissions
are the `haify-csi-ha-failover` ClusterRole in `10-rbac.yaml`.

## Haify Copilot access (optional)

`60-haify-ai-apps-rbac.yaml` lets `haify-ai` create databases on Haify volumes
(the `haify_k8s_app_create` tool). It may create namespaces, Secrets, PVCs, Services
and Deployments, and read Pods, PVs and StorageClasses. It cannot update or
delete anything. haify-ai runs outside the cluster and reads its kubeconfig from
the file named in `HAIFY_AI_KUBECONFIG`. Build that file from the token Secret:

```bash
TOKEN=$(kubectl -n haify-ai get secret haify-ai-token -o jsonpath='{.data.token}' | base64 -d)
kubectl config view --minify --raw --flatten > haify-ai.kubeconfig
KUBECONFIG=haify-ai.kubeconfig kubectl config set-credentials haify-ai --token="$TOKEN"
KUBECONFIG=haify-ai.kubeconfig kubectl config set-context --current --user=haify-ai
```

When the request names no `storageClass`, the tool picks a Haify StorageClass
that does not set `allowRemoteVolumeAccess: "true"`, so the database runs next
to its data. Only if there is none does it pick one that does.

## When registry.k8s.io is unreachable

`registry.k8s.io` redirects to regional Google Artifact Registry hosts, which
some networks (mainland China among them) cannot reach. On every node, point
k3s at a mirror in `/etc/rancher/k3s/registries.yaml` and restart k3s:

```yaml
mirrors:
  registry.k8s.io:
    endpoint: ["https://k8s.m.daocloud.io"]
```
