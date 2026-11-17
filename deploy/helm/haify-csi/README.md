# haify-csi Helm chart

Installs the Haify CSI driver. It gives Kubernetes volumes that DRBD replicates
across your own servers, with snapshots, clones, and resizing while the volume
is in use.

## Before installing

- A running Haify controller, and a pool on at least as many nodes as the
  number of replicas each StorageClass asks for.
- Every Kubernetes node that runs volumes is registered with Haify **under the
  same name as its Kubernetes node**, and has DRBD 9 and `drbd-utils`. The
  chart can load the DRBD 9 kernel module itself
  (`drbdModuleLoader.enabled`, see
  [DRBD on Kubernetes nodes](../../../docs/kubernetes-drbd.md)). When the
  StorageClass sets `allowRemoteVolumeAccess: "true"`, a node with no replica
  of its own can still use a volume; it reads and writes over the network.
- The `haify-csi` image on every node, or in a registry (`image.repository`).
  `deploy/k8s/README.md` shows how to build it.
- For shared filesystems (`ReadWriteMany`): a range of free addresses in a
  network the storage nodes are attached to and every Kubernetes node can
  reach, and the NFS server on the storage nodes. See
  [Shared filesystems](#shared-filesystems-readwritemany).

## Install

```bash
cat > haify-values.yaml <<'VALUES'
controller:
  address: 192.168.1.10        # the controller's floating IP with Self-HA
storageClasses:
  - name: haify
    default: true
    pool: vg0
    replicas: 2
VALUES

helm install haify-csi deploy/helm/haify-csi -n kube-system -f haify-values.yaml
```

If the controller has API authentication on, set `token.value` and the chart
creates the Secret. Or create the Secret yourself (key `token`) and put its
name in `token.existingSecret`.

Define StorageClasses in a values file. A flag such as
`--set storageClasses[0].pool=...` makes Helm replace the whole list, so the
entry loses its name and every other field.

## Values

| Value | Default | |
| ----- | ------- | - |
| `controller.address` | (required) | The Haify controller's IP |
| `controller.port` | `3374` | Its gRPC port |
| `token.existingSecret` / `token.value` | | API token, when the controller requires one |
| `image.repository`, `image.tag`, `image.pullPolicy` | `haify-csi`, `latest`, `IfNotPresent` | The plugin image |
| `storageClasses` | one class `haify` on pool `vg0`, 2 replicas | `name`, `pool`, `replicas`, `default`, `reclaimPolicy`, `volumeBindingMode`, `allowVolumeExpansion`, and any other driver `parameters` |
| `volumeAttributesClasses` | none | VolumeAttributesClasses to create, each a `name` and `parameters` (`replicas`, `resourceProfile`, `readIOPS`, `writeIOPS`, `readBytesPerSecond`, `writeBytesPerSecond`). Needs Kubernetes 1.34+ |
| `nodePlugin.qosInterval` | `30s` | How often the node plugin re-reads the I/O limits of published volumes, so a changed class reaches running Pods. `0s` applies limits only at publish |
| `nodePlugin.localAgent` | `false` | For nodes the controller cannot SSH into (RHCOS): the node plugin configures DRBD on its node itself, and the node mounts volumes as a diskless client. Nodes reached over SSH are unaffected. See [Nodes without SSH](../../k8s/README.md#nodes-without-ssh-local-agent) |
| `nodePlugin.zoneTopology` | `true` | Publish each node's `topology.kubernetes.io/zone` and `region` as topology and copy them onto the Haify node as the labels `zone` and `region`, so a class with `faultDomainLabel: zone` spreads replicas across zones. A `zone` or `region` label set by hand on the Haify node is kept. Nodes without the Kubernetes labels are unaffected. See [Zones](../../k8s/README.md#zones) |
| `nodePlugin.zoneSyncInterval` | `5m` | How often the Haify node's labels are compared with the Kubernetes node's. `0s` copies them once, when the plugin starts |
| `snapshotClass.enabled` | `true` | Created only when the cluster has the snapshot CRDs |
| `snapshotClass.name`, `snapshotClass.default`, `snapshotClass.deletionPolicy` | `haify-snapshot`, `false`, `Delete` | The VolumeSnapshotClass's name, whether it is the default class, and whether deleting a `VolumeSnapshot` also deletes the snapshot |
| `snapshotClass.parameters` | `{}` | `backupTarget: <Haify backup target>` also uploads every snapshot to that target, and `keepLocalSnapshot: "false"` deletes the local snapshot once uploaded. See "Snapshots that leave the cluster" in `deploy/k8s/README.md` |
| `groupSnapshots.enabled` | `auto` | Volume group snapshots (several PVCs at one instant). `auto` turns on the `csi-snapshotter`'s `CSIVolumeGroupSnapshot` gate and creates the class only when the cluster serves `groupSnapshots.apiVersion`; with the gate on and the CRDs missing, the sidecar stops handling ordinary snapshots too. The snapshot-controller needs the same gate. See "Volume group snapshots" in `deploy/k8s/README.md` |
| `groupSnapshots.apiVersion` | `groupsnapshot.storage.k8s.io/v1beta1` | The version `sidecars.snapshotter` speaks: `v1beta1` for external-snapshotter v8.2 and v8.3, `v1beta2` for v8.4 and v8.5, `v1` from v8.6 |
| `groupSnapshots.className`, `groupSnapshots.default`, `groupSnapshots.deletionPolicy` | `haify-group-snapshot`, `false`, `Delete` | The VolumeGroupSnapshotClass |
| `kubeletDir` | `/var/lib/kubelet` | Change for distributions that move it |
| `operationTimeout` | `120s` | How long provisioning, resizing or a snapshot may take |
| `controllerPlugin.replicas` | `2` | How many copies of the controller plugin run. Only one works at a time; the others wait, and when the working one stops (its node goes down, it is evicted or restarted), another takes over within about half a minute. The HA failover controller runs in this pod, so with `1` a node failure that takes the controller down too waits Kubernetes' five minutes. Each replica runs on a node of its own; on a single-node cluster set `1` |
| `controllerPlugin.tolerations` | not-ready and unreachable for 10s | The controller pod leaves a dead node after 10 seconds instead of Kubernetes' 300 |
| `controllerPlugin.*`, `nodePlugin.*` | | `nodeSelector`, `tolerations`, `resources` |
| `haFailover.enabled` | `true` | When a node dies and the other replicas of its volumes keep quorum, taint it `haify.csi/lost-quorum:NoSchedule` and force-delete its Pods that use those volumes, so their StatefulSet or Deployment recreates them on a node with a replica. A Pod with the annotation or label `haify.csi/ha-failover: "false"` is left alone. See `deploy/k8s/README.md`, "When a node dies" |
| `haFailover.interval` | `5s` | How often the volumes in use are checked |
| `haFailover.grace` | `10s` | How long a node Kubernetes still reports `Ready` must look lost before its Pods are moved. A node reported `NotReady` or `Unknown` is acted on at once |
| `haFailover.maxLostFraction` | `0.5` | Nothing is moved while more than this share of the nodes look lost, which is more likely a network partition |
| `nodeDrain.enabled` | `true` | When Kubernetes takes a node down for maintenance, drain it in Haify too (it takes no new replicas, and gateways, HA services and the controller's metadata move off it), and return it to service afterwards. Acts on a node with the annotation or label `haify.csi/drain: "true"` or one of `nodeDrain.taints`: the node stops taking replicas as soon as it has asked for `nodeDrain.delay`, and the drain finishes once no Pod on it uses a Haify volume. See [Draining a node for maintenance](../../k8s/README.md#draining-a-node-for-maintenance) |
| `nodeDrain.onCordon` | `false` | `true` drains every cordoned node, so `kubectl drain` and `kubectl uncordon` need no annotation. `haify.csi/drain: "false"` on a node keeps it out |
| `nodeDrain.taints` | `medik8s.io/drain`, `kubevirt.io/drain` | Taint keys that mark a node as under maintenance |
| `nodeDrain.delay` | `30s` | How long a node must keep asking before it is drained |
| `nodeDrain.interval` | `10s` | How often the nodes are looked at |
| `nodeDrain.maxNodes` | `1` | How many nodes may be drained in Haify at once; others wait |
| `openshift.enabled` | `false` | Lets the plugins' service accounts use the `privileged` SCC. Detected by itself on OpenShift and MicroShift; set it only for an offline `helm template` |
| `sidecars.*` | the registry.k8s.io sig-storage images | Images of the provisioner, resizer, snapshotter, liveness probe and node driver registrar |
| `drbdModuleLoader.enabled` | `false` | A DaemonSet that loads the DRBD 9 kernel module on the nodes ([DRBD on Kubernetes nodes](../../../docs/kubernetes-drbd.md)) |
| `drbdModuleLoader.minVersion` | `9.2.0` | The oldest DRBD module accepted; a node with it or newer is left alone, and the node plugin waits until a node has it |
| `drbdModuleLoader.image.*` | `quay.io/piraeusdatastore/drbd9-noble`, `v9.3.4` | The loader image for the nodes' distribution; the tag is the DRBD version |
| `drbdModuleLoader.how` | `compile` | `compile` (needs the kernel headers on the node), `shipped_modules` or `deps_only` |
| `drbdModuleLoader.distros` | `[]` | One loader per distribution: `name`, `image`, `nodeSelector` |
| `drbdModuleLoader.blockHostModule` | `true` | Write `/etc/modprobe.d/haify-drbd-loader.conf` so the distribution's DRBD 8.4 cannot load first |
| `drbdModuleLoader.bringUpResources` | `true` | After loading DRBD, run the node's `haify-drbd-up.service` to bring its resources up |
| `drbdModuleLoader.waitInNodePlugin` | `true` | The node plugin waits for DRBD `minVersion` before serving volumes (also with `kmm.enabled`) |
| `drbdModuleLoader.signingKeySecret` | | Secret with `signing_key.pem` and `signing_key.x509`, for Secure Boot |
| `drbdModuleLoader.nodeSelector`, `tolerations`, `resources`, `extraEnv` | | For the loader DaemonSets |
| `drbdModuleLoader.kmm.enabled` | `false` | A Kernel Module Management `Module` for DRBD (OpenShift); needs `kmm.containerImage` |
| `drbdModuleLoader.kmm.*` | | `containerImage`, `kernelRegexp`, `build.*` (in-cluster build), `sign.*`, `imageRepoSecret`, `selector`, `tolerations`, `inTreeModulesToRemove` |

## Encrypted volumes

A StorageClass with `encryption: "true"` in its `parameters` encrypts its
volumes on the disks (LUKS2 beneath DRBD, LVM pools only); one with
`replicationTLS: "true"` encrypts the DRBD connections between nodes (run
`haify replication-tls setup` first). Each node makes and keeps its own keys;
the chart creates no Secret for them. `values.yaml` has a commented example,
and `deploy/k8s/README.md` explains both, where the keys are and what losing a
node's root disk means.

## Shared filesystems (ReadWriteMany)

A StorageClass with `sharedFilesystem: nfs` in its `parameters` provisions
filesystems that Pods on several nodes mount at once. Each volume is served by
a Haify NFS gateway on a floating address taken from the class's
`nfsServiceIPs`, and the gateway moves to another replica when its node fails.

```yaml
storageClasses:
  - name: haify-shared
    pool: vg0
    replicas: 2
    volumeBindingMode: Immediate
    allowVolumeExpansion: true
    parameters:
      sharedFilesystem: nfs
      nfsServiceIPs: 192.168.1.200-192.168.1.220/24
```

The range is written with the prefix length of its network, and holds one
address per shared volume. `deploy/k8s/README.md`, "Shared filesystems
(ReadWriteMany)", covers the addresses, the requirements on the nodes and what
a shared volume does differently.

## Changing a volume in place

On Kubernetes 1.34 and later, naming a VolumeAttributesClass in a PVC changes
that volume's replicas, resource profile or I/O limits while it stays in use:

```yaml
volumeAttributesClasses:
  - name: haify-three-copies
    parameters:
      replicas: "3"
```

```bash
kubectl patch pvc data -p '{"spec":{"volumeAttributesClassName":"haify-three-copies"}}'
```

The chart's csi-resizer (v2.0.0) does the change. What each parameter does,
and what shrinking will not do, is in `deploy/k8s/README.md`. The chart's
external-provisioner (v5) does not apply a class at creation; name the class
after the PVC is bound, or set `sidecars.provisioner` to a v6 image on
Kubernetes 1.34+.

## Changing a StorageClass

Kubernetes does not allow changing a StorageClass's parameters. To change one,
delete the StorageClass and upgrade the release. Volumes that already exist keep
working, because they do not need the class after they are created. Or add a
class under a new name.
