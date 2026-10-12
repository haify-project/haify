# OpenStack

This directory is one Python package with two parts:

- `haify_cinder`, a Cinder volume driver;
- `haify_horizon`, a Horizon plugin that shows where each volume's replicas
  are ([below](#horizon)).

With `haify_cinder`, each Cinder volume is a Haify resource, replicated by
DRBD across the storage nodes. A compute host uses it as the block device
`/dev/drbd/by-res/<resource>/0`. A host that holds a replica reads and writes
its own disk. A host that holds none joins the resource as a DRBD diskless
client and reaches the replicas over the network. There is no iSCSI target in
the data path, so there is no gateway node that can fail under a running
instance.

## What it does

| Cinder operation | Haify |
| --- | --- |
| create, delete, extend | a resource with one volume, exactly the size Cinder asked for, labelled `haify.openstack/managed-by=cinder` |
| snapshot, delete snapshot | a resource snapshot. It is taken on every replica at the same moment, with I/O paused on all of them while it is taken |
| revert to snapshot | rolls every replica back together. A volume that was extended after the snapshot keeps its new size |
| volume from snapshot, clone | a new resource on the same nodes as the source's replicas, filled from the snapshot on one of them. A clone first takes a snapshot of the source to copy from, and deletes it afterwards |
| attach | if the host holds no replica, it joins as a diskless client. The volume is then made Primary there. The promotion is never forced on a host that has lost quorum |
| detach | the volume is made Secondary on the host, and a diskless client leaves the resource |
| capacity | the free and total space of the pool, added up over its nodes and divided by the number of copies each volume keeps |

The label tells the controller that OpenStack decides where the volume is
Primary. The controller therefore refuses `haify ha create`, gateways and
database apps on it, so drbd-reactor never fights Nova for the role. It also
raises no alert when a detached volume is Secondary on every node.

### One writer, and live migration

DRBD lets only one host write at a time, and Haify resources never promote
themselves. When a volume is attached to a host while another host holds it
Primary, there are two cases:

- The other host also has an attachment of the volume. This is a live
  migration. Nova attaches the volume to the destination before the move and
  detaches it from the source afterwards. In between, both hosts hold it
  Primary. This window closes when Nova detaches the source.
- The other host has no attachment. Something else left the volume Primary
  there, such as an image copy or a failed detach, and the driver makes it
  Secondary. If something on that host still has the device open, DRBD
  refuses, and the attach fails with the reason. A second writer is never
  started.

An evacuation deletes the old host's attachment before attaching the new
host, so it never opens the window. If the old host is down, its role does
not matter. If it is up and its guest is still running, the guest holds the
device open, and the attach on the new host is refused.

Detaching from a host that cannot be reached still succeeds. Its guest is
gone, and DRBD comes back up as Secondary there.

## Requirements

- haify-controller, with a pool on the storage nodes.
- DRBD 9 and drbd-utils on every compute host and on the cinder-volume host.
  The controller must reach each of them the same way it reaches a storage
  node: registered with `haify node register`, and reachable with the
  controller's SSH key. A compute host needs no pool and no replica. The
  cinder-volume host attaches volumes itself, to copy images into and out of
  them.
- The libvirt hook (`deploy/libvirt`, `install.sh`) on every compute host.
  Nova does not ask Cinder when it starts an instance, for example after the
  host rebooted. The hook makes the instance's disks Primary again at that
  point. It also does the libvirt side of live migration.

Live migration needs SSH from root on each compute host to the user in
`live_migration_uri` on the other compute hosts. Nova migrates peer to peer,
so the libvirt daemon makes that connection, and it runs as root.

## Install

On the cinder-volume host, into the Python environment Cinder runs from:

```bash
pip install ./deploy/openstack
```

`cinder.conf`:

```ini
[DEFAULT]
enabled_backends = haify

[haify]
volume_driver = haify_cinder.driver.HaifyDriver
volume_backend_name = haify
haify_controller = 192.168.1.10,192.168.1.11
haify_pool = pool0
```

Then restart cinder-volume and create a volume type for the backend:

```bash
openstack volume type create haify --property volume_backend_name=haify
```

| Option | Default | |
| --- | --- | --- |
| `haify_controller` | | REST addresses: `host`, `host:port` (3375) or `http(s)://…`. List every node that can run the controller under Self-HA. The driver tries them in turn until one answers |
| `haify_token_file` | | file holding the bearer token, needed when the controller has `[auth]` or `[rbac]` on |
| `haify_pool` | | pool for new volumes. Empty lets the controller choose |
| `haify_storage_type` | | `lvm`, `lvm-thin` or `zfs`. Empty uses the controller's default |
| `haify_replicas` | 2 | replicas per volume. The controller picks the nodes |
| `haify_nodes` | | put every volume on exactly these nodes instead. The replica count is then ignored |
| `haify_resource_prefix` | `cinder-` | the resource for volume `<id>` is named `<prefix><id>` |
| `haify_node_map` | | `compute1:node-a,…`, for when Nova's host names differ from the Haify node names. Hosts not listed use their short host name |
| `haify_on_no_quorum` | `suspend-io` | while a host has lost quorum, its guests' I/O waits instead of failing. With `io-error` it fails, and the guest usually needs a reboot afterwards |
| `haify_copy_timeout` | 3600 | seconds to wait for calls that copy or resync data (at least 60) |

Creating a volume from a snapshot and cloning a volume work on LVM and ZFS
pools. On ZFS, the controller reads the snapshot through a temporary read-only
clone, `<pool>/haify-populate-<new resource>-0`, and destroys it when the copy
is done.

cinder-volume starts even when the controller does not answer, for example
while Self-HA is moving it to another node. Until the controller answers, the
backend reports itself down, and every operation fails with the reason.

The driver and the plugin connect to the controller directly and ignore
`http_proxy`. The controller is on the storage network, and urllib does not
understand CIDR ranges in `no_proxy`, so a proxy set for internet access would
otherwise catch these calls.

## Horizon

The plugin adds two views, both for admins only. Project users see neither.

- A **Haify** panel under Admin › Volume, with two tables:
  - one row per Cinder volume on Haify: the instance it is attached to, the
    node where it is Primary, every replica with its state, and whether it is
    healthy, syncing or degraded. A Haify resource whose Cinder volume no
    longer exists, for example after a failed delete, is listed too;
  - one row per node: its pool's free and total space, and how many replicas,
    tiebreakers, diskless clients and Primaries of Cinder volumes it holds.
- A **Replication** tab on the details page of a Haify volume. It shows the
  volume's members with their roles, disk states and sync progress, the
  snapshots on the replicas under their Cinder names, and the resource's
  labels. When the Haify UI address is set, it also links to the resource in
  Haify's web UI.

On each Horizon host, into Horizon's Python environment:

```bash
pip install ./deploy/openstack
cp "$(python -c 'import haify_horizon, os; print(os.path.dirname(haify_horizon.__file__))')/enabled/_2225_admin_haify_panel.py" \
   /path/to/openstack_dashboard/local/enabled/
cat > /path/to/openstack_dashboard/local/local_settings.d/_90_haify.py <<'EOF'
HAIFY_CONTROLLER = "192.168.1.10,192.168.1.11"
HAIFY_POOL = "pool0"                          # as haify_pool
HAIFY_UI_URL = "http://192.168.1.10:3376"     # optional: links into Haify
# HAIFY_TOKEN_FILE, HAIFY_RESOURCE_PREFIX ("cinder-"), HAIFY_BACKENDS (["haify"])
EOF
systemctl reload apache2
```

The panel fetches volume statuses from the controller eight at a time. When
the controller cannot be reached, the panel says so and lists nothing.

## Tests

```bash
python3 -m unittest discover -s deploy/openstack/tests -p 'test_*.py'
```

The tests replace the controller with a fake and stub out the few Cinder and
oslo modules the driver imports, so they run without OpenStack installed. With
Django installed, they also compile the plugin's templates. `make ci` runs them
with Django when uv is available.
