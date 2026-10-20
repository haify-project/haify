# OpenNebula storage on Haify

The Haify driver for OpenNebula keeps every image as its own Haify resource,
replicated by DRBD across your hosts. A VM keeps its disks when a host fails,
and every host can run it: a host without a copy of a disk reads and writes it
over the network.

It is two OpenNebula drivers, both `haify`:

- the **datastore driver** (`DS_MAD`) imports, creates, clones and deletes
  images;
- the **transfer driver** (`TM_MAD`) gives a VM's disks to the host that runs
  it, and moves them in live migrations.

The host that runs a guest holds its disks Primary. The libvirt hook
(`deploy/libvirt`) does that on each host, as it does for any guest: it
promotes the disks when the guest starts or migrates in, and demotes them when
the guest stops or has left. The transfer driver makes sure the host holds
each disk before that, and handles disks attached to a running guest, which
libvirt runs no hook for. A disk that loses quorum pauses its I/O instead of
failing it, and the guest carries on when quorum returns.

## Requirements

- OpenNebula 7 with KVM hosts.
- Every KVM host registered as a Haify node under the name OpenNebula knows it
  by (or with that address), with DRBD 9 and drbd-utils. A host that holds no
  copy of a disk joins it as a diskless client.
- The libvirt hook on every KVM host (`deploy/libvirt/install.sh`).
- The controller reachable from the frontend on its gRPC port (3374). The
  frontend does not need to be a Haify node.

## Install

Build the driver with `make build` (it lands in `bin/haify-opennebula`; build
it with `GOOS=linux` on another system). On the frontend, as root:

```bash
./deploy/opennebula/install.sh --binary bin/haify-opennebula
sudo -iu oneadmin onehost sync -f
```

The script installs `/usr/local/bin/haify-opennebula` and the action scripts
in `/var/lib/one/remotes`, registers the `haify` drivers in
`/etc/one/oned.conf` (keeping the old file as `oned.conf.haify-bak`), allows
live disk snapshots of `haify` disks in `/etc/one/vmm_exec/vmm_execrc`, copies
`/etc/haify/token` to oneadmin's `~/.haify/token`, and restarts OpenNebula.

On every KVM host, as root:

```bash
./deploy/libvirt/install.sh --controller <controller address> --token-file /etc/haify/token
```

## Create the datastores

A system datastore and an image datastore, both on the `haify` transfer
driver:

```bash
cat > system.tpl <<'EOF'
NAME   = "haify_system"
TYPE   = "SYSTEM_DS"
TM_MAD = "haify"
EOF
cat > images.tpl <<'EOF'
NAME             = "haify_images"
DS_MAD           = "haify"
TM_MAD           = "haify"
DISK_TYPE        = "BLOCK"
HAIFY_CONTROLLER = "192.168.1.10"
HAIFY_POOL       = "pool0"
HAIFY_REPLICAS   = "2"
EOF
onedatastore create system.tpl
onedatastore create images.tpl
```

| Image datastore attribute | |
| --- | --- |
| `HAIFY_POOL` | The Haify pool images are made in (required) |
| `HAIFY_CONTROLLER` | The controller's gRPC address, `host[:port]`, port 3374 by default; under Self-HA its floating IP. Default `127.0.0.1` |
| `HAIFY_REPLICAS` | Copies of each image, default 2 |
| `HAIFY_PREFIX` | Prefix of the resource names, default `one` |
| `BRIDGE_LIST` | The host that writes imported images; default the image's first replica |

The system datastore keeps each VM's directory (its context, its deployment
file, links to its disks) on the host running it, as the `ssh` driver does.
Volatile disks live there too, unreplicated.

## What it does

| | |
| --- | --- |
| Import (`oneimage create --path`) | A new resource of the image's size, written from a replica host: qcow2 and raw images are converted to a raw device. Anything OpenNebula's downloader takes works as the path |
| Datablock (`oneimage create --type DATABLOCK`) | An empty resource, formatted when the image has an `FS` |
| Persistent image | The VM uses the image's own resource |
| Non-persistent image | The VM gets its own resource, `one-vm-<vm>-<disk>`, copied from the image on the image's hosts and as large as the disk asks; it is deleted with the VM |
| Clone (`oneimage clone`), save as (`onevm disk-saveas`) | A full, replicated copy on the source's hosts, taken from a snapshot of every copy at the same instant |
| Disk attach and detach on a running VM | The host joins the disk and makes it Primary before the attach, and gives it up after the detach |
| Disk snapshots | Haify snapshots on every copy, named `one-snap-<id>`. A running guest is snapshotted crash-consistent; reverting needs the VM powered off |
| Disk resize | Grows the resource; a running guest sees the new size |
| Live migration | The destination joins each disk first. The hook opens the dual-primary window for the hand-over and closes it after, so the disk ends up Primary on the destination only |
| Datastore capacity | The pool's space on every host, counted once per copy |

Each resource is labelled `haify.opennebula/managed-by=opennebula`, with the
image, VM and datastore it belongs to, so the controller keeps drbd-reactor
promoters off it and the web UI shows it as an OpenNebula disk.

## Host failures

Use OpenNebula's `host_error` hook in recreate mode:

```bash
cat > host_error.tpl <<'EOF'
NAME            = "host_error"
TYPE            = "state"
COMMAND         = "ft/host_error.rb"
ARGUMENTS       = "$TEMPLATE -r -p 2"
ARGUMENTS_STDIN = "yes"
RESOURCE        = "HOST"
STATE           = "ERROR"
REMOTE          = "no"
EOF
onehook create host_error.tpl
```

When a host fails, its VMs start again on the other hosts. A persistent disk
keeps everything written before the failure, because every write reached its
other copies first. A non-persistent disk starts again from a new copy of its
image, as OpenNebula's recreate does on any storage. That copy needs as many
working hosts with room in the pool as `HAIFY_REPLICAS`: with two storage
hosts and one of them down, only VMs on persistent images come back, so give
VMs that must survive a host failure persistent images, or run three storage
hosts. The resched mode (`-m`) skips VMs on a system datastore that is not
shared, so it does nothing here.

If OpenNebula cancels a VM's cleanup on the failed host, the VM's old copy
(`one-vm-<vm>-<disk>`) is left behind and still counts against the
datastore's free space; delete it with `haify resource delete`.

DRBD's quorum keeps a host that lost its peers from writing, so a cut-off host
cannot corrupt a disk while its VMs restart elsewhere. Fencing (`fence_host.sh`)
still stops the guests themselves from running twice.

## Uninstall

Delete the Haify images and datastores, then restore `oned.conf.haify-bak`,
remove `kvm-haify qemu-haify` from `LIVE_DISK_SNAPSHOTS`, delete
`/var/lib/one/remotes/{datastore,tm}/haify` and restart OpenNebula.
