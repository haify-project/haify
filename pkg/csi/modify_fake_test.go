package csi

import (
	"context"
	"fmt"

	haifypb "github.com/haify-project/haify/api/proto/v1"
)

// modifyFake is the fake backend plus what ControllerModifyVolume needs. It
// keeps the resource's diskful node list and labels in step with the calls,
// the way the controller does, so a second ModifyVolume sees the first's
// result.
type modifyFake struct {
	*fakeBackend
	added, removed []string // "resource/node"
	labelCalls     []map[string]string
	profileCalls   []string // "resource=profile"
	addErr         error
}

func newModifyFake(nodes ...string) *modifyFake {
	return &modifyFake{fakeBackend: newFakeBackend(nodes...)}
}

func (f *modifyFake) AddReplica(_ context.Context, resource, node string) error {
	if f.addErr != nil {
		return f.addErr
	}
	r, ok := f.resources[resource]
	if !ok {
		return fmt.Errorf("resource %q not found", resource)
	}
	f.added = append(f.added, resource+"/"+node)
	r.Nodes = append(append([]string{}, r.Nodes...), node)
	if r.NodeStates == nil {
		r.NodeStates = map[string]*haifypb.NodeResourceState{}
	}
	// Synced at once, and named by its node rather than keyed by it.
	r.NodeStates["host-"+node] = &haifypb.NodeResourceState{Role: "Secondary", DiskState: "UpToDate", Node: node}
	return nil
}

func (f *modifyFake) RemoveReplica(_ context.Context, resource, node string) error {
	r, ok := f.resources[resource]
	if !ok {
		return fmt.Errorf("resource %q not found", resource)
	}
	f.removed = append(f.removed, resource+"/"+node)
	var kept []string
	for _, n := range r.Nodes {
		if n != node {
			kept = append(kept, n)
		}
	}
	r.Nodes = kept
	delete(r.NodeStates, "host-"+node)
	for _, info := range f.nodes {
		if info.GetName() == node {
			delete(r.NodeStates, info.GetAddress())
		}
	}
	return nil
}

func (f *modifyFake) SetResourceLabels(_ context.Context, resource string, set map[string]string, remove []string) (map[string]string, error) {
	r, ok := f.resources[resource]
	if !ok {
		return nil, fmt.Errorf("resource %q not found", resource)
	}
	f.labelCalls = append(f.labelCalls, set)
	if r.Labels == nil {
		r.Labels = map[string]string{}
	}
	for k, v := range set {
		if v == "" {
			delete(r.Labels, k)
			continue
		}
		r.Labels[k] = v
	}
	for _, k := range remove {
		delete(r.Labels, k)
	}
	return r.Labels, nil
}

func (f *modifyFake) SetResourceProfile(_ context.Context, resource, profile string) error {
	r, ok := f.resources[resource]
	if !ok {
		return fmt.Errorf("resource %q not found", resource)
	}
	f.profileCalls = append(f.profileCalls, resource+"="+profile)
	r.Profile = profile
	return nil
}

// seedVolume puts a provisioned volume on nodes into the fake, every replica
// UpToDate, the first one Primary.
func (f *modifyFake) seedVolume(name string, nodes []string, labels map[string]string) *haifypb.ResourceInfo {
	states := map[string]*haifypb.NodeResourceState{}
	for i, n := range nodes {
		role := "Secondary"
		if i == 0 {
			role = "Primary"
		}
		// Keyed by address, as GetResource keys them by DRBD host, with no
		// node name: the driver has to resolve it from the node list.
		addr := n
		for _, info := range f.nodes {
			if info.GetName() == n {
				addr = info.GetAddress()
			}
		}
		states[addr] = &haifypb.NodeResourceState{Role: role, DiskState: "UpToDate"}
	}
	r := &haifypb.ResourceInfo{
		Name:       name,
		Nodes:      append([]string{}, nodes...),
		Labels:     labels,
		NodeStates: states,
		Volumes:    []*haifypb.VolumeInfo{{VolumeId: 0, Device: "/dev/drbd1000", SizeGb: 1, Pool: "haify_vg0"}},
	}
	f.resources[name] = r
	return r
}

var _ volumeModifier = (*modifyFake)(nil)
