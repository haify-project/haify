#!/usr/bin/env python3
"""libvirt qemu hook: Haify volumes follow the guests that use them.

A Haify volume is a DRBD device, and DRBD lets one node write at a time: the
node running a guest must hold its disks Primary, and every other node must
not. libvirt knows nothing of that, so this hook does it at the points libvirt
calls out to (/etc/libvirt/hooks/qemu.d/, with the guest's XML on stdin):

  prepare begin   before the guest starts here, by start, restore or an
                  incoming live migration: promote every Haify disk.
  migrate begin   on the destination of a live migration, before prepare:
                  note that the migration is under way.
  release end     after the guest stopped here, or left in a migration:
                  demote every Haify disk and close the dual-primary window.

A disk is a Haify disk when its source is /dev/drbd/by-res/<resource>/<vol>,
or a link to it (OpenNebula links each disk into the VM's directory).

Promotion goes through haify-controller, which promotes with a quorum guard
(it forces nothing a partitioned node could split-brain with), attaches a host
that holds no replica as a diskless client, and opens the dual-primary window
for a live migration. While another node holds a disk Primary the hook opens
that window only when libvirt has just told it a migration to this host is
under way; any other Primary is a leftover, and starting a second writer on it
is refused. When the controller cannot be reached, a disk that is already up
here is promoted or demoted with plain `drbdadm` (never --force), which DRBD's
own quorum still guards; a migration then needs the controller.

A resource that a drbd-reactor promoter manages on this node (haify ha create,
and so `haify ha create --vm`) is left alone: drbd-reactor owns its role.

Configuration: /etc/haify/libvirt.conf, KEY=VALUE lines:
  CONTROLLER  REST addresses of haify-controller, comma-separated,
              host[:port] (port 3375) or a full http(s):// URL
  NODE        this host's Haify node name (default: the short host name)
  TOKEN_FILE  bearer token, when the controller has [auth] or [rbac]
"""

import json
import os
import re
import socket
import subprocess
import sys
import syslog
import time
import urllib.error
import urllib.request
import xml.etree.ElementTree as ET

CONFIG_PATH = "/etc/haify/libvirt.conf"
STATE_DIR = "/run/haify-libvirt"
REACTOR_DIR = "/etc/drbd-reactor.d"
BY_RES = re.compile(r"^/dev/drbd/by-res/([A-Za-z0-9_.-]+)/(\d+)$")
DEVICE_WAIT = 30  # seconds for /dev/drbd/by-res/... to appear after a promote
# Marks the resources a guest's disks are on; the web UI lists guests by it.
DOMAIN_LABEL = "haify.libvirt/domain"


class HookError(Exception):
    """Fails the libvirt operation, with the message libvirt shows."""


class Unreachable(HookError):
    """No controller address could be reached at all."""


def log(msg):
    syslog.syslog(syslog.LOG_INFO, msg)


def run(*cmd):
    """Runs a command on this node; returns (exit code, output)."""
    try:
        p = subprocess.run(cmd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, timeout=120)
        return p.returncode, p.stdout
    except (OSError, subprocess.TimeoutExpired) as e:
        return 127, str(e)


def read_config(path=CONFIG_PATH):
    cfg = {}
    try:
        with open(path) as f:
            for line in f:
                line = line.strip()
                if not line or line.startswith("#") or "=" not in line:
                    continue
                k, v = line.split("=", 1)
                cfg[k.strip()] = v.strip().strip('"').strip("'")
    except FileNotFoundError:
        pass
    cfg.setdefault("NODE", socket.gethostname().split(".")[0])
    return cfg


def haify_disks(domain_xml):
    """The Haify resources a guest's disks are on, in order, without repeats."""
    if not domain_xml.strip():
        return []
    try:
        root = ET.fromstring(domain_xml)
    except ET.ParseError as e:
        raise HookError(f"cannot read the domain XML libvirt passed: {e}")
    seen = []
    for disk in root.iter("disk"):
        src = disk.find("source")
        if src is None:
            continue
        path = src.get("dev") or src.get("file") or ""
        m = BY_RES.match(path)
        if not m and os.path.islink(path):
            m = BY_RES.match(os.readlink(path))
        if m and m.group(1) not in seen:
            seen.append(m.group(1))
    return seen


def reactor_managed(resource, reactor_dir=None):
    """True when an enabled drbd-reactor promoter here manages resource."""
    reactor_dir = reactor_dir or REACTOR_DIR
    try:
        names = os.listdir(reactor_dir)
    except FileNotFoundError:
        return False
    pattern = re.compile(r"^\s*\[promoter\.resources\.%s\]\s*$" % re.escape(resource), re.M)
    for name in names:
        if not name.endswith(".toml"):
            continue
        try:
            with open(os.path.join(reactor_dir, name)) as f:
                if pattern.search(f.read()):
                    return True
        except OSError:
            continue
    return False


# The controller is on the storage network, so it is reached directly: a host
# with http_proxy set would otherwise send every call to the proxy, as urllib
# honours no_proxy host names but not CIDR ranges.
direct_open = urllib.request.build_opener(urllib.request.ProxyHandler({})).open


class Controller:
    """haify-controller's REST gateway, tried address by address."""

    def __init__(self, cfg, opener=direct_open):
        self.urls = []
        for entry in (cfg.get("CONTROLLER") or "").split(","):
            entry = entry.strip().rstrip("/")
            if not entry:
                continue
            if "://" not in entry:
                entry = "http://" + entry
            if not re.search(r":\d+$", entry.split("://", 1)[1]):
                entry += ":3375"
            self.urls.append(entry)
        self.token = None
        if cfg.get("TOKEN_FILE"):
            with open(cfg["TOKEN_FILE"]) as f:
                self.token = f.read().strip() or None
        self.opener = opener

    def request(self, method, path, payload=None):
        if not self.urls:
            raise Unreachable("haify controller unreachable: CONTROLLER is not set in " + CONFIG_PATH)
        data = json.dumps(payload).encode() if payload is not None else None
        failed = []
        for base in self.urls:
            req = urllib.request.Request(base + path, data=data, method=method)
            req.add_header("Content-Type", "application/json")
            if self.token:
                req.add_header("Authorization", "Bearer " + self.token)
            try:
                with self.opener(req, timeout=60) as resp:
                    body = resp.read().decode() or "{}"
            except urllib.error.HTTPError as e:
                # grpc-gateway renders gRPC errors as {"code":..,"message":..}.
                try:
                    msg = json.loads(e.read().decode()).get("message") or e.reason
                except ValueError:
                    msg = e.reason
                raise HookError(f"haify controller error ({e.code}) for {method} {path}: {msg}")
            except (urllib.error.URLError, OSError) as e:
                failed.append(f"{base}: {getattr(e, 'reason', e)}")
                continue
            out = json.loads(body)
            # Application failures come back as HTTP 200 with success=false,
            # and protojson omits a false field: a missing success is a failure.
            if isinstance(out, dict) and "message" in out and not out.get("success"):
                raise HookError(f"haify: {out['message']}")
            return out
        raise Unreachable("haify controller unreachable at " + "; ".join(failed))


def other_primary(status, node):
    """The Haify node other than node that holds the resource Primary, or None."""
    for host, st in (status.get("status", status).get("nodeStates") or {}).items():
        if st.get("role") == "Primary" and (st.get("node") or host) != node:
            return st.get("node") or host
    return None


def participates(info, node):
    res = info.get("resource", info)
    for key in ("nodes", "disklessNodes", "disklessClients"):
        if node in (res.get(key) or []):
            return True
    return False


def wait_for_device(resource, timeout=DEVICE_WAIT):
    path = f"/dev/drbd/by-res/{resource}/0"
    deadline = time.time() + timeout
    while not os.path.exists(path):
        if time.time() >= deadline:
            raise HookError(f"{resource} was promoted here but {path} did not appear within {timeout}s")
        time.sleep(0.5)


def local_role(resource):
    """This node's role and the peers holding resource Primary, from DRBD."""
    rc, out = run("drbdsetup", "status", resource)
    if rc != 0:
        return None, []
    # [ \t], not \s: \s matches the newline and would join a line to the next.
    m = re.search(r"^\S+[ \t]+role:(\S+)", out, re.M)
    peers = [p for p in re.findall(r"^  (\S+)[ \t].*\brole:Primary\b", out, re.M) if ":" not in p]
    return (m.group(1) if m else None), peers


def incoming_marker(domain):
    return os.path.join(STATE_DIR, domain + ".incoming")


def promote(ctl, node, domain, resource):
    """Makes resource Primary here for domain. Returns True when it opened the
    dual-primary window (so a failure later knows to close it)."""
    try:
        info = ctl.request("GET", f"/v1/resources/{resource}")
        if not participates(info, node):
            ctl.request("POST", f"/v1/resources/{resource}/diskless-clients", {"resource": resource, "node": node})
            log(f"{domain}: attached {node} to {resource} as a diskless client")
        peer = other_primary(ctl.request("GET", f"/v1/resources/{resource}/status"), node)
    except Unreachable as e:
        promote_locally(domain, resource, str(e))
        return False

    opened = False
    if peer:
        if not os.path.exists(incoming_marker(domain)):
            raise HookError(
                f"{resource} is still Primary on {peer} and no live migration of {domain} to {node} is under "
                f"way. Something on {peer} still has it open, or an earlier stop failed there. Refusing a "
                f"second writer: stop whatever uses it on {peer}, or `haify resource secondary {resource} "
                f"{peer}`, then try again.")
        ctl.request("POST", f"/v1/resources/{resource}/dual-primary",
                    {"resource": resource, "enable": True, "nodes": [peer, node]})
        opened = True
        log(f"{domain}: dual-primary window open on {resource} for the migration from {peer}")
    try:
        ctl.request("POST", f"/v1/resources/{resource}/primary",
                    {"resource": resource, "node": node, "quorumGuarded": True})
        wait_for_device(resource)
    except HookError:
        if opened:
            close_window(ctl, resource)
        raise
    log(f"{domain}: {resource} Primary on {node}")
    label(ctl, domain, resource, info)
    return opened


def label(ctl, domain, resource, info):
    """Records on the resource which guest uses it. Best effort: a missing
    label costs the web UI a row, never the guest its start."""
    labels = info.get("resource", info).get("labels") or {}
    if labels.get(DOMAIN_LABEL) == domain:
        return
    try:
        ctl.request("POST", f"/v1/resources/{resource}/labels",
                    {"resource": resource, "labels": {DOMAIN_LABEL: domain}})
    except HookError as e:
        log(f"{domain}: could not label {resource} with the guest: {e}")


def promote_locally(domain, resource, why):
    if not os.path.exists(f"/dev/drbd/by-res/{resource}/0"):
        raise HookError(f"{why}; and {resource} is not up on this node, so it cannot be promoted without "
                        f"the controller")
    role, peers = local_role(resource)
    if role == "Primary":
        return
    if peers:
        raise HookError(f"{why}; and {peers[0]} holds {resource} Primary. A live migration needs the controller")
    rc, out = run("drbdadm", "primary", resource)
    if rc != 0:
        raise HookError(f"{why}; promoting {resource} on this node failed too: {out.strip()}")
    log(f"{domain}: controller unreachable; promoted {resource} with drbdadm")


def close_window(ctl, resource):
    try:
        ctl.request("POST", f"/v1/resources/{resource}/dual-primary", {"resource": resource, "enable": False})
    except Unreachable:
        run("drbdadm", "net-options", "--allow-two-primaries=no", resource)


def demote(ctl, node, domain, resource):
    """Makes resource Secondary here and closes any dual-primary window. Every
    step runs even when an earlier one failed; the first failure is raised."""
    errors = []
    try:
        ctl.request("POST", f"/v1/resources/{resource}/secondary", {"resource": resource, "node": node})
    except Unreachable as e:
        if os.path.exists(f"/dev/drbd/by-res/{resource}/0"):
            rc, out = run("drbdadm", "secondary", resource)
            if rc != 0:
                errors.append(f"{e}; demoting {resource} here failed too: {out.strip()}")
            run("drbdadm", "net-options", "--allow-two-primaries=no", resource)
            log(f"{domain}: controller unreachable; demoted {resource} with drbdadm. Once it is back, run "
                f"`haify resource dual-primary {resource} off` if a migration was under way")
        return errors
    except HookError as e:
        errors.append(str(e))
    try:
        close_window(ctl, resource)
    except HookError as e:
        errors.append(str(e))
    try:
        info = ctl.request("GET", f"/v1/resources/{resource}")
        res = info.get("resource", info)
        if node in (res.get("disklessClients") or []):
            ctl.request("DELETE", f"/v1/resources/{resource}/diskless-clients/{node}")
            log(f"{domain}: detached {node} from {resource}")
    except HookError as e:
        log(f"{domain}: could not detach {node} from {resource} (it stays attached): {e}")
    if not errors:
        log(f"{domain}: {resource} Secondary on {node}")
    return errors


def handle(domain, op, sub, domain_xml, cfg, ctl):
    resources = [r for r in haify_disks(domain_xml) if not reactor_managed(r)]
    if not resources:
        return
    node = cfg["NODE"]
    if op == "migrate" and sub == "begin":
        os.makedirs(STATE_DIR, exist_ok=True)
        with open(incoming_marker(domain), "w") as f:
            f.write(" ".join(resources) + "\n")
        return
    if op == "prepare" and sub == "begin":
        done = []
        try:
            for r in resources:
                promote(ctl, node, domain, r)
                done.append(r)
        except HookError:
            for r in done:  # leave nothing half-started
                demote(ctl, node, domain, r)
            raise
        finally:
            if os.path.exists(incoming_marker(domain)):
                os.unlink(incoming_marker(domain))
        return
    if op == "release" and sub == "end":
        errors = []
        for r in resources:
            errors += demote(ctl, node, domain, r)
        if os.path.exists(incoming_marker(domain)):
            os.unlink(incoming_marker(domain))
        if errors:
            raise HookError("; ".join(errors))


def main(argv):
    if len(argv) < 3:
        return 0
    domain, op, sub = argv[0], argv[1], argv[2]
    if (op, sub) not in {("prepare", "begin"), ("migrate", "begin"), ("release", "end")}:
        return 0
    syslog.openlog("haify-libvirt-hook")
    domain_xml = sys.stdin.read()
    cfg = read_config()
    try:
        handle(domain, op, sub, domain_xml, cfg, Controller(cfg))
    except HookError as e:
        log(f"{domain} {op} {sub}: {e}")
        print(f"haify: {e}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
