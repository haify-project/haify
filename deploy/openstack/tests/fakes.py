"""A controller that records calls, for the backend tests."""

from haify_cinder.client import HaifyError, Unreachable


class FakeClient:
    def __init__(self):
        self.calls = []
        self.res = {}       # name -> resource info
        self.roles = {}     # name -> {node: role}
        self.snaps = {}     # name -> [snapshot names]
        self.pool_list = []
        self.fail = {}      # method name -> exception to raise

    def _call(self, name, *args):
        self.calls.append((name,) + args)
        if name in self.fail:
            raise self.fail[name]

    def names(self):
        return [c[0] for c in self.calls]

    def resources(self):
        self._call("resources")
        return [{"name": n} for n in self.res]

    def resource(self, name):
        self._call("resource", name)
        if name not in self.res:
            raise HaifyError(f"resource {name} not found")
        return self.res[name]

    def find_resource(self, name):
        self._call("find_resource", name)
        return self.res.get(name)

    def status(self, name):
        self._call("status", name)
        return {"nodeStates": {f"host-{n}": {"node": n, "role": r} for n, r in self.roles.get(name, {}).items()}}

    def create_resource(self, spec, timeout=None):
        self._call("create_resource", spec)
        self.res[spec["name"]] = {"name": spec["name"], "nodes": spec.get("nodes") or ["n1", "n2"]}

    def delete_resource(self, name, timeout=None):
        self._call("delete_resource", name)
        self.res.pop(name, None)

    def resize(self, name, size_bytes, timeout=None):
        self._call("resize", name, size_bytes)

    def populate(self, name, src, snap, node, timeout=None):
        self._call("populate", name, src, snap, node)

    def primary(self, name, node):
        self._call("primary", name, node)
        self.roles.setdefault(name, {})[node] = "Primary"

    def secondary(self, name, node):
        self._call("secondary", name, node)
        self.roles.setdefault(name, {})[node] = "Secondary"

    def dual_primary(self, name, enable, nodes=()):
        self._call("dual_primary", name, enable, list(nodes))

    def attach_client(self, name, node):
        self._call("attach_client", name, node)
        self.res[name].setdefault("disklessClients", []).append(node)

    def detach_client(self, name, node):
        self._call("detach_client", name, node)
        self.res[name]["disklessClients"].remove(node)

    def snapshots(self, name):
        self._call("snapshots", name)
        return self.snaps.get(name, [])

    def create_snapshot(self, name, snap, timeout=None):
        self._call("create_snapshot", name, snap)

    def delete_snapshot(self, name, snap):
        self._call("delete_snapshot", name, snap)

    def rollback(self, name, snap, timeout=None):
        self._call("rollback", name, snap)

    def pools(self):
        self._call("pools")
        return self.pool_list


__all__ = ["FakeClient", "HaifyError", "Unreachable"]
