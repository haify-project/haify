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
  on another replica when that node fails. `haify ha evict <resource> --live`
  moves such a guest to another replica without stopping it.

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
the host (`ha create`): drbd-reactor decides its role. It also leaves alone a
resource whose HA guest the controller is moving with `ha evict --live`, for
as long as the move lasts.

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
  <iothreads>1</iothreads>
  ...
  <devices>
    <disk type='block' device='disk'>
      <driver name='qemu' type='raw' cache='none' io='native' discard='unmap' iothread='1'/>
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
- Set `discard='unmap'` when the volume is on a thin pool. Space the guest
  frees (deleted files, `fstrim`) then goes back to the pool on every
  replica. Without it the pool only ever grows, and a thin pool that fills up
  stops every volume on it. Inside the guest, run `fstrim` on a timer
  (`fstrim.timer` on most distributions) or mount with `discard`.
- Give a busy disk its own I/O thread (`<iothreads>` and `iothread='1'`), so
  its I/O does not wait behind the rest of the emulator. Use one thread per
  disk that carries real load, not more threads than the host has idle cores.
- A guest you migrate with `virsh` yourself must be defined on every host it
  may run on, with the same UUID. If the XML has no `<uuid>`, each
  `virsh define` makes up a different one, and a migration then fails against
  the definition already on the destination. For a guest under HA, Haify
  does this: see below.
- Give the guest a display device. A Debian 13 cloud kernel with no video
  device hangs before its first console line; adding `<video>` fixes it.

## Live migration

```bash
virsh migrate --live web1 qemu+ssh://kvm2/system
```

The destination's hook opens the dual-primary window for the two hosts and
promotes the disks there. Once the guest has moved, the source's hook demotes
them and closes the window, which leaves one Primary.

## Growing a disk

```bash
haify resource resize-volume web1 0 40G
```

When the resource carries the `haify.libvirt/domain` label and the guest is
running, the controller tells QEMU the new size on the node the guest runs
on (`virsh blockresize`), so the guest sees the larger disk at once. The disk
is found by its source, so the order of a guest's disks does not matter.
Growing the partition and the filesystem inside the guest is still up to the
guest.

The resize succeeds whether or not the guest could be told, and its reply
says which happened. A guest that is not running sees the new size when it
next starts. If a running guest could not be told, the reply gives the
`virsh blockresize` command to run on its node, and a
`resource.guest_resize_failed` event is raised.

## Snapshots

A snapshot of a guest's volume, taken by hand, by a snapshot schedule or for
a backup, is taken with the guest's filesystems frozen when the guest is
running and its guest agent answers (`virsh domfsfreeze`). Install
`qemu-guest-agent` in the guest and give it the agent channel:

```xml
<channel type='unix'>
  <target type='virtio' name='org.qemu.guest_agent.0'/>
</channel>
```

Such a snapshot is filesystem-consistent: the filesystems were flushed and
nothing is half-written. Without the agent, or with the guest shut off or
being migrated, the snapshot is taken all the same and is crash-consistent:
it holds what a power cut at that instant would have left. The reply says
which one it is and why, and the snapshot lists show it.

A freeze stops writes on all of the guest's disks, whichever resource is
being snapshotted, and lasts only while the snapshots are taken. Snapshots of
several resources of one guest take turns. The guest never stays frozen:
before freezing it, the node arms a timer that thaws it after 90 seconds
whatever happens to the controller, and a snapshot that outlasts the timer is
reported as crash-consistent. If a guest does not thaw when asked, a
`resource.guest_thaw_failed` event is raised and the node keeps trying.

## High availability

```bash
virsh define web1.xml        # on one replica
haify ha create web1 --vm web1
```

The promoter runs `ocf:heartbeat:VirtualDomain` (from `resource-agents`) with
the definition libvirt keeps in `/etc/libvirt/qemu/web1.xml`. drbd-reactor
promotes the resource and starts the guest on one replica. When that node
fails, another replica with quorum promotes the resource and starts the guest.
The guest boots as it would after a power cut, from what was on disk. If its
host loses power, the guest is running on another replica about 20 seconds
later. Do not start or migrate an HA guest with `virsh`; move it with
`haify ha evict web1`, which restarts it on another replica, or
`haify ha evict web1 --live`, which does not stop it.

### The guest's definition

Every replica that may run the guest needs its definition, under the same
UUID. `ha create --vm` takes the definition from the replica that has it,
keeps it in the controller's database, and defines the guest on every other
diskful replica with `virsh define`, autostart off. A node whose libvirt
rejects the definition (a machine type it does not have, for example) is
reported then, and `ha create` stops before any promoter is written.

Where the definition comes from:

- the node the guest runs on, when it runs;
- otherwise any replica, when every copy carries the same UUID;
- the node named with `--vm-from <node>`, when replicas hold copies under
  different UUIDs.

After that:

| When | What happens |
| --- | --- |
| a replica is added (`haify resource add-replica`) | the guest is defined there before the promoter is |
| a replica is removed | the definition Haify placed there is removed |
| `haify ha delete` | the definitions Haify placed are removed; the guest keeps running |
| you change the guest on one node | nothing, until `haify ha vm-sync` |

A definition is only removed from a node Haify defined it on, never from the
node you defined it on yourself, and never where the guest is running or
libvirt does not answer. Such a node keeps its definition and the controller
logs why.

When you change the guest (`virsh edit`, or `virsh define` of a new XML), do
it on one node and spread it:

```bash
haify ha vm-status web1
haify ha vm-sync web1               # the one edited node is the source
haify ha vm-sync web1 --from kvm2   # when more than one was edited
```

`vm-status` shows each replica as defined, not defined, differs (edited there
since the last sync) or another UUID. `vm-sync` does not touch the running
guest: the change applies when the guest next starts. The daily inspection
(`haify inspect`) reports a replica where an HA guest is not defined or is
defined under another UUID, so it is known before a failover needs that
replica.

The definition can hold a VNC or SPICE password. It is copied to a node over
SSH as a file only root can read, never on a command line, and is not written
to any log.

### Moving a guest without stopping it

```bash
haify ha evict web1 --live             # Haify picks the node
haify ha evict web1 --live --to kvm3
haify ha migration web1                # the move in progress, or the last one
```

`--to` must name a diskful replica whose copy is UpToDate and connected to
the node the guest runs on, with quorum, drbd-reactor running, and the guest
defined under the right UUID. Without `--to`, the first replica that meets all
of that is taken, the HA config's preferred nodes first. Before anything is
changed the command also requires that every replica answers, that the
guest's chain is the guest alone (no mount, address or service beside it),
and that root on the source can reach libvirt on the target over SSH
(`qemu+ssh://<target>/system`), which is how libvirt migrates. Anything
missing is refused with the reason, and a resource that runs no guest is
refused outright.

The move then goes like this:

1. drbd-reactor's promoter for the resource is paused on every replica. The
   guest keeps running.
2. DRBD is told to allow two Primaries between the source and the target, and
   the target is promoted.
3. `virsh migrate --live` copies the guest to the target. `ha evict` shows how
   much memory has been copied.
4. The source is demoted and DRBD goes back to one Primary.
5. The promoter is enabled on the target, where drbd-reactor finds the guest
   running and takes it over as it is, then on the other replicas.

The command ends with one of:

| Result | Meaning |
| --- | --- |
| moved | the guest runs on the target and was never stopped |
| stayed | the migration did not complete; the guest runs on the node it was on and was never stopped. The reason libvirt gave is shown |
| restarted | the guest was found stopped during the move, and drbd-reactor started it again as after a failure |
| attention | something could not be put back; the message lists each thing left and the command for it |

Whatever happens in between, the end is put together from where the guest
actually runs: that node is the one Primary, every other node is demoted,
two Primaries are no longer allowed, and the promoter is enabled again, first
where the guest runs. A migration that has not copied the guest's memory
within `--timeout` (30 minutes unless you say otherwise) is given up and ends
as "stayed". If the controller itself restarts during a move, the migration
carries on, and the controller that comes up finishes the same way.

While a move runs, `ha evict`, `ha delete` and `ha vm-sync` on the same
resource are refused. `--no-wait` returns as soon as the move has started.
The web UI offers the same as **Move live** on the KVM page and on the
guest's card on the HA page, and `haify-mcp` as `haify_ha_evict` with `live`.
