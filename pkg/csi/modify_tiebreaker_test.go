package csi

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestModifyVolumeShrinkToTwoPutsTheTiebreakerBack(t *testing.T) {
	// Grown from two to three, the tiebreaker n3 became a diskful replica;
	// shrinking back must leave a third vote, on the node it removed.
	f := newModifyFake("n1", "n2", "n3")
	f.seedVolume("pvc_a", []string{"n1", "n2", "n3"}, map[string]string{placementTopologyLabel: topologyAnywhere})
	require.NoError(t, modifyVolume(t, f, "pvc_a", map[string]string{"replicas": "2"}))
	require.Len(t, f.removed, 1)
	removed := f.removed[0][len("pvc_a/"):]
	assert.Equal(t, []string{"pvc_a/" + removed}, f.tiebreakers)
	assert.Equal(t, []string{removed}, f.resources["pvc_a"].DisklessNodes)

	require.NoError(t, modifyVolume(t, f, "pvc_a", map[string]string{"replicas": "2"}))
	assert.Len(t, f.tiebreakers, 1, "idempotent once the tiebreaker is there")
}

func TestModifyVolumeAtTwoWithoutTiebreakerAddsOne(t *testing.T) {
	// A shrink whose tiebreaker step failed is retried at the wanted count.
	f := newModifyFake("n1", "n2", "n3")
	f.seedVolume("pvc_a", []string{"n1", "n2"}, map[string]string{placementTopologyLabel: topologyAnywhere})
	require.NoError(t, modifyVolume(t, f, "pvc_a", map[string]string{"replicas": "2"}))
	assert.Empty(t, f.removed)
	assert.Equal(t, []string{"pvc_a/n3"}, f.tiebreakers)
}

func TestModifyVolumeShrinkToThreeNeedsNoTiebreaker(t *testing.T) {
	f := newModifyFake("n1", "n2", "n3", "n4")
	f.seedVolume("pvc_a", []string{"n1", "n2", "n3", "n4"}, map[string]string{placementTopologyLabel: topologyAnywhere})
	require.NoError(t, modifyVolume(t, f, "pvc_a", map[string]string{"replicas": "3"}))
	assert.Len(t, f.removed, 1)
	assert.Empty(t, f.tiebreakers)
}

func TestModifyVolumeKeepsAnExistingTiebreaker(t *testing.T) {
	f := newModifyFake("n1", "n2", "n3")
	r := f.seedVolume("pvc_a", []string{"n1", "n2"}, map[string]string{placementTopologyLabel: topologyAnywhere})
	r.DisklessNodes = []string{"n3"}
	require.NoError(t, modifyVolume(t, f, "pvc_a", map[string]string{"replicas": "2"}))
	assert.Empty(t, f.tiebreakers)
}
