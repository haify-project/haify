# Haify storage plugin for Proxmox VE

This plugin puts Proxmox VM disks on DRBD resources that haify-controller
manages. Every write reaches all replicas before it completes. When a node
fails, PVE HA restarts the guest on a surviving node. Live migration copies
only RAM: every node can already reach the disk, from its own replica or over
the network, so no disk data moves.

This is the Proxmox counterpart of the Kubernetes CSI driver in `deploy/k8s`.
To move VMs off VMware vSAN onto this storage, see
[Evacuating VMware vSAN](../../docs/vsan-evacuation.md).

## Bootstrap a cluster

`bootstrap.sh` turns an existing PVE cluster into one with a working `haify`
storage, in one command. Run it as root on any one PVE node. It takes the
controller from one of two places: a checkout with Linux binaries for the
nodes (`GOOS=linux GOARCH=amd64 make build` puts them in `bin/`), or
`HAIFY_CONTROLLER_DEB` set to a `haify-controller_<version>_<arch>.deb`:

```bash
./deploy/proxmox/bootstrap.sh --devices /dev/sdb --vip 192.168.1.250/24 --dry-run
./deploy/proxmox/bootstrap.sh --devices /dev/sdb --vip 192.168.1.250/24
```

| Step | What it does | Skipped when |
| ---- | ------------ | ------------ |
| 1 | Reads the member list from `/etc/pve/.members` (or from `pvecm nodes` + `corosync.conf` if that fails). Stops unless every member is online. Checks that root can SSH to each member without a prompt, the same way PVE connects (`HostKeyAlias=<node>`, the node's `ssh_known_hosts` in `/etc/pve`) | Never. It changes nothing |
| 2 | On every node: adds the LINBIT repository (`packages.linbit.com/public`, suite `proxmox-8` or `proxmox-9` to match `pveversion`). Installs headers for the running kernel, `proxmox-default-headers`, and `drbd-dkms drbd-utils drbd-reactor sudo`. Loads DRBD and stops if it is not 9.x. Writes a minimal `/etc/drbd-reactor.toml` if there is none, and enables drbd-reactor | The repository line is there, the packages are installed, DRBD 9 is loaded and the reactor runs |
| 3 | Writes `/root/.dispatch/config.toml` on every node that can run the controller. It logs in as root with PVE's root key and has one section per node address | The file is already identical. A different existing file is kept, with a warning |
| 4 | Installs the controller on those nodes: the `.deb`, or the binaries the way `make install-controller` does (`haify-controller` + `service-ip` to `/opt/haify/bin`, `service-ip` + `haify` to `/usr/local/bin`, both systemd units). Writes `/etc/haify/controller.toml` from `configs/controller.toml.example`, with `listen_address = "0.0.0.0"` and the dispatch config path filled in | Same package version, or same file checksums. An existing `controller.toml` is never replaced |
| 5 | Starts the controller on this node (or on the first storage node, if this one has no storage). Registers every PVE node under its PVE name and corosync address. Creates the pool on each storage node | A controller already running on any node is used. Registered nodes and existing pools are left alone |
| 6 | Runs `haify ha self enable --vip ... --pool ... --nodes <storage nodes>` and waits until the controller answers on the VIP | Only one storage node, `--no-self-ha`, or Self-HA already on |
| 7 | Copies this directory to each node and runs `preflight.sh`, then `install.sh` (or installs `HAIFY_PLUGIN_DEB` instead) | The installed plugin files already match these (or that package version is installed) |
| 8 | Runs `pvesm add haify <id> --controller <every controller-capable node> --haifypool <pool> --replicas <n> --storagetype <pool type> --content images,rootdir --shared 1` | A storage with that ID and type `haify` exists |
| 9 | Warns when corosync has an even vote count and no QDevice, and when two-replica disks would have no DRBD tiebreaker (fewer than three nodes) | Never. It only reports |

The script never picks a disk itself: you name them with `--devices`. That path
is used on every storage node. For a node where it differs, add
`--node-devices pve3=/dev/nvme1n1`. `/dev/disk/by-id/` paths are the safest
choice. All disks are checked before step 2, so a bad disk stops the run
before anything is installed. A disk with partitions, or with any signature
`wipefs` reports, is refused unless you pass `--force-wipe`. A disk that is
mounted or used by LVM, dm or md (a PVE system disk, `local-lvm`) is refused
even then. `--storage-nodes` puts the pool on some nodes only. The other nodes
still get DRBD, registration and the plugin, and run their guests as diskless
DRBD clients.

You can run the script again at any time. Each step checks the current state
before it changes anything, so on a cluster that is already set up it only
prints what it found. With `--dry-run` it runs the read-only checks over SSH
and prints each change it would make, with the node, instead of making it. If
a step fails, the script stops there and prints the command to resume with
`--from-step N`. Step 1 always runs again, because every later step needs the
member list. `--yes` skips the confirmation prompt. `--help` lists every
option and the environment variables
(`HAIFY_BIN_DIR`, `HAIFY_CONFIG_DIR`, `HAIFY_CONTROLLER_DEB`,
`HAIFY_PLUGIN_DEB`, `HAIFY_LINBIT_REPO`, `HAIFY_LINBIT_KEY_URL`,
`HAIFY_LINBIT_KEY_FINGERPRINT`).

The script does not:

- create the PVE cluster (run `pvecm create` / `pvecm add` first; a one-node
  cluster is fine);
- set up a QDevice or PVE HA rules (see [HA](#ha));
- turn on API auth or TLS. Until you set `[auth]`/`[rbac]` and the storage's
  `apitoken`, anyone on the network can use the controller's API;
- put DRBD on a separate storage network;
- create ZFS pools (`--pool-type` is `lvm-thin` or `lvm`, because Self-HA's
  metadata volume is LVM);
- restart a controller whose binary it updated (it tells you; restart it
  yourself);
- remove anything.

Before running it on a real cluster:

- **`apt-get update` must succeed on every node.** A node with the
  `pve-enterprise` repository and no subscription fails it; switch that node to
  the no-subscription repository first.
- The script downloads the LINBIT signing key over HTTPS and trusts it on that
  basis alone. To check the key, set `HAIFY_LINBIT_KEY_FINGERPRINT` to the
  fingerprint from LINBIT's documentation; a key with another fingerprint is
  then refused. If the public Proxmox suite lacks a package (`drbd-reactor`
  among them), set `HAIFY_LINBIT_REPO` to a source line that has it.
- **Kernel upgrades rebuild DRBD.** DRBD is a DKMS module. If a new PVE kernel
  is newer than the installed `drbd-dkms` supports, the node has no DRBD after
  the reboot. After every kernel upgrade, and before you reboot, check that
  `dkms status drbd` lists the new kernel as installed. The script installs
  `proxmox-default-headers` so that every new kernel gets its headers.
- Nodes are registered and reached at the address PVE lists for them (the
  corosync link). To move DRBD traffic to a storage network later, run
  `haify node set-address <node> <ip> --replication-address <ip>`.
- The VIP must be a free address in the nodes' subnet. Self-HA moves the
  controller's database onto DRBD, which takes a minute or two. Its log is
  `/var/log/haify/selfha-handoff.log` on the node that ran it.
- `install.sh` restarts the PVE daemons that load storage plugins (`pvedaemon`,
  `pveproxy`, `pvestatd`, `pvescheduler`, `pve-ha-crm`, `pve-ha-lrm`). Running
  guests are not affected. It also adds the LVM filter described under
  Requirements. HA cannot start a guest on a node whose `pve-ha-lrm` was
  started before the plugin was installed.

## What it is

The plugin is a Perl module, `HaifyPlugin.pm`, for storage type `haify`. Its
REST client and naming helpers live under `PVE/Storage/Custom/Haify/`. It turns
Proxmox storage API calls into haify-controller REST calls. Apart from a few
local commands (promoting or demoting a disk when no controller answers,
opening a snapshot), it keeps no storage logic of its own. It uses only `HTTP::Tiny` + `JSON::PP`, both of which ship
with Proxmox VE, so the plugin needs no extra Perl modules and no Haify
binaries on the node.

The plugin implements storage API versions 11 to 16. It tells PVE the version
PVE itself speaks when that is between 11 and 16, and 16 when PVE is newer. If
a PVE release accepts no version between 11 and 16 at all, `api()` reports the
accepted version closest to that range, so the storage still loads.

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
  disk writes an LVM header onto that disk. This host sees the same disk as
  `/dev/drbdN` (and, for an encrypted resource, also as the
  `/dev/mapper/haify_*` device under it). Without the filter, the host's LVM
  finds the guest's volume group and may activate it. That holds the device
  open, and the VM can then neither migrate nor fail over. The package (or
  `install.sh`) adds `"r|^/dev/drbd|", "r|^/dev/mapper/haify_|"` to the front
  of `global_filter` in `/etc/lvm/lvm.conf` and keeps PVE's own entries
  (`lvm-filter.sh`; `HAIFY_SKIP_LVM_FILTER=1` skips it). LVM must also keep
  `devices/scan_lvs = 0`, the default, so it does not scan the backing LVs.

A PVE node does not have to provide any disks. In the usual layout, storage
nodes hold the replicas and PVE nodes run the guests. A PVE node without a
replica attaches to each disk it uses as a diskless client.

`preflight.sh <controller>[,<controller>...]` checks all of these requirements
except SSH. Pass it the `controller` value from storage.cfg, and
`HAIFY_CA=<file>` if a private CA signed the controller's certificate. It exits
non-zero if anything required is missing. A missing LVM filter is only a
warning, because installing the plugin adds it. The script is in this
directory and, once the package is installed, also in
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

The package takes the place of `install.sh` and installs the same modules:
`HaifyPlugin.pm` in `/usr/share/perl5/PVE/Storage/Custom/` and the helpers in
`.../Custom/Haify/`, plus `lvm-filter.sh` and `preflight.sh` in
`/usr/share/haify-pve-plugin/`. It depends on `libpve-storage-perl`,
`drbd-utils` and `lvm2`. You still install the DRBD 9 kernel module
(`drbd-dkms`) yourself. On installation and on every upgrade it:

1. checks that the module compiles (`perl -c`). If it does not, it stops
   there: the PVE daemons are not restarted and keep running the previous
   plugin, and apt reports the package as not configured;
2. adds the LVM filter with `lvm-filter.sh`, unless `HAIFY_SKIP_LVM_FILTER=1`
   (`HAIFY_SKIP_LVM_FILTER=1 apt install ./haify-pve-plugin_*_all.deb`). If
   that fails, you get a warning, not an error;
3. adds Haify to the web interface (see Storage entry below);
4. orders DRBD's shutdown after the guests (see the HA section);
5. runs `systemctl try-restart` on `pvedaemon pveproxy pvestatd pvescheduler
   pve-ha-crm pve-ha-lrm`, the daemons that load storage plugins. Running
   guests are not affected.

`apt remove haify-pve-plugin` removes the modules, takes Haify out of the web
interface and restarts the same daemons. If `storage.cfg` still has `haify:`
entries, it warns but leaves them in place, because other nodes may still use
them. It also leaves the LVM filter in `lvm.conf`.

You can move a node from `install.sh` to the package just by installing the
package: dpkg replaces the files in place. Do not run `install.sh --uninstall`
afterwards, because it deletes files the package now owns.

### With install.sh

Without the package, on every PVE node:

```bash
./preflight.sh 192.168.1.10     # verify prerequisites first
sudo ./install.sh               # compile-checks, copies the modules, adds the LVM filter, restarts the PVE daemons
```

`install.sh` does what the package does, straight from the checkout, without
dpkg tracking the files. To uninstall, first remove the `haify:` entries from `storage.cfg`,
then run `sudo ./install.sh --uninstall`.

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

`shared 1` tells PVE that every node can reach the disk. Live migration then
copies only RAM, and `ha-manager` may restart a guest on any node.

The dialog has every option listed below. On **Add** it fills in the
controller list with the cluster nodes' addresses, and **Pool** lists the Haify
volume groups this node holds. The controller list cannot be changed once the
storage exists.

Each Haify storage's page (a node → the storage) has a **Haify** tab. For every
disk it shows the nodes that hold a replica and their state, the node the guest
runs on, and whether the replicas are in sync. The data comes from the DRBD
state on the node you are viewing and refreshes every 10 seconds. The plugin
adds these as `haify-*` fields to the volumes in PVE's storage content API, so
`pvesh get /nodes/<node>/storage/<id>/content` shows them too.

PVE has no way for a storage plugin to add itself to the web interface. So the
plugin adds `haify-storage.js` to the page template: one script tag after
`pvemanagerlib.js` in `/usr/share/pve-manager/index.html.tpl`. A `pve-manager`
upgrade replaces the template, so an apt hook
(`/etc/apt/apt.conf.d/90haify-pve-gui`) adds the tag again afterwards.
Uninstalling removes the tag, the script and the hook. With the package, the
hook stays until `apt purge`; it does nothing once the plugin is gone.

### Options

| Option | Meaning |
| ------ | ------- |
| `controller` | Required. Comma-separated addresses, each `host`, `host:port` or `[v6]:port` (default port 3375), optionally with `https://` in front. Under Self-HA, list every node that can run the controller. The plugin skips an address that refuses the connection and tries the next. Once a request has reached a controller, the plugin never sends it to another one, even if it then fails, because the first may have carried it out. `pvesm set` cannot change this option; edit `/etc/pve/storage.cfg` to add addresses |
| `controllerca` | The PEM CA bundle that signed the controller's certificate, for `https://` addresses. Keep it in `/etc/pve` so every node has it. Unset: the system trust store. The plugin checks the certificate including its names, so the certificate must cover every address listed. `https://` needs `[tls] rest = true` on the controller |
| `haifypool` | The Haify pool new disks are created in, written as `haify pool list` prints it (`haify_vg0`) or without the prefix (`vg0`) |
| `haifynodes` | Comma-separated Haify nodes to put the replicas on. Overrides `replicas` |
| `replicas` | How many replicas to create when Haify picks the nodes by free space (1-16) |
| `storagetype` | `lvm`, `lvm-thin` or `zfs`. Unset: the controller's default |
| `resourceprefix` | Prefix for generated resource names (default `pve`). If several PVE clusters share one Haify cluster, give each its own prefix, because VM ids are unique only within one PVE cluster |
| `apitoken` | Bearer token, needed when Haify `[auth]`/`[rbac]` is on. Set it in the web interface or with `pvesm add/set --apitoken`. PVE treats it as a secret, and the plugin keeps it in `/etc/pve/priv/storage/<id>.haify-token` (readable by root only, on every node), not in `storage.cfg`. A token written into `storage.cfg` by hand still works, but every node can read that file |
| `onnoquorum` | What a new disk does when its node loses quorum or can no longer reach an UpToDate copy. `suspend-io` (default): the guest's I/O pauses and resumes when quorum is back. `io-error`: the guest gets I/O errors and usually remounts its filesystems read-only. This applies to disks created after you set it. For an existing disk, run `haify resource set-options <resource> --drbd-options on-no-quorum=<value>,on-no-data-accessible=<value>` |
| `exactsize` | `1` (the default): each new or resized disk gets exactly the size PVE asks for, rounded up to a 512-byte sector. The backing volume is still allocated in whole GiB, but the DRBD device is limited to the exact size. `0`: sizes are rounded up to the next whole GiB. Then restoring a vzdump backup and an online Move Disk onto this storage both fail, because both refuse a disk that is not exactly the source's size. Default `1` |

Standard PVE options `nodes`, `disable`, `content`, `shared` and `bwlimit` are
also accepted. `content` may be `images` and `rootdir`; the only format is
`raw`. See `storage.cfg.example` for commented entries.

## How it maps

| Proxmox | Haify REST call |
| --- | --- |
| one VM disk `vm-<vmid>-disk-<n>` | one DRBD resource `<prefix>-<vmid>-<n>` |
| another volume of the VM: `vm-<vmid>-cloudinit`, `vm-<vmid>-state-<snap>` (a snapshot's RAM), `vm-<vmid>-fleece-<n>` (backup fleecing) | one DRBD resource `<prefix>-<vmid>-<name>` |
| `alloc_image` | `POST /v1/resources` (the exact size PVE asks for, or whole GiB with `exactsize 0`; protocol C) |
| `free_image` | `DELETE /v1/resources/<res>` (removes everything that belongs to the resource) |
| `list_images` | `GET /v1/resources`, filtered by `<prefix>-` |
| `activate_volume` | `POST .../diskless-clients` if this node is not in the resource, then a quorum-guarded `POST .../primary` |
| `deactivate_volume` | `POST .../secondary`, then close any dual-primary window, then `DELETE .../diskless-clients/<node>` if this node is only a diskless client |
| `volume_resize` | `PATCH /v1/resources/<res>/volumes/0` (grows the disk while it is in use) |
| `volume_snapshot` / rollback / delete | `POST/DELETE /v1/resources/<res>/snapshots[/<name>[/rollback]]`: a snapshot on every replica |
| a template's disk `base-<vmid>-disk-<n>` | DRBD resource `<prefix>-base-<vmid>-<n>` |
| `create_base` / `rename_volume` | `POST /v1/resources/<res>/rename` |
| `clone_image` | a new disk on the template's nodes, filled with `POST .../populate` |
| `status` | `GET /v1/pools`; reports the smallest node's copy of `haifypool` |

Because each disk is its own resource, you can resize, snapshot and delete
each disk on its own. The CSI driver does the same for each PVC.

The Haify web interface shows the same disks from the storage side, under
**Proxmox VE**: one card per guest, with each disk's replicas and whether they
are in sync, the node the guest runs on, and a link to it in the PVE interface.

The storage reports the capacity of its smallest node, because every replica
has to fit on its own node. Reporting the sum would let PVE accept a disk that
cannot be placed. On a thin pool, the figures are the thin pool's own size and
free data space (`thinSizeBytes`, `thinDataPercent`). The volume group's free
space would show close to 0 however empty the pool is, because the thin pool
takes up nearly the whole group. A storage set to `storagetype lvm` creates
thick LVs directly in the group, so it reports the group's figures.

A snapshot is taken on every diskful replica at the same moment. The disk's
I/O is paused on all of them while the snapshots are made. If the controller
fails during that pause, a timer on each node resumes I/O after one minute.
Each replica's snapshot also holds that replica's DRBD metadata, so a rollback
restores all replicas together and DRBD has nothing to resync. If you lose a
node, you lose only that node's copy of the snapshot; the other replicas keep
theirs. A snapshot taken by an earlier version of the plugin exists on one
replica only and is rolled back the old way: that replica is restored and the
others resync the whole disk from it.

A snapshot can be opened read-only, but only on a node that holds a replica.
`vzdump` needs this to back up a container in snapshot mode. On a node without
a replica that backup fails, and the error names the replica nodes to run it
on.

Converting a VM to a template renames its disks to `base-<vmid>-disk-<n>`; the
data does not move. A linked clone of a template is a full copy, because a
DRBD device has no copy-on-write at the image level. The copy goes on the
template's replica nodes with the template's exact size, and does not depend
on the template afterwards. Reassigning a disk to another VM also renames it.
Both kinds of rename need the disk stopped and without snapshots. If something
is in the way, the error says what.

## Live migration and the dual-primary window

During a live migration the source and the target both have the disk open for
a moment. DRBD allows only one Primary unless `allow-two-primaries` is set, so
the plugin opens a short dual-primary window around the hand-off:

- `activate_volume` opens the window only when another node is Primary and PVE
  is live-migrating the VM from that node. It knows a migration is under way
  because the VM's config is still on the source node and has `lock: migrate`.
  Any other Primary is a leftover, usually from an earlier `deactivate_volume`
  that could not reach the controller. Then activation fails and names the
  node, so the guest never runs with two writers allowed. Demote the leftover
  with `haify resource secondary <resource> <node>` and start the guest again.
  Containers never get a window, because they migrate by restarting.
- The window is opened only on the source and the target, the two nodes that
  are Primary at the same time. A host that is down elsewhere in the resource
  does not block the migration.
- The window is closed again whatever happens: after a failed promote, when
  the device does not appear within 20 seconds, and in `deactivate_volume`,
  which always closes it, because the source deactivates after the hand-off.

The window is set at runtime only (`drbdadm net-options`) and never written to
the `.res` file. So even if the controller crashes and leaves it open, a reboot
or `drbdadm adjust` brings back single-primary. To check or repair by hand:

```bash
haify resource dual-primary <resource> off
```

**This is not a way to use one volume from two machines.** An ordinary
filesystem mounted on two machines at once gets corrupted, whatever DRBD
allows.

## Diskless clients come and go

A host with no replica of a disk attaches to it as a diskless client when the
guest starts there. It detaches when the guest stops or migrates away. (Earlier
versions left a host attached to every disk of every guest it had ever run, so
one such host being down blocked operations on all of those disks.) If a detach
fails, it is logged and the host stays attached. If the client is the only
quorum vote a two-replica resource has besides its replicas, it is not removed:
it becomes the resource's tiebreaker.

## When the controller is unreachable

Starting and stopping a guest normally go through the controller. (In earlier
versions, with the controller unreachable, no guest could start and HA could
restart none.) Now, when no controller answers and the disk is already up on
this node:

- `activate_volume` promotes it with plain `drbdadm primary`, never with
  `--force`. DRBD still refuses unless this node has quorum and can reach an
  UpToDate copy. The plugin also refuses while another node holds the disk
  Primary, because only the controller can tell a live migration from a
  leftover.
- the plugin finds the disk's device locally (`/dev/drbd/by-res/<resource>/0`),
  so qemu can start.
- `deactivate_volume` demotes with `drbdadm secondary` and clears this node's
  side of any dual-primary window. If a migration was under way, run
  `haify resource dual-primary <resource> off` once the controller is back.

The controller is still needed for a disk that is not up on the node, for a
host's first attach to a disk, and for a live migration. If a controller
answers with an error, the plugin does not work around it.

## HA

When `ha-manager` restarts a guest on another node, it calls `activate_volume`
there. That promotion is quorum-guarded: Haify force-promotes only if the node
holds DRBD quorum, and refuses otherwise. A node cut off from the others
therefore cannot take over, so HA failover cannot cause split brain. The cost
is that a resource which has lost quorum does not fail over automatically.
A node also does not take over with an Outdated copy (one a peer has marked as
missing writes) while the node that has those writes is away. The guest waits
for that node to come back instead of starting on old data.

The plugin makes it safe to reboot a node with guests running. Both the package
and `install.sh` order drbd-utils' `drbd-graceful-disconnect.service` after `pve-guests` and
`pve-ha-lrm`, so at shutdown the guests stop and their disks are demoted
before DRBD disconnects. Without that order, DRBD disconnected first. A guest
kept writing to its copy, which its peers had just been told was the only
current one, and HA then started the guest on another node with the older
data.

PVE decides where a guest runs, and so where its disks are Primary.
`haify ha create` and the Haify gateways put a drbd-reactor promoter on a
resource, and that promoter would fight PVE for the Primary role. So both
refuse a disk the plugin created (the plugin labels each one
`haify.pve/managed-by=pve`), and any resource with diskless clients. Use PVE
HA for guests, not drbd-reactor.

### Where HA should restart a guest

A guest can run on any PVE node. A node without a replica attaches as a
diskless client and reads and writes over the network. So every node can be an
HA target, but the replica nodes are faster. Tell `ha-manager` to prefer them
without forbidding the others. A strict rule would leave the guest down when
both replica nodes are down:

```bash
haify resource list | grep pve-100-        # the guest's disks and their nodes

# PVE 9: node affinity rules
ha-manager rules add node-affinity haify-vm-100 --resources vm:100 \
    --nodes pve1:2,pve2:2,pve3:1 --strict 0

# PVE 8: HA groups
ha-manager groupadd haify-pve1-pve2 --nodes pve1:2,pve2:2,pve3:1
ha-manager set vm:100 --group haify-pve1-pve2
```

In the web interface, **Datacenter → HA** shows the replica nodes of each HA
guest, with a warning sign when the guest runs on a node without a replica. On
PVE 9, the **Prefer nodes with Haify replicas** button creates or updates such
a non-strict rule, named `haify-vm-<id>`, for every HA guest with disks on
Haify. PVE 8 shows only the column. A rule takes effect at once: a guest
running on another node is migrated to a replica node right away.

Guests whose disks are on the same replica nodes can share one rule or group.
For a guest whose disks are on different replica pairs, the button prefers the
nodes all of its disks have in common. In a "two storage nodes plus one
tiebreaker" cluster, the third machine is both a DRBD tiebreaker for Haify and
a QDevice for corosync. It needs both roles because a QDevice alone gives DRBD
no quorum vote. It is usually not a PVE node at all; if it is one, give it the
lowest priority.

## Thin pools and Discard

On a thin pool, turn on **Discard** for each disk (`discard=on`; the checkbox
under the disk's Advanced options). Without it QEMU drops the guest's TRIM
requests, so space the guest frees in its filesystem stays allocated in the
pool on every replica. With it, an `fstrim` in the guest reaches the thin pool
of every replica through DRBD, from diskless clients too:

```bash
qm set 103 --scsi0 haify0:vm-103-disk-0,discard=on
```

The controller's daily trim (`[storage.thin] trim_schedule`) covers only
filesystems that Haify mounts itself. Space deleted inside a guest goes back to
the replicas' thin pools only through an `fstrim` in the guest, with Discard
on.

## I/O limits

PVE limits a VM disk's I/O itself: QEMU enforces the disk options `mbps_rd`,
`mbps_wr`, `iops_rd`, `iops_wr` (and their `_max` bursts) on the guest's
requests, whatever the storage. They work on Haify disks as on any other; set
them in the VM's hardware tab or with
`qm set <vmid> --scsi0 haify0:vm-<vmid>-disk-0,iops_wr=2000`. Haify adds no
limits of its own.

## Limitations

- The only format is raw. DRBD exports a raw block device. qcow2 is not
  supported and not needed, because snapshots come from Haify, not from the
  image format.
- Linked clones are full copies. They take the template's full size and the
  time to copy it. A clone cannot be made from a snapshot.
- Volume names are `vm-<vmid>-<name>`, with `<name>` starting with a letter
  and made of letters, digits, `_` and `-` (at most 64), and
  `base-<vmid>-disk-<n>`. PVE's own names all fit.
- A snapshot can be opened only on a node that holds a replica, and only on
  LVM.
- With `exactsize 0`, disks are allocated in whole gigabytes, which vzdump
  restore and Move Disk cannot use.
- Dual-primary is refused on WAN resources, so a guest whose disk is
  WAN-replicated (asynchronous) cannot live-migrate.

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
