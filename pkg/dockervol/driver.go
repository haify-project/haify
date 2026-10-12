// Package dockervol is a Docker volume plugin backed by Haify: `docker volume
// create -d haify` makes a replicated volume, and a container that uses it gets
// it on whichever host it runs on — in a Swarm, a service that moves to
// another node finds its data there.
//
// It speaks Docker's volume plugin protocol (JSON over HTTP on a unix socket)
// and does what the CSI node plugin does on mount: join the volume as a
// diskless client when this host holds no replica, promote it here with the
// quorum guard, format it once and mount it.
package dockervol

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/csi"
	"github.com/haify-project/haify/pkg/util"
	"go.uber.org/zap"
)

// Labels on the Haify resource behind each Docker volume.
const (
	LabelVolume = "haify.docker/volume"
	LabelFS     = "haify.docker/fs"
	resPrefix   = "docker-"
)

// Backend is the part of the Haify client the plugin uses.
type Backend interface {
	CreateResourceRequest(ctx context.Context, req *haifypb.CreateResourceRequest) error
	GetResource(ctx context.Context, name string) (*haifypb.ResourceInfo, error)
	ListResources(ctx context.Context) ([]*haifypb.ResourceInfo, error)
	DeleteResource(ctx context.Context, name string) error
	AttachDisklessClient(ctx context.Context, resource, node string) error
	PromoteForNode(ctx context.Context, resource, node string) error
	SetSecondary(ctx context.Context, resource, node string) error
}

// Defaults applies to a volume created without the matching -o option.
type Defaults struct {
	SizeBytes uint64
	Pool      string
	Replicas  uint32
	FS        string
}

// Driver serves one host.
type Driver struct {
	backend  Backend
	mounter  csi.Mounter
	node     string
	root     string // mountpoints live under here
	defaults Defaults
	// deviceWait is how long Mount waits for the device to appear.
	deviceWait time.Duration
	// device is the block device of a resource's first volume.
	device func(resource string) string

	mu sync.Mutex
	// users counts the containers using each mounted volume on this host;
	// Docker calls Mount and Unmount once per container.
	users map[string]map[string]bool
}

// New returns a driver for node, mounting under root.
func New(backend Backend, mounter csi.Mounter, node, root string, defaults Defaults) *Driver {
	return &Driver{backend: backend, mounter: mounter, node: node, root: root, defaults: defaults,
		deviceWait: 60 * time.Second, users: map[string]map[string]bool{},
		device: func(res string) string { return "/dev/drbd/by-res/" + res + "/0" }}
}

var volumeNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// resourceName is the Haify resource behind a Docker volume. DRBD names take
// no dots, so they become underscores; the label keeps the real name.
func resourceName(volume string) (string, error) {
	if !volumeNameRE.MatchString(volume) || len(volume) > 100 {
		zap.L().Debug("resourceName: invalid volume name", zap.String("volume", volume), zap.Int("len", len(volume)))
		return "", fmt.Errorf("%q is not a valid volume name", volume)
	}
	return resPrefix + strings.ReplaceAll(volume, ".", "_"), nil
}

func (d *Driver) mountpoint(volume string) string { return filepath.Join(d.root, volume) }

// Create makes the volume. Options: size (e.g. 20G), pool, replicas, fs
// (ext4 or xfs).
func (d *Driver) Create(ctx context.Context, volume string, opts map[string]string) error {
	log := zap.L()
	log.Debug("Driver.Create: creating volume", zap.String("volume", volume), zap.Int("opts", len(opts)),
		zap.String("node", d.node))
	res, err := resourceName(volume)
	if err != nil {
		return err
	}
	size, pool, replicas, fs := d.defaults.SizeBytes, d.defaults.Pool, d.defaults.Replicas, d.defaults.FS
	for k, v := range opts {
		log.Debug("Driver.Create: option", zap.String("volume", volume), zap.String("key", k), zap.String("value", v))
		switch k {
		case "size":
			if size, err = util.ParseSize(v); err != nil || size == 0 {
				log.Debug("Driver.Create: bad size", zap.String("volume", volume), zap.String("size", v), zap.Error(err))
				return fmt.Errorf("size %q: give it like 20G", v)
			}
		case "pool":
			pool = v
		case "replicas":
			n, perr := strconv.ParseUint(v, 10, 32)
			if perr != nil || n < 1 {
				log.Debug("Driver.Create: bad replicas", zap.String("volume", volume), zap.String("replicas", v),
					zap.Error(perr))
				return fmt.Errorf("replicas %q is not a number of copies", v)
			}
			replicas = uint32(n)
		case "fs":
			if v != "ext4" && v != "xfs" {
				log.Debug("Driver.Create: bad fs", zap.String("volume", volume), zap.String("fs", v))
				return fmt.Errorf("fs %q: ext4 or xfs", v)
			}
			fs = v
		default:
			log.Debug("Driver.Create: unknown option", zap.String("volume", volume), zap.String("key", k))
			return fmt.Errorf("unknown option %q (size, pool, replicas, fs)", k)
		}
	}
	if pool == "" {
		log.Debug("Driver.Create: no pool given and no default", zap.String("volume", volume))
		return fmt.Errorf("no pool: pass -o pool=<pool> or start the plugin with --pool")
	}
	start := time.Now()
	existing, gerr := d.backend.GetResource(ctx, res)
	log.Debug("Driver.Create: looked up existing resource", zap.String("resource", res),
		zap.Bool("found", existing != nil), zap.Duration("took", time.Since(start)), zap.Error(gerr))
	if existing != nil {
		if existing.GetLabels()[LabelVolume] == volume {
			log.Debug("Driver.Create: volume already exists, nothing to do", zap.String("volume", volume),
				zap.String("resource", res))
			return nil // Docker creates again on `docker run -v` of an existing volume
		}
		log.Debug("Driver.Create: resource name taken by something else", zap.String("resource", res),
			zap.String("label_volume", existing.GetLabels()[LabelVolume]))
		return fmt.Errorf("a Haify resource named %s already exists and is not this volume", res)
	}
	log.Debug("Driver.Create: creating resource", zap.String("resource", res), zap.Uint64("size_bytes", size),
		zap.String("pool", pool), zap.Uint32("replicas", replicas), zap.String("fs", fs))
	start = time.Now()
	err = d.backend.CreateResourceRequest(ctx, &haifypb.CreateResourceRequest{
		Name: res, Protocol: "C", SizeBytes: size, Pool: pool, Replicas: replicas,
		Labels: map[string]string{LabelVolume: volume, LabelFS: fs},
	})
	log.Debug("Driver.Create: create resource returned", zap.String("resource", res),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}

// volumeInfo is what Get and List report.
type volumeInfo struct {
	Name       string
	Mountpoint string
	Status     map[string]any
}

func (d *Driver) info(r *haifypb.ResourceInfo) volumeInfo {
	v := volumeInfo{Name: r.GetLabels()[LabelVolume], Status: map[string]any{
		"resource": r.GetName(), "replicas": r.GetNodes(),
	}}
	ok, err := d.mounter.IsMountPoint(d.mountpoint(v.Name))
	zap.L().Debug("Driver.info: mount check", zap.String("volume", v.Name), zap.String("resource", r.GetName()),
		zap.String("mountpoint", d.mountpoint(v.Name)), zap.Bool("mounted", ok), zap.Error(err))
	if ok {
		v.Mountpoint = d.mountpoint(v.Name)
	}
	return v
}

// Get describes one volume.
func (d *Driver) Get(ctx context.Context, volume string) (volumeInfo, error) {
	res, err := resourceName(volume)
	if err != nil {
		return volumeInfo{}, err
	}
	start := time.Now()
	r, err := d.backend.GetResource(ctx, res)
	zap.L().Debug("Driver.Get: looked up resource", zap.String("volume", volume), zap.String("resource", res),
		zap.Bool("found", r != nil), zap.String("label_volume", r.GetLabels()[LabelVolume]),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil || r == nil || r.GetLabels()[LabelVolume] != volume {
		return volumeInfo{}, fmt.Errorf("no such volume: %s", volume)
	}
	return d.info(r), nil
}

// List describes every Haify Docker volume.
func (d *Driver) List(ctx context.Context) ([]volumeInfo, error) {
	log := zap.L()
	start := time.Now()
	all, err := d.backend.ListResources(ctx)
	log.Debug("Driver.List: listed resources", zap.Int("count", len(all)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, err
	}
	var out []volumeInfo
	for _, r := range all {
		isVolume := r.GetLabels()[LabelVolume] != ""
		log.Debug("Driver.List: resource", zap.String("resource", r.GetName()),
			zap.String("volume", r.GetLabels()[LabelVolume]), zap.Bool("docker_volume", isVolume))
		if isVolume {
			out = append(out, d.info(r))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	log.Debug("Driver.List: volumes", zap.Int("count", len(out)))
	return out, nil
}

// Remove deletes the volume and its data on every node.
func (d *Driver) Remove(ctx context.Context, volume string) error {
	log := zap.L()
	log.Debug("Driver.Remove: removing volume", zap.String("volume", volume))
	if _, err := d.Get(ctx, volume); err != nil {
		log.Debug("Driver.Remove: volume not found", zap.String("volume", volume), zap.Error(err))
		return err
	}
	res, _ := resourceName(volume)
	start := time.Now()
	err := d.backend.DeleteResource(ctx, res)
	log.Debug("Driver.Remove: delete resource returned", zap.String("resource", res),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}

// Mount makes the volume usable on this host for container id.
func (d *Driver) Mount(ctx context.Context, volume, id string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	log := zap.L()
	mp := d.mountpoint(volume)
	log.Debug("Driver.Mount: mounting volume", zap.String("volume", volume), zap.String("container", id),
		zap.String("node", d.node), zap.String("mountpoint", mp))
	ok, merr := d.mounter.IsMountPoint(mp)
	log.Debug("Driver.Mount: mount check", zap.String("mountpoint", mp), zap.Bool("mounted", ok), zap.Error(merr))
	if ok {
		log.Debug("Driver.Mount: already mounted here, adding user", zap.String("volume", volume),
			zap.String("container", id))
		d.addUser(volume, id)
		return mp, nil
	}
	res, err := resourceName(volume)
	if err != nil {
		return "", err
	}
	start := time.Now()
	r, err := d.backend.GetResource(ctx, res)
	log.Debug("Driver.Mount: looked up resource", zap.String("resource", res), zap.Bool("found", r != nil),
		zap.String("label_volume", r.GetLabels()[LabelVolume]), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil || r == nil || r.GetLabels()[LabelVolume] != volume {
		return "", fmt.Errorf("no such volume: %s", volume)
	}
	if !holds(r, d.node) {
		log.Debug("Driver.Mount: node holds no replica, joining as diskless client", zap.String("resource", res),
			zap.String("node", d.node), zap.Strings("replicas", r.GetNodes()))
		start = time.Now()
		err := d.backend.AttachDisklessClient(ctx, res, d.node)
		log.Debug("Driver.Mount: attach diskless returned", zap.String("resource", res), zap.String("node", d.node),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			return "", fmt.Errorf("join %s from %s: %w", volume, d.node, err)
		}
	} else {
		log.Debug("Driver.Mount: node already holds the resource", zap.String("resource", res), zap.String("node", d.node))
	}
	start = time.Now()
	err = d.backend.PromoteForNode(ctx, res, d.node)
	log.Debug("Driver.Mount: promote returned", zap.String("resource", res), zap.String("node", d.node),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return "", fmt.Errorf("make %s usable on %s: %w", volume, d.node, err)
	}
	device := d.device(res)
	if err := d.waitDevice(device); err != nil {
		log.Debug("Driver.Mount: device never appeared, releasing", zap.String("resource", res),
			zap.String("device", device), zap.Error(err))
		d.release(ctx, res)
		return "", err
	}
	fs := r.GetLabels()[LabelFS]
	if fs == "" {
		log.Debug("Driver.Mount: no fs label, using ext4", zap.String("resource", res))
		fs = "ext4"
	}
	if err := d.mounter.EnsureDir(mp); err != nil {
		log.Debug("Driver.Mount: creating mountpoint failed, releasing", zap.String("mountpoint", mp), zap.Error(err))
		d.release(ctx, res)
		return "", err
	}
	start = time.Now()
	err = d.mounter.FormatAndMount(device, mp, fs, nil)
	log.Debug("Driver.Mount: format and mount returned", zap.String("device", device), zap.String("mountpoint", mp),
		zap.String("fs", fs), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		d.release(ctx, res)
		return "", fmt.Errorf("mount %s: %w", volume, err)
	}
	d.addUser(volume, id)
	log.Debug("Driver.Mount: mounted", zap.String("volume", volume), zap.String("mountpoint", mp),
		zap.Int("users", len(d.users[volume])))
	return mp, nil
}

// release gives up the Primary role after a failed Mount, best effort: the
// mount's own error is what the caller reports.
func (d *Driver) release(ctx context.Context, res string) {
	start := time.Now()
	err := d.backend.SetSecondary(ctx, res, d.node)
	zap.L().Debug("Driver.release: set secondary returned", zap.String("resource", res), zap.String("node", d.node),
		zap.Duration("took", time.Since(start)), zap.Error(err))
}

// Unmount releases the volume for container id; the last one out unmounts it
// and gives up the Primary role, so another host can take it.
func (d *Driver) Unmount(ctx context.Context, volume, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	log := zap.L()
	log.Debug("Driver.Unmount: releasing volume", zap.String("volume", volume), zap.String("container", id),
		zap.String("node", d.node))
	if u := d.users[volume]; u != nil {
		delete(u, id)
		if len(u) > 0 {
			log.Debug("Driver.Unmount: other containers still use it, keeping mounted", zap.String("volume", volume),
				zap.Int("users", len(u)))
			return nil
		}
		delete(d.users, volume)
	} else {
		log.Debug("Driver.Unmount: no users recorded, unmounting", zap.String("volume", volume))
	}
	res, err := resourceName(volume)
	if err != nil {
		return err
	}
	start := time.Now()
	err = d.mounter.Unmount(d.mountpoint(volume))
	log.Debug("Driver.Unmount: unmount returned", zap.String("mountpoint", d.mountpoint(volume)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("unmount %s: %w", volume, err)
	}
	start = time.Now()
	err = d.backend.SetSecondary(ctx, res, d.node)
	log.Debug("Driver.Unmount: set secondary returned", zap.String("resource", res), zap.String("node", d.node),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("release %s on %s: %w", volume, d.node, err)
	}
	return nil
}

// Path is where the volume is mounted on this host, or "" when it is not.
func (d *Driver) Path(volume string) string {
	ok, err := d.mounter.IsMountPoint(d.mountpoint(volume))
	zap.L().Debug("Driver.Path: mount check", zap.String("volume", volume),
		zap.String("mountpoint", d.mountpoint(volume)), zap.Bool("mounted", ok), zap.Error(err))
	if ok {
		return d.mountpoint(volume)
	}
	return ""
}

func (d *Driver) addUser(volume, id string) {
	if d.users[volume] == nil {
		d.users[volume] = map[string]bool{}
	}
	d.users[volume][id] = true
}

func holds(r *haifypb.ResourceInfo, node string) bool {
	return slices.Contains(r.GetNodes(), node) || slices.Contains(r.GetDisklessClients(), node)
}

func (d *Driver) waitDevice(device string) error {
	log := zap.L()
	start := time.Now()
	deadline := start.Add(d.deviceWait)
	log.Debug("Driver.waitDevice: waiting for device", zap.String("device", device), zap.Duration("timeout", d.deviceWait))
	for {
		if _, err := os.Stat(device); err == nil {
			log.Debug("Driver.waitDevice: device present", zap.String("device", device),
				zap.Duration("took", time.Since(start)))
			return nil
		}
		if time.Now().After(deadline) {
			log.Debug("Driver.waitDevice: timed out", zap.String("device", device), zap.Duration("took", time.Since(start)))
			return fmt.Errorf("%s did not appear within %s", device, d.deviceWait)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
