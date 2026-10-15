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
| `examples/volumegroupsnapshotclass.yaml` | VolumeGroupSnapshotClass `haify-group-snapshot`; not applied with the rest (needs the group snapshot CRDs, see below) |
| `60-haify-ai-apps-rbac.yaml` | ServiceAccount + token Secret for the Haify Copilot's `haify_k8s_app_create` tool |

## Prerequisites

- Every Kubernetes node that runs volumes is a registered Haify node, with the
  DRBD 9 kernel module, `drbd-utils`, and LVM or ZFS. Instead of installing
  `drbd-dkms` on each node, the module can be loaded from Kubernetes:
  [DRBD on Kubernetes nodes](../../docs/kubernetes-drbd.md)
  (`optional/drbd-loader.yaml`, or the chart's `drbdModuleLoader`).
- **The Kubernetes node name must be the same as the Haify node name.** The
  node plugin reports `spec.nodeName` as its location, and the controller
  plugin looks that name up in `haify node list`.
- When the node plugin starts, it registers its node with the controller under
  that name and the node's `status.hostIP` (`--node-name`/`--node-ip`, filled
  from `NODE_NAME`/`NODE_IP`). Registering a node that is already registered
  changes nothing. The controller still needs SSH access to that address.
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
| `faultDomainLabel` | `host` | Haify node label used to spread replicas: replicas go to nodes with different values of it when possible (`haify node label <node> host=<name>`). A node without the label counts as its own domain |
| `resourceProfile` | none | Haify resource profile. Its pool, replica count and storage type are used for any of the three the StorageClass does not set |
| `resourceLabels` | none | Labels put on the Haify resource, written `key=value,key=value`. The driver always adds `haify.csi/managed-by=csi`, and `haify.csi/pvc=<namespace>/<name>` to name the claim (external-provisioner passes the claim with `--extra-create-metadata`, which `20-controller.yaml` sets) |
| `readBytesPerSecond`, `writeBytesPerSecond` | none | Read and write bandwidth limit for the Pod on this volume, as a size per second (`200Mi`, `1G`) |
| `readIOPS`, `writeIOPS` | none | Read and write operations per second limit for the Pod on this volume |
| `encryption` | `false` | `true` encrypts the volume on the disks: each replica's backing volume is a LUKS2 container beneath DRBD. LVM pools only. See [Encrypted volumes](#encrypted-volumes) |
| `replicationTLS` | `false` | `true` encrypts the DRBD connections between the volume's nodes with kernel TLS. The nodes must be prepared with `haify replication-tls setup`. See [Encrypted volumes](#encrypted-volumes) |
| `drNode` | none | Haify node at another site that gets an asynchronous DR replica of every volume. See [Cross-site disaster recovery](#cross-site-disaster-recovery) |
| `drEndpoint` | required with `drNode` | The DR site's public address or host name, without a port |
| `drEgressAddress` | none | Source address the primary site's proxy uses to reach the DR site |

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

The nodes are Haify nodes like any other: the DRBD 9 kernel module and
`drbd-utils` installed, and root SSH from the controller. That holds on
MicroShift running on RHEL, CentOS Stream or Ubuntu. OpenShift Container
Platform nodes run RHCOS, which takes no packages and no root SSH, so they
cannot be Haify nodes yet. The chart can load the DRBD module on OpenShift
nodes through the Kernel Module Management operator
([DRBD on Kubernetes nodes](../../docs/kubernetes-drbd.md#openshift-kernel-module-management)),
but that provides the module only.

## What the driver supports

- Expansion: with `allowVolumeExpansion: true` and the `csi-resizer` sidecar,
  raising a PVC's request grows the LV, the DRBD device and the filesystem
  while the volume stays in use.
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
  below). `ReadWriteMany` filesystems and the other multi-node modes are
  refused at provisioning: a DRBD resource has one Primary, and a filesystem
  cannot be written from two nodes.
- Replica health on the PVC and its Pods: the controller plugin checks each
  volume's replicas every `--health-interval` (default `1m`). When a replica
  disconnects, loses its disk, falls out of date or resyncs, it posts a
  `Warning VolumeDegraded` event on the PVC, and `Normal VolumeRecovered` when
  the replica is healthy again. The same event goes on every Pod using the PVC
  at that moment, with the claim's name in front of the message
  (`PVC data: ...`), so `kubectl describe pod` shows it too. Events are posted
  only when the state changes, not every check. The first sync of a new volume
  is not reported. Kubernetes keeps events for an hour by default.
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
