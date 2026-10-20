"""Tests for haify-hook.py: python3 -m unittest discover -s deploy/libvirt"""

import importlib.util
import io
import json
import os
import tempfile
import unittest
import urllib.error

_spec = importlib.util.spec_from_file_location(
    "haify_hook", os.path.join(os.path.dirname(__file__), "haify-hook.py"))
hook = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(hook)


def domain(*devs):
    disks = "".join(f"<disk type='block' device='disk'><source dev='{d}'/><target dev='vd{chr(97 + i)}'/></disk>"
                    for i, d in enumerate(devs))
    return f"<domain type='kvm'><name>vm1</name><devices>{disks}</devices></domain>"


class FakeController:
    """Answers like haify-controller's REST gateway and records every call."""

    def __init__(self, nodes=("k1", "k2"), clients=(), primary_on=None, fail=None, unreachable=False,
                 labels=None):
        self.calls = []
        self.nodes, self.clients, self.primary_on = list(nodes), list(clients), primary_on
        self.labels = labels if labels is not None else {hook.DOMAIN_LABEL: "vm1"}
        self.fail = fail or {}
        self.unreachable = unreachable

    def request(self, method, path, payload=None):
        if self.unreachable:
            raise hook.Unreachable("haify controller unreachable at http://c:3375: refused")
        self.calls.append((method, path, payload))
        for (m, p), msg in self.fail.items():
            if m == method and path.endswith(p):
                raise hook.HookError(msg)
        if method == "GET" and path.endswith("/status"):
            states = {}
            if self.primary_on:
                states["h-" + self.primary_on] = {"role": "Primary", "node": self.primary_on}
            return {"success": True, "status": {"nodeStates": states}}
        if method == "GET":
            return {"success": True, "resource": {"nodes": self.nodes, "disklessClients": self.clients,
                                                  "labels": self.labels}}
        return {"success": True}

    def posts(self):
        return [(p, b) for m, p, b in self.calls if m in ("POST", "DELETE")]


class HookTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.state = os.path.join(self.tmp.name, "state")
        self.reactor = os.path.join(self.tmp.name, "reactor")
        os.makedirs(self.reactor)
        self._patch("STATE_DIR", self.state)
        self._patch("REACTOR_DIR", self.reactor)
        self._patch("wait_for_device", lambda r, timeout=0: None)
        self._patch("log", lambda msg: None)
        self.ran = []
        self.drbd_status = ""
        self._patch("run", self.fake_run)
        self.cfg = {"NODE": "k2"}

    def _patch(self, name, value):
        old = getattr(hook, name)
        setattr(hook, name, value)
        self.addCleanup(setattr, hook, name, old)

    def fake_run(self, *cmd):
        self.ran.append(cmd)
        if cmd[:2] == ("drbdsetup", "status"):
            return 0, self.drbd_status
        return 0, ""

    def handle(self, ctl, op, sub, xml=None):
        hook.handle("vm1", op, sub, xml if xml is not None else domain("/dev/drbd/by-res/vm1/0"), self.cfg, ctl)


class DiskParsing(HookTest):
    def test_only_haify_by_res_disks_once_each(self):
        xml = domain("/dev/drbd/by-res/a/0", "/dev/sda", "/dev/drbd/by-res/a/1", "/dev/drbd/by-res/b/0", "/dev/drbd0")
        self.assertEqual(hook.haify_disks(xml), ["a", "b"])

    def test_a_link_to_a_haify_disk_is_one(self):
        with tempfile.TemporaryDirectory() as d:
            link = os.path.join(d, "disk.0")
            os.symlink("/dev/drbd/by-res/one-img-3/0", link)
            other = os.path.join(d, "disk.1")
            os.symlink("/dev/sdz", other)
            self.assertEqual(hook.haify_disks(domain(link, other)), ["one-img-3"])

    def test_no_disks_no_calls(self):
        ctl = FakeController()
        self.handle(ctl, "prepare", "begin", xml=domain("/dev/sda"))
        self.assertEqual(ctl.calls, [])


class Start(HookTest):
    def test_promotes_with_the_quorum_guard(self):
        ctl = FakeController()
        self.handle(ctl, "prepare", "begin")
        self.assertEqual(ctl.posts(), [("/v1/resources/vm1/primary",
                                        {"resource": "vm1", "node": "k2", "quorumGuarded": True})])

    def test_the_resource_is_labelled_with_its_guest(self):
        ctl = FakeController(labels={"team": "a"})
        self.handle(ctl, "prepare", "begin")
        self.assertEqual(ctl.posts()[-1], ("/v1/resources/vm1/labels",
                                           {"resource": "vm1", "labels": {"haify.libvirt/domain": "vm1"}}))

    def test_a_failed_label_does_not_fail_the_start(self):
        ctl = FakeController(labels={}, fail={("POST", "/labels"): "denied"})
        self.handle(ctl, "prepare", "begin")
        self.assertIn(("/v1/resources/vm1/primary", {"resource": "vm1", "node": "k2", "quorumGuarded": True}),
                      ctl.posts())

    def test_a_host_without_a_replica_attaches_first(self):
        ctl = FakeController(nodes=["k1", "k3"])
        self.handle(ctl, "prepare", "begin")
        self.assertEqual([p for p, _ in ctl.posts()],
                         ["/v1/resources/vm1/diskless-clients", "/v1/resources/vm1/primary"])

    def test_refuses_a_second_writer_without_a_migration(self):
        ctl = FakeController(primary_on="k1")
        with self.assertRaisesRegex(hook.HookError, "still Primary on k1"):
            self.handle(ctl, "prepare", "begin")
        self.assertEqual(ctl.posts(), [], "nothing is promoted and no window is opened")

    def test_a_failed_disk_undoes_the_ones_before_it(self):
        ctl = FakeController(fail={("POST", "/b/primary"): "no quorum"})
        xml = domain("/dev/drbd/by-res/a/0", "/dev/drbd/by-res/b/0")
        with self.assertRaisesRegex(hook.HookError, "no quorum"):
            self.handle(ctl, "prepare", "begin", xml=xml)
        self.assertIn(("/v1/resources/a/secondary", {"resource": "a", "node": "k2"}), ctl.posts())

    def test_reactor_managed_resources_are_left_alone(self):
        with open(os.path.join(self.reactor, "haify-ha-vm1.toml"), "w") as f:
            f.write("[[promoter]]\n[promoter.resources.vm1]\nstart = []\n")
        ctl = FakeController()
        self.handle(ctl, "prepare", "begin")
        self.handle(ctl, "release", "end")
        self.assertEqual(ctl.calls, [])


class Migration(HookTest):
    def test_incoming_migration_opens_the_window_for_the_two_nodes(self):
        ctl = FakeController(primary_on="k1")
        self.handle(ctl, "migrate", "begin")
        self.handle(ctl, "prepare", "begin")
        self.assertEqual(ctl.posts(), [
            ("/v1/resources/vm1/dual-primary", {"resource": "vm1", "enable": True, "nodes": ["k1", "k2"]}),
            ("/v1/resources/vm1/primary", {"resource": "vm1", "node": "k2", "quorumGuarded": True}),
        ])
        self.assertFalse(os.path.exists(hook.incoming_marker("vm1")), "the marker is used once")

    def test_a_failed_promote_closes_the_window(self):
        ctl = FakeController(primary_on="k1", fail={("POST", "/primary"): "refused"})
        self.handle(ctl, "migrate", "begin")
        with self.assertRaises(hook.HookError):
            self.handle(ctl, "prepare", "begin")
        self.assertIn(("/v1/resources/vm1/dual-primary", {"resource": "vm1", "enable": False}), ctl.posts())

    def test_the_marker_does_not_outlive_a_failed_start(self):
        ctl = FakeController(primary_on="k1", fail={("POST", "/primary"): "refused"})
        self.handle(ctl, "migrate", "begin")
        with self.assertRaises(hook.HookError):
            self.handle(ctl, "prepare", "begin")
        ctl2 = FakeController(primary_on="k1")
        with self.assertRaisesRegex(hook.HookError, "no live migration"):
            self.handle(ctl2, "prepare", "begin")


class Stop(HookTest):
    def test_demotes_and_closes_the_window(self):
        ctl = FakeController()
        self.handle(ctl, "release", "end")
        self.assertEqual(ctl.posts(), [
            ("/v1/resources/vm1/secondary", {"resource": "vm1", "node": "k2"}),
            ("/v1/resources/vm1/dual-primary", {"resource": "vm1", "enable": False}),
        ])

    def test_a_diskless_client_is_detached(self):
        ctl = FakeController(nodes=["k1", "k3"], clients=["k2"])
        self.handle(ctl, "release", "end")
        self.assertEqual(ctl.calls[-1][:2], ("DELETE", "/v1/resources/vm1/diskless-clients/k2"))

    def test_the_window_closes_even_when_the_demote_fails(self):
        ctl = FakeController(fail={("POST", "/secondary"): "device busy"})
        with self.assertRaisesRegex(hook.HookError, "device busy"):
            self.handle(ctl, "release", "end")
        self.assertIn(("/v1/resources/vm1/dual-primary", {"resource": "vm1", "enable": False}), ctl.posts())


class WithoutController(HookTest):
    def setUp(self):
        super().setUp()
        self._patch("os", _FakeOS(os, exists=True))

    def test_promotes_locally_with_plain_drbdadm(self):
        self.drbd_status = "vm1 role:Secondary\n  disk:UpToDate\n  k1 role:Secondary\n"
        self.handle(FakeController(unreachable=True), "prepare", "begin")
        self.assertIn(("drbdadm", "primary", "vm1"), self.ran)
        self.assertNotIn("--force", sum(self.ran, ()))

    def test_refuses_locally_while_a_peer_is_primary(self):
        self.drbd_status = "vm1 role:Secondary\n  disk:UpToDate\n  k1 role:Primary\n"
        with self.assertRaisesRegex(hook.HookError, "k1 holds vm1 Primary"):
            self.handle(FakeController(unreachable=True), "prepare", "begin")
        self.assertNotIn(("drbdadm", "primary", "vm1"), self.ran)

    def test_demotes_locally(self):
        self.handle(FakeController(unreachable=True), "release", "end")
        self.assertIn(("drbdadm", "secondary", "vm1"), self.ran)
        self.assertIn(("drbdadm", "net-options", "--allow-two-primaries=no", "vm1"), self.ran)


class _FakeOS:
    """os, with path.exists answering for /dev paths."""

    def __init__(self, real, exists):
        self._real, self._exists = real, exists
        self.path = _FakePath(real.path, exists)

    def __getattr__(self, name):
        return getattr(self._real, name)


class _FakePath:
    def __init__(self, real, exists):
        self._real, self._exists = real, exists

    def exists(self, p):
        return self._exists if p.startswith("/dev/") else self._real.exists(p)

    def __getattr__(self, name):
        return getattr(self._real, name)


class ControllerClient(unittest.TestCase):
    def opener(self, answers):
        calls = []

        def open_(req, timeout=0):
            calls.append(req.full_url)
            a = answers.pop(0)
            if isinstance(a, Exception):
                raise a
            return io.BytesIO(json.dumps(a).encode())
        return open_, calls

    def test_addresses_get_the_default_port_and_fail_over_in_order(self):
        op, calls = self.opener([urllib.error.URLError("refused"), {"success": True}])
        ctl = hook.Controller({"CONTROLLER": "10.0.0.1, http://10.0.0.2:4000"}, opener=op)
        ctl.request("GET", "/v1/nodes")
        self.assertEqual(calls, ["http://10.0.0.1:3375/v1/nodes", "http://10.0.0.2:4000/v1/nodes"])

    def test_nothing_reachable_is_unreachable(self):
        op, _ = self.opener([urllib.error.URLError("refused")])
        with self.assertRaises(hook.Unreachable):
            hook.Controller({"CONTROLLER": "10.0.0.1"}, opener=op).request("GET", "/v1/nodes")

    def test_success_false_is_a_failure(self):
        op, _ = self.opener([{"message": "no quorum on k2"}])
        with self.assertRaisesRegex(hook.HookError, "no quorum on k2"):
            hook.Controller({"CONTROLLER": "c"}, opener=op).request("POST", "/x", {})

    def test_http_errors_carry_the_controller_message(self):
        err = urllib.error.HTTPError("u", 412, "Precondition Failed", {}, io.BytesIO(b'{"code":9,"message":"busy"}'))
        op, _ = self.opener([err])
        with self.assertRaisesRegex(hook.HookError, r"\(412\).*busy"):
            hook.Controller({"CONTROLLER": "c"}, opener=op).request("POST", "/x", {})


if __name__ == "__main__":
    unittest.main()
