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
  DRBD 9 kernel module, `drbd-utils`, and LVM or ZFS.
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
cannot be Haify nodes yet.

## What the driver supports

- Expansion: with `allowVolumeExpansion: true` and the `csi-resizer` sidecar,
  raising a PVC's request grows the LV, the DRBD device and the filesystem
  while the volume stays in use.
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
survive losing the whole cluster, ship snapshots off-site.

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

## When a node dies

When a node loses power, the replacement pod starts on a surviving replica
node with its data intact. Nothing needs cleaning up on the Haify side, and the
old node's replica resyncs by itself when the node comes back.

Most of the wait is Kubernetes deciding the node is gone. By default it evicts
the pod only after the node has been unreachable for 5 minutes (the 300-second
tolerations Kubernetes adds to every pod that sets none). With the 15-second tolerations
below, the replacement pod runs about a minute and a half after the power
loss. Haify's part of that, moving the volume, takes about 10 seconds. To
shorten the wait for a workload, lower the `not-ready` and `unreachable`
tolerations on the pod:

```yaml
tolerations:
- {key: node.kubernetes.io/not-ready,   operator: Exists, effect: NoExecute, tolerationSeconds: 15}
- {key: node.kubernetes.io/unreachable, operator: Exists, effect: NoExecute, tolerationSeconds: 15}
```

Only pods managed by a controller (Deployment, StatefulSet) are recreated. A
bare `Pod` on a dead node is deleted and not replaced; its volume is kept.

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
