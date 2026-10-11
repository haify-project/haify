package controller

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func loadInventory(t *testing.T, file string) *blockInventory {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + file)
	require.NoError(t, err)
	return parseBlockInventory(string(raw))
}

func diskByDevice(disks []NodeDisk, dev string) *NodeDisk {
	for i := range disks {
		if disks[i].Device == dev {
			return &disks[i]
		}
	}
	return nil
}

// A real Ubuntu node on Proxmox: a system disk with LVM root, two pool disks
// under DRBD, and one empty disk nobody has used.
func TestNodeDisksOnARealNode(t *testing.T) {
	disks := loadInventory(t, "blockdevices_orange.txt").disks("orange1")

	sda := diskByDevice(disks, "/dev/sda")
	require.NotNil(t, sda)
	assert.Equal(t, DiskBusy, sda.State)
	assert.Equal(t, "sda2 mounted at /boot", sda.Detail)

	assert.Nil(t, diskByDevice(disks, "/dev/sda1"), "a 1 MiB BIOS boot partition is not offered")
	assert.Nil(t, diskByDevice(disks, "/dev/sda3"), "a partition in the root VG is not listed on its own")

	sdb := diskByDevice(disks, "/dev/sdb")
	require.NotNil(t, sdb)
	assert.Equal(t, DiskFree, sdb.State)
	assert.Equal(t, uint64(20<<30), sdb.SizeBytes)
	assert.Equal(t, "QEMU HARDDISK", sdb.Model, "raw-mode escapes are decoded")
	assert.Equal(t, "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi1", sdb.ByID)

	for _, dev := range []string{"/dev/sdc", "/dev/sdd"} {
		d := diskByDevice(disks, dev)
		require.NotNil(t, d)
		assert.Equal(t, DiskPool, d.State, dev)
		assert.Equal(t, "haify_vg0", d.Pool, dev)
		assert.Equal(t, "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_"+map[string]string{"/dev/sdc": "drive-scsi2", "/dev/sdd": "drive-scsi3"}[dev], d.ByID,
			"the disk's own id wins over the LVM PV uuid link")
	}
	for _, d := range disks {
		assert.NotContains(t, d.Device, "drbd")
		assert.NotContains(t, d.Device, "mapper")
	}
}

func TestNodeDisksOnALimaVM(t *testing.T) {
	disks := loadInventory(t, "blockdevices_lima.txt").disks("node-a")
	vdc := diskByDevice(disks, "/dev/vdc")
	assert.Nil(t, vdc, "the 268 MiB cloud-init seed is below the pool minimum")
	vda := diskByDevice(disks, "/dev/vda")
	require.NotNil(t, vda)
	assert.Equal(t, DiskBusy, vda.State)
	assert.Equal(t, "vda1 mounted at /", vda.Detail)
	assert.Equal(t, DiskPool, diskByDevice(disks, "/dev/vdb").State)
}

func TestCheckPoolDisk(t *testing.T) {
	inv := loadInventory(t, "blockdevices_orange.txt")

	d, err := inv.checkPoolDisk("orange1", "/dev/sdb", false)
	require.NoError(t, err)
	assert.Equal(t, "/dev/sdb", d.Device)

	d, err = inv.checkPoolDisk("orange1", "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi1", false)
	require.NoError(t, err, "a by-id link resolves to its disk")
	assert.Equal(t, "/dev/sdb", d.Device)

	_, err = inv.checkPoolDisk("orange1", "/dev/sdc", true)
	assert.ErrorContains(t, err, "already in pool haify_vg0")

	_, err = inv.checkPoolDisk("orange1", "/dev/sda", true)
	assert.ErrorContains(t, err, "in use: sda2 mounted at /boot", "--wipe never reaches a disk in use")

	_, err = inv.checkPoolDisk("orange1", "/dev/sdx", false)
	assert.ErrorContains(t, err, "/dev/sdx is not a disk on orange1; empty disks there: /dev/sdb (20.0 GiB)")

	_, err = inv.checkPoolDisk("orange1", "/dev/mapper/haify_vg0-pgha_data", true)
	assert.ErrorContains(t, err, "is a lvm, not a disk")

	_, err = inv.checkPoolDisk("orange1", "/dev/sda1", false)
	assert.ErrorContains(t, err, "a pool disk must be at least 1 GiB")
}

const oldDisksListing = `D|/dev/sde||107374182400|disk|0|0|sata||OLD\x20SSD|S1
B|/dev/sde||gpt
D|/dev/sde1|/dev/sde|107373133824|part|0|0|sata|||
B|/dev/sde1|ntfs|
D|/dev/sdf||107374182400|disk|0|1|sata||X|S2
B|/dev/sdf|zfs_member|
D|/dev/sdg||107374182400|disk|0|1|sata||X|S3
B|/dev/sdg|LVM2_member|
D|/dev/sdh||107374182400|disk|0|1|sata||X|S4
B|/dev/sdh|linux_raid_member|
D|/dev/md0|/dev/sdh|107373182400|raid1|0|1||||
D|/dev/sdi||107374182400|disk|0|1|sata||X|S5
B|/dev/sdi||
D|/dev/sdj||107374182400|disk|0|1|sata||X|S6
B|/dev/sdj|ext4|
D|/dev/sdk||107374182400|disk|0|1|sata||X|S7
B|/dev/sdk|LVM2_member|
D|/dev/sdl||107374182400|disk|0|1|sata||X|S8
B|/dev/sdl||
P|/dev/sdk|
Z|/dev/sdl1|tank
K|/dev/sdj
I|/dev/disk/by-id/wwn-0x5000c500a1|/dev/sdi
I|/dev/disk/by-id/ata-X_S5|/dev/sdi
`

func TestCheckPoolDiskSignatures(t *testing.T) {
	inv := parseBlockInventory(oldDisksListing)
	disks := inv.disks("n1")

	sde := diskByDevice(disks, "/dev/sde")
	require.NotNil(t, sde)
	assert.Equal(t, DiskWipe, sde.State)
	assert.Equal(t, "a gpt partition table with sde1 (ntfs)", sde.Detail)
	assert.NotNil(t, diskByDevice(disks, "/dev/sde1"), "a partition holding only old data is offered too")

	_, err := inv.checkPoolDisk("n1", "/dev/sde", false)
	assert.ErrorContains(t, err, "holds a gpt partition table with sde1 (ntfs). If nothing on it is needed, retry with --wipe")
	d, err := inv.checkPoolDisk("n1", "/dev/sde", true)
	require.NoError(t, err)
	assert.Equal(t, []string{"/dev/sde1"}, d.partitions, "partitions are erased before the disk")

	assert.Contains(t, diskByDevice(disks, "/dev/sdf").Detail, "pool that is not imported")
	assert.Contains(t, diskByDevice(disks, "/dev/sdg").Detail, "LVM label of a volume group this node does not have")
	assert.Equal(t, DiskBusy, diskByDevice(disks, "/dev/sdh").State, "an assembled array holds its members")
	assert.Equal(t, "used by md0", diskByDevice(disks, "/dev/sdh").Detail)

	sdi := diskByDevice(disks, "/dev/sdi")
	assert.Equal(t, DiskFree, sdi.State)
	assert.Equal(t, "/dev/disk/by-id/wwn-0x5000c500a1", sdi.ByID, "the WWN names the disk")

	assert.Equal(t, "backing disk of a DRBD resource", diskByDevice(disks, "/dev/sdj").Detail)
	assert.Equal(t, DiskFree, diskByDevice(disks, "/dev/sdk").State, "a PV in no group holds nothing")
}

// A ZFS whole-disk vdev lives on partition 1; the disk is busy through it.
func TestCheckPoolDiskZFSPartition(t *testing.T) {
	inv := parseBlockInventory(oldDisksListing + "D|/dev/sdl1|/dev/sdl|107373133824|part|0|1||||\n")
	_, err := inv.checkPoolDisk("n1", "/dev/sdl", true)
	assert.ErrorContains(t, err, "in use: in ZFS pool tank")
}

// A Proxmox host's system disk: BIOS boot, the EFI system partition (vfat,
// not mounted — proxmox-boot-tool mounts it only to update it) and the root
// VG. The ESP looked like a 1 GiB partition holding only an old filesystem.
func TestFirmwarePartitionsAreNeverOffered(t *testing.T) {
	listing := `D|/dev/sda||256060514304|disk|0|0|sata||SPCC\x20Solid\x20State\x20Disk|AA1|
B|/dev/sda||gpt
D|/dev/sda1|/dev/sda|1031168|part|0|0|sata||||21686148-6449-6e6f-744e-656564454649
B|/dev/sda1||
D|/dev/sda2|/dev/sda|1073741824|part|0|0|sata||||c12a7328-f81f-11d2-ba4b-00a0c93ec93b
B|/dev/sda2|vfat|
D|/dev/sda3|/dev/sda|254985813504|part|0|0|sata||||e6d6d379-f507-44c2-a23c-238f2a3df928
B|/dev/sda3|LVM2_member|
P|/dev/sda3|pve
`
	inv := parseBlockInventory(listing)
	disks := inv.disks("hp")
	assert.Nil(t, diskByDevice(disks, "/dev/sda2"))
	assert.Equal(t, "sda1 holds a BIOS boot partition", diskByDevice(disks, "/dev/sda").Detail)
	_, err := inv.checkPoolDisk("hp", "/dev/sda2", true)
	assert.ErrorContains(t, err, "in use: the EFI system partition")
}
