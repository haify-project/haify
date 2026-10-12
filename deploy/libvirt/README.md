# KVM with libvirt

A Haify volume is a block device, `/dev/drbd/by-res/<resource>/<volume>`, and
libvirt runs a guest on it like on any other disk. But DRBD lets only one node
write at a time, and libvirt does not know that. The host that runs a guest
must hold the guest's disks Primary, and no other host may. A live migration
needs both hosts Primary for a moment. Haify creates resources with
`auto-promote no`, so without help `virsh start` fails with
`Read-only file system`.

Two pieces handle this:

- The libvirt hook (`haify-hook.py`) promotes and demotes a guest's disks
  as libvirt starts, stops and migrates it. `virsh start`, `virsh shutdown`
  and `virsh migrate --live` then work as on shared storage.
- `haify ha create <resource> --vm <guest>` hands a guest to drbd-reactor.
  drbd-reactor runs the guest where the resource is Primary, and restarts it
  on another replica when that node fails.

## What the hook does

libvirt calls `/etc/libvirt/hooks/qemu.d/haify` at each step of a guest's
life, with the guest's XML. A disk whose source is
`/dev/drbd/by-res/<resource>/<volume>` is a Haify disk; others are ignored.

| libvirt step | on this host |
| --- | --- |
| `prepare begin` (start, restore, incoming migration) | promote each Haify disk, quorum-guarded; a host with no replica first joins as a diskless client |
| `migrate begin` (destination of a live migration) | note the migration, so `prepare` may open the dual-primary window |
| `release end` (stopped, or left in a migration) | demote each disk, close the dual-primary window, detach a diskless client |

If another node holds a disk Primary, the hook opens the dual-primary window
only when libvirt has just announced a live migration to this host. Any other
Primary is a leftover. Then the hook refuses to start a second writer and says
why, and libvirt shows that as the start error. If one disk fails to promote,
the hook demotes the disks it already promoted.

Promotion goes through haify-controller. When no controller answers, a disk
that is already up on the host is promoted or demoted with plain `drbdadm`
(never `--force`), and DRBD's own quorum check still applies. A live migration
cannot go ahead without the controller.

The hook leaves alone any resource that a drbd-reactor promoter manages on
the host (`ha create`): drbd-reactor decides its role.

When the hook starts a guest, it labels each of the guest's resources
`haify.libvirt/domain=<guest>` (`ha create --vm` does the same). The web UI
shows a **KVM** page once such a label exists. It lists each guest with the
nodes that hold its disks' replicas, the node it runs on, and whether it is
under HA.

## Install

Run this on every KVM host, as root. The host needs `python3`, DRBD 9 and
drbd-utils, and should be registered as a Haify node. A host that only runs
guests and holds no replica must at least be reachable from the controller:

```bash
./deploy/libvirt/install.sh --controller 192.168.1.10,192.168.1.11 --node kvm1
```

`--controller` lists the controller's REST addresses (port 3375 by default).
Under Self-HA, list every host that can run the controller. `--node` is the
host's Haify node name, if it differs from its short host name. `--token-file`
points at a bearer token, if the controller has `[auth]` or `[rbac]`.

The script writes the settings to `/etc/haify/libvirt.conf` and puts the hook
next to any other qemu hook. It also makes DRBD disconnect at shutdown only
after the libvirt-guests service has stopped the guests. Otherwise a guest
could still be writing when DRBD disconnects, and its data would split from
the copy it is restarted on elsewhere. Finally it restarts libvirt's daemon
(`virtqemud` or `libvirtd`) to load the hook; running guests are not affected.
The hook logs to the journal as `haify-libvirt-hook`.

## Defining a guest

```xml
<domain type='kvm'>
  <name>web1</name>
  <uuid>6f1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d</uuid>
  ...
  <devices>
    <disk type='block' device='disk'>
      <driver name='qemu' type='raw' cache='none' io='native'/>
      <source dev='/dev/drbd/by-res/web1/0'/>
      <target dev='vda' bus='virtio'/>
    </disk>
    <video><model type='vga'/></video>
    ...
```

- Use the `by-res` path as the disk source. libvirt also accepts `/dev/drbdN`,
  but the hook does not recognise it and so does not promote the disk.
- Set `cache='none'` (or `directsync`): libvirt refuses to live-migrate a guest
  on shared block storage with host caching.
- Define the guest on every host it may run on, with the same UUID. If the
  XML has no `<uuid>`, each `virsh define` makes up a different one, and a
  migration then fails against the definition already on the destination.
- Give the guest a display device. A Debian 13 cloud kernel with no video
  device hangs before its first console line; adding `<video>` fixes it.

## Live migration

```bash
virsh migrate --live web1 qemu+ssh://kvm2/system
```

The destination's hook opens the dual-primary window for the two hosts and
promotes the disks there. Once the guest has moved, the source's hook demotes
them and closes the window, which leaves one Primary.

## High availability

```bash
virsh define web1.xml        # on every diskful replica, autostart off
haify ha create web1 --vm web1
```

The promoter runs `ocf:heartbeat:VirtualDomain` (from `resource-agents`) with
the definition libvirt keeps in `/etc/libvirt/qemu/web1.xml`. drbd-reactor
promotes the resource and starts the guest on one replica. When that node
fails, another replica with quorum promotes the resource and starts the guest.
The guest boots as it would after a power cut, from what was on disk. If its
host loses power, the guest is running on another replica about 20 seconds
later. Do not start or migrate an HA guest with `virsh`; move it with
`haify ha evict web1`.
