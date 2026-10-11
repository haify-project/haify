package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Status is read from the Primary first once it is known, whether the node
// that answered was the Primary itself or a Secondary naming it as a peer.
func TestStatusAskOrderPrefersLastPrimary(t *testing.T) {
	rm := NewResourceManager(newBasicTestController(&fakeDeploymentClient{}))
	hosts := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}
	nodes := []string{"a", "b", "c"}

	assert.Equal(t, []int{0, 1, 2}, rm.statusAskOrder("r", hosts))

	// a answered as a Secondary and names c as Primary.
	rm.rememberPrimaryHost("r", hosts, nodes, 0, "Secondary", map[string]*ResourceNodeState{
		"a": {Role: "Secondary"},
		"c": {Role: "Primary"},
	})
	assert.Equal(t, []int{2, 0, 1}, rm.statusAskOrder("r", hosts))

	// b answered and is Primary itself.
	rm.rememberPrimaryHost("r", hosts, nodes, 1, "Primary", nil)
	assert.Equal(t, []int{1, 0, 2}, rm.statusAskOrder("r", hosts))

	// A remembered host no longer in the list changes nothing.
	assert.Equal(t, []int{0, 1}, rm.statusAskOrder("r", []string{"10.0.0.1", "10.0.0.3"}))
	assert.Equal(t, []int{0, 1, 2}, rm.statusAskOrder("other", hosts))
}
