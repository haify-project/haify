"""haify-controller's REST gateway, as the Cinder driver uses it.

Only the standard library, so the driver adds no package to a Cinder install
and the tests run without one.
"""

import json
import re
import urllib.error
import urllib.parse
import urllib.request

DEFAULT_REST_PORT = 3375


class HaifyError(Exception):
    """The controller refused or failed an operation."""


class Unreachable(HaifyError):
    """No controller address answered at all."""


def endpoints(addresses):
    """Base URLs from controller addresses: host, host:port, [v6]:port, or a
    full http(s):// URL; the port defaults to 3375."""
    if isinstance(addresses, str):
        addresses = addresses.split(",")
    urls = []
    for entry in addresses:
        entry = (entry or "").strip().rstrip("/")
        if not entry:
            continue
        if "://" not in entry:
            entry = "http://" + entry
        if not re.search(r":\d+$", entry.split("://", 1)[1]):
            entry += f":{DEFAULT_REST_PORT}"
        urls.append(entry)
    return urls


# The controller is on the storage network, so it is reached directly. A host
# with http_proxy set for the internet would otherwise send every call to the
# proxy: urllib honours no_proxy host names but not the CIDR ranges
# (192.168.0.0/16) such hosts usually list.
direct_open = urllib.request.build_opener(urllib.request.ProxyHandler({})).open


class Client:
    """Calls the controller, trying each address until one answers. Under
    Self-HA only the node running the controller does; the others refuse."""

    def __init__(self, addresses, token=None, timeout=60, opener=direct_open):
        self.urls = endpoints(addresses)
        self.token = token
        self.timeout = timeout
        self.opener = opener

    def request(self, method, path, payload=None, timeout=None):
        if not self.urls:
            raise Unreachable("no haify controller address is configured (haify_controller)")
        data = json.dumps(payload).encode() if payload is not None else None
        failed = []
        for base in self.urls:
            req = urllib.request.Request(base + path, data=data, method=method)
            req.add_header("Content-Type", "application/json")
            if self.token:
                req.add_header("Authorization", "Bearer " + self.token)
            try:
                with self.opener(req, timeout=timeout or self.timeout) as resp:
                    body = resp.read().decode() or "{}"
            except urllib.error.HTTPError as e:
                # grpc-gateway renders gRPC errors as {"code":..,"message":..}.
                try:
                    msg = json.loads(e.read().decode()).get("message") or e.reason
                except ValueError:
                    msg = e.reason
                raise HaifyError(f"{method} {path}: {msg} (HTTP {e.code})")
            except (urllib.error.URLError, OSError) as e:
                failed.append(f"{base}: {getattr(e, 'reason', e)}")
                continue
            out = json.loads(body)
            # Application failures come back as HTTP 200 with success=false,
            # and protojson omits a false field: a missing success is a failure.
            if isinstance(out, dict) and "message" in out and not out.get("success"):
                raise HaifyError(f"{method} {path}: {out['message']}")
            return out
        raise Unreachable("haify controller unreachable at " + "; ".join(failed))

    # Resources

    def resources(self):
        return self.request("GET", "/v1/resources").get("resources") or []

    def resource(self, name):
        out = self.request("GET", f"/v1/resources/{q(name)}")
        return out.get("resource", out)

    def find_resource(self, name):
        """The resource, or None when the controller has none of that name."""
        try:
            return self.resource(name)
        except Unreachable:
            raise
        except HaifyError as e:
            if f"resource not found: {name}" in str(e):
                return None
            raise

    def status(self, name):
        out = self.request("GET", f"/v1/resources/{q(name)}/status")
        return out.get("status", out)

    def create_resource(self, spec, timeout=None):
        return self.request("POST", "/v1/resources", spec, timeout=timeout)

    def delete_resource(self, name, timeout=None):
        return self.request("DELETE", f"/v1/resources/{q(name)}", timeout=timeout)

    def resize(self, name, size_bytes, timeout=None):
        return self.request("PATCH", f"/v1/resources/{q(name)}/volumes/0",
                            {"resource": name, "volumeId": 0, "sizeBytes": size_bytes}, timeout=timeout)

    def populate(self, name, src, snap, node, timeout=None):
        """Fills the new resource name from snapshot snap of src, taken on
        every replica. The controller finds the snapshot's device on node,
        whatever the pool type."""
        return self.request("POST", f"/v1/resources/{q(name)}/populate",
                            {"resource": name, "volumeId": 0, "node": node,
                             "sourceSnapshot": {"resource": src, "volumeId": 0, "name": snap, "replicated": True}},
                            timeout=timeout)

    def set_labels(self, name, labels):
        return self.request("POST", f"/v1/resources/{q(name)}/labels", {"resource": name, "labels": labels})

    # Roles

    def primary(self, name, node):
        # Quorum-guarded: a normal promote, forced only when the node holds
        # quorum, so a partitioned node never splits the brain.
        return self.request("POST", f"/v1/resources/{q(name)}/primary",
                            {"resource": name, "node": node, "quorumGuarded": True})

    def secondary(self, name, node):
        return self.request("POST", f"/v1/resources/{q(name)}/secondary", {"resource": name, "node": node})

    def dual_primary(self, name, enable, nodes=()):
        payload = {"resource": name, "enable": enable}
        if nodes:
            payload["nodes"] = list(nodes)
        return self.request("POST", f"/v1/resources/{q(name)}/dual-primary", payload)

    def attach_client(self, name, node):
        return self.request("POST", f"/v1/resources/{q(name)}/diskless-clients", {"resource": name, "node": node})

    def detach_client(self, name, node):
        return self.request("DELETE", f"/v1/resources/{q(name)}/diskless-clients/{q(node)}")

    # Snapshots

    def snapshots(self, name):
        return self.request("GET", f"/v1/resources/{q(name)}/snapshots").get("names") or []

    def create_snapshot(self, name, snap, timeout=None):
        return self.request("POST", f"/v1/resources/{q(name)}/snapshots", {"resource": name, "name": snap},
                            timeout=timeout)

    def delete_snapshot(self, name, snap):
        return self.request("DELETE", f"/v1/resources/{q(name)}/snapshots/{q(snap)}")

    def rollback(self, name, snap, timeout=None):
        return self.request("POST", f"/v1/resources/{q(name)}/snapshots/{q(snap)}/rollback",
                            {"resource": name, "name": snap}, timeout=timeout)

    # Pools

    def pools(self):
        return self.request("GET", "/v1/pools").get("pools") or []


def q(segment):
    return urllib.parse.quote(str(segment), safe="")
