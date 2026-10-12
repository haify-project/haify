"""What the Cinder driver does, in Haify's terms.

One Cinder volume is one Haify resource with one DRBD volume. A compute host
reads and writes it through /dev/drbd/by-res/<resource>/0, locally when it
holds a replica and over the network as a diskless client when it does not.

DRBD lets one node write at a time and Haify resources are created with
`auto-promote no`, so attaching a volume to a host makes it Primary there and
detaching it makes it Secondary. A live migration needs it Primary on both
hosts for a moment: Nova attaches the volume to the destination while the
source still has it, and detaches it from the source once the guest has
moved. The dual-primary window is opened for exactly that case, a Primary on
a host that holds an attachment of the volume; Nova evacuates a guest by
deleting the old host's attachment first, so an evacuation never opens it.

Nothing here imports Cinder, so the logic is tested without an install.
"""

import re

from haify_cinder.client import HaifyError, Unreachable

GiB = 1024 ** 3

# Marks a resource whose Primary OpenStack decides. haify-controller then
# refuses a drbd-reactor promoter on it (ha create, a gateway), which would
# fight Nova for the role, and does not alarm on a detached volume being
# Secondary everywhere.
MANAGED_BY_LABEL = "haify.openstack/managed-by"
MANAGED_BY = "cinder"

# Snapshot names: what the controller accepts for a resource snapshot.
SNAPSHOT_NAME_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_-]{0,39}$")


class Settings:
    """The driver's configuration, as plain values."""

    def __init__(self, controller, token=None, pool="", storage_type="", replicas=2, nodes=None,
                 prefix="cinder-", node_map=None, on_no_quorum="suspend-io", copy_timeout=3600):
        self.controller = controller
        self.token = token
        self.pool = pool or ""
        self.storage_type = storage_type or ""
        self.replicas = int(replicas or 0)
        self.nodes = [n for n in (nodes or []) if n]
        self.prefix = prefix or ""
        self.node_map = dict(node_map or {})
        self.on_no_quorum = on_no_quorum or "suspend-io"
        self.copy_timeout = int(copy_timeout or 3600)


class Backend:
    def __init__(self, client, settings, log=None):
        self.client = client
        self.s = settings
        self.log = log or (lambda level, msg: None)

    # Names

    def resource_name(self, volume_id):
        return self.s.prefix + volume_id

    def snapshot_name(self, snapshot_id):
        if not SNAPSHOT_NAME_RE.match(snapshot_id):
            raise HaifyError(f"snapshot id {snapshot_id!r} cannot name a Haify snapshot")
        return snapshot_id

    def node_for(self, host):
        """The Haify node name of a host Cinder or Nova names: through
        haify_node_map when listed there, else its short host name."""
        if not host:
            return None
        if host in self.s.node_map:
            return self.s.node_map[host]
        short = host.split(".")[0]
        return self.s.node_map.get(short, short)

    # Volumes

    def create(self, volume_id, size_gb, nodes=None, pool=None):
        spec = {
            "name": self.resource_name(volume_id),
            "sizeGb": int(size_gb),
            # Exactly the size Cinder asked for: an image is copied in only
            # when it fits, and the copy checks the device's size.
            "sizeBytes": int(size_gb) * GiB,
            "protocol": "C",
            # A guest whose disk errors out on lost quorum remounts its
            # filesystems read-only and needs a reboot; one whose I/O is
            # suspended waits, and carries on when quorum returns.
            "drbdOptions": {
                "on-no-quorum": self.s.on_no_quorum,
                "on-no-data-accessible": self.s.on_no_quorum,
            },
            "labels": {MANAGED_BY_LABEL: MANAGED_BY},
        }
        pool = pool or self.s.pool
        if pool:
            spec["pool"] = pool
        if self.s.storage_type:
            spec["storageType"] = self.s.storage_type
        if nodes:
            spec["nodes"] = list(nodes)
        elif self.s.nodes:
            spec["nodes"] = list(self.s.nodes)
        elif self.s.replicas:
            spec["replicas"] = self.s.replicas
        self.client.create_resource(spec, timeout=self.s.copy_timeout)
        self.log("info", f"created {spec['name']} ({size_gb} GiB)")
        return spec["name"]

    def delete(self, volume_id):
        name = self.resource_name(volume_id)
        if self.client.find_resource(name) is None:
            self.log("info", f"{name} does not exist; nothing to delete")
            return
        self.client.delete_resource(name, timeout=self.s.copy_timeout)
        self.log("info", f"deleted {name}")

    def extend(self, volume_id, new_size_gb):
        self.client.resize(self.resource_name(volume_id), int(new_size_gb) * GiB, timeout=self.s.copy_timeout)

    # Snapshots

    def create_snapshot(self, volume_id, snapshot_id):
        # Taken on every replica at once, with I/O suspended across them.
        self.client.create_snapshot(self.resource_name(volume_id), self.snapshot_name(snapshot_id),
                                    timeout=self.s.copy_timeout)

    def delete_snapshot(self, volume_id, snapshot_id):
        name = self.resource_name(volume_id)
        snap = self.snapshot_name(snapshot_id)
        if self.client.find_resource(name) is None or snap not in self.client.snapshots(name):
            self.log("info", f"snapshot {snap} of {name} does not exist; nothing to delete")
            return
        self.client.delete_snapshot(name, snap)

    def revert(self, volume_id, snapshot_id):
        self.client.rollback(self.resource_name(volume_id), self.snapshot_name(snapshot_id),
                             timeout=self.s.copy_timeout)

    def create_from_snapshot(self, volume_id, size_gb, src_volume_id, snapshot_id):
        """A new volume holding what the snapshot holds. The copy runs on one
        node that has both, so the new volume goes on the source's replica
        nodes; DRBD replicates every copied block to the others."""
        src = self.resource_name(src_volume_id)
        snap = self.snapshot_name(snapshot_id)
        info = self.client.resource(src)
        vols = info.get("volumes") or []
        nodes = [n for n in (info.get("nodes") or []) if n != info.get("drNode")]
        if not vols or not nodes:
            raise HaifyError(f"{src} has no volume or no replica to copy from")
        name = self.create(volume_id, size_gb, nodes=nodes, pool=self.s.pool or None)
        try:
            self.client.populate(name, src, snap, nodes[0], timeout=self.s.copy_timeout)
        except HaifyError:
            # A half-filled volume must not stay behind looking like a copy.
            try:
                self.client.delete_resource(name, timeout=self.s.copy_timeout)
            except HaifyError as e:
                self.log("warning", f"could not delete the half-filled {name}: {e}")
            raise
        self.log("info", f"filled {name} from snapshot {snap} of {src} on {nodes[0]}")
        return name

    def clone(self, volume_id, size_gb, src_volume_id):
        """A copy of a volume, attached or not: taken from a snapshot made for
        it, so the copy is of one instant, then the snapshot goes."""
        src = self.resource_name(src_volume_id)
        snap = "clone-" + volume_id[:8]
        self.client.create_snapshot(src, snap, timeout=self.s.copy_timeout)
        try:
            return self.create_from_snapshot(volume_id, size_gb, src_volume_id, snap)
        finally:
            try:
                self.client.delete_snapshot(src, snap)
            except HaifyError as e:
                self.log("warning", f"could not delete the temporary snapshot {snap} of {src}: {e}")

    # Attach and detach

    def connect(self, volume_id, host, attached_hosts=()):
        """Makes the volume writable on host and returns Cinder's connection
        info. attached_hosts are the hosts the volume's other attachments are
        on."""
        name = self.resource_name(volume_id)
        node = self.node_for(host)
        if not node:
            raise HaifyError("the connector names no host")
        info = self.client.resource(name)
        if node not in participants(info):
            self.client.attach_client(name, node)
            self.log("info", f"attached {node} to {name} as a diskless client")

        opened = False
        peer = other_primary(self.client.status(name), node)
        if peer:
            if peer in {self.node_for(h) for h in attached_hosts if h}:
                # The volume is attached on the peer and now here: a live
                # migration, until Nova detaches it from the source.
                self.client.dual_primary(name, True, [peer, node])
                opened = True
                self.log("info", f"dual-primary window open on {name} for a migration from {peer} to {node}")
            else:
                # Primary with no attachment there: left over from an image
                # copy or a failed detach. DRBD refuses to demote a device
                # something holds open, so a real user keeps it.
                try:
                    self.client.secondary(name, peer)
                except HaifyError as e:
                    raise HaifyError(
                        f"{name} is Primary on {peer}, which holds no attachment of it, and could not be made "
                        f"Secondary there: {e}. Refusing a second writer; stop whatever uses it on {peer}")
                self.log("info", f"demoted a leftover Primary of {name} on {peer}")
        try:
            self.client.primary(name, node)
        except HaifyError:
            if opened:
                self.close_window(name)
            raise
        self.log("info", f"{name} Primary on {node}")
        return {
            "driver_volume_type": "local",
            "data": {"device_path": f"/dev/drbd/by-res/{name}/0"},
        }

    def disconnect(self, volume_id, host, attached_hosts=()):
        """Makes the volume Secondary on host, closes any dual-primary window
        and detaches a diskless client. A host that cannot be reached is
        skipped: its guest is gone (an evacuation), and DRBD comes back up
        Secondary there."""
        name = self.resource_name(volume_id)
        node = self.node_for(host)
        info = self.client.find_resource(name)
        if info is None or not node:
            return
        if node in participants(info):
            try:
                self.client.secondary(name, node)
            except Unreachable:
                raise
            except HaifyError as e:
                if "held open" in str(e):
                    raise HaifyError(f"{name} is still in use on {node}: {e}")
                self.log("warning", f"could not make {name} Secondary on {node}: {e}")
        self.close_window(name)
        others = {self.node_for(h) for h in attached_hosts if h}
        if node in (info.get("disklessClients") or []) and node not in others:
            try:
                self.client.detach_client(name, node)
                self.log("info", f"detached {node} from {name}")
            except HaifyError as e:
                self.log("warning", f"could not detach {node} from {name} (it stays attached): {e}")

    def close_window(self, name):
        try:
            self.client.dual_primary(name, False)
        except HaifyError as e:
            self.log("warning", f"could not close the dual-primary window of {name}: {e}")

    # Capacity

    def stats(self):
        """Capacity across the pool's nodes, divided by the copies each volume
        keeps. thin is true when any of them is thin."""
        pools = [p for p in self.client.pools() if self.in_pool(p)]
        copies = len(self.s.nodes) or self.s.replicas or 2
        spaces = [pool_space(p) for p in pools]
        total = sum(t for _, _, t in spaces) / GiB / copies
        free = sum(f for _, f, _ in spaces) / GiB / copies
        thin = any(th for th, _, _ in spaces)
        return {"total_capacity_gb": round(total, 2), "free_capacity_gb": round(free, 2), "thin": thin}

    def in_pool(self, p):
        # The API names a pool by its volume group, haify_<pool>.
        return not self.s.pool or p.get("name") in (self.s.pool, "haify_" + self.s.pool)


def participants(info):
    out = []
    for key in ("nodes", "disklessNodes", "disklessClients"):
        out += info.get(key) or []
    return out


def other_primary(status, node):
    """The Haify node other than node that holds the resource Primary, or None."""
    for host, st in (status.get("nodeStates") or {}).items():
        if st.get("role") == "Primary" and (st.get("node") or host) != node:
            return st.get("node") or host
    return None


def pool_space(p):
    """(thin, free bytes, total bytes) of one node's pool, as `haify pool list`
    counts them: a thin pool by its thin LV, not by the volume group around it."""
    capacity = num(p.get("capacityBytes"))
    if capacity:
        return bool(p.get("thin")), num(p.get("availableBytes")), capacity
    # A controller from before capacity_bytes: work it out the same way.
    thin_size = num(p.get("thinSizeBytes"))
    if p.get("thinPoolLv") and thin_size > 0:
        used = min(thin_size * num(p.get("thinDataPercent")) / 100, thin_size)
        return True, thin_size - used, thin_size
    total, free = num(p.get("totalBytes")), num(p.get("freeBytes"))
    if not total:
        total, free = num(p.get("totalGb")) * 1e9, num(p.get("freeGb")) * 1e9
    return bool(p.get("thin")), free, total


def num(v):
    # protojson renders 64-bit integers as strings.
    try:
        return float(v or 0)
    except (TypeError, ValueError):
        return 0.0
