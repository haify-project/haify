# haify-csi Helm chart

Installs the Haify CSI driver: Kubernetes volumes replicated by DRBD across your
own servers, with snapshots, clones and online resizing.

## Before installing

- A running Haify controller, with a pool on at least as many nodes as each
  StorageClass wants replicas.
- Every Kubernetes node that runs volumes registered with Haify **under the same
  name as its Kubernetes node**, with DRBD 9 and `drbd-utils`. A node with no
  replica of its own reads and writes over the network when the StorageClass
  sets `allowRemoteVolumeAccess: "true"`.
- The `haify-csi` image on every node, or in a registry (`image.repository`);
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

With API authentication on, add `token.value`, or create the Secret yourself and
name it in `token.existingSecret` (key `token`).

Give StorageClasses in a values file: `--set storageClasses[0].pool=...`
replaces the whole entry, name included.

## Values

| Value | Default | |
| ----- | ------- | - |
| `controller.address` | (required) | The Haify controller's IP |
| `controller.port` | `3374` | Its gRPC port |
| `token.existingSecret` / `token.value` | | API token, when the controller requires one |
| `image.repository`, `image.tag`, `image.pullPolicy` | `haify-csi`, `latest`, `IfNotPresent` | The plugin image |
| `storageClasses` | one class `haify` on pool `vg0`, 2 replicas | `name`, `pool`, `replicas`, `default`, `reclaimPolicy`, `volumeBindingMode`, `allowVolumeExpansion`, and any other driver `parameters` |
| `snapshotClass.enabled` | `true` | Created only when the cluster has the snapshot CRDs |
| `kubeletDir` | `/var/lib/kubelet` | Change for distributions that move it |
| `operationTimeout` | `120s` | How long provisioning, resizing or a snapshot may take |
| `controllerPlugin.*`, `nodePlugin.*` | | `nodeSelector`, `tolerations`, `resources` |

## Changing a StorageClass

Kubernetes does not let a StorageClass's parameters change. To change one,
delete the StorageClass and upgrade the release; volumes already provisioned
keep working, since they do not depend on the class after they are created. Or
add a class under a new name.
