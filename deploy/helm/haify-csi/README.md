# haify-csi Helm chart

Installs the Haify CSI driver. It gives Kubernetes volumes that DRBD replicates
across your own servers, with snapshots, clones, and resizing while the volume
is in use.

## Before installing

- A running Haify controller, and a pool on at least as many nodes as the
  number of replicas each StorageClass asks for.
- Every Kubernetes node that runs volumes is registered with Haify **under the
  same name as its Kubernetes node**, and has DRBD 9 and `drbd-utils`. When the
  StorageClass sets `allowRemoteVolumeAccess: "true"`, a node with no replica
  of its own can still use a volume; it reads and writes over the network.
- The `haify-csi` image on every node, or in a registry (`image.repository`).
  `deploy/k8s/README.md` shows how to build it.

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
| `snapshotClass.enabled` | `true` | Created only when the cluster has the snapshot CRDs |
| `snapshotClass.name`, `snapshotClass.default`, `snapshotClass.deletionPolicy` | `haify-snapshot`, `false`, `Delete` | The VolumeSnapshotClass's name, whether it is the default class, and whether deleting a `VolumeSnapshot` also deletes the snapshot |
| `kubeletDir` | `/var/lib/kubelet` | Change for distributions that move it |
| `operationTimeout` | `120s` | How long provisioning, resizing or a snapshot may take |
| `controllerPlugin.replicas` | `1` | How many copies of the controller plugin run. Only one works at a time; with `2` or more, the others wait, and when the working one stops (its node goes down, it is evicted or restarted), another takes over within about half a minute. Each replica runs on a node of its own. With `1`, provisioning, resizing and snapshots wait until Kubernetes starts the pod again elsewhere |
| `controllerPlugin.*`, `nodePlugin.*` | | `nodeSelector`, `tolerations`, `resources` |
| `sidecars.*` | the registry.k8s.io sig-storage images | Images of the provisioner, resizer, snapshotter, liveness probe and node driver registrar |

## Changing a StorageClass

Kubernetes does not allow changing a StorageClass's parameters. To change one,
delete the StorageClass and upgrade the release. Volumes that already exist keep
working, because they do not need the class after they are created. Or add a
class under a new name.
