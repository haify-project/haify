package opennebula

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// dsContext is one datastore action: its data, the controller and the
// datastore's settings.
type dsContext struct {
	a        *DSAction
	b        Backend
	prefix   string
	pool     string
	replicas uint32
	data64   string // the action as received, for downloader.sh
}

func (d *Driver) dsContext(stdin []byte) (*dsContext, error) {
	a, err := ParseDSAction(stdin)
	if err != nil {
		return nil, err
	}
	b, err := d.backendFor(&a.Datastore)
	if err != nil {
		return nil, err
	}
	replicas, err := replicasOf(&a.Datastore)
	if err != nil {
		return nil, err
	}
	c := &dsContext{a: a, b: b, prefix: prefixOf(&a.Datastore), pool: a.Datastore.Template.Get(AttrPool),
		replicas: replicas, data64: strings.TrimSpace(string(stdin))}
	zap.L().Debug("Driver.dsContext: read action", zap.Int("image_id", a.Image.ID), zap.Int("ds_id", a.Datastore.ID),
		zap.String("pool", c.pool), zap.Uint32("replicas", replicas), zap.String("prefix", c.prefix))
	return c, nil
}

// newImage creates the resource of the action's image, empty, of its size.
func (c *dsContext) newImage(ctx context.Context) (string, error) {
	if c.pool == "" {
		return "", fmt.Errorf("the datastore has no %s", AttrPool)
	}
	if c.a.Image.Size <= 0 {
		return "", fmt.Errorf("image %d has no size", c.a.Image.ID)
	}
	name := ImageResource(c.prefix, c.a.Image.ID)
	req := createRequest(name, uint64(c.a.Image.Size)*uint64(mib), nil, c.labels())
	req.Pool, req.Replicas = c.pool, c.replicas
	start := time.Now()
	err := c.b.CreateResourceRequest(ctx, req)
	zap.L().Debug("dsContext.newImage: created", zap.String("resource", name), zap.Int64("size_mb", c.a.Image.Size),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return "", fmt.Errorf("creating %s: %w", name, err)
	}
	return name, nil
}

func (c *dsContext) labels() map[string]string {
	return map[string]string{LabelManaged: managedValue, LabelImage: strconv.Itoa(c.a.Image.ID),
		LabelDatastore: strconv.Itoa(c.a.Datastore.ID)}
}

// writer picks the host that writes a new image's data, and the Haify node
// on it: the datastore's first BRIDGE_LIST host, else the image's first
// replica. OpenNebula's driver user reaches it over SSH.
func (c *dsContext) writer(ctx context.Context, resource string) (host, node string, err error) {
	if bridges := strings.Fields(c.a.Datastore.Template.Get(attrBridges)); len(bridges) > 0 {
		node, err = nodeFor(ctx, c.b, bridges[0])
		return bridges[0], node, err
	}
	r, err := findResource(ctx, c.b, resource)
	if err != nil || r == nil || len(r.GetNodes()) == 0 {
		return "", "", fmt.Errorf("%s has no replica to write from: %v", resource, err)
	}
	node = r.GetNodes()[0]
	nodes, err := c.b.ListNodes(ctx)
	if err != nil {
		return "", "", err
	}
	for _, n := range nodes {
		if n.GetName() == node {
			return n.GetAddress(), node, nil
		}
	}
	return node, node, nil
}

// DSCp imports an image: the image file (any URL downloader.sh takes,
// qcow2 or raw) is converted onto a new resource from a writer host.
// Prints "<source> raw".
func (d *Driver) DSCp(ctx context.Context, stdin []byte) (string, error) {
	log := zap.L()
	c, err := d.dsContext(stdin)
	if err != nil {
		return "", err
	}
	name, err := c.newImage(ctx)
	if err != nil {
		return "", err
	}
	if err := d.fill(ctx, c, name); err != nil {
		if derr := c.b.DeleteResource(ctx, name); derr != nil {
			log.Debug("Driver.DSCp: dropping the failed image failed", zap.String("resource", name), zap.Error(derr))
		}
		return "", err
	}
	return name + " raw", nil
}

// fill writes the action's image file onto resource.
func (d *Driver) fill(ctx context.Context, c *dsContext, resource string) error {
	log := zap.L()
	host, node, err := c.writer(ctx, resource)
	if err != nil {
		return err
	}
	if err := ensureMember(ctx, c.b, resource, node); err != nil {
		return err
	}
	defer leave(ctx, c.b, resource, node)
	if err := c.b.PromoteForNode(ctx, resource, node); err != nil {
		return fmt.Errorf("making %s Primary on %s to write it: %w", resource, node, err)
	}
	defer func() {
		err := c.b.SetSecondary(ctx, resource, node)
		log.Debug("Driver.fill: demoted the writer", zap.String("resource", resource), zap.String("node", node), zap.Error(err))
	}()
	tmp := fmt.Sprintf("/var/tmp/haify-%s.%d", resource, time.Now().UnixNano())
	decomp := "--nodecomp"
	if !yes(c.a.Datastore.Template.Get("NO_DECOMPRESS")) {
		decomp = ""
	}
	download := fmt.Sprintf(`set -o pipefail
%s %s %s - | ssh %s "cat > %s"`, shq(filepath.Join(d.Remotes, "datastore", "downloader.sh")), decomp,
		shq(c.a.Image.Path), shq(host), tmp)
	start := time.Now()
	out, err := d.Shell.Local(ctx, []string{"DRV_ACTION=" + c.data64}, download)
	log.Debug("Driver.fill: downloaded", zap.String("resource", resource), zap.String("writer", host),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		_, _ = d.Shell.Remote(ctx, host, "rm -f "+tmp)
		return fmt.Errorf("downloading %s to %s: %v: %s", c.a.Image.Path, host, err, strings.TrimSpace(out))
	}
	convert := fmt.Sprintf(`set -e
trap 'rm -f %[1]s' EXIT
for i in $(seq %[3]d); do [ -e %[2]s ] && break; sleep 0.5; done
qemu-img convert -n -O raw %[1]s %[2]s
sync
`, tmp, shq(devicePath(resource)), deviceWait*2)
	start = time.Now()
	out, err = d.Shell.Remote(ctx, host, convert)
	log.Debug("Driver.fill: converted", zap.String("resource", resource), zap.String("writer", host),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("writing the image onto %s on %s: %v: %s", resource, host, err, strings.TrimSpace(out))
	}
	return nil
}

// DSMkfs makes an empty image (a datablock), formatted when it has an FS.
// Prints the source.
func (d *Driver) DSMkfs(ctx context.Context, stdin []byte) (string, error) {
	log := zap.L()
	c, err := d.dsContext(stdin)
	if err != nil {
		return "", err
	}
	name, err := c.newImage(ctx)
	if err != nil {
		return "", err
	}
	fs := strings.ToLower(c.a.Image.FS)
	if fs == "" || fs == "raw" || fs == "save_as" {
		return name, nil
	}
	_, node, err := c.writer(ctx, name)
	if err == nil {
		err = ensureMember(ctx, c.b, name, node)
	}
	if err == nil {
		if err = c.b.PromoteForNode(ctx, name, node); err == nil {
			err = d.mkfs(ctx, c.b, name, node, fs)
			if derr := c.b.SetSecondary(ctx, name, node); derr != nil && err == nil {
				err = derr
			}
		}
		leave(ctx, c.b, name, node)
	}
	log.Debug("Driver.DSMkfs: formatted", zap.String("resource", name), zap.String("fs", fs), zap.Error(err))
	if err != nil {
		if derr := c.b.DeleteResource(ctx, name); derr != nil {
			log.Debug("Driver.DSMkfs: dropping the failed image failed", zap.String("resource", name), zap.Error(derr))
		}
		return "", fmt.Errorf("formatting %s as %s: %w", name, fs, err)
	}
	return name, nil
}

// DSRm deletes an image's resource.
func (d *Driver) DSRm(ctx context.Context, stdin []byte) error {
	c, err := d.dsContext(stdin)
	if err != nil {
		return err
	}
	name := c.a.Image.Source
	r, err := findResource(ctx, c.b, name)
	zap.L().Debug("Driver.DSRm: deleting", zap.String("resource", name), zap.Bool("found", r != nil), zap.Error(err))
	if err != nil || r == nil {
		return err
	}
	if err := c.b.DeleteResource(ctx, name); err != nil {
		return fmt.Errorf("deleting %s: %w", name, err)
	}
	return nil
}

// DSClone copies an image into a new one (oneimage clone). Prints
// "<source> raw".
func (d *Driver) DSClone(ctx context.Context, stdin []byte) (string, error) {
	c, err := d.dsContext(stdin)
	if err != nil {
		return "", err
	}
	src, err := findResource(ctx, c.b, c.a.Image.Path)
	if err != nil {
		return "", err
	}
	if src == nil {
		return "", fmt.Errorf("source image %s does not exist", c.a.Image.Path)
	}
	name := ImageResource(c.prefix, c.a.Image.ID)
	size := uint64(c.a.Image.Size) * uint64(mib)
	if s := resourceBytes(src); s > size {
		size = s
	}
	zap.L().Debug("Driver.DSClone: cloning", zap.String("source", src.GetName()), zap.String("resource", name))
	if err := copyResource(ctx, c.b, src, "", name, size, c.labels()); err != nil {
		return "", err
	}
	return name + " raw", nil
}

// DSMonitor reports the datastore's pool: what one more copy-set of data
// can use, so the space of every replica counts once.
func (d *Driver) DSMonitor(ctx context.Context, stdin []byte) (string, error) {
	c, err := d.dsContext(stdin)
	if err != nil {
		return "", err
	}
	pools, err := c.b.ListPools(ctx)
	if err != nil {
		return "", fmt.Errorf("listing pools: %w", err)
	}
	var total, free uint64
	for _, p := range pools {
		if p.GetName() != c.pool && p.GetName() != "haify_"+c.pool {
			continue
		}
		t, f := poolBytes(p)
		total += t
		free += f
	}
	n := uint64(max(c.replicas, 1))
	totalMB, freeMB := total/n/uint64(mib), free/n/uint64(mib)
	zap.L().Debug("Driver.DSMonitor: pool", zap.String("pool", c.pool), zap.Uint64("total_mb", totalMB),
		zap.Uint64("free_mb", freeMB))
	return fmt.Sprintf("USED_MB=%d\nTOTAL_MB=%d\nFREE_MB=%d", totalMB-freeMB, totalMB, freeMB), nil
}

// poolBytes is a pool's size and free space: a thin pool's own, else its
// volume group's.
func poolBytes(p *haifypb.PoolInfo) (total, free uint64) {
	if p.GetThinPoolLv() != "" && p.GetThinSizeBytes() > 0 {
		total = p.GetThinSizeBytes()
		used := uint64(float64(total) * p.GetThinDataPercent() / 100)
		return total, total - min(used, total)
	}
	return p.GetTotalGb() << 30, p.GetFreeGb() << 30
}

// DSSnapshot reverts to, deletes or flattens onto a snapshot of an image
// that no VM uses. Flattening reverts and drops every snapshot.
func (d *Driver) DSSnapshot(ctx context.Context, action string, stdin []byte) error {
	c, err := d.dsContext(stdin)
	if err != nil {
		return err
	}
	name, snap := c.a.Image.Source, snapshotName(c.a.Image.TargetSnapshot)
	start := time.Now()
	switch action {
	case "snap_delete":
		err = c.b.DeleteResourceSnapshot(ctx, name, snap)
	case "snap_revert":
		err = c.b.RollbackResourceSnapshot(ctx, name, snap)
	case "snap_flatten":
		if err = c.b.RollbackResourceSnapshot(ctx, name, snap); err == nil {
			var names []string
			if names, err = c.b.ListResourceSnapshots(ctx, name); err == nil {
				for _, s := range names {
					if strings.HasPrefix(s, "one-snap-") {
						if derr := c.b.DeleteResourceSnapshot(ctx, name, s); derr != nil && err == nil {
							err = derr
						}
					}
				}
			}
		}
	default:
		err = fmt.Errorf("unknown snapshot action %q", action)
	}
	zap.L().Debug("Driver.DSSnapshot: done", zap.String("action", action), zap.String("resource", name),
		zap.String("snapshot", snap), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("%s %s of %s: %w", action, snap, name, err)
	}
	return nil
}

// mkfs formats resource on node through the controller.
func (d *Driver) mkfs(ctx context.Context, b Backend, resource, node, fs string) error {
	if !slices.Contains([]string{"ext4", "ext3", "ext2", "xfs", "btrfs", "vfat"}, fs) {
		return fmt.Errorf("unsupported filesystem %q", fs)
	}
	return b.CreateFilesystem(ctx, resource, 0, node, fs)
}
