# DRBD on Kubernetes nodes

Every Kubernetes node that holds a Haify volume, or uses one as a diskless
client, needs the DRBD 9 kernel module, version 9.2 or newer. There are three
ways to get it there:

| | Use it when |
| --- | --- |
| Install `drbd-dkms` on each node ([node prerequisites §1](./node-prerequisites.md#1-drbd-and-drbd-reactor-all-nodes)) | You install packages on the nodes anyway. The module is on disk and loads at boot before anything else needs it. |
| The chart's DRBD module loader (`drbdModuleLoader.enabled`) | The nodes have no DRBD 9 package, or you do not want to keep one in step with every kernel update. A DaemonSet builds DRBD for the running kernel and loads it. |
| A KMM `Module` (`drbdModuleLoader.kmm.enabled`) | OpenShift, or any cluster with the Kernel Module Management operator. KMM builds a module image per kernel and loads it. |

The loader and KMM provide the kernel module only. `drbd-utils`,
`drbd-reactor` and the rest of [node prerequisites](./node-prerequisites.md)
are still installed on the node itself, and the controller still reaches the
node over SSH.

## The module loader

```yaml
# haify-values.yaml
drbdModuleLoader:
  enabled: true
  image:
    repository: quay.io/piraeusdatastore/drbd9-noble   # the nodes' distribution
    tag: v9.3.4                                        # the DRBD version it loads
```

The `load` step uses the DRBD module loader images of the
[Piraeus](https://piraeus.io/docs/v2/how-to/drbd-loader/) project. Pick the one
for the nodes' distribution:

| Distribution | Image |
| --- | --- |
| Ubuntu 26.04 | `quay.io/piraeusdatastore/drbd9-resolute` |
| Ubuntu 24.04 | `quay.io/piraeusdatastore/drbd9-noble` |
| Ubuntu 22.04 | `quay.io/piraeusdatastore/drbd9-jammy` |
| Debian 13 | `quay.io/piraeusdatastore/drbd9-trixie` |
| Debian 12 | `quay.io/piraeusdatastore/drbd9-bookworm` |
| Debian 11 | `quay.io/piraeusdatastore/drbd9-bullseye` |
| RHEL 10, AlmaLinux 10, Rocky Linux 10, Oracle Linux 10 | `quay.io/piraeusdatastore/drbd9-almalinux10` |
| RHEL 9, AlmaLinux 9, Rocky Linux 9, Oracle Linux 9 | `quay.io/piraeusdatastore/drbd9-almalinux9` |
| RHEL 8, AlmaLinux 8, Rocky Linux 8, Oracle Linux 8 | `quay.io/piraeusdatastore/drbd9-almalinux8` |

The tag is the DRBD version (`v9.3.4`). `kubectl get nodes -o wide` shows each
node's OS image in the `OS-IMAGE` column.

When the nodes run more than one distribution, give each its own entry in
`distros`. Each entry becomes a DaemonSet `haify-drbd-loader-<name>` on the
nodes its `nodeSelector` picks. The labels of
[Node Feature Discovery](https://kubernetes-sigs.github.io/node-feature-discovery/)
can tell the distributions apart, or label the nodes yourself:

```yaml
drbdModuleLoader:
  enabled: true
  distros:
    - name: noble
      image: quay.io/piraeusdatastore/drbd9-noble:v9.3.4
      nodeSelector: {feature.node.kubernetes.io/system-os_release.VERSION_ID: "24.04"}
    - name: el9
      image: quay.io/piraeusdatastore/drbd9-almalinux9:v9.3.4
      nodeSelector: {feature.node.kubernetes.io/system-os_release.VERSION_ID.major: "9"}
```

`drbdModuleLoader.nodeSelector` applies to every loader, for example to keep
it off nodes that already have `drbd-dkms`.

### What it does on a node

Each loader pod runs these steps in order:

1. **prepare** reads the loaded DRBD version (`/sys/module/drbd/version`, or
   `/proc/drbd`) and prints it.
   - DRBD `minVersion` (default 9.2.0) or newer is loaded: nothing else
     happens on the node.
   - No DRBD is loaded: it writes `/etc/modprobe.d/haify-drbd-loader.conf`
     (below).
   - An older DRBD is loaded and has no devices: it writes that file and
     unloads the old module. This is the DRBD 8.4 that Debian and Ubuntu ship
     in their kernels: `drbdadm` and `drbdsetup` run `modprobe drbd` whenever
     DRBD is not loaded, so it is usually loaded by the time Kubernetes starts
     the loader.
   - An older DRBD is loaded and has devices: it stops with an error that names
     them. That is someone's running storage. Bring its resources down or
     reboot the node, and the pod tries again.
2. **load** builds DRBD for the running kernel and inserts `drbd` and
   `drbd_transport_tcp`. If a new enough module is already loaded, it does
   nothing and exits.
3. **bring-up** runs the node's `haify-drbd-up.service` again, only when the
   module was loaded in step 2. At boot that unit ran before DRBD was there,
   so the node rejoined none of its resources. This step brings them up.
   `drbd-reactor` needs nothing: it retries `drbdsetup events2` every two
   seconds and starts managing the resources once DRBD is loaded.
4. **drbd** keeps running. The pod is Ready while DRBD `minVersion` or newer is
   loaded.

The node plugin (`haify-csi-node`) waits in its `wait-for-drbd` init container
until the node has DRBD `minVersion`, so no volume is staged on a node without
it. On a node where no loader runs (excluded by its `nodeSelector`), the node
plugin waits until DRBD is there some other way. Set
`drbdModuleLoader.waitInNodePlugin: false` if that is not what you want.

### How it differs from a drbd-dkms node

- The module is not installed on disk. The loader inserts it from memory, so
  after every reboot the node has no DRBD until the loader pod has run again.
  With `how: compile` that means building DRBD again, which takes a minute or
  two.
- `/etc/modprobe.d/haify-drbd-loader.conf` contains `install drbd /bin/false`.
  It stops `modprobe` from loading any `drbd` module from the node's disk, so
  the distribution's DRBD 8.4 cannot be loaded before the loader's DRBD 9. The
  loader inserts its own module with `insmod`, which the file does not affect.
  `blockHostModule: false` leaves the file out. Delete the file if the node
  later gets DRBD from `drbd-dkms`.
- The loader loads DRBD with `usermode_helper=disabled`. DRBD then runs no
  handler scripts. Haify's own resource configuration uses none, but a
  `handlers` section you set through resource options does not run on these
  nodes.

### Kernel headers

`how: compile` (the default) builds DRBD against the headers of the running
kernel, which the loader reads from the node's `/usr/src` and `/lib/modules`.
Install them on each node, together with the metapackage that brings them for
future kernels:

```bash
apt-get install -y linux-headers-$(uname -r) linux-headers-generic   # Ubuntu
apt-get install -y linux-headers-$(uname -r) linux-headers-amd64     # Debian
dnf install -y kernel-devel-$(uname -r)                               # RHEL family
```

Without the headers of the new kernel, the loader fails after a kernel update
and the node stays without DRBD. The build may also download a compatibility
patch for the kernel from `spaas.drbd.io`, so the nodes need outbound HTTPS
there unless the DRBD source already carries one for that kernel.

`how: shipped_modules` needs neither: it installs a prebuilt module from the
image, which exists only for the distribution's own kernels. `how: deps_only`
loads only the modules DRBD depends on.

### Transports and encrypted replication

The loader inserts `drbd_transport_tcp`, which every Haify resource uses, and
`drbd_transport_lb-tcp` and `drbd_transport_rdma` when it built them. For a
kernel without the in-kernel TLS handshake API, DRBD builds its own
`handshake` module, and the loader inserts that too.

[Encrypted replication](./node-prerequisites.md#8-encrypted-replication-optional-all-nodes-of-an-encrypted-resource)
also needs the kernel `tls` module and `tlshd` from `ktls-utils` on the node.
Neither comes from the loader; `haify replication-tls setup` checks for them,
loads `tls` at boot and enables `tlshd`.

### Secure Boot

With Secure Boot on, the kernel refuses to load a module that is not signed
by a key it trusts, and a module the loader builds is unsigned. `mokutil
--sb-state` on a node shows whether Secure Boot is on. To load DRBD there:

1. Create a key and certificate:
   ```bash
   openssl req -x509 -new -nodes -utf8 -sha256 -days 36500 -batch -outform DER \
     -subj "/CN=DRBD module signing key" \
     -addext basicConstraints=critical,CA:FALSE -addext keyUsage=digitalSignature \
     -out signing_key.x509 -keyout signing_key.pem
   ```
2. Enroll the certificate on every node: `mokutil --import signing_key.x509`,
   then reboot and confirm the enrollment on the node's console ("Enroll
   MOK").
3. Give the loader the key, and it signs the module before loading it:
   ```bash
   kubectl -n kube-system create secret generic drbd-signing-keys \
     --from-file=signing_key.pem --from-file=signing_key.x509
   ```
   ```yaml
   drbdModuleLoader:
     signingKeySecret: drbd-signing-keys
   ```

Signing uses `scripts/sign-file` from the node's kernel build directory, so
these nodes need the kernel headers even with `how: shipped_modules`.

## OpenShift: Kernel Module Management

OpenShift nodes run RHCOS, which has no kernel headers in `/usr/src`. There,
use the [Kernel Module Management](https://kmm.sigs.k8s.io/) operator (KMM),
installed from OperatorHub. The chart creates a `Module` that loads
`drbd_transport_tcp`, and with it `drbd`, on the nodes its selector picks:

```yaml
drbdModuleLoader:
  kmm:
    enabled: true
    # Where KMM looks for the module image of a kernel, and pushes it after
    # building it. KMM fills in ${KERNEL_FULL_VERSION}.
    containerImage: image-registry.openshift-image-registry.svc:5000/kube-system/haify-drbd:${KERNEL_FULL_VERSION}
    selector:
      node-role.kubernetes.io/worker: ""
```

The first time KMM meets a kernel and finds no image for it, it builds one in
the cluster from the Dockerfile in the ConfigMap `haify-drbd-kmod-dockerfile`:
the build stage is OpenShift's Driver Toolkit for that kernel (KMM fills in
`DTK_AUTO`), downloads the DRBD source (`build.drbdVersion`) from
`pkg.linbit.com`, and the final image holds the modules under
`/opt/lib/modules/<kernel>/`. After an OpenShift update brings a new kernel,
KMM builds again. `imageRepoSecret` names the credentials for pulling and
pushing `containerImage`. To use an image you built elsewhere, set
`build.enabled: false`; `build.dockerfile` replaces the whole Dockerfile.

On a cluster with Secure Boot, put the private key (key `key`) and the
certificate (key `cert`) in two Secrets and name them in `kmm.sign.keySecret`
and `kmm.sign.certSecret`; KMM signs the modules after the build. The
certificate must be enrolled on the nodes as above.

KMM's module loader pods run as the service account `haify-drbd-kmm`, which
the chart lets use the `privileged` SCC.

The module is only one of the things a Haify node needs. RHCOS nodes take no
packages and no root SSH, so they cannot be Haify storage nodes; see
[OpenShift and MicroShift](../deploy/k8s/README.md#openshift-and-microshift).

## Checking the nodes

```bash
# Every node's DRBD, as its node plugin sees it
scripts/k8s-drbd-versions.sh kube-system 9.2.0

# What the loader did on each node
kubectl -n kube-system get pods -l app=haify-drbd-loader -o wide
kubectl -n kube-system logs <loader-pod> -c prepare
kubectl -n kube-system logs <loader-pod> -c load
kubectl -n kube-system logs <loader-pod> -c bring-up

# On a node
cat /proc/drbd            # version: 9.3.4 ...
```

`scripts/k8s-drbd-versions.sh` runs `haify-csi drbd-module check` in each node
plugin and prints one line per node. For a node whose plugin is still waiting
for DRBD, it prints the loader's last log line instead. It exits 1 if any node
is below the version.

A loader pod stuck in `Init:CrashLoopBackOff` says why in the log of the
failing step. The usual causes are missing kernel headers (`load`), an old
DRBD module with devices on it (`prepare`), and an unsigned module on a
Secure Boot node (`load`, `Key was rejected by service`).

## Without Helm

`deploy/k8s/optional/drbd-loader.yaml` is the same loader as the chart with
`drbdModuleLoader.enabled=true`; `kubectl apply -f deploy/k8s/` does not apply
it. Set the image of its `load` container to the nodes' distribution. To make
the node plugin wait for DRBD, add this init container to the DaemonSet in
`deploy/k8s/30-node.yaml`:

```yaml
      initContainers:
        - name: wait-for-drbd
          image: haify-csi:latest
          command: [/usr/local/bin/haify-csi, drbd-module, wait, --min-version=9.2.0]
```
