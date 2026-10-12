# Evacuating VMware vSAN to Proxmox VE on Haify

This runbook moves virtual machines off a VMware vSAN cluster onto Proxmox VE
(PVE), with the guest disks on Haify through the
[Haify storage plugin](../deploy/proxmox/README.md). It puts together the
documented behavior of vSphere, PVE and Haify. It is not a product feature,
and nothing in it automates the move.

Rehearse the whole procedure with a VM you can lose before you move one you
cannot.

## Why there is a staging step

PVE 8.2 and later ship an ESXi import wizard. PVE connects to an ESXi host
(6.5 to 8.0) and copies a VM's configuration and disks into PVE storage. With
*live import* it can start the VM in PVE while the disks are still being
copied. The wizard cannot import disks that live on vSAN.

So the disks first move off vSAN, onto a datastore the wizard can read. Haify
can provide that datastore as an NFS export:

```
 vSAN datastore
     │  Storage vMotion (vCenter), VM keeps running
     ▼
 NFS datastore "haify-staging"  ◄── Haify NFS gateway (one resource, floating service IP)
     │  PVE ESXi import wizard, or qm disk import
     ▼
 PVE storage "haify0" (type haify) ── one DRBD resource per guest disk
```

The staging datastore is temporary. Once PVE has accepted the last VM, you
unmount it and delete its resource.

## Target layout

The common small layout is two storage nodes plus one tiebreaker:

- Two storage nodes hold the replicas (a Haify pool on each). Usually they are
  also the two PVE nodes that run the guests. Separate PVE nodes that only run
  guests work too: they attach to each disk as diskless DRBD clients
  (`deploy/proxmox/README.md`).
- A third machine runs DRBD 9 and is registered as a Haify node, without a
  pool. Haify makes it the diskless tiebreaker of every two-replica resource
  (`[resource] auto_tiebreaker`, on by default), so one storage node can fail
  and the other keeps quorum. The same machine can be the corosync QDevice
  that gives a two-node PVE cluster its third vote. It has to be both:
  **a QDevice alone gives DRBD no quorum vote.** Without DRBD on the third
  machine, losing one storage node unexpectedly leaves every disk without
  quorum.

Haify leaves to PVE what PVE already does:

| Concern | Who does it |
| ------- | ----------- |
| Restarting a guest on a surviving node (VM-level HA) | PVE `ha-manager`. Haify only lets the disk be promoted there, and only with quorum |
| Fencing a node that stopped responding | PVE HA's fencing |
| Backing up guests | PVE `vzdump` to Proxmox Backup Server |
| Replicated disks, quorum, live migration without copying data | Haify |

Do not put guest disks under `haify ha create` or a Haify gateway. Both refuse
them; the plugin's README explains why.

## Prerequisites

- [ ] PVE 8.2 or later on every PVE node; source ESXi hosts 6.5 to 8.0.
- [ ] A Haify cluster, set up per the [deployment guide](deployment-guide.md):
      controller running, the two storage nodes and the tiebreaker registered,
      a pool on both storage nodes. `haify node list`, `haify pool list`.
- [ ] The Haify plugin on every PVE node ([plugin README](../deploy/proxmox/README.md),
      preferably the `haify-pve-plugin` package) and one `haify:` storage entry in
      `/etc/pve/storage.cfg`, here called `haify0`, with `content images` and
      `shared 1`. `preflight.sh` passes on every PVE node.
- [ ] NFS gateway packages on both storage nodes: `resource-agents-extra`
      and `nfs-kernel-server` (deployment guide, section 2).
- [ ] A free IP address for the gateway's service IP, in a subnet the ESXi
      hosts' NFS VMkernel interfaces can reach. NFSv3 must be allowed between
      them (2049, plus rpcbind 111 and mountd if a firewall sits in between).
- [ ] PVE nodes can reach the ESXi hosts' management interface (HTTPS) for the
      import wizard, and the staging service IP if you use `qm disk import`.
- [ ] vCenter can run Storage vMotion for running VMs. Without it, a
      powered-off VM still moves with the same "change storage only"
      migration.
- [ ] Windows guests have the VirtIO drivers installed while still on ESXi
      (see [Guest preparation](#guest-preparation)).
- [ ] A list of VMs in waves, each with its provisioned disk sizes, owner
      and acceptable downtime.

## Capacity planning

During a wave, data sits in two places at once: the staging resource and the
final guest disks. Both are replicated, so each takes its full size on every
storage node.

- Staging resource: at least the provisioned size of every disk in the
  largest wave, plus headroom. ESXi often stores disks thin on an NFS
  datastore, which then needs less, but plan for the provisioned size.
  Do not let it fill up in the middle of a wave.
- Final disks on `haify0`: each disk's provisioned size, byte for byte (see
  [Sizes](#sizes-exact-by-default)).
- Per storage node during a wave: the staging size plus the final size of
  every VM imported so far. On a thin pool only written data counts. But if an
  overcommitted thin pool fills up, writes stop on every volume in it, so
  watch `haify pool list`.

Example: a wave of six VMs with 2 TiB provisioned in total, on two replicas.
Staging is 2.2 TiB and the final disks are 2 TiB. Each storage node needs about
4.2 TiB free while that wave is in progress, and keeps 2 TiB once the staging
space is freed.

Plan the network as well. Storage vMotion sends every block from ESXi to the
gateway, and DRBD copies each write to the second storage node. The import
then reads it back from NFS and writes it again into a replicated `haify0`
disk. The replication link is as much a bottleneck as the ESXi uplinks.

## Guest preparation

Do these on ESXi, before the VM's cut-over window:

- Windows: install the VirtIO drivers (`virtio-win`) while the VM still
  runs on ESXi. A Windows guest whose boot disk is on a VirtIO SCSI
  controller does not boot without the driver. If a VM was moved without
  them, give its disk a SATA bus in PVE first, boot, install the drivers, then
  switch to VirtIO SCSI.
- Linux: the NIC changes from VMware's to VirtIO, so the interface name and
  MAC may change. Note the network configuration so you can fix it on first
  boot. Check that the initramfs contains the `virtio_scsi`/`virtio_blk`
  drivers (most distributions include them).
- VMware snapshots: delete or consolidate them first.
- VMware templates: import a template as an ordinary VM, then make it a PVE
  template with `qm template <vmid>`. Linked clones of it are full copies.

Proxmox's "Migrate to Proxmox VE" guide covers the guest side (VMware Tools,
drivers, network) in more depth.

## Step 1: Staging resource and NFS gateway (Haify)

Create one resource on the two storage nodes, then an NFS gateway on it. A
resource carries one gateway; give the resource nothing else to do (no
`ha create`, no manual mounts).

```bash
haify resource create --name vsanstage --port 7100 --size 2300G \
    --nodes stor1,stor2 --pool vg0
haify resource status vsanstage

haify gateway nfs create --resource vsanstage \
    --service-ip 192.0.2.60/24 --export-path /srv/vsan-staging \
    --allowed-ips 192.0.2.0/24
```

- The gateway adds its own small state volume and formats the data volume
  (`--fs-type`, default `ext4`; `xfs` and `btrfs` are accepted).
- `--export-path` must be a dedicated directory (`/srv/...`). It is both the
  mount point on the active node and the path ESXi mounts.
- The export maps every client to root (`rw,all_squash,anonuid=0,anongid=0`),
  which ESXi needs. Limit it with `--allowed-ips` to the ESXi VMkernel
  subnet (and the PVE nodes, for `qm disk import`). Without the flag, any
  address can mount it.

To verify:

```bash
haify gateway status --resource vsanstage    # running, on which node
ssh <active node> sudo exportfs -v          # the export and its options
```

Then test a switchover **before** any VM depends on the datastore. With the
datastore mounted on ESXi (step 2) and still empty, run
`haify ha evict vsanstage` and check that ESXi still shows it as accessible.

## Step 2: Mount it on ESXi

On every ESXi host that runs VMs of the wave (or once in vCenter: *New
Datastore → NFS → NFS 3*, selecting the hosts):

```bash
esxcli storage nfs add --host 192.0.2.60 --share /srv/vsan-staging --volume-name haify-staging
```

Use NFS 3.

To verify:

```bash
esxcli storage nfs list          # haify-staging: Accessible true, Mounted true, Read-Only false
vmkping 192.0.2.60
```

and create and delete a folder in the datastore browser.

## Step 3: Storage vMotion off vSAN

In vCenter: *VM → Migrate → Change storage only → haify-staging*, for each VM
of the wave (PowerCLI: `Move-VM -VM <name> -Datastore haify-staging`). Running
VMs keep running. Move a few at a time and avoid production hours (see the
network note under [Capacity planning](#capacity-planning)).

To verify:

- the migration task completed, and the VM's *Datastores* tab lists only
  `haify-staging`;
- on the gateway's active node, `ls -la /srv/vsan-staging/<vm>/` shows the
  `.vmx` and `.vmdk` files;
- `haify pool list` still has the room the rest of the wave needs.

You can undo this step completely: Storage vMotion the VM back to vSAN.

## Step 4: Import into Proxmox VE

Shut the VM down in vSphere first. Importing a running VM copies a disk that
is still changing. The VM's downtime starts here.

### With the import wizard

1. *Datacenter → Storage → Add → ESXi*: the ESXi host that has `haify-staging`
   mounted, with its credentials.
2. Select that storage, the VM, *Import*.
3. Target storage `haify0`, format raw (the only format the plugin
   supports). Check the CPU, memory, network bridge and disk bus the wizard
   proposes (SATA for a Windows VM without VirtIO drivers).
4. Leave *live import* off for the first VMs. Read Proxmox's notes on live
   import before you rely on it, and rehearse it on `haify0` first.

### With qm disk import

Use this for full control, or for a disk the wizard does not handle. Mount the
staging export read-only on a PVE node (the node must be inside
`--allowed-ips`) and import each disk into an empty VM.

```bash
mkdir -p /mnt/vsan-staging
mount -t nfs -o ro,vers=3 192.0.2.60:/srv/vsan-staging /mnt/vsan-staging

qm create 101 --name app01 --memory 8192 --cores 4 --ostype l26 \
    --scsihw virtio-scsi-single --net0 virtio,bridge=vmbr0
qm disk import 101 /mnt/vsan-staging/app01/app01.vmdk haify0 --format raw
qm set 101 --scsi0 <volume id printed by the import> --boot order=scsi0
```

Pass the descriptor `.vmdk`, not the `-flat.vmdk` next to it.

Either way, the plugin's `alloc_image` creates the disk: one DRBD resource per
disk, named `<resourceprefix>-<vmid>-<n>` (`pve-101-0` with the default
prefix).

### Sizes: exact by default

The plugin gives every new disk exactly the size PVE asks for. A VMware disk
whose size is not a whole number of GiB therefore arrives byte for byte.
Online Move Disk and vzdump restore onto `haify0` both depend on this, because
each refuses a disk that is not exactly the source's size. A storage set to
`exactsize 0` rounds disks up to whole GiB instead, and loses both.

To verify:

```bash
qm config 101                              # disks on haify0, sizes as expected
pvesm list haify0 --vmid 101
haify resource list | grep pve-101-          # one resource per disk, on stor1 and stor2
haify resource status pve-101-0              # UpToDate on both replicas
```

## Step 5: First boot and hand-over to PVE

1. Start the VM from the console. Fix the network configuration if the
   interface changed. On Windows, check the VirtIO devices in Device Manager,
   and switch a SATA disk to VirtIO SCSI if you used that workaround.
2. Install `qemu-guest-agent` and `qm set 101 --agent 1`; remove VMware Tools
   following Proxmox's guide.
3. The VM's owner checks the application.
4. Put it under PVE HA (`ha-manager add vm:101`), with the node preference the
   plugin README describes, and into a backup job to PBS.
5. Live-migrate it once between the two storage nodes. It should copy RAM only.

## Step 6: Clean up

After the VM is accepted, delete it in vSphere (*Delete from Disk*). That
frees its space on the staging datastore for the next wave. After the last
wave:

```bash
umount /mnt/vsan-staging                                   # on PVE, if mounted
esxcli storage nfs remove --volume-name haify-staging         # on every ESXi host
haify gateway delete --resource vsanstage
haify resource delete vsanstage
```

## Rollback

The ESXi copy of a VM stays intact until you delete it in step 6, so every
step up to acceptance can be undone:

| Reached | To roll back |
| ------- | ------------ |
| Step 1-2 | Unmount the datastore; delete the gateway and resource |
| Step 3 (on staging, still running on ESXi) | Storage vMotion back to vSAN |
| Step 4 (imported, not started in PVE) | `qm destroy <vmid>` (removes its `haify0` disks); power the VM on in vSphere |
| Step 5 (running in PVE) | Stop the PVE VM, power the vSphere VM on. **Anything written in PVE since the cut-over is lost**; this runbook has no way back from PVE to vSphere |

Never run both copies at once: they would claim the same IP addresses, and
the application's data would diverge.

## Cut-over window checklist

Days before:

- [ ] Prerequisites done, gateway switchover tested (step 1), rehearsal VM
      migrated end to end.
- [ ] Wave's VMs on `haify-staging` (step 3); `haify pool list` has room for their
      final disks.
- [ ] VirtIO drivers in Windows guests; VMware snapshots consolidated.
- [ ] Owners know the window, the acceptance checks and the rollback point.
- [ ] `haify resource list` shows every resource UpToDate on both storage
      nodes; `haify health-check` passes on every node; `pvecm status` is
      quorate.

In the window, per VM:

- [ ] Shut down in vSphere.
- [ ] Import to `haify0` (step 4) and verify disks and sizes.
- [ ] First boot, network, guest agent (step 5).
- [ ] Owner accepts, or roll back (table above).
- [ ] HA and backup job added.

After the window:

- [ ] One PBS backup of each migrated VM has completed.
- [ ] Accepted VMs deleted in vSphere; staging space freed.
- [ ] After the last wave: staging datastore, gateway and resource removed.

## Known limitations

- vSAN disks cannot be imported directly, which is why the staging
  datastore exists. A VM already on a VMFS or NFS datastore can skip steps 1-3.
- A storage set to `exactsize 0` cannot take an online Move Disk or a vzdump
  restore: see [Sizes](#sizes-exact-by-default).
- Windows guests need the VirtIO drivers before cut-over, or the SATA
  workaround.
- Storage vMotion loads the network and the staging resource's
  replication link, so spread the waves out.
- Staging capacity limits the size of a wave.
- The plugin's limits apply to imported VMs: raw only, linked clones are full
  copies, and a snapshot can be opened only on a node that holds a replica
  (plugin README,
  [Limitations](../deploy/proxmox/README.md#limitations)).
- VM-level HA, fencing and PBS backups are PVE's job, outside Haify's scope.
