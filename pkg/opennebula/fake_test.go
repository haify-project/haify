package opennebula

import (
	"context"
	"encoding/base64"
	"fmt"
	"slices"
	"strings"

	haifypb "github.com/haify-project/haify/api/proto/v1"
)

// fakeBackend is a controller holding resources in memory and recording
// every call that changes something.
type fakeBackend struct {
	resources map[string]*haifypb.ResourceInfo
	primary   map[string][]string // resource -> nodes holding it Primary
	snapshots map[string][]string
	calls     []string
	fail      map[string]error // call name -> error
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{resources: map[string]*haifypb.ResourceInfo{}, primary: map[string][]string{},
		snapshots: map[string][]string{}, fail: map[string]error{}}
}

func (f *fakeBackend) record(format string, args ...any) error {
	call := fmt.Sprintf(format, args...)
	f.calls = append(f.calls, call)
	return f.fail[strings.Fields(call)[0]]
}

func (f *fakeBackend) add(name string, nodes ...string) *haifypb.ResourceInfo {
	r := &haifypb.ResourceInfo{Name: name, Nodes: nodes, DisklessNodes: []string{"fe"},
		Volumes: []*haifypb.VolumeInfo{{SizeBytes: 3 << 30, Pool: "haify_pool0"}}, Labels: map[string]string{}}
	f.resources[name] = r
	return r
}

func (f *fakeBackend) GetResource(_ context.Context, name string) (*haifypb.ResourceInfo, error) {
	if r, ok := f.resources[name]; ok {
		return r, nil
	}
	return nil, fmt.Errorf("resource not found: %s", name)
}

func (f *fakeBackend) CreateResourceRequest(_ context.Context, req *haifypb.CreateResourceRequest) error {
	if err := f.record("create %s %d %s", req.Name, req.SizeBytes, strings.Join(req.Nodes, ",")); err != nil {
		return err
	}
	r := f.add(req.Name, req.Nodes...)
	if len(req.Nodes) == 0 {
		r.Nodes = []string{"n1", "n2"}
	}
	r.Labels = req.Labels
	return nil
}

func (f *fakeBackend) DeleteResource(_ context.Context, name string) error {
	delete(f.resources, name)
	return f.record("delete %s", name)
}

func (f *fakeBackend) AttachDisklessClient(_ context.Context, resource, node string) error {
	f.resources[resource].DisklessClients = append(f.resources[resource].DisklessClients, node)
	return f.record("attach %s %s", resource, node)
}

func (f *fakeBackend) DetachDisklessClient(_ context.Context, resource, node string) error {
	r := f.resources[resource]
	r.DisklessClients = slices.DeleteFunc(r.DisklessClients, func(n string) bool { return n == node })
	return f.record("detach %s %s", resource, node)
}

func (f *fakeBackend) PromoteForNode(_ context.Context, resource, node string) error {
	if err := f.record("promote %s %s", resource, node); err != nil {
		return err
	}
	f.primary[resource] = append(f.primary[resource], node)
	return nil
}

func (f *fakeBackend) SetSecondary(_ context.Context, resource, node string) error {
	if err := f.record("demote %s %s", resource, node); err != nil {
		return err
	}
	f.primary[resource] = slices.DeleteFunc(f.primary[resource], func(n string) bool { return n == node })
	return nil
}

func (f *fakeBackend) SetDualPrimary(_ context.Context, resource string, enable bool) error {
	return f.record("dual-primary %s %v", resource, enable)
}

func (f *fakeBackend) ResourceStatus(_ context.Context, name string) (*haifypb.ResourceStatus, error) {
	st := &haifypb.ResourceStatus{Name: name, NodeStates: map[string]*haifypb.NodeResourceState{}}
	for _, n := range f.resources[name].GetNodes() {
		st.NodeStates[n] = &haifypb.NodeResourceState{Role: "Secondary"}
	}
	for _, n := range f.primary[name] {
		st.NodeStates[n] = &haifypb.NodeResourceState{Role: "Primary"}
	}
	return st, nil
}

func (f *fakeBackend) CreateResourceSnapshot(_ context.Context, resource, name string) error {
	f.snapshots[resource] = append(f.snapshots[resource], name)
	return f.record("snapshot %s %s", resource, name)
}

func (f *fakeBackend) ListResourceSnapshots(_ context.Context, resource string) ([]string, error) {
	return f.snapshots[resource], nil
}

func (f *fakeBackend) RollbackResourceSnapshot(_ context.Context, resource, name string) error {
	return f.record("rollback %s %s", resource, name)
}

func (f *fakeBackend) DeleteResourceSnapshot(_ context.Context, resource, name string) error {
	f.snapshots[resource] = slices.DeleteFunc(f.snapshots[resource], func(s string) bool { return s == name })
	return f.record("delete-snapshot %s %s", resource, name)
}

func (f *fakeBackend) PopulateVolumeFromSnapshot(_ context.Context, resource string, _ uint32,
	src *haifypb.PopulateSnapshotSource, node string) (uint64, error) {
	return 0, f.record("populate %s from %s@%s on %s", resource, src.GetResource(), src.GetName(), node)
}

func (f *fakeBackend) ResizeVolumeBytes(_ context.Context, resource string, _ uint32, size uint64) error {
	return f.record("resize %s %d", resource, size)
}

func (f *fakeBackend) SetResourceLabels(_ context.Context, resource string, set map[string]string,
	remove []string) (map[string]string, error) {
	r := f.resources[resource]
	for k, v := range set {
		r.Labels[k] = v
	}
	for _, k := range remove {
		delete(r.Labels, k)
	}
	return r.Labels, nil
}

func (f *fakeBackend) ListNodes(context.Context) ([]*haifypb.NodeInfo, error) {
	return []*haifypb.NodeInfo{{Name: "n1", Address: "10.0.0.1"}, {Name: "n2", Address: "10.0.0.2"},
		{Name: "n3", Address: "10.0.0.3"}, {Name: "fe", Address: "10.0.0.9"}}, nil
}

func (f *fakeBackend) ListPools(context.Context) ([]*haifypb.PoolInfo, error) {
	return []*haifypb.PoolInfo{
		{Name: "haify_pool0", Node: "n1", ThinPoolLv: "thin", ThinSizeBytes: 10 << 30, ThinDataPercent: 30},
		{Name: "haify_pool0", Node: "n2", ThinPoolLv: "thin", ThinSizeBytes: 10 << 30, ThinDataPercent: 50},
		{Name: "haify_other", Node: "n3", TotalGb: 100, FreeGb: 100},
	}, nil
}

func (f *fakeBackend) CreateFilesystem(_ context.Context, resource string, _ uint32, node, fstype string) error {
	return f.record("mkfs %s %s %s", resource, node, fstype)
}

// fakeOne answers onevm and onedatastore from fixed XML.
type fakeOne struct {
	vms map[string]string
	ds  map[string]string
}

func (o fakeOne) Output(_ context.Context, name string, args ...string) ([]byte, error) {
	id := args[len(args)-1]
	var m map[string]string
	switch name {
	case "onevm":
		m = o.vms
	case "onedatastore":
		m = o.ds
	}
	if x, ok := m[id]; ok {
		return []byte(x), nil
	}
	return nil, fmt.Errorf("%s %s: not found", name, id)
}

// fakeShell records the scripts it is asked to run.
type fakeShell struct {
	remote []string
	local  []string
	fail   bool
}

func (s *fakeShell) Remote(_ context.Context, host, script string) (string, error) {
	s.remote = append(s.remote, host+": "+script)
	if s.fail {
		return "boom", fmt.Errorf("exit status 1")
	}
	return "", nil
}

func (s *fakeShell) Local(_ context.Context, _ []string, script string) (string, error) {
	s.local = append(s.local, script)
	return "", nil
}

const dsXML = `<DATASTORE><ID>101</ID><NAME>haify_images</NAME><TEMPLATE>
<HAIFY_POOL><![CDATA[pool0]]></HAIFY_POOL><HAIFY_REPLICAS><![CDATA[2]]></HAIFY_REPLICAS></TEMPLATE></DATASTORE>`

func vmXML(id, lcm int, disks ...string) string {
	return fmt.Sprintf("<VM><ID>%d</ID><LCM_STATE>%d</LCM_STATE><TEMPLATE>%s</TEMPLATE></VM>", id, lcm,
		strings.Join(disks, ""))
}

func diskXML(id int, source string, persistent bool, sizeMB int) string {
	p, c := "NO", "YES"
	if persistent {
		p, c = "YES", "NO"
	}
	return fmt.Sprintf("<DISK><DISK_ID>%d</DISK_ID><SOURCE>%s</SOURCE><PERSISTENT>%s</PERSISTENT><CLONE>%s</CLONE>"+
		"<TM_MAD>haify</TM_MAD><DATASTORE_ID>101</DATASTORE_ID><SIZE>%d</SIZE></DISK>", id, source, p, c, sizeMB)
}

// testDriver is a driver on the fakes, with the VM's XML for vm id 7.
func testDriver(b *fakeBackend, vm string) (*Driver, *fakeShell, *[]string) {
	sh := &fakeShell{}
	var delegated []string
	d := &Driver{
		Connect: func(string) (Backend, error) { return b, nil },
		One:     fakeOne{vms: map[string]string{"7": vm}, ds: map[string]string{"101": dsXML}},
		Shell:   sh,
		Remotes: "/var/lib/one/remotes",
		Delegate: func(_ context.Context, tm, action string, args []string, _ []byte) error {
			delegated = append(delegated, tm+"/"+action+" "+strings.Join(args, " "))
			return nil
		},
	}
	return d, sh, &delegated
}

func b64(s string) []byte { return []byte(base64.StdEncoding.EncodeToString([]byte(s))) }
