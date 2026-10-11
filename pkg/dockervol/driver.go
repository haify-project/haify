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
		return "", fmt.Errorf("%q is not a valid volume name", volume)
	}
	return resPrefix + strings.ReplaceAll(volume, ".", "_"), nil
}

func (d *Driver) mountpoint(volume string) string { return filepath.Join(d.root, volume) }

// Create makes the volume. Options: size (e.g. 20G), pool, replicas, fs
// (ext4 or xfs).
func (d *Driver) Create(ctx context.Context, volume string, opts map[string]string) error {
	res, err := resourceName(volume)
	if err != nil {
		return err
	}
	size, pool, replicas, fs := d.defaults.SizeBytes, d.defaults.Pool, d.defaults.Replicas, d.defaults.FS
	for k, v := range opts {
		switch k {
		case "size":
			if size, err = util.ParseSize(v); err != nil || size == 0 {
				return fmt.Errorf("size %q: give it like 20G", v)
			}
		case "pool":
			pool = v
		case "replicas":
			n, perr := strconv.ParseUint(v, 10, 32)
			if perr != nil || n < 1 {
				return fmt.Errorf("replicas %q is not a number of copies", v)
			}
			replicas = uint32(n)
		case "fs":
			if v != "ext4" && v != "xfs" {
				return fmt.Errorf("fs %q: ext4 or xfs", v)
			}
			fs = v
		default:
			return fmt.Errorf("unknown option %q (size, pool, replicas, fs)", k)
		}
	}
	if pool == "" {
		return fmt.Errorf("no pool: pass -o pool=<pool> or start the plugin with --pool")
	}
	if existing, _ := d.backend.GetResource(ctx, res); existing != nil {
		if existing.GetLabels()[LabelVolume] == volume {
			return nil // Docker creates again on `docker run -v` of an existing volume
		}
		return fmt.Errorf("a Haify resource named %s already exists and is not this volume", res)
	}
	return d.backend.CreateResourceRequest(ctx, &haifypb.CreateResourceRequest{
		Name: res, Protocol: "C", SizeBytes: size, Pool: pool, Replicas: replicas,
		Labels: map[string]string{LabelVolume: volume, LabelFS: fs},
	})
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
	if ok, _ := d.mounter.IsMountPoint(d.mountpoint(v.Name)); ok {
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
	r, err := d.backend.GetResource(ctx, res)
	if err != nil || r == nil || r.GetLabels()[LabelVolume] != volume {
		return volumeInfo{}, fmt.Errorf("no such volume: %s", volume)
	}
	return d.info(r), nil
}

// List describes every Haify Docker volume.
func (d *Driver) List(ctx context.Context) ([]volumeInfo, error) {
	all, err := d.backend.ListResources(ctx)
	if err != nil {
		return nil, err
	}
	var out []volumeInfo
	for _, r := range all {
		if r.GetLabels()[LabelVolume] != "" {
			out = append(out, d.info(r))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Remove deletes the volume and its data on every node.
func (d *Driver) Remove(ctx context.Context, volume string) error {
	if _, err := d.Get(ctx, volume); err != nil {
		return err
	}
	res, _ := resourceName(volume)
	return d.backend.DeleteResource(ctx, res)
}

// Mount makes the volume usable on this host for container id.
func (d *Driver) Mount(ctx context.Context, volume, id string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	mp := d.mountpoint(volume)
	if ok, _ := d.mounter.IsMountPoint(mp); ok {
		d.addUser(volume, id)
		return mp, nil
	}
	res, err := resourceName(volume)
	if err != nil {
		return "", err
	}
	r, err := d.backend.GetResource(ctx, res)
	if err != nil || r == nil || r.GetLabels()[LabelVolume] != volume {
		return "", fmt.Errorf("no such volume: %s", volume)
	}
	if !holds(r, d.node) {
		if err := d.backend.AttachDisklessClient(ctx, res, d.node); err != nil {
			return "", fmt.Errorf("join %s from %s: %w", volume, d.node, err)
		}
	}
	if err := d.backend.PromoteForNode(ctx, res, d.node); err != nil {
		return "", fmt.Errorf("make %s usable on %s: %w", volume, d.node, err)
	}
	device := d.device(res)
	if err := d.waitDevice(device); err != nil {
		_ = d.backend.SetSecondary(ctx, res, d.node)
		return "", err
	}
	fs := r.GetLabels()[LabelFS]
	if fs == "" {
		fs = "ext4"
	}
	if err := d.mounter.EnsureDir(mp); err != nil {
		_ = d.backend.SetSecondary(ctx, res, d.node)
		return "", err
	}
	if err := d.mounter.FormatAndMount(device, mp, fs, nil); err != nil {
		_ = d.backend.SetSecondary(ctx, res, d.node)
		return "", fmt.Errorf("mount %s: %w", volume, err)
	}
	d.addUser(volume, id)
	return mp, nil
}

// Unmount releases the volume for container id; the last one out unmounts it
// and gives up the Primary role, so another host can take it.
func (d *Driver) Unmount(ctx context.Context, volume, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if u := d.users[volume]; u != nil {
		delete(u, id)
		if len(u) > 0 {
			return nil
		}
		delete(d.users, volume)
	}
	res, err := resourceName(volume)
	if err != nil {
		return err
	}
	if err := d.mounter.Unmount(d.mountpoint(volume)); err != nil {
		return fmt.Errorf("unmount %s: %w", volume, err)
	}
	if err := d.backend.SetSecondary(ctx, res, d.node); err != nil {
		return fmt.Errorf("release %s on %s: %w", volume, d.node, err)
	}
	return nil
}

// Path is where the volume is mounted on this host, or "" when it is not.
func (d *Driver) Path(volume string) string {
	if ok, _ := d.mounter.IsMountPoint(d.mountpoint(volume)); ok {
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
	deadline := time.Now().Add(d.deviceWait)
	for {
		if _, err := os.Stat(device); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not appear within %s", device, d.deviceWait)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
