package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNormalizeManagedName(t *testing.T) {
	assert.Equal(t, "haify_data-pool", normalizeManagedName("data-pool"))
	assert.Equal(t, "haify_data-pool", normalizeManagedName("haify_data-pool"))
	assert.Equal(t, "", normalizeManagedName(""))
}

func TestNormalizeLVMPoolType(t *testing.T) {
	tests := []struct {
		input    string
		expected string
		wantErr  bool
	}{
		{input: "", expected: "vg"},
		{input: "vg", expected: "vg"},
		{input: "lvm", expected: "vg"},
		{input: "lvm-thin", expected: "thin_pool"},
		{input: "thin-pool", expected: "thin_pool"},
		{input: "thin_pool", expected: "thin_pool"},
		{input: "zfs", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			actual, err := normalizeLVMPoolType(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tt.expected, actual)
		})
	}
}

func TestNormalizeManagedZFSPath(t *testing.T) {
	assert.Equal(t, "haify_tank/app", normalizeManagedZFSPath("tank/app"))
	assert.Equal(t, "haify_tank/app@snap1", normalizeManagedZFSPath("tank/app@snap1"))
	assert.Equal(t, "haify_tank/app@snap1", normalizeManagedZFSPath("haify_tank/app@snap1"))
}

func TestParseByteCount(t *testing.T) {
	value, err := parseByteCount("1073741824B")
	assert.NoError(t, err)
	assert.Equal(t, uint64(1073741824), value)

	value, err = parseByteCount("1073741824.00B")
	assert.NoError(t, err)
	assert.Equal(t, uint64(1073741824), value)
}

func TestParseLVMPoolLine(t *testing.T) {
	name, total, free, pv, ok := parseLVMPoolLine("haify_data-pool|10737418240B|5368709120B")
	assert.True(t, ok)
	assert.Equal(t, "haify_data-pool", name)
	assert.Equal(t, uint64(10737418240), total)
	assert.Equal(t, uint64(5368709120), free)
	assert.Equal(t, "", pv)

	// With the optional pv_name field (vgs -o ...,pv_name).
	name, _, _, pv, ok = parseLVMPoolLine("haify_vg0|10737418240B|5368709120B|/dev/sdc")
	assert.True(t, ok)
	assert.Equal(t, "haify_vg0", name)
	assert.Equal(t, "/dev/sdc", pv)

	_, _, _, _, ok = parseLVMPoolLine("broken")
	assert.False(t, ok)
}

func TestParseZFSSnapshotLine(t *testing.T) {
	name, volume, createdAt, ok := parseZFSSnapshotLine("haify_tank/app@snap1 0B 0B 2026-03-13-23:30")
	assert.True(t, ok)
	assert.Equal(t, "snap1", name)
	assert.Equal(t, "haify_tank/app", volume)
	assert.Equal(t, "2026-03-13-23:30", createdAt)

	_, _, _, ok = parseZFSSnapshotLine("haify_tank/app@snap1 0B")
	assert.False(t, ok)

	// -p output: the creation time is Unix seconds.
	_, _, createdAt, ok = parseZFSSnapshotLine("haify_tank/app@snap1\t0\t12288\t1791768600")
	assert.True(t, ok)
	assert.Equal(t, "2026-10-12 01:30:00 +0000", createdAt)

	// The human form keeps all its words.
	_, _, createdAt, ok = parseZFSSnapshotLine("haify_tank/app@snap1 0B 0B Mon Oct 12 1:30 2026")
	assert.True(t, ok)
	assert.Equal(t, "Mon Oct 12 1:30 2026", createdAt)
}
