import os
import sys
import unittest

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from fakes import FakeClient, HaifyError, Unreachable  # noqa: E402
from haify_cinder.backend import GiB, MANAGED_BY_LABEL, Backend, Settings  # noqa: E402

VOL = "0d6f3b1e-7c2a-4e55-9a0b-1f2e3d4c5b6a"
RES = "cinder-" + VOL
SNAP = "5a4b3c2d-1e0f-4a9b-8c7d-6e5f4a3b2c1d"


def backend(**kw):
    c = FakeClient()
    return Backend(c, Settings(controller=["ctl"], **kw)), c


class Create(unittest.TestCase):
    def test_spec(self):
        b, c = backend(pool="pool0", replicas=3)
        self.assertEqual(b.create(VOL, 10), RES)
        spec = c.calls[0][1]
        self.assertEqual(spec["sizeBytes"], 10 * GiB, "exactly the size Cinder asked for")
        self.assertEqual(spec["pool"], "pool0")
        self.assertEqual(spec["replicas"], 3)
        self.assertNotIn("nodes", spec)
        self.assertEqual(spec["labels"], {MANAGED_BY_LABEL: "cinder"})
        self.assertEqual(spec["drbdOptions"]["on-no-quorum"], "suspend-io")

    def test_fixed_nodes_win_over_replicas(self):
        b, c = backend(nodes=["a", "b"], replicas=3)
        b.create(VOL, 1)
        self.assertEqual(c.calls[0][1]["nodes"], ["a", "b"])
        self.assertNotIn("replicas", c.calls[0][1])

    def test_delete_is_idempotent(self):
        b, c = backend()
        b.delete(VOL)
        self.assertNotIn("delete_resource", c.names())
        c.res[RES] = {"name": RES}
        b.delete(VOL)
        self.assertIn(("delete_resource", RES), c.calls)


class Snapshots(unittest.TestCase):
    def test_delete_missing_snapshot_is_a_no_op(self):
        b, c = backend()
        c.res[RES] = {"name": RES}
        b.delete_snapshot(VOL, SNAP)
        self.assertNotIn("delete_snapshot", c.names())
        c.snaps[RES] = [SNAP]
        b.delete_snapshot(VOL, SNAP)
        self.assertIn(("delete_snapshot", RES, SNAP), c.calls)

    def test_from_snapshot_copies_on_a_source_replica(self):
        b, c = backend()
        src = "cinder-src"
        c.res[src] = {"name": src, "nodes": ["n2", "n3"],
                      "volumes": [{"pool": "haify_pool0", "backingVolume": "cinder-src_data"}]}
        b.create_from_snapshot(VOL, 5, "src", SNAP)
        create = [x for x in c.calls if x[0] == "create_resource"][0][1]
        self.assertEqual(create["nodes"], ["n2", "n3"], "the copy needs the source and target on one node")
        self.assertIn(("populate", RES, src, SNAP, "n2"), c.calls,
                      "the snapshot is named for the controller to find, not given as a device path")

    def test_failed_copy_deletes_the_new_volume(self):
        b, c = backend()
        c.res["cinder-src"] = {"name": "cinder-src", "nodes": ["n1"],
                               "volumes": [{"pool": "vg", "backingVolume": "lv"}]}
        c.fail["populate"] = HaifyError("dd failed")
        with self.assertRaises(HaifyError):
            b.create_from_snapshot(VOL, 5, "src", SNAP)
        self.assertIn(("delete_resource", RES), c.calls)

    def test_clone_goes_through_a_temporary_snapshot(self):
        b, c = backend()
        c.res["cinder-src"] = {"name": "cinder-src", "nodes": ["n1", "n2"],
                               "volumes": [{"pool": "vg", "backingVolume": "lv"}]}
        b.clone(VOL, 5, "src")
        tmp = "clone-" + VOL[:8]
        names = c.names()
        self.assertLess(names.index("create_snapshot"), names.index("populate"))
        self.assertIn(("populate", RES, "cinder-src", tmp, "n1"), c.calls)
        self.assertEqual(c.calls[-1], ("delete_snapshot", "cinder-src", tmp))

    def test_zfs_backend_copies_the_same_way(self):
        b, c = backend(storage_type="zfs")
        c.res["cinder-src"] = {"name": "cinder-src", "nodes": ["n1", "n2"],
                               "volumes": [{"pool": "haify_tank", "backingVolume": "cinder-src_data"}]}
        b.create_from_snapshot(VOL, 5, "src", SNAP)
        self.assertIn(("populate", RES, "cinder-src", SNAP, "n1"), c.calls)

    def test_source_without_replica_is_refused_before_anything_is_created(self):
        b, c = backend()
        c.res["cinder-src"] = {"name": "cinder-src", "nodes": [], "volumes": [{"pool": "vg"}]}
        with self.assertRaises(HaifyError):
            b.create_from_snapshot(VOL, 5, "src", SNAP)
        self.assertNotIn("create_resource", c.names())


class Connect(unittest.TestCase):
    def setUp(self):
        self.b, self.c = backend()
        self.c.res[RES] = {"name": RES, "nodes": ["n1", "n2"], "disklessNodes": ["n3"]}

    def test_replica_host_is_promoted(self):
        info = self.b.connect(VOL, "n1.example.com")
        self.assertEqual(info, {"driver_volume_type": "local",
                                "data": {"device_path": f"/dev/drbd/by-res/{RES}/0"}})
        self.assertIn(("primary", RES, "n1"), self.c.calls)
        self.assertNotIn("attach_client", self.c.names())
        self.assertNotIn("dual_primary", self.c.names())

    def test_host_without_replica_joins_as_diskless_client(self):
        self.b.connect(VOL, "compute9")
        names = self.c.names()
        self.assertLess(names.index("attach_client"), names.index("primary"))

    def test_node_map(self):
        b, c = backend(node_map={"compute1": "n1"})
        c.res[RES] = {"name": RES, "nodes": ["n1", "n2"]}
        b.connect(VOL, "compute1")
        self.assertIn(("primary", RES, "n1"), c.calls)

    def test_live_migration_opens_the_window(self):
        self.c.roles[RES] = {"n1": "Primary"}
        self.b.connect(VOL, "n2", attached_hosts=["n1"])
        self.assertIn(("dual_primary", RES, True, ["n1", "n2"]), self.c.calls)
        names = self.c.names()
        self.assertLess(names.index("dual_primary"), names.index("primary"))

    def test_leftover_primary_without_attachment_is_demoted(self):
        self.c.roles[RES] = {"n1": "Primary"}
        self.b.connect(VOL, "n2")
        self.assertIn(("secondary", RES, "n1"), self.c.calls)
        self.assertNotIn("dual_primary", self.c.names())

    def test_primary_in_use_elsewhere_is_refused(self):
        # An evacuation whose source still runs the guest: Nova deleted the
        # old attachment, DRBD refuses the demote because qemu holds it open.
        self.c.roles[RES] = {"n1": "Primary"}
        self.c.fail["secondary"] = HaifyError("State change failed: (-12) Device is held open by someone")
        with self.assertRaisesRegex(HaifyError, "Refusing a second writer"):
            self.b.connect(VOL, "n2")
        self.assertNotIn("primary", self.c.names())

    def test_failed_promote_closes_the_window(self):
        self.c.roles[RES] = {"n1": "Primary"}
        self.c.fail["primary"] = HaifyError("no quorum")
        with self.assertRaises(HaifyError):
            self.b.connect(VOL, "n2", attached_hosts=["n1"])
        self.assertEqual(self.c.calls[-1], ("dual_primary", RES, False, []))


class Disconnect(unittest.TestCase):
    def setUp(self):
        self.b, self.c = backend()
        self.c.res[RES] = {"name": RES, "nodes": ["n1", "n2"], "disklessClients": ["c9"]}

    def test_demotes_then_closes_the_window(self):
        self.b.disconnect(VOL, "n1")
        self.assertEqual(self.c.calls[1:], [("secondary", RES, "n1"), ("dual_primary", RES, False, [])])

    def test_diskless_client_is_detached(self):
        self.b.disconnect(VOL, "c9")
        self.assertIn(("detach_client", RES, "c9"), self.c.calls)

    def test_client_kept_while_another_attachment_is_there(self):
        self.b.disconnect(VOL, "c9", attached_hosts=["c9"])
        self.assertNotIn("detach_client", self.c.names())

    def test_unreachable_node_does_not_block_an_evacuation(self):
        self.c.fail["secondary"] = HaifyError("ssh: connect to host n1: No route to host")
        self.b.disconnect(VOL, "n1")
        self.assertIn("dual_primary", self.c.names())

    def test_device_in_use_fails_the_detach(self):
        self.c.fail["secondary"] = HaifyError("Device is held open by someone")
        with self.assertRaisesRegex(HaifyError, "still in use"):
            self.b.disconnect(VOL, "n1")

    def test_controller_down_fails_the_detach(self):
        self.c.fail["secondary"] = Unreachable("haify controller unreachable")
        with self.assertRaises(Unreachable):
            self.b.disconnect(VOL, "n1")

    def test_deleted_volume(self):
        del self.c.res[RES]
        self.b.disconnect(VOL, "n1")
        self.assertEqual(self.c.names(), ["find_resource"])


class Stats(unittest.TestCase):
    def test_capacity_is_divided_by_the_copies(self):
        b, c = backend(pool="p", replicas=2)
        thin = {"name": "haify_p", "type": "vg", "totalBytes": str(20 * GiB), "freeBytes": str(3 * GiB),
                "thinPoolLv": "haify_p_thin", "thinSizeBytes": str(16 * GiB), "thinDataPercent": 25}
        c.pool_list = [
            thin, dict(thin, node="n2", thinDataPercent=0),
            {"name": "haify_other", "totalBytes": str(999 * GiB), "freeBytes": str(999 * GiB)},
        ]
        # A thin pool counts its thin LV (16G, 12G and 16G free), not the VG.
        self.assertEqual(b.stats(), {"total_capacity_gb": 16.0, "free_capacity_gb": 14.0, "thin": True})

    def test_allocatable_fields_win(self):
        b, c = backend(replicas=2)
        c.pool_list = [{"name": "haify_p", "thin": True, "capacityBytes": str(16 * GiB),
                        "availableBytes": str(6 * GiB), "totalBytes": str(20 * GiB), "freeBytes": "0"}] * 2
        self.assertEqual(b.stats(), {"total_capacity_gb": 16.0, "free_capacity_gb": 6.0, "thin": True})

    def test_thick_pool(self):
        b, c = backend(replicas=2)
        c.pool_list = [{"name": "haify_p", "totalBytes": str(10 * GiB), "freeBytes": str(4 * GiB)}] * 2
        self.assertEqual(b.stats(), {"total_capacity_gb": 10.0, "free_capacity_gb": 4.0, "thin": False})


if __name__ == "__main__":
    unittest.main()
