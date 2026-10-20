// Package opennebula is the OpenNebula storage driver for Haify: a datastore
// driver (DS_MAD=haify) that keeps every image as a replicated Haify
// resource, and a transfer driver (TM_MAD=haify) that gives a VM's disks to
// the host running it.
//
// The driver decides where a disk exists, and the libvirt qemu hook
// (deploy/libvirt) on each host where it is written: the hook promotes a disk
// when a guest starts or migrates in and demotes it when the guest stops, as
// it does for any guest. So the transfer actions make sure the host holds the
// disk (as a replica or a diskless client) and link it into the VM directory
// as disk.<n>; only a disk attached to or detached from a running guest,
// which libvirt runs no hook for, is promoted and demoted here. The image and
// snapshot actions run through the controller.
//
// OpenNebula runs both drivers on the frontend as oneadmin. They reach hosts
// over oneadmin's SSH, as every OpenNebula driver does, and the controller
// over its gRPC API.
package opennebula

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// Labels on the Haify resources the driver makes.
const (
	// LabelManaged marks a resource whose Primary OpenNebula decides; the
	// controller then keeps promoters off it.
	LabelManaged   = "haify.opennebula/managed-by"
	LabelImage     = "haify.opennebula/image"
	LabelVM        = "haify.opennebula/vm"
	LabelDatastore = "haify.opennebula/datastore"
	managedValue   = "opennebula"
)

// Datastore template attributes the driver reads.
const (
	AttrController = "HAIFY_CONTROLLER"
	AttrPool       = "HAIFY_POOL"
	AttrReplicas   = "HAIFY_REPLICAS"
	AttrPrefix     = "HAIFY_PREFIX"
	attrBridges    = "BRIDGE_LIST"
)

// DefaultController is the controller's gRPC address when a datastore names
// none: the frontend runs it, or its Self-HA floating IP is set there.
const DefaultController = "127.0.0.1:3374"

const (
	mib        = int64(1) << 20
	deviceWait = 60 // seconds for a disk's device to appear on a host
)

// Backend is the part of the Haify client the driver uses.
type Backend interface {
	GetResource(ctx context.Context, name string) (*haifypb.ResourceInfo, error)
	CreateResourceRequest(ctx context.Context, req *haifypb.CreateResourceRequest) error
	DeleteResource(ctx context.Context, name string) error
	AttachDisklessClient(ctx context.Context, resource, node string) error
	DetachDisklessClient(ctx context.Context, resource, node string) error
	PromoteForNode(ctx context.Context, resource, node string) error
	SetSecondary(ctx context.Context, resource, node string) error
	SetDualPrimary(ctx context.Context, resource string, enable bool) error
	ResourceStatus(ctx context.Context, name string) (*haifypb.ResourceStatus, error)
	CreateResourceSnapshot(ctx context.Context, resource, name string) error
	ListResourceSnapshots(ctx context.Context, resource string) ([]string, error)
	RollbackResourceSnapshot(ctx context.Context, resource, name string) error
	DeleteResourceSnapshot(ctx context.Context, resource, name string) error
	PopulateVolumeFromSnapshot(ctx context.Context, resource string, volumeID uint32, src *haifypb.PopulateSnapshotSource, node string) (uint64, error)
	ResizeVolumeBytes(ctx context.Context, resource string, volumeID uint32, sizeBytes uint64) error
	SetResourceLabels(ctx context.Context, resource string, set map[string]string, remove []string) (map[string]string, error)
	ListNodes(ctx context.Context) ([]*haifypb.NodeInfo, error)
	ListPools(ctx context.Context) ([]*haifypb.PoolInfo, error)
	CreateFilesystem(ctx context.Context, resource string, volumeID uint32, node, fstype string) error
}

// Shell runs commands for the driver.
type Shell interface {
	// Remote runs script with bash on host, over SSH.
	Remote(ctx context.Context, host, script string) (string, error)
	// Local runs script with bash on the frontend, with env added.
	Local(ctx context.Context, env []string, script string) (string, error)
}

// Driver runs one datastore or transfer action.
type Driver struct {
	// Connect returns the client for a controller address.
	Connect func(controller string) (Backend, error)
	One     One
	Shell   Shell
	// Remotes is OpenNebula's remotes directory (/var/lib/one/remotes).
	Remotes string
	// Delegate runs another transfer driver's action (the ssh driver's, for
	// the system datastore's files), passing stdin through.
	Delegate func(ctx context.Context, tm, action string, args []string, stdin []byte) error

	backend    Backend
	controller string
}

// backendFor connects to the controller a datastore names, once.
func (d *Driver) backendFor(ds *Datastore) (Backend, error) {
	controller := DefaultController
	if ds != nil && ds.Template.Get(AttrController) != "" {
		controller = ds.Template.Get(AttrController)
	}
	if d.backend != nil && d.controller == controller {
		return d.backend, nil
	}
	zap.L().Debug("Driver.backendFor: connecting", zap.String("controller", controller))
	b, err := d.Connect(controller)
	if err != nil {
		zap.L().Debug("Driver.backendFor: connect failed", zap.String("controller", controller), zap.Error(err))
		return nil, fmt.Errorf("connecting to the Haify controller at %s: %w", controller, err)
	}
	d.backend, d.controller = b, controller
	return b, nil
}

// ImageResource names the resource of image id.
func ImageResource(prefix string, id int) string {
	return fmt.Sprintf("%s-img-%d", prefix, id)
}

var (
	imageResourceRE = regexp.MustCompile(`^(.+)-img-\d+$`)
	imageIDRE       = regexp.MustCompile(`-img-(\d+)$`)
)

// diskResource names the resource behind a VM's disk: the image's own for a
// persistent image, else the VM's private copy of it.
func diskResource(vmID int, disk *Disk) (string, error) {
	if disk == nil {
		return "", fmt.Errorf("no such disk")
	}
	if yes(disk.Persistent) || !yes(disk.Clone) {
		return disk.Source, nil
	}
	m := imageResourceRE.FindStringSubmatch(disk.Source)
	if m == nil {
		zap.L().Debug("diskResource: source is not a Haify image", zap.String("source", disk.Source))
		return "", fmt.Errorf("disk %d's image %q is not a Haify image", disk.ID, disk.Source)
	}
	return fmt.Sprintf("%s-vm-%d-%d", m[1], vmID, disk.ID), nil
}

func snapshotName(id string) string { return "one-snap-" + id }

func devicePath(resource string) string { return "/dev/drbd/by-res/" + resource + "/0" }

func prefixOf(ds *Datastore) string {
	if p := ds.Template.Get(AttrPrefix); p != "" {
		return p
	}
	return "one"
}

func replicasOf(ds *Datastore) (uint32, error) {
	v := ds.Template.Get(AttrReplicas)
	if v == "" {
		return 2, nil
	}
	n, err := strconv.ParseUint(v, 10, 32)
	if err != nil || n < 1 {
		zap.L().Debug("replicasOf: bad value", zap.String("value", v), zap.Error(err))
		return 0, fmt.Errorf("%s=%q is not a number of copies", AttrReplicas, v)
	}
	return uint32(n), nil
}

// findResource returns the resource, or nil when the controller has none of
// that name.
func findResource(ctx context.Context, b Backend, name string) (*haifypb.ResourceInfo, error) {
	start := time.Now()
	r, err := b.GetResource(ctx, name)
	zap.L().Debug("findResource: looked up", zap.String("resource", name), zap.Bool("found", r != nil),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "not found") {
			return nil, nil
		}
		return nil, err
	}
	return r, nil
}

// nodeFor maps an OpenNebula host name to the Haify node on that host: the
// node of that name, of that short name, or with that address.
func nodeFor(ctx context.Context, b Backend, host string) (string, error) {
	nodes, err := b.ListNodes(ctx)
	if err != nil {
		zap.L().Debug("nodeFor: ListNodes failed", zap.String("host", host), zap.Error(err))
		return "", fmt.Errorf("listing Haify nodes: %w", err)
	}
	short := strings.SplitN(host, ".", 2)[0]
	for _, n := range nodes {
		if n.GetName() == host || n.GetName() == short || n.GetAddress() == host {
			zap.L().Debug("nodeFor: matched", zap.String("host", host), zap.String("node", n.GetName()))
			return n.GetName(), nil
		}
	}
	zap.L().Debug("nodeFor: no node on host", zap.String("host", host), zap.Int("nodes", len(nodes)))
	return "", fmt.Errorf("host %s is not a Haify node; register it with "+
		"haify node register --name %s --address <its IP>", host, short)
}

func member(r *haifypb.ResourceInfo, node string) bool {
	return slices.Contains(r.GetNodes(), node) || slices.Contains(r.GetDisklessNodes(), node) ||
		slices.Contains(r.GetDisklessClients(), node)
}

// ensureMember makes node hold resource: a node with no replica joins it as
// a diskless client and reads and writes it over the network.
func ensureMember(ctx context.Context, b Backend, resource, node string) error {
	r, err := findResource(ctx, b, resource)
	if err != nil {
		return err
	}
	if r == nil {
		return fmt.Errorf("resource %s does not exist on the Haify controller", resource)
	}
	if member(r, node) {
		zap.L().Debug("ensureMember: already a member", zap.String("resource", resource), zap.String("node", node))
		return nil
	}
	start := time.Now()
	err = b.AttachDisklessClient(ctx, resource, node)
	zap.L().Debug("ensureMember: attached diskless client", zap.String("resource", resource), zap.String("node", node),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("joining %s from %s: %w", resource, node, err)
	}
	return nil
}

// leave detaches node from resource when it joined as a diskless client.
// Best effort: a client left attached costs nothing and is reused next time.
func leave(ctx context.Context, b Backend, resource, node string) {
	r, err := findResource(ctx, b, resource)
	if err != nil || r == nil || !slices.Contains(r.GetDisklessClients(), node) {
		zap.L().Debug("leave: nothing to detach", zap.String("resource", resource), zap.String("node", node),
			zap.Bool("found", r != nil), zap.Error(err))
		return
	}
	err = b.DetachDisklessClient(ctx, resource, node)
	zap.L().Debug("leave: detached diskless client", zap.String("resource", resource), zap.String("node", node), zap.Error(err))
}

// primaries returns the nodes holding resource Primary.
func primaries(ctx context.Context, b Backend, resource string) ([]string, error) {
	st, err := b.ResourceStatus(ctx, resource)
	if err != nil {
		zap.L().Debug("primaries: ResourceStatus failed", zap.String("resource", resource), zap.Error(err))
		return nil, err
	}
	var out []string
	for node, s := range st.GetNodeStates() {
		if s.GetRole() == "Primary" {
			out = append(out, node)
		}
	}
	slices.Sort(out)
	zap.L().Debug("primaries: read", zap.String("resource", resource), zap.Strings("primary", out))
	return out, nil
}

// linkDisk makes path on host a link to resource's device, once it appears.
func (d *Driver) linkDisk(ctx context.Context, host, resource, path string) error {
	dev := devicePath(resource)
	script := fmt.Sprintf(`set -e
mkdir -p %[1]s
for i in $(seq %[4]d); do [ -e %[2]s ] && break; sleep 0.5; done
[ -e %[2]s ] || { echo "%[2]s did not appear on $(hostname)" >&2; exit 1; }
ln -sfn %[2]s %[3]s
`, shq(pathDir(path)), shq(dev), shq(path), deviceWait*2)
	start := time.Now()
	out, err := d.Shell.Remote(ctx, host, script)
	zap.L().Debug("Driver.linkDisk: linked", zap.String("host", host), zap.String("resource", resource),
		zap.String("path", path), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("linking %s as %s on %s: %v: %s", dev, path, host, err, strings.TrimSpace(out))
	}
	return nil
}

// unlinkDisk removes a disk link on host. Best effort: the host may be the
// one that failed.
func (d *Driver) unlinkDisk(ctx context.Context, host, path string) {
	out, err := d.Shell.Remote(ctx, host, "rm -f "+shq(path))
	zap.L().Debug("Driver.unlinkDisk: removed", zap.String("host", host), zap.String("path", path),
		zap.String("output", strings.TrimSpace(out)), zap.Error(err))
}

func pathDir(p string) string { return Location{Path: p}.Dir() }

// shq quotes s for a POSIX shell.
func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
