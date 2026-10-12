# Haify storage plugin for Proxmox VE

Back Proxmox VM disks with DRBD resources managed by haify-controller. Guests
get synchronous replication, HA restart on a surviving node, and RAM-only live
migration: the disk is already on every node, so migration copies no data.

This is the Proxmox counterpart of the Kubernetes CSI driver in `deploy/k8s`.
For moving VMs off
VMware vSAN onto this storage, see
[Evacuating VMware vSAN](../../docs/vsan-evacuation.md).

## Bootstrap a cluster

`bootstrap.sh` takes an existing PVE cluster to a working `haify` storage in one
command. Run it as root on any one PVE node, either from a checkout holding
Linux binaries for the nodes (`GOOS=linux GOARCH=amd64 make build` puts them in
`bin/`) or with `HAIFY_CONTROLLER_DEB` pointing at a
`haify-controller_<version>_<arch>.deb`:

```bash
./deploy/proxmox/bootstrap.sh --devices /dev/sdb --vip 192.168.1.250/24 --dry-run
./deploy/proxmox/bootstrap.sh --devices /dev/sdb --vip 192.168.1.250/24
```

| Step | What it does | Skipped when |
| ---- | ------------ | ------------ |
| 1 | Reads the members from `/etc/pve/.members` (falling back to `pvecm nodes` + `corosync.conf`), refuses unless every member is online, and checks root SSH to each in batch mode, the way PVE itself connects (`HostKeyAlias=<node>`, the node's `ssh_known_hosts` in `/etc/pve`) | never: it changes nothing |
| 2 | On every node: the LINBIT repository (`packages.linbit.com/public`, suite `proxmox-8` or `proxmox-9` from `pveversion`), headers for the running kernel plus `proxmox-default-headers`, `drbd-dkms drbd-utils drbd-reactor sudo`; loads DRBD and requires 9.x; writes a minimal `/etc/drbd-reactor.toml` if there is none; enables drbd-reactor | repository line present, packages installed, DRBD 9 loaded, reactor running |
| 3 | `/root/.dispatch/config.toml` on every node that can run the controller: root, PVE's root key, one section per node address | file identical; a different existing file is kept, with a warning |
| 4 | Installs the controller there: the `.deb`, or `haify-controller` + `service-ip` to `/opt/haify/bin`, `service-ip` + `haify` to `/usr/local/bin` and both systemd units, as `make install-controller` does; writes `/etc/haify/controller.toml` from `configs/controller.toml.example` with `listen_address = "0.0.0.0"` and the dispatch path pinned | same package version / same file checksums; an existing `controller.toml` is never replaced |
| 5 | Starts the controller on this node (or the first storage node), registers every PVE node under its PVE name and corosync address, creates the pool on each storage node | a controller already running anywhere is used; node registered; pool present |
| 6 | `haify ha self enable --vip ... --pool ... --nodes <storage nodes>` and waits for the controller to answer on the VIP | one storage node, `--no-self-ha`, or Self-HA already on |
| 7 | Copies this directory to each node, runs `preflight.sh` and `install.sh` (or installs `HAIFY_PLUGIN_DEB`) | installed plugin files match these (or that package version) |
| 8 | `pvesm add haify <id> --controller <every controller-capable node> --haifypool <pool> --replicas <n> --storagetype <pool type> --content images,rootdir --shared 1` | the storage ID exists as type `haify` |
| 9 | Reports a missing corosync QDevice (even vote count) and a missing DRBD tiebreaker (two replicas, fewer than three nodes) | never: it only reports |

The script never guesses disks; `--devices` names them. The path must be the
same on every storage node; use `--node-devices pve3=/dev/nvme1n1` for a node
that differs. `/dev/disk/by-id/` paths are safest. All disks are checked before
step 2, so a bad one stops the run before anything is installed. A disk with
partitions or any signature `wipefs` reports is refused unless `--force-wipe`
is given. A disk that is mounted or held by LVM/dm/md (a PVE system disk,
`local-lvm`) is refused even then. `--storage-nodes` limits the pool to some
nodes; the rest still get DRBD, registration and the plugin, and run guests as
diskless DRBD clients.

Rerunning the script is safe. Each step checks before it changes anything, so
on a bootstrapped cluster the run prints only what it found. With `--dry-run`
it runs the read-only checks over SSH and prints each change, with its node,
instead of making it. On failure it stops at the first failed step and prints
the command to resume with `--from-step N`; step 1 always runs again, since
every later step needs the member list. `--yes` skips the confirmation prompt.
`--help` lists every option, including the environment variables
(`HAIFY_BIN_DIR`, `HAIFY_CONFIG_DIR`, `HAIFY_CONTROLLER_DEB`,
`HAIFY_PLUGIN_DEB`, `HAIFY_LINBIT_REPO`, `HAIFY_LINBIT_KEY_URL`,
`HAIFY_LINBIT_KEY_FINGERPRINT`).

The script does not:

- create the PVE cluster (run `pvecm create` / `pvecm add` first; a one-node
  cluster is fine);
- set up a QDevice or PVE HA rules (see [HA](#ha));
- turn on API auth or TLS (the controller's API is open on the network until
  `[auth]`/`[rbac]` and the storage's `apitoken` are set);
- put DRBD on a separate storage network;
- create ZFS pools (`--pool-type` is `lvm-thin` or `lvm`, because Self-HA's
  metadata volume is LVM);
- restart a controller whose binary it updated (it says so; restart it
  yourself);
- remove anything.

Before running it on a real cluster:

- **`apt-get update` must succeed on every node.** A node with the
  `pve-enterprise` repository and no subscription fails it; switch that node to
  the no-subscription repository first.
- The LINBIT key is fetched over HTTPS and trusted on that basis unless
  `HAIFY_LINBIT_KEY_FINGERPRINT` is set, in which case a key with another
  fingerprint is refused. Take the fingerprint from LINBIT's documentation.
  If the public Proxmox suite lacks a package (`drbd-reactor` among them), set
  `HAIFY_LINBIT_REPO` to a source line that has it.
- **Kernel upgrades rebuild DRBD.** DRBD is a DKMS module, and a PVE kernel
  newer than the installed `drbd-dkms` supports leaves the node without DRBD
  after the reboot. After every kernel upgrade, before rebooting, check that
  `dkms status drbd` lists the new kernel as installed.
  `proxmox-default-headers` is installed so new kernels get headers.
- Nodes are registered and reached at the address PVE lists for them (the
  corosync link). To move DRBD traffic to a storage network afterwards, run
  `haify node set-address <node> <ip> --replication-address <ip>`.
- The VIP must be a free address in the nodes' subnet. Self-HA moves the
  controller's database onto DRBD, which takes a minute or two; its log is
  `/var/log/haify/selfha-handoff.log` on the node that ran it.
- `install.sh` restarts the PVE daemons that load storage plugins (`pvedaemon`,
  `pveproxy`, `pvestatd`, `pvescheduler`, `pve-ha-crm`, `pve-ha-lrm`), which
  running guests do not notice, and adds the LVM filter described under
  Requirements. HA cannot start a guest on a node whose `pve-ha-lrm` predates
  the plugin.

## What it is

A Perl module (`HaifyPlugin.pm`, storage type `haify`, with its REST client and
naming helpers under `PVE/Storage/Custom/Haify/`) that translates Proxmox storage
API calls into haify-controller REST calls and holds no storage logic of its own.
It uses `HTTP::Tiny` + `JSON::PP`, both of which ship with Proxmox VE, so a PVE
node needs no extra packages and no Haify binaries.

The plugin implements storage API versions 11 to 16 and declares the version
the PVE release speaks when it falls in that range, 16 on anything newer. On a
release whose accepted window lies entirely outside 11 to 16, `api()` reports
the accepted version nearest to that range, so the storage still loads.

## Requirements

Each PVE node that will run guests off Haify storage needs:

- The DRBD 9 kernel module and `drbd-utils`. The hypervisor has to see
  `/dev/drbdN` locally to back a VM disk. Install LINBIT's `drbd-dkms` and
  `drbd-utils`.
- `sudo`, and SSH access from the controller. The controller runs `drbdadm`
  and writes `/etc/drbd.d` on this node over SSH through `sudo`. A minimal
  Debian/PVE install may not ship `sudo`.
- Registration as a Haify node under its PVE node name (`hostname`), e.g.
  `haify node register --name pve1 --address <ip>`. The plugin attaches and
  promotes by node name.
- Network access to the controller's REST port (default 3375).
- An LVM filter that skips DRBD devices. A guest that uses LVM inside its
  disk writes a PV header to it, and this host sees that disk as `/dev/drbdN`
  (and, for an encrypted resource, as the `/dev/mapper/haify_*` container under
  it). Without the filter, the host's LVM finds the guest's volume group and may
  activate it, which holds the device open, and the VM can then neither migrate
  nor fail over. The package (or `install.sh`) prepends
  `"r|^/dev/drbd|", "r|^/dev/mapper/haify_|"` to `global_filter` in
  `/etc/lvm/lvm.conf`, keeping PVE's own entries (`lvm-filter.sh`;
  `HAIFY_SKIP_LVM_FILTER=1` skips it). It also needs `devices/scan_lvs = 0`, the
  default, so the backing LVs are not scanned.

A PVE node does not need to contribute any disks. In the normal topology,
storage nodes hold the replicas and PVE nodes run the guests: a compute-only
hypervisor attaches to each volume as a diskless client.

`preflight.sh <controller>[,<controller>...]` (the `controller` value from
storage.cfg; `HAIFY_CA=<file>` for a private CA) checks all of these
requirements except SSH. It exits non-zero if anything required is missing; a
missing LVM filter is only a warning, because installing adds it. The script is
in this directory and, once the package is installed, in
`/usr/share/haify-pve-plugin/`.

## Install

### From the package (preferred)

Build the package once, on any Debian/Ubuntu machine with a checkout (it needs
only `dpkg-deb` and Perl, not Go or Node.js):

```bash
make deb-pve-plugin        # dist/haify-pve-plugin_<version>_all.deb
```

`make deb` builds it along with the controller packages. Then, on every PVE
node:

```bash
./preflight.sh 192.168.1.10                  # from the checkout: verify prerequisites first
apt install ./haify-pve-plugin_*_all.deb       # the ./ makes apt install a local file
```

The package replaces `install.sh` and installs the same modules:
`HaifyPlugin.pm` in `/usr/share/perl5/PVE/Storage/Custom/` and the helpers in
`.../Custom/Haify/`, plus `lvm-filter.sh` and `preflight.sh` in
`/usr/share/haify-pve-plugin/`. It depends on `libpve-storage-perl`,
`drbd-utils` and `lvm2`; the DRBD 9 kernel module (`drbd-dkms`) is still
yours to install. On installation and on every upgrade it:

1. checks that the module compiles (`perl -c`). If it does not, it stops
   there: the PVE daemons are not restarted and keep running the previous
   plugin, and apt reports the package as not configured;
2. adds the LVM filter with `lvm-filter.sh`, unless `HAIFY_SKIP_LVM_FILTER=1`
   (`HAIFY_SKIP_LVM_FILTER=1 apt install ./haify-pve-plugin_*_all.deb`). A
   failure there is a warning, not an error;
3. runs `systemctl try-restart` on `pvedaemon pveproxy pvestatd pvescheduler
   pve-ha-crm pve-ha-lrm`, every daemon that loads storage plugins, which does
   not affect running guests.

`apt remove haify-pve-plugin` removes the modules and restarts the same daemons.
It changes nothing else. It warns if `storage.cfg` still has `haify:` entries
but leaves them in place (other nodes may still use them), and it leaves the
LVM filter in `lvm.conf`.

A node set up with `install.sh` can move to the package directly, since dpkg
replaces the files in place. Do not run `install.sh --uninstall` afterwards:
it deletes files the package now owns.

### With install.sh

Without the package, on every PVE node:

```bash
./preflight.sh 192.168.1.10     # verify prerequisites first
sudo ./install.sh               # compile-checks, copies the modules, adds the LVM filter, restarts the PVE daemons
```

`install.sh` does what the package does, from the checkout and without dpkg
knowing about the files. Uninstall with `sudo ./install.sh --uninstall` after
removing the `haify:` entries from `storage.cfg`.

### Storage entry

With the plugin on every node, add the storage once for the cluster: in the web
interface under **Datacenter → Storage → Add → Haify**, or as an entry in
`/etc/pve/storage.cfg` (cluster-wide):

```
haify: haify0
        controller 192.168.1.10
        haifypool haify_vg0
        replicas 2
        content images
        shared 1
```

`shared 1` makes PVE treat the disk as reachable from every node, so live
migration copies only RAM and `ha-manager` may restart a guest anywhere.

The dialog has every option listed below. On **Add** it fills in the
controller list with the cluster nodes' addresses, and **Pool** lists the Haify
volume groups this node holds. The controller list is fixed once the storage
exists.

Each Haify storage's page (a node → the storage) has a **Haify** tab. It lists
every disk with the nodes holding a replica and their state, the node the guest
runs on, and whether the replicas are in step, read from that node's own DRBD
every 10 seconds. The plugin adds these as `haify-*` fields to the volumes
PVE's storage content API returns, so
`pvesh get /nodes/<node>/storage/<id>/content` shows them too.

PVE gives a storage plugin no way to add itself to its interface, so the plugin
adds `haify-storage.js` to the page template
(`/usr/share/pve-manager/index.html.tpl`, one script tag after
`pvemanagerlib.js`). An apt hook (`/etc/apt/apt.conf.d/90haify-pve-gui`) adds
it again after a `pve-manager` upgrade replaces the template. Uninstalling
removes the tag, the script and the hook.

### Options

| Option | Meaning |
| ------ | ------- |
| `controller` | Required. Comma-separated addresses, each `host`, `host:port` or `[v6]:port` (port defaults to 3375), optionally prefixed with `https://`. Under Self-HA list every node that can run the controller: an address that refuses the connection is skipped and the next tried; a request that reached a controller and then failed is never resent elsewhere. `pvesm set` cannot change it (it is a fixed option); edit `/etc/pve/storage.cfg` to add addresses |
| `controllerca` | PEM CA bundle that signs the controller's certificate, for `https://` addresses, e.g. kept in `/etc/pve` so every node has it. Unset: the system trust store. The certificate is verified, names included, so it must cover the addresses listed. `https://` needs `[tls] rest = true` on the controller |
| `haifypool` | Haify pool new volumes are carved from, as `haify pool list` prints it (`haify_vg0`) or without the prefix (`vg0`) |
| `haifynodes` | Comma-separated Haify nodes to place replicas on. Takes precedence over `replicas` |
| `replicas` | Replica count for auto-placement by free space (1-16) |
| `storagetype` | `lvm`, `lvm-thin` or `zfs`. Unset: the controller's default |
| `resourceprefix` | Prefix for generated resource names (default `pve`). Give each PVE cluster its own when several share one Haify cluster: VM ids are only unique within a PVE cluster |
| `apitoken` | Bearer token when Haify `[auth]`/`[rbac]` is enabled. Set it in the web interface or with `pvesm add/set --apitoken`: PVE treats it as sensitive and the plugin keeps it in `/etc/pve/priv/storage/<id>.haify-token` (root only, every node), not in `storage.cfg`. A token written into `storage.cfg` by hand still works, but that file is readable cluster-wide |
| `onnoquorum` | What a new disk does when its node loses quorum or every UpToDate copy: `suspend-io` (default; the guest's I/O freezes and carries on when quorum returns) or `io-error` (the guest sees I/O errors and typically remounts read-only). Applies to disks created from then on; change an existing one with `haify resource set-options <resource> --drbd-options on-no-quorum=<value>,on-no-data-accessible=<value>` |
| `exactsize` | `1` (the default) gives each new or resized disk exactly the size PVE asks for, rounded up to a 512-byte sector (the backing volume is still allocated in GiB; the DRBD device is capped at the exact size). `0` rounds up to the next whole GiB instead, and then restoring a vzdump backup and online Move Disk onto this storage both fail: they refuse a disk that is not byte-for-byte the source's size. Default `1` |

Standard PVE options `nodes`, `disable`, `content`, `shared` and `bwlimit` are
also accepted. `content` may be `images` and `rootdir`; the only format is
`raw`. See `storage.cfg.example` for commented entries.

## How it maps

| Proxmox | Haify REST call |
| --- | --- |
| one VM disk `vm-<vmid>-disk-<n>` | one DRBD resource `<prefix>-<vmid>-<n>` |
| another volume of the VM: `vm-<vmid>-cloudinit`, `vm-<vmid>-state-<snap>` (a snapshot's RAM), `vm-<vmid>-fleece-<n>` (backup fleecing) | one DRBD resource `<prefix>-<vmid>-<name>` |
| `alloc_image` | `POST /v1/resources` (the exact size PVE asks for, or whole GiB with `exactsize 0`; protocol C) |
| `free_image` | `DELETE /v1/resources/<res>` (cascade teardown) |
| `list_images` | `GET /v1/resources`, filtered by `<prefix>-` |
| `activate_volume` | `POST .../diskless-clients` if this node is not in the resource, then a quorum-guarded `POST .../primary` |
| `deactivate_volume` | `POST .../secondary`, then close any dual-primary window, then `DELETE .../diskless-clients/<node>` if this node is only a diskless client |
| `volume_resize` | `PATCH /v1/resources/<res>/volumes/0` (online grow) |
| `volume_snapshot` / rollback / delete | `POST/DELETE /v1/resources/<res>/snapshots[/<name>[/rollback]]`: a snapshot on every replica |
| a template's disk `base-<vmid>-disk-<n>` | DRBD resource `<prefix>-base-<vmid>-<n>` |
| `create_base` / `rename_volume` | `POST /v1/resources/<res>/rename` |
| `clone_image` | a new disk on the template's nodes, filled with `POST .../populate` |
| `status` | `GET /v1/pools`; reports the smallest node's copy of `haifypool` |

One disk per resource means each disk resizes, snapshots and deletes
independently, the same model the CSI driver uses for a PVC.

The Haify web interface shows the same disks from the storage side, under
**Proxmox VE**: one card per guest, with each disk's replicas and whether they
are in step, the node the guest runs on, and a link to it in the PVE interface.

Capacity is reported for the smallest node, because a replica must fit on every
node that holds one; reporting the sum would let PVE accept a disk that cannot
be placed. On a thin pool the figure is the thin pool's own size and unused
data space (`thinSizeBytes`, `thinDataPercent`). The volume group's free space
would read ~0 however empty the pool is, since the group is nearly all thin
pool. A storage set to `storagetype lvm` allocates thick LVs from the group and
reports the group.

Snapshots are taken on every diskful replica at once, with I/O suspended
across them for the moment it takes (a timer on each node resumes I/O after a
minute whatever happens to the controller). Each copy also carries its
replica's DRBD metadata, so rolling back restores every replica together and
DRBD resyncs nothing. Losing a node loses one copy of the snapshot, and the
snapshot survives on the other replicas. A snapshot taken by an earlier version
of the plugin lives on one replica only and is rolled back the old way: that
replica is restored and the others resync the whole disk from it.

A snapshot can be opened read-only on a node that holds a replica, which is
what `vzdump` in snapshot mode needs for a container. On a node without a
replica the backup fails, naming the replica nodes to run it on.

Converting a VM to a template renames its disks to `base-<vmid>-disk-<n>`; the
data stays where it is. A linked clone of a template is a full copy, because a
DRBD device has no image-level copy-on-write. The copy is made on the
template's replica nodes at the template's exact size, so it is independent of
the template. Reassigning a disk to another VM renames it. Both renames need
the disk stopped and without snapshots; the error says what is in the way.

## Live migration and the dual-primary window

During a live migration the source and target both hold the disk open for a
moment. DRBD forbids two Primaries unless `allow-two-primaries` is set, so the
plugin brackets the hand-off:

- `activate_volume` opens the window only when another node currently holds
  Primary and PVE is live-migrating the VM from there, which it detects by the
  VM's config still sitting on the source node with `lock: migrate`. Any other
  Primary is a leftover (typically an earlier `deactivate_volume` that could not
  reach the controller). Activation then fails and names the node, instead of
  letting the guest run with two writers allowed. Demote the leftover with
  `haify resource secondary <resource> <node>` and start the guest again.
  Containers never qualify, because they migrate by restart.
- The window is opened only on the migration's source and target, the two ends
  of the one connection that carries two Primaries. A host that is down
  elsewhere in the resource does not block the migration.
- The window is closed again on every exit path: a failed promote, a device that
  does not appear within 20 seconds, and `deactivate_volume` (which closes
  unconditionally, since the source deactivates after hand-off).

The toggle is runtime-only (`drbdadm net-options`) and is never written to the
`.res` file, so a crashed controller cannot strand it: a reboot or
`drbdadm adjust` restores single-primary. To check or repair by hand:

```bash
haify resource dual-primary <resource> off
```

**This is not a way to use one volume from two machines.** An ordinary
filesystem mounted twice will corrupt regardless of what DRBD permits.

## Diskless clients come and go

A host with no replica of a disk attaches to it as a diskless client when the
guest starts there, and detaches when the guest stops or migrates away. In
earlier versions hosts stayed attached to every disk of every guest they had
ever run, so one of them being down blocked operations on all of those disks. A
detach that fails is logged and leaves the host attached, as before. A client
that is a resource's last quorum vote besides two replicas is kept as the
resource's tiebreaker instead of being removed.

## When the controller is unreachable

Starting and stopping a guest go through the controller. In earlier versions an
unreachable controller meant no guest could start and HA could restart none.
Now, when no controller answers and the disk is already up on this node:

- `activate_volume` promotes it with plain `drbdadm primary`, never with
  `--force`, so DRBD still refuses without quorum and an UpToDate copy within
  reach. It is refused while another node holds the disk Primary: only the
  controller can tell a live migration from a leftover.
- the disk's path resolves locally (`/dev/drbd/by-res/<resource>/0`), so qemu
  can be started.
- `deactivate_volume` demotes with `drbdadm secondary` and clears this node's
  side of any dual-primary window. Run `haify resource dual-primary <resource>
  off` once the controller is back if a migration was under way.

A disk that is not up on the node, a first attach, and a live migration still
need the controller. An error from a controller that answered is never
bypassed.

## HA

When `ha-manager` restarts a guest elsewhere, it calls `activate_volume` on the
new node, and the promotion there is quorum-guarded: Haify force-promotes only
if that node holds DRBD quorum and refuses otherwise. A partitioned node
therefore cannot take over, so HA failover cannot split-brain. The trade-off is
deliberate: a resource that has lost quorum will not fail over automatically.
Nor does a node take over with an Outdated copy, one a peer marked as missing
writes, while the node holding those writes is away: the guest waits for that
node to come back rather than start on old data.

Rebooting a node with guests running is safe: the installer orders drbd-utils'
`drbd-graceful-disconnect.service` after `pve-guests` and `pve-ha-lrm`, so the
guests are stopped and their disks demoted before DRBD disconnects. Without
that order the disconnect came first, a guest kept writing to a copy its peers
had just been told was the only current one, and HA started it on another node
with the older data.

PVE decides where a guest runs, and with it where its disks are Primary.
`haify ha create` and the Haify gateways would put a drbd-reactor promoter on
the disk to fight PVE for the role, so both refuse a disk the plugin created
(the plugin labels each one `haify.pve/managed-by=pve`) and any resource with
diskless clients. Use PVE HA for guests; do not use drbd-reactor for them.

### Where HA should restart a guest

A guest runs on any PVE node. A node without a replica attaches as a diskless
client and reads and writes over the network, so every node is a valid HA
target, but the replica nodes are the fast ones. Tell `ha-manager` to prefer
them without forbidding the rest (a strict rule would leave the guest down
when both replica nodes are down):

```bash
haify resource list | grep pve-100-        # the guest's disks and their nodes

# PVE 9: node affinity rules
ha-manager rules add node-affinity haify-vm-100 --resources vm:100 \
    --nodes pve1:2,pve2:2,pve3:1 --strict 0

# PVE 8: HA groups
ha-manager groupadd haify-pve1-pve2 --nodes pve1:2,pve2:2,pve3:1
ha-manager set vm:100 --group haify-pve1-pve2
```

In the web interface, **Datacenter → HA** shows each HA guest's replica nodes,
with a warning sign when the guest runs on a node without one. On PVE 9,
**Prefer nodes with Haify replicas** writes or updates such a non-strict rule,
`haify-vm-<id>`, for every HA guest with disks on Haify; on PVE 8 only the
column is shown. A rule takes effect at once: a guest running on another node
is migrated to a replica node right away.

Guests whose disks share replica nodes can share a rule or group. A guest
whose disks sit on different replica pairs prefers the nodes common to all of
them. In a "two storage nodes plus one tiebreaker" cluster, the third machine
is both a DRBD tiebreaker for Haify and a QDevice for corosync. It needs both
roles because a QDevice alone gives DRBD no quorum vote. It is usually not a
PVE node at all; when it is one, give it the lowest priority.

## Thin pools and Discard

On a thin pool, give each disk **Discard** (`discard=on`; the checkbox under
the disk's Advanced options). Without it QEMU drops the guest's TRIM, so space
a guest frees inside its filesystem stays allocated in the pool on every
replica. With it, a guest `fstrim` reaches every replica's thin pool through
DRBD, diskless clients included:

```bash
qm set 103 --scsi0 haify0:vm-103-disk-0,discard=on
```

With Discard on, an `fstrim` in the guest returns the space it deleted to the
thin pools of every replica. The controller's daily trim (`[storage.thin] trim_schedule`) covers only
filesystems Haify mounts itself. Guest disks need Discard.

## I/O limits

PVE limits a VM disk's I/O itself: QEMU enforces the disk options `mbps_rd`,
`mbps_wr`, `iops_rd`, `iops_wr` (and their `_max` bursts) on the guest's
requests, whatever the storage. They work on Haify disks as on any other; set
them in the VM's hardware tab or with
`qm set <vmid> --scsi0 haify0:vm-<vmid>-disk-0,iops_wr=2000`. Haify adds no
limits of its own.

## Limitations

- The only format is raw. DRBD exports a raw block device; qcow2 is not
  supported and not needed, since snapshots come from Haify rather than the
  image format.
- Linked clones are full copies. They take the template's full size and the
  time to copy it.
- Volume names are `vm-<vmid>-<name>`, with `<name>` starting with a letter
  and made of letters, digits, `_` and `-` (at most 64), and
  `base-<vmid>-disk-<n>`. PVE's own names all fit.
- Snapshots can be accessed only on a node holding a replica, and only for LVM.
- With `exactsize 0`, allocation is in whole gigabytes, which vzdump restore
  and Move Disk cannot use.
- WAN resources are refused for dual-primary, so a guest cannot live-migrate
  across a WAN-replicated (asynchronous) resource.

## Tests

```bash
cd deploy/proxmox && prove t/
```

The suite stubs the PVE modules and the REST client, so it runs on any machine
with plain Perl.

```bash
./deploy/proxmox/bootstrap/test.sh
```

runs `bootstrap.sh --dry-run` against a stub cluster (an `ssh` on `PATH` that
answers the read-only checks as a fresh or a bootstrapped node would). It
needs bash and perl, not PVE.
