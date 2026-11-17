package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haify-project/haify/pkg/deployment"
	"github.com/haify-project/haify/pkg/event"
)

// A running guest is resized on the node it runs on, by the disk's source.
func TestGuestToldOfResize(t *testing.T) {
	n := &guestNode{resize: "target=vdb\nbytes=21474836480\nguest=resized\n"}
	ctrl := guestFixture(t, n)
	note := ctrl.resources.TellGuestOfResize(context.Background(), "web1-data", 0, 20<<30)
	assert.Equal(t, "guest web1 on 10.0.0.1 now sees the new size on vdb", note)
	j := n.joined()
	assert.Contains(t, j, "10.0.0.1: RESIZE")
	assert.NotContains(t, j, "10.0.0.2: RESIZE")
	assert.Contains(t, j, "src='/dev/drbd/by-res/web1-data/0'")
	assert.Contains(t, j, "want=21474836480")
	assert.Equal(t, "Volume resized successfully; "+note, resizedMessage(note))
	assert.Equal(t, "Volume resized successfully", resizedMessage(""))
}

// What stops the guest from being told never fails the resize; it is said,
// and raised as an event only when a running guest still sees the old size.
func TestGuestNotToldOfResize(t *testing.T) {
	cases := []struct {
		out, want string
		event     bool
	}{
		{"guest=not-running\ndetail=shut off\n", "guest web1 is not running on 10.0.0.1; it sees the new size when it next starts", false},
		{"guest=no-virsh\n", "10.0.0.1 has no virsh, so guest web1 was not told; guest web1 sees the new size when it next starts", false},
		{"guest=undefined\n", "10.0.0.1 has no guest named web1", false},
		{"guest=no-disk\n", "guest web1 on 10.0.0.1 has no disk on /dev/drbd/by-res/web1-data/0", true},
		{"target=vdb\nguest=failed\ndetail=error: internal error\n", "was not told the new size (error: internal error); run `virsh blockresize web1", true},
		{"", "was not told the new size (the node did not answer)", true},
	}
	for _, c := range cases {
		n := &guestNode{resize: c.out}
		ctrl := guestFixture(t, n)
		ctrl.events = event.NewBus(10)
		events, stop := ctrl.events.Subscribe(event.Filter{})
		note := ctrl.resources.TellGuestOfResize(context.Background(), "web1-data", 0, 20<<30)
		assert.Contains(t, note, c.want, c.out)
		select {
		case e := <-events:
			assert.True(t, c.event, "unexpected event for %q: %s", c.out, e.Message)
			assert.Equal(t, event.TypeGuestResizeFailed, e.Type)
			assert.Equal(t, "web1-data", e.Resource)
		default:
			assert.False(t, c.event, "no event for %q", c.out)
		}
		stop()
	}
}

// A guest that is off, and a resource of no guest.
func TestGuestResizeNothingToTell(t *testing.T) {
	n := &guestNode{}
	ctrl := guestFixture(t, n)
	assert.Empty(t, ctrl.resources.TellGuestOfResize(context.Background(), "plain", 0, 1<<30))
	assert.NotContains(t, n.joined(), "RESIZE")

	dep := ctrl.deployment.(*fakeDeploymentClient)
	dep.drbdStatusFunc = func(_ context.Context, hosts []string, resource string) (*deployment.ExecResult, error) {
		return successExecResult(hosts, resource+" role:Secondary\n"), nil
	}
	note := ctrl.resources.TellGuestOfResize(context.Background(), "web1-data", 0, 1<<30)
	require.Equal(t, "guest web1 is not running; it sees the new size when it next starts", note)
	assert.NotContains(t, n.joined(), "RESIZE")
}
