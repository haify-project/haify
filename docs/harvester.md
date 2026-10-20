# Harvester on Haify

Harvester runs its virtual machines on KubeVirt, so the Haify CSI driver gives
it replicated VM disks: images, root and data disks, and VM snapshots all live
on Haify volumes. When a storage node fails, the VMs keep running on the
replicas that are left; their writes pause for a second or two.

Harvester's operating system is immutable: there is no package manager for
`drbd-utils` or LVM, and the controller does not manage it over SSH. So the
replicas live on Haify storage nodes beside the Harvester cluster, and each
Harvester node joins its VMs' volumes as a **local agent**: the CSI node
plugin brings the volumes up on it and reads and writes them over the
network. The Harvester nodes need only the DRBD 9 kernel module.

## 1. The DRBD module on every Harvester node

Harvester publishes an image with the exact kernel sources of each release,
`rancher/harvester-kernel-module-devel`, tagged like the node's OS image:

```bash
# on a Harvester node
yq .osImage /etc/harvester-release.yaml    # rancher/harvester-os:v1.9-20260915
```

Build DRBD with the image of that tag, in a privileged Pod on the node, and
put both modules in `/var/lib/third-party`. `drbd_transport_tcp.ko` is
needed as well as `drbd.ko`: DRBD 9 loads its transport separately, and does
not find it outside the kernel's module tree.

```yaml
apiVersion: v1
kind: Pod
metadata: {name: drbd-build, namespace: default}
spec:
  restartPolicy: Never
  nodeName: <node>
  containers:
  - name: build
    image: rancher/harvester-kernel-module-devel:v1.9-20260915
    securityContext: {privileged: true}
    command: [sh, -c]
    args:
    - |
      set -e
      curl -Lsf https://pkg.linbit.com/downloads/drbd/9/drbd-9.3.4.tar.gz | tar -xzf -
      make -C drbd-9.3.4/drbd all KDIR=/usr/src/linux -j8
      mkdir -p /host/var/lib/third-party
      cp drbd-9.3.4/drbd/build-current/drbd.ko drbd-9.3.4/drbd/build-current/drbd_transport_tcp.ko /host/var/lib/third-party/
    volumeMounts: [{name: host, mountPath: /host/var/lib}]
  volumes: [{name: host, hostPath: {path: /var/lib}}]
```

Use the DRBD version the storage nodes run. Then load the modules now, and at
every boot with a `CloudInit` resource:

```bash
sudo modprobe --allow-unsupported /var/lib/third-party/drbd.ko
sudo modprobe --allow-unsupported /var/lib/third-party/drbd_transport_tcp.ko
```

```yaml
apiVersion: node.harvesterhci.io/v1beta1
kind: CloudInit
metadata: {name: drbd-loader}
spec:
  matchSelector: {harvesterhci.io/managed: "true"}
  filename: 99_drbd_loader
  contents: |
    stages:
      initramfs:
        - name: load drbd
          commands:
          - modprobe --allow-unsupported /var/lib/third-party/drbd.ko
          - modprobe --allow-unsupported /var/lib/third-party/drbd_transport_tcp.ko
```

A Harvester upgrade brings a new kernel: build the modules again with the new
OS image's tag before the nodes reboot into it.

## 2. The CSI driver

Install the chart (`deploy/helm/haify-csi`) with the nodes as local agents
and a StorageClass that Harvester accepts. Harvester's admission webhook
refuses a StorageClass of a provisioner it does not know unless it carries
the CDI annotations; they tell CDI which volume and access modes the driver
offers, which snapshot class to use, and to clone through CSI.

```yaml
controller:
  address: 192.168.1.10          # the Haify controller (its floating IP with Self-HA)
token:
  existingSecret: haify-api-token
nodePlugin:
  localAgent: true
storageClasses:
  - name: haify
    pool: vg0
    replicas: 2
    volumeBindingMode: Immediate
    parameters:
      allowRemoteVolumeAccess: "true"
    annotations:
      cdi.harvesterhci.io/storageProfileVolumeModeAccessModes: '{"Block":["ReadWriteMany","ReadWriteOnce"],"Filesystem":["ReadWriteOnce"]}'
      cdi.harvesterhci.io/storageProfileVolumeSnapshotClass: haify-snapshot
      cdi.harvesterhci.io/storageProfileCloneStrategy: csi-clone
snapshotClass:
  enabled: true
  name: haify-snapshot
```

`allowRemoteVolumeAccess` is required: a Harvester node never holds a replica.
`ReadWriteMany` block volumes are what lets Harvester live-migrate a VM
between its nodes (see [Live migration of virtual machines](../deploy/k8s/README.md#live-migration-of-virtual-machines-kubevirt-openshift-virtualization)).

```bash
kubectl create namespace haify-csi
kubectl -n haify-csi create secret generic haify-api-token --from-file=token=/etc/haify/token
helm install haify-csi deploy/helm/haify-csi -n haify-csi -f haify-values.yaml
```

Each Harvester node then appears in `haify node list` as a `local agent`.

## 3. Harvester settings

Register the driver in **Advanced › Settings › csi-driver-config**, so that
Harvester's VM snapshots of Haify disks use the Haify snapshot class. Add an
entry beside Longhorn's:

```json
"haify.csi.liliang-cn.com": {
  "volumeSnapshotClassName": "haify-snapshot",
  "backupVolumeSnapshotClassName": ""
}
```

Harvester's VM *backups* to a backup target are a Longhorn feature; VM
snapshots work on Haify volumes, and Haify's own snapshot backups (the
`backupTarget` StorageClass parameter) take Haify volumes off the cluster.

CDI downloads an image into a scratch volume first, on the default class
unless told otherwise. Put the scratch space on Haify too, so that images
neither depend on Longhorn nor fail on small images, where Longhorn's scratch
volume is sized too tight for the file system on it:

```bash
kubectl patch cdi cdi --type merge -p '{"spec":{"config":{"scratchSpaceStorageClass":"haify"}}}'
```

## 4. Images and VMs

An image goes on Haify with the CDI backend:

```yaml
apiVersion: harvesterhci.io/v1beta1
kind: VirtualMachineImage
metadata: {name: debian13, namespace: default}
spec:
  displayName: debian-13
  sourceType: download
  url: https://cloud.debian.org/images/cloud/trixie/latest/debian-13-genericcloud-amd64.qcow2
  backend: cdi
  targetStorageClassName: haify
```

VMs created from it in the Harvester UI get Haify root disks, cloned from the
image through CSI, and data disks on the `haify` class. The VM's disks are
the PVCs Harvester shows; `haify resource list` names each one by its claim
(`haify.csi/pvc=<namespace>/<name>`).

VM snapshots (**Take VM Snapshot**) and restores, to the same VM or a new
one, work on these disks. Harvester snapshots and restores only the disks a
VM refers to as PersistentVolumeClaims, which is how the UI creates them; a
VM written by hand with `dataVolume` volumes is snapshotted with no disks.

## When a storage node fails

The Harvester nodes read and write over the network from the replicas, so a
storage node going down takes nothing from the VMs: DRBD carries on with the
replicas left, and the guest sees its writes pause for one or two seconds.
When the node is back, its replica resyncs in the background.
