package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Auto-diskful is off unless asked for, and a use time shorter than a pod
// restart or a live migration takes is refused rather than obeyed.
func TestSelfHealAutoDiskfulValidation(t *testing.T) {
	off := SelfHealConfig{}
	assert.NoError(t, off.validate())
	assert.False(t, off.DiskfulEnabled())

	on := SelfHealConfig{AutoDiskful: "on", AutoDiskfulAfterMinutes: 60}
	assert.NoError(t, on.validate())
	assert.True(t, on.DiskfulEnabled())
	assert.False(t, on.DiskfulDryRun())

	dry := SelfHealConfig{AutoDiskful: "dry-run", AutoDiskfulAfterMinutes: minAutoDiskfulMinutes}
	assert.NoError(t, dry.validate())
	assert.True(t, dry.DiskfulEnabled())
	assert.True(t, dry.DiskfulDryRun())

	short := SelfHealConfig{AutoDiskful: "on", AutoDiskfulAfterMinutes: 5}
	assert.ErrorContains(t, short.validate(), "auto_diskful_after_minutes")
	assert.NoError(t, (&SelfHealConfig{AutoDiskfulAfterMinutes: 0}).validate(), "not checked while off")

	bad := SelfHealConfig{AutoDiskful: "yes"}
	assert.ErrorContains(t, bad.validate(), "off, dry-run or on")

	// The two loops are switched separately.
	evictOnly := SelfHealConfig{AutoEvict: "on", AfterMinutes: 60}
	assert.NoError(t, evictOnly.validate())
	assert.False(t, evictOnly.DiskfulEnabled())
}
