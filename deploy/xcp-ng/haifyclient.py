#!/usr/bin/python3
"""A small client for the Haify controller's REST API, for the XCP-ng SR
driver. Python 3.6 standard library only: it runs in dom0."""

import json
import os
import urllib.error
import urllib.parse
import urllib.request

DEFAULT_PORT = 3375
DEFAULT_TOKEN_FILE = "/etc/haify/token"


class HaifyError(Exception):
    """The controller refused a request, or could not be reached."""


def base_url(controller):
    """The REST base URL for a controller given as host, host:port or URL."""
    controller = controller.strip().rstrip("/")
    if "://" in controller:
        return controller
    if controller.count(":") == 1 or (controller.startswith("[") and "]:" in controller):
        return "http://" + controller
    return "http://%s:%d" % (controller, DEFAULT_PORT)


def read_token(path):
    """The API token in path, or "" when there is none."""
    path = path or DEFAULT_TOKEN_FILE
    try:
        with open(path) as f:
            return f.read().strip()
    except (IOError, OSError):
        return ""


class HaifyClient(object):
    def __init__(self, controller, token="", timeout=180):
        self.base = base_url(controller)
        self.token = token
        self.timeout = timeout

    def request(self, method, path, body=None):
        """Sends one request and returns the decoded answer. The controller
        reports a refusal as success: false with a message; that becomes a
        HaifyError, as does any transport failure."""
        data = None
        headers = {"Accept": "application/json"}
        if body is not None:
            data = json.dumps(body).encode()
            headers["Content-Type"] = "application/json"
        if self.token:
            headers["Authorization"] = "Bearer " + self.token
        req = urllib.request.Request(self.base + path, data=data, headers=headers, method=method)
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as resp:
                raw = resp.read()
        except urllib.error.HTTPError as e:
            raw = e.read()
            msg = _message(raw) or e.reason
            raise HaifyError("%s %s: %s" % (method, path, msg))
        except (urllib.error.URLError, OSError) as e:
            raise HaifyError("cannot reach the Haify controller at %s: %s" % (self.base, e))
        try:
            out = json.loads(raw.decode() or "{}")
        except ValueError:
            raise HaifyError("%s %s: unreadable answer" % (method, path))
        if isinstance(out, dict) and out.get("success") is False:
            raise HaifyError("%s %s: %s" % (method, path, out.get("message", "refused")))
        return out

    # The calls the driver makes.

    def resources(self):
        return self.request("GET", "/v1/resources").get("resources", [])

    def resource(self, name):
        """The resource, or None when it does not exist."""
        try:
            return self.request("GET", "/v1/resources/" + urllib.parse.quote(name)).get("resource")
        except HaifyError as e:
            if "not found" in str(e).lower():
                return None
            raise

    def nodes(self):
        return self.request("GET", "/v1/nodes").get("nodes", [])

    def pools(self):
        return self.request("GET", "/v1/pools").get("pools", [])

    def create(self, payload):
        self.request("POST", "/v1/resources", payload)

    def delete(self, name):
        self.request("DELETE", "/v1/resources/" + urllib.parse.quote(name))

    def attach_diskless(self, name, node):
        self.request("POST", "/v1/resources/%s/diskless-clients" % urllib.parse.quote(name),
                     {"resource": name, "node": node})

    def promote(self, name, node):
        self.request("POST", "/v1/resources/%s/primary" % urllib.parse.quote(name),
                     {"resource": name, "node": node, "quorumGuarded": True})

    def demote(self, name, node):
        self.request("POST", "/v1/resources/%s/secondary" % urllib.parse.quote(name),
                     {"resource": name, "node": node})

    def resize(self, name, size_bytes):
        self.request("PATCH", "/v1/resources/%s/volumes/0" % urllib.parse.quote(name),
                     {"resource": name, "volumeId": 0, "sizeBytes": size_bytes})

    def snapshot(self, name, snap):
        self.request("POST", "/v1/resources/%s/snapshots" % urllib.parse.quote(name),
                     {"resource": name, "name": snap})

    def delete_snapshot(self, name, snap):
        self.request("DELETE", "/v1/resources/%s/snapshots/%s" % (urllib.parse.quote(name), urllib.parse.quote(snap)))

    def populate(self, name, src, snap, node):
        # The controller finds the snapshot's device on node, whatever the
        # pool type: a ZFS snapshot has no device path to give it.
        self.request("POST", "/v1/resources/%s/populate" % urllib.parse.quote(name),
                     {"resource": name, "volumeId": 0, "node": node,
                      "sourceSnapshot": {"resource": src, "volumeId": 0, "name": snap, "replicated": True}})


def _message(raw):
    try:
        return json.loads(raw.decode()).get("message", "")
    except Exception:
        return ""


def node_name():
    """This host's Haify node name: its short host name."""
    return os.uname()[1].split(".")[0]
