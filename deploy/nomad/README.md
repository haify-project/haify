# Nomad volumes on Haify

Nomad runs CSI plugins as ordinary jobs, so the Haify CSI driver that serves
Kubernetes serves Nomad as well: the same `haify-csi` image, with two job
files in place of the Kubernetes manifests. A job that claims a Haify volume
can run on any client that holds a replica of it (or on any client, with
`allowRemoteVolumeAccess`), and when its client goes down Nomad starts it on
another one with the data.

| File | What it is |
| ---- | ---------- |
| `haify-csi-controller.nomad.hcl` | Service job, two instances: creates, deletes, snapshots and expands volumes |
| `haify-csi-node.nomad.hcl` | System job on every client: brings volumes up and mounts them for allocations |
| `volume.hcl` | An example volume for `nomad volume create` |

## Prerequisites

- Every Nomad client that runs volumes is a registered Haify node with the
  DRBD 9 kernel module, `drbd-utils`, and LVM or ZFS. **The Nomad node name
  must be the Haify node name**: the node plugin registers its client with the
  controller under `${node.unique.name}` and the client's address.
- The Docker task driver on every client, with privileged containers allowed:

  ```hcl
  plugin "docker" {
    config {
      allow_privileged = true
    }
  }
  ```

- **ACLs enabled** on the Nomad cluster (`acl { enabled = true }`). When a
  client goes down, the Nomad servers release the volume claims of its
  allocations so the job can claim the volume elsewhere; with ACLs off, Nomad
  2.0 servers refuse their own release request ("Permission denied" in the
  server log) and the job waits on its volume until the lost client returns.
- The image on every client. Build it from the repository's `Dockerfile.csi`
  and load it, or push it to a registry the clients use:

  ```bash
  docker build -f Dockerfile.csi -t haify-csi:latest .
  docker save haify-csi:latest | ssh client1 docker load
  ```

## Install

Set `--haify-controller` in both job files to your controller's address (its
floating IP with Self-HA). With API authentication on, store the token where
the plugin jobs read it:

```bash
nomad var put nomad/jobs token=@/etc/haify/token
nomad job run haify-csi-controller.nomad.hcl
nomad job run haify-csi-node.nomad.hcl
nomad plugin status haify        # Controllers Healthy 2, Nodes Healthy = clients
```

## Use

```bash
nomad volume create volume.hcl
```

```hcl
group "db" {
  volume "data" {
    type            = "csi"
    source          = "pgdata"
    access_mode     = "single-node-writer"
    attachment_mode = "file-system"
  }

  task "postgres" {
    driver = "docker"
    config {
      image = "postgres:17"
    }
    volume_mount {
      volume      = "data"
      destination = "/var/lib/postgresql/data"
    }
  }
}
```

The volume's `parameters` are the StorageClass parameters of the Kubernetes
driver (`pool`, `replicas`, `storageType`, `allowRemoteVolumeAccess`,
`faultDomainLabel`, `encryption`, `replicationTLS`, I/O limits, ...), listed in
[StorageClass parameters](../k8s/README.md#storageclass-parameters).

Without `allowRemoteVolumeAccess = "true"`, Nomad places a job that uses the
volume only on clients that hold a replica. With it, the job can run on any
client; on one without a replica the volume joins as a diskless client and
reads and writes over the network.

The volume is the Haify resource of the same name, so `haify resource status
pgdata` shows it. A name that a Haify resource the driver did not make
already has (a gateway's resource, `haify-meta`) is refused, so a Nomad
volume never takes over someone else's data.

Snapshots and deleting:

```bash
nomad volume snapshot create pgdata before-upgrade
nomad volume snapshot list -plugin haify
nomad volume delete pgdata        # deletes the Haify resource and its data
```

## When a client goes down

Nomad marks the client down when it stops heartbeating, and its allocations
lost. The servers then release the lost allocations' volume claims, and the
replacement allocation claims the volume on another client, where Haify brings
it up with the data the surviving replicas hold.

How soon the claims are released is set on the servers. By default Nomad
looks every 5 minutes, at volumes that have not changed for 5 minutes, so a
job can wait up to about 10 minutes for its volume. Shorten both:

```hcl
server {
  csi_volume_claim_gc_interval  = "1m"
  csi_volume_claim_gc_threshold = "1m"
}
```

With these, a job comes back on another client about a minute and a half
after its client loses power.

## What is Kubernetes-only

The driver's Kubernetes extras need the Kubernetes API and are off under
Nomad: Pod and PVC events for degraded volumes, the HA controller that
restarts Pods off a dead node (Nomad does that itself), PVC labels on Haify
resources, and VolumeAttributesClass changes.
