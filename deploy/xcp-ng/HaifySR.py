#!/usr/bin/python3
#
# HaifySR: XCP-ng storage on Haify. Every virtual disk (VDI) is its own Haify
# resource, a DRBD device replicated across the Haify nodes, so a VM's disks
# survive losing a host, and every host in the pool can run the VM: a host
# without a copy of a disk reads and writes it over the network.
#
# XAPI decides where a disk is in use and this driver follows it:
#   vdi_activate (before tapdisk opens the disk)  joins the disk from this host
#                                                 if needed, then makes this
#                                                 host its DRBD Primary;
#   vdi_deactivate (after tapdisk closed it)      gives the role up again.
# With ATOMIC_PAUSE, attach and detach do nothing on their own: a live
# migration deactivates on the source before it activates on the destination,
# so two hosts never hold a disk at once.

from sm_typing import Dict, List, Optional, Tuple, override

import time
import xmlrpc.client

import SR
import SRCommand
import VDI
import util
import xs_errors

from haifyclient import HaifyClient, HaifyError, node_name, read_token

CAPABILITIES = ["ATOMIC_PAUSE", "SR_PROBE", "SR_UPDATE",
                "VDI_CREATE", "VDI_DELETE", "VDI_ATTACH", "VDI_DETACH",
                "VDI_ACTIVATE", "VDI_DEACTIVATE", "VDI_RESIZE",
                "VDI_SNAPSHOT", "VDI_CLONE", "VDI_UPDATE"]

CONFIGURATION: List[Tuple[str, str]] = [
    ("controller", "The Haify controller's REST address, host[:port] (port 3375); with Self-HA its floating IP"),
    ("pool", "The Haify pool the disks are made in"),
    ("replicas", "Copies of each disk (default 2)"),
    ("token-file", "File holding the API token on every host (default /etc/haify/token)"),
]

DRIVER_INFO = {
    "name": "Haify",
    "description": "Replicated block storage: every disk is a Haify DRBD resource",
    "vendor": "Haify",
    "copyright": "Haify",
    "driver_version": "1.0",
    "required_api_version": "1.0",
    "capabilities": CAPABILITIES,
    "configuration": CONFIGURATION,
}

TYPE = "haify"

# Labels on each disk's resource. managed-by tells the controller that XAPI
# decides where the disk is Primary, so nothing else may put a promoter on it.
LABEL_MANAGED = "haify.xcp/managed-by"
LABEL_SR = "haify.xcp/sr"
LABEL_VDI = "haify.xcp/vdi"

MIB = 1 << 20
DEVICE_WAIT = 60


def resource_name(vdi_uuid):
    return "xcp-" + vdi_uuid


def device_path(resource):
    return "/dev/drbd/by-res/%s/0" % resource


def fail(code, msg):
    util.SMlog("HaifySR: %s" % msg)
    return xs_errors.XenError(code, opterr=msg)


class HaifySR(SR.SR):
    """Haify storage repository"""

    @override
    @staticmethod
    def handles(type) -> bool:
        return type == TYPE

    @override
    def load(self, sr_uuid) -> None:
        self.sr_vditype = "aio"
        for key in ("controller", "pool"):
            if not self.dconf.get(key):
                raise fail("ConfigParamsMissing", "device-config:%s is required" % key)
        self.pool = self.dconf["pool"]
        try:
            self.replicas = int(self.dconf.get("replicas", "2"))
        except ValueError:
            raise fail("ConfigParamsMissing", "device-config:replicas must be a number")
        self.client = HaifyClient(self.dconf["controller"], read_token(self.dconf.get("token-file")))
        self.node = node_name()

    def call(self, what, fn, *args):
        """Runs a controller call, turning a refusal into a XAPI error."""
        try:
            return fn(*args)
        except HaifyError as e:
            raise fail("SRUnavailable", "%s: %s" % (what, e))

    def owned(self):
        """This SR's disks as the controller lists them. A failure raises:
        an empty answer would make the scan forget every disk."""
        return [r for r in self.call("list disks", self.client.resources)
                if (r.get("labels") or {}).get(LABEL_SR) == self.uuid]

    def pool_room(self):
        """Usable and used bytes of the pool, counted once per copy."""
        total = used = 0
        for p in self.call("list pools", self.client.pools):
            if p.get("name") not in (self.pool, "haify_" + self.pool):
                continue
            cap = int(p.get("capacityBytes") or 0)
            total += cap
            used += cap - int(p.get("availableBytes") or 0)
        n = max(self.replicas, 1)
        return total // n, max(used, 0) // n

    @override
    def create(self, sr_uuid, size) -> None:
        nodes = [p.get("node") for p in self.call("list pools", self.client.pools)
                 if p.get("name") in (self.pool, "haify_" + self.pool)]
        if len(nodes) < self.replicas:
            raise fail("SRUnavailable", "pool %s is on %d node(s); %d replicas need as many"
                       % (self.pool, len(nodes), self.replicas))

    @override
    def delete(self, sr_uuid) -> None:
        if self.owned():
            raise xs_errors.XenError("SRNotEmpty")

    @override
    def attach(self, sr_uuid) -> None:
        names = [n.get("name") for n in self.call("list nodes", self.client.nodes)]
        if self.node not in names:
            raise fail("SRUnavailable", "this host (%s) is not a Haify node; register it with "
                       "haify node register --name %s --address <its IP>" % (self.node, self.node))
        util.pread2(["modprobe", "drbd"])

    @override
    def detach(self, sr_uuid) -> None:
        pass

    @override
    def probe(self) -> str:
        return util.SRtoXML({})

    @override
    def scan(self, sr_uuid) -> None:
        for r in self.owned():
            uuid = r["labels"].get(LABEL_VDI)
            if uuid:
                self.vdis[uuid] = HaifyVDI(self, uuid, resource=r)
        self.stats()
        super(HaifySR, self).scan(sr_uuid)

    @override
    def update(self, sr_uuid) -> None:
        self.stats()
        self._db_update()

    def stats(self):
        self.physical_size, self.physical_utilisation = self.pool_room()
        self.virtual_allocation = sum(v.size for v in self.vdis.values())

    @override
    def vdi(self, uuid) -> VDI.VDI:
        return HaifyVDI(self, uuid)


class HaifyVDI(VDI.VDI):
    def __init__(self, sr, uuid, resource=None):
        self._resource = resource
        super(HaifyVDI, self).__init__(sr, uuid)

    @override
    def load(self, vdi_uuid) -> None:
        self.vdi_type = "aio"
        self.location = resource_name(vdi_uuid)
        self.path = device_path(self.location)
        r = self._resource
        if r:
            vols = r.get("volumes") or [{}]
            self.size = int(vols[0].get("sizeBytes") or 0) or int(vols[0].get("sizeGb") or 0) << 30
            self.utilisation = self.size

    def payload(self, uuid, size, nodes=None):
        body = {
            "name": resource_name(uuid),
            "sizeBytes": size,
            "pool": self.sr.pool,
            "protocol": "C",
            # A guest whose disk loses quorum waits rather than seeing I/O
            # errors, and carries on when quorum returns.
            "drbdOptions": {"on-no-quorum": "suspend-io", "on-no-data-accessible": "suspend-io"},
            "labels": {LABEL_MANAGED: "xcp-ng", LABEL_SR: self.sr.uuid, LABEL_VDI: uuid},
        }
        if nodes:
            body["nodes"] = nodes
        else:
            body["replicas"] = self.sr.replicas
        return body

    @override
    def create(self, sr_uuid, vdi_uuid, size) -> str:
        size = (int(size) + MIB - 1) // MIB * MIB
        self.sr.call("create disk", self.sr.client.create, self.payload(vdi_uuid, size))
        self.size = self.utilisation = size
        self._db_introduce()
        return self.get_params()

    @override
    def delete(self, sr_uuid, vdi_uuid, data_only=False) -> None:
        if self.sr.call("read disk", self.sr.client.resource, self.location) is not None:
            self.sr.call("delete disk", self.sr.client.delete, self.location)
        self._db_forget()

    @override
    def attach(self, sr_uuid, vdi_uuid) -> str:
        r = self.sr.call("read disk", self.sr.client.resource, self.location)
        if r is None:
            raise fail("VDIUnavailable", "disk %s does not exist on the Haify controller" % self.location)
        node = self.sr.node
        members = (r.get("nodes") or []) + (r.get("disklessClients") or []) + (r.get("disklessNodes") or [])
        if node not in members:
            # No copy here: join the disk over the network. A quorum
            # tiebreaker is a member already and is used as it is.
            self.sr.call("join disk from %s" % node, self.sr.client.attach_diskless, self.location, node)
        if not util.wait_for_path(self.path, DEVICE_WAIT):
            raise fail("VDIUnavailable", "%s did not appear on %s" % (self.path, node))
        return super(HaifyVDI, self).attach(sr_uuid, vdi_uuid)

    @override
    def detach(self, sr_uuid, vdi_uuid) -> None:
        # A host that joined a disk stays joined: the next start or migration
        # back here is faster, and a diskless member costs nothing.
        pass

    @override
    def activate(self, sr_uuid, vdi_uuid) -> Optional[Dict[str, str]]:
        self.sr.call("make %s Primary on %s" % (self.location, self.sr.node),
                     self.sr.client.promote, self.location, self.sr.node)
        return None

    @override
    def deactivate(self, sr_uuid, vdi_uuid) -> None:
        self.sr.call("release %s on %s" % (self.location, self.sr.node),
                     self.sr.client.demote, self.location, self.sr.node)

    @override
    def resize(self, sr_uuid, vdi_uuid, size) -> str:
        size = (int(size) + MIB - 1) // MIB * MIB
        if size < self.current_size():
            raise xs_errors.XenError("VDISize", opterr="shrinking is not supported")
        self.sr.call("grow disk", self.sr.client.resize, self.location, size)
        self.size = self.utilisation = size
        self._db_update()
        return self.get_params()

    @override
    def update(self, sr_uuid, vdi_uuid) -> None:
        pass

    def current_size(self):
        r = self.sr.call("read disk", self.sr.client.resource, self.location) or {}
        vols = r.get("volumes") or [{}]
        return int(vols[0].get("sizeBytes") or 0) or int(vols[0].get("sizeGb") or 0) << 30

    @override
    def snapshot(self, sr_uuid, vdi_uuid) -> str:
        return self.copy(read_only=True)

    @override
    def clone(self, sr_uuid, vdi_uuid) -> str:
        return self.copy(read_only=False)

    def copy(self, read_only):
        """A snapshot or clone is a disk of its own, replicated like any
        other: an LVM snapshot of this disk, taken at one instant on every
        copy, filled into a new resource on the same nodes, then dropped."""
        src = self.sr.call("read disk", self.sr.client.resource, self.location)
        if src is None:
            raise fail("VDIUnavailable", "disk %s does not exist on the Haify controller" % self.location)
        vol = (src.get("volumes") or [{}])[0]
        nodes = src.get("nodes") or []
        if not nodes or not vol.get("pool") or not vol.get("backingVolume"):
            raise fail("VDIUnavailable", "disk %s has no copy to snapshot" % self.location)
        size = self.current_size()
        new_uuid = util.gen_uuid()
        snap = "xcp%d" % int(time.time())
        self.sr.call("snapshot disk", self.sr.client.snapshot, self.location, snap)
        try:
            new = HaifyVDI(self.sr, new_uuid)
            self.sr.call("create the copy", self.sr.client.create, new.payload(new_uuid, size, nodes))
            try:
                source = "/dev/%s/%s_snap_%s" % (vol["pool"], vol["backingVolume"], snap)
                self.sr.call("fill the copy", self.sr.client.populate, new.location, source, nodes[0])
            except Exception:
                self.sr.call("drop the partial copy", self.sr.client.delete, new.location)
                raise
        finally:
            try:
                self.sr.client.delete_snapshot(self.location, snap)
            except HaifyError as e:
                util.SMlog("HaifySR: could not drop snapshot %s of %s: %s" % (snap, self.location, e))

        new.size = new.utilisation = size
        new.read_only = read_only
        new.label = self.label if hasattr(self, "label") else ""
        if read_only:
            new.is_a_snapshot = True
            new.snapshot_of = self.sr.session.xenapi.VDI.get_by_uuid(self.uuid)
            new.snapshot_time = time.strftime("%Y%m%dT%H:%M:%SZ", time.gmtime())
        new.sm_config = {}
        new._db_introduce()
        return new.get_params()


if __name__ == "__main__":
    SRCommand.run(HaifySR, DRIVER_INFO)
else:
    SR.registerSR(HaifySR)
