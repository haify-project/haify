package opennebula

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func TestParseLocationAndDiskID(t *testing.T) {
	l := ParseLocation("one2:/var/lib/one//datastores/100/7/disk.3")
	if l.Host != "one2" || l.DiskID() != 3 || l.Dir() != "/var/lib/one/datastores/100/7" {
		t.Fatalf("got %+v disk %d dir %s", l, l.DiskID(), l.Dir())
	}
	if id := ParseLocation("one2:/var/lib/one//datastores/100/7/").DiskID(); id != -1 {
		t.Fatalf("a VM directory is no disk, got %d", id)
	}
	if l := ParseLocation("one-img-4"); l.Host != "" || l.Path != "one-img-4" {
		t.Fatalf("no host: %+v", l)
	}
}

func TestDiskResource(t *testing.T) {
	persistent := &Disk{ID: 0, Source: "one-img-4", Persistent: "YES", Clone: "NO"}
	clone := &Disk{ID: 2, Source: "one-img-4", Persistent: "NO", Clone: "YES"}
	if r, _ := diskResource(7, persistent); r != "one-img-4" {
		t.Fatalf("a persistent disk is its image, got %s", r)
	}
	if r, _ := diskResource(7, clone); r != "one-vm-7-2" {
		t.Fatalf("a non-persistent disk is the VM's copy, got %s", r)
	}
	if _, err := diskResource(7, &Disk{Source: "/var/lib/one/foo", Clone: "YES"}); err == nil {
		t.Fatal("a source that is no Haify image must be refused")
	}
}

func TestLinkPersistentDiskJoinsTheHostAndLinksIt(t *testing.T) {
	b := newFakeBackend()
	b.add("one-img-4", "n1", "n2")
	d, sh, _ := testDriver(b, vmXML(7, 1, diskXML(0, "one-img-4", true, 3072)))
	if err := d.TMLink(context.Background(), false, []string{"fe:one-img-4", "n3:/ds/100/7/disk.0", "7", "101"}); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(b.calls, "attach one-img-4 n3") {
		t.Fatalf("a host without a replica joins as a diskless client: %v", b.calls)
	}
	if len(sh.remote) != 1 || !strings.Contains(sh.remote[0], "ln -sfn '/dev/drbd/by-res/one-img-4/0' '/ds/100/7/disk.0'") {
		t.Fatalf("link script: %v", sh.remote)
	}
	if slices.ContainsFunc(b.calls, func(c string) bool { return strings.HasPrefix(c, "promote") }) {
		t.Fatalf("a starting guest is promoted by the libvirt hook, not the driver: %v", b.calls)
	}
	if b.resources["one-img-4"].Labels[LabelVM] != "7" {
		t.Fatal("the resource names its VM")
	}
}

func TestHotAttachPromotesAndHotDetachDemotes(t *testing.T) {
	b := newFakeBackend()
	b.add("one-img-4", "n1", "n2")
	d, _, _ := testDriver(b, vmXML(7, lcmHotplug, diskXML(2, "one-img-4", true, 1024)))
	ctx := context.Background()
	if err := d.TMLink(ctx, false, []string{"fe:one-img-4", "n1:/ds/100/7/disk.2", "7", "101"}); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(b.calls, "promote one-img-4 n1") {
		t.Fatalf("libvirt runs no hook for a hot attach, so the driver promotes: %v", b.calls)
	}
	if err := d.TMMvds(ctx, []string{"n1:/ds/100/7/disk.2", "fe:one-img-4", "7", "101"}); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(b.calls, "demote one-img-4 n1") {
		t.Fatalf("a hot detach demotes: %v", b.calls)
	}
}

func TestCloneCopiesTheImageAndLeavesItPrimaryNowhere(t *testing.T) {
	b := newFakeBackend()
	b.add("one-img-4", "n1", "n2")
	d, _, _ := testDriver(b, vmXML(7, 1, diskXML(0, "one-img-4", false, 4096)))
	// The controller's own demote after the copy can fail and only warn.
	b.primary["one-vm-7-0"] = []string{"n1"}
	if err := d.TMLink(context.Background(), true, []string{"fe:one-img-4", "n2:/ds/100/7/disk.0", "7", "101"}); err != nil {
		t.Fatal(err)
	}
	want := []string{"create one-vm-7-0 4294967296 n1,n2", "demote one-vm-7-0 n1"}
	for _, w := range want {
		if !slices.Contains(b.calls, w) {
			t.Fatalf("missing %q in %v", w, b.calls)
		}
	}
	if !slices.ContainsFunc(b.calls, func(c string) bool { return strings.HasPrefix(c, "populate one-vm-7-0 from one-img-4@") }) {
		t.Fatalf("the copy is filled from a snapshot of the image: %v", b.calls)
	}
	if len(b.snapshots["one-img-4"]) != 0 {
		t.Fatalf("the snapshot taken for the copy is dropped: %v", b.snapshots)
	}
}

func TestDeleteDropsOnlyNonPersistentCopies(t *testing.T) {
	b := newFakeBackend()
	b.add("one-img-4", "n1", "n2")
	b.add("one-vm-7-1", "n1", "n2")
	d, _, delegated := testDriver(b, vmXML(7, 1, diskXML(0, "one-img-4", true, 0), diskXML(1, "one-img-4", false, 0)))
	ctx := context.Background()
	if err := d.TMDelete(ctx, []string{"n1:/ds/100/7/disk.1", "7", "101"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.resources["one-vm-7-1"]; ok {
		t.Fatal("the VM's copy is deleted")
	}
	if err := d.TMDelete(ctx, []string{"n1:/ds/100/7/disk.0", "7", "101"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.resources["one-img-4"]; !ok {
		t.Fatal("a persistent image survives its VM")
	}
	if err := d.TMDelete(ctx, []string{"n1:/ds/100/7/", "7", "100"}); err != nil {
		t.Fatal(err)
	}
	if len(*delegated) != 1 || !strings.HasPrefix((*delegated)[0], "ssh/delete") {
		t.Fatalf("the VM directory is the ssh driver's: %v", *delegated)
	}
}

func TestMvAfterHostFailureLinksOnTheNewHost(t *testing.T) {
	b := newFakeBackend()
	b.add("one-img-4", "n1", "n2")
	d, sh, _ := testDriver(b, vmXML(7, lcmPrologMigrateUnknown, diskXML(0, "one-img-4", true, 0)))
	if err := d.TMMv(context.Background(), []string{"n1:/ds/100/7/disk.0", "n3:/ds/100/7/disk.0", "7", "101"}); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(b.calls, "attach one-img-4 n3") || len(sh.remote) != 1 || !strings.HasPrefix(sh.remote[0], "n3:") {
		t.Fatalf("calls %v remote %v", b.calls, sh.remote)
	}
	if slices.ContainsFunc(sh.remote, func(s string) bool { return strings.HasPrefix(s, "n1:") }) {
		t.Fatal("the failed host is not touched")
	}
}

func TestMigrateSettlesOnOnePrimary(t *testing.T) {
	vm := vmXML(7, 3, diskXML(0, "one-img-4", true, 0))
	for _, tc := range []struct {
		action, wantDemote string
		args               []string
		delegated          int
	}{
		// The guest left n1; the ssh driver called back for the disks.
		{"postmigrate", "demote one-img-4 n1", []string{"n1", "n2", "/ds/100/7", "7", "100", "ssh"}, 0},
		// The guest never left n1; the ssh driver's failmigrate calls nobody back.
		{"failmigrate", "demote one-img-4 n2", []string{"n1", "n2", "/ds/100/7", "7", "100"}, 1},
	} {
		b := newFakeBackend()
		b.add("one-img-4", "n1", "n2")
		b.primary["one-img-4"] = []string{"n1", "n2"}
		d, _, delegated := testDriver(b, vm)
		if err := d.TMMigrate(context.Background(), tc.action, tc.args, b64(vm)); err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(b.calls, tc.wantDemote) || !slices.Contains(b.calls, "dual-primary one-img-4 false") {
			t.Fatalf("%s: %v", tc.action, b.calls)
		}
		if len(*delegated) != tc.delegated {
			t.Fatalf("%s delegated %v", tc.action, *delegated)
		}
	}
}

func TestPremigrateCallbackJoinsTheDestination(t *testing.T) {
	vm := vmXML(7, 3, diskXML(0, "one-img-4", true, 0))
	b := newFakeBackend()
	b.add("one-img-4", "n1", "n2")
	d, _, delegated := testDriver(b, vm)
	args := []string{"n1", "n3", "/ds/100/7", "7", "100", "ssh"}
	if err := d.TMMigrate(context.Background(), "premigrate", args, b64(vm)); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(b.calls, "attach one-img-4 n3") || len(*delegated) != 0 {
		t.Fatalf("calls %v delegated %v", b.calls, *delegated)
	}
}

func TestSnapshotsAreNamedAfterOpenNebulasID(t *testing.T) {
	b := newFakeBackend()
	b.add("one-img-4", "n1", "n2")
	d, _, _ := testDriver(b, vmXML(7, 3, diskXML(0, "one-img-4", true, 0)))
	ctx := context.Background()
	for _, a := range []string{"snap_create_live", "snap_revert", "snap_delete"} {
		if err := d.TMSnapshot(ctx, a, []string{"n1:/ds/100/7/disk.0", "3", "7", "101"}); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"snapshot one-img-4 one-snap-3", "rollback one-img-4 one-snap-3", "delete-snapshot one-img-4 one-snap-3"}
	if !slices.Equal(b.calls, want) {
		t.Fatalf("got %v", b.calls)
	}
}

func TestMonitorCountsEveryCopyOnce(t *testing.T) {
	b := newFakeBackend()
	d, _, _ := testDriver(b, "")
	action := `<DS_DRIVER_ACTION_DATA><DATASTORE><ID>101</ID><TEMPLATE><HAIFY_POOL>pool0</HAIFY_POOL>
<HAIFY_REPLICAS>2</HAIFY_REPLICAS></TEMPLATE></DATASTORE></DS_DRIVER_ACTION_DATA>`
	out, err := d.DSMonitor(context.Background(), b64(action))
	if err != nil {
		t.Fatal(err)
	}
	// 20 GiB of thin pool over two copies; 30% and 50% used.
	if out != "USED_MB=4096\nTOTAL_MB=10240\nFREE_MB=6144" {
		t.Fatalf("got %q", out)
	}
}

func TestImportCreatesWritesAndReleases(t *testing.T) {
	b := newFakeBackend()
	d, sh, _ := testDriver(b, "")
	action := `<DS_DRIVER_ACTION_DATA><IMAGE><ID>4</ID><PATH>/var/tmp/deb.qcow2</PATH><SIZE>3072</SIZE></IMAGE>
<DATASTORE><ID>101</ID><TEMPLATE><HAIFY_POOL>pool0</HAIFY_POOL></TEMPLATE></DATASTORE></DS_DRIVER_ACTION_DATA>`
	out, err := d.DSCp(context.Background(), b64(action))
	if err != nil {
		t.Fatal(err)
	}
	if out != "one-img-4 raw" {
		t.Fatalf("got %q", out)
	}
	want := []string{"create one-img-4 3221225472 ", "promote one-img-4 n1", "demote one-img-4 n1"}
	if !slices.Equal(b.calls, want) {
		t.Fatalf("calls %v", b.calls)
	}
	if len(sh.local) != 1 || !strings.Contains(sh.local[0], "downloader.sh") || !strings.Contains(sh.local[0], "ssh '10.0.0.1'") {
		t.Fatalf("download %v", sh.local)
	}
	if len(sh.remote) != 1 || !strings.Contains(sh.remote[0], "qemu-img convert -n -O raw") {
		t.Fatalf("convert %v", sh.remote)
	}
}

func TestFailedImportLeavesNoResource(t *testing.T) {
	b := newFakeBackend()
	d, sh, _ := testDriver(b, "")
	sh.fail = true
	action := `<DS_DRIVER_ACTION_DATA><IMAGE><ID>4</ID><PATH>/x</PATH><SIZE>10</SIZE></IMAGE>
<DATASTORE><ID>101</ID><TEMPLATE><HAIFY_POOL>pool0</HAIFY_POOL></TEMPLATE></DATASTORE></DS_DRIVER_ACTION_DATA>`
	if _, err := d.DSCp(context.Background(), b64(action)); err == nil {
		t.Fatal("expected the failed write to fail the import")
	}
	if _, ok := b.resources["one-img-4"]; ok {
		t.Fatal("the half-written image is deleted")
	}
}

func TestLCMStates(t *testing.T) {
	if !leavingHost(lcmEpilogStop) || leavingHost(lcmPrologResume) {
		t.Fatal("leavingHost")
	}
	if !sourceGone(lcmPrologMigrateUnknown) || sourceGone(lcmEpilogStop) {
		t.Fatal("sourceGone")
	}
}
