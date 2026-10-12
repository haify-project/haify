package deployment

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"go.uber.org/zap"
)

// ============ ZFS Operations ============

// ZFSCreatePool creates a ZFS pool. Compression and dedup are set on its root
// dataset (zpool create -O), so every zvol carved from it inherits them.
//
// A zpool has no thin/thick mode: that is a per-zvol property decided at
// volume creation (zfs create -s -V / refreservation).
func (c *Client) ZFSCreatePool(ctx context.Context, hosts []string, poolName string, vdevs []string, opts ...ZFSOption) (*ExecResult, error) {
	var o zfsOptions
	for _, opt := range opts {
		opt(&o)
	}
	log := c.log()
	log.Debug("ZFSCreatePool: creating pool",
		zap.Strings("hosts", hosts), zap.String("pool", poolName), zap.Strings("vdevs", vdevs),
		zap.String("compression", o.compression), zap.Bool("dedup", o.dedup))
	props, err := o.createFlags()
	if err != nil {
		log.Debug("ZFSCreatePool: invalid options", zap.String("pool", poolName), zap.Error(err))
		return nil, err
	}
	cmd := fmt.Sprintf("sudo zpool create -f%s %s %s", props, poolName, strings.Join(vdevs, " "))
	return c.Exec(ctx, hosts, cmd)
}

// ZFSDestroyPool destroys a ZFS pool
func (c *Client) ZFSDestroyPool(ctx context.Context, hosts []string, poolName string) (*ExecResult, error) {
	c.log().Debug("ZFSDestroyPool: destroying pool", zap.Strings("hosts", hosts), zap.String("pool", poolName))
	cmd := fmt.Sprintf("sudo zpool destroy -f %s", poolName)
	return c.Exec(ctx, hosts, cmd)
}

// ZFSListPools lists ZFS pools
func (c *Client) ZFSListPools(ctx context.Context, hosts []string) (*ExecResult, error) {
	c.log().Debug("ZFSListPools: listing pools", zap.Strings("hosts", hosts))
	cmd := "sudo zpool list -Hp -o name,size,free,alloc,cap"
	return c.Exec(ctx, hosts, cmd)
}

// ZFSGetPool gets ZFS pool status
func (c *Client) ZFSGetPool(ctx context.Context, hosts []string, poolName string) (*ExecResult, error) {
	c.log().Debug("ZFSGetPool: reading pool status", zap.Strings("hosts", hosts), zap.String("pool", poolName))
	cmd := fmt.Sprintf("sudo zpool status %s", poolName)
	return c.Exec(ctx, hosts, cmd)
}

// ZFSCreateDataset creates a ZFS dataset
func (c *Client) ZFSCreateDataset(ctx context.Context, hosts []string, datasetName string, opts ...ZFSOption) (*ExecResult, error) {
	c.log().Debug("ZFSCreateDataset: creating dataset", zap.Strings("hosts", hosts), zap.String("dataset", datasetName))
	cmd := fmt.Sprintf("sudo zfs create %s", datasetName)
	return c.Exec(ctx, hosts, cmd)
}

// ZFSCreateThinDataset creates a thin-provisioned ZFS dataset (zvol)
func (c *Client) ZFSCreateThinDataset(ctx context.Context, hosts []string, poolName, datasetName, size string) (*ExecResult, error) {
	c.log().Debug("ZFSCreateThinDataset: creating sparse zvol",
		zap.Strings("hosts", hosts), zap.String("pool", poolName), zap.String("dataset", datasetName), zap.String("size", size))
	cmd := fmt.Sprintf("sudo zfs create -s -V %s %s/%s", size, poolName, datasetName)
	return c.Exec(ctx, hosts, cmd)
}

// ZFSDestroyDataset destroys a ZFS dataset
func (c *Client) ZFSDestroyDataset(ctx context.Context, hosts []string, datasetName string) (*ExecResult, error) {
	c.log().Debug("ZFSDestroyDataset: destroying dataset", zap.Strings("hosts", hosts), zap.String("dataset", datasetName))
	// -r removes dependent snapshots too: a dataset delete through the
	// management API is an explicit teardown, and without -r any dataset
	// that was ever snapshotted becomes undeletable ("has children").
	cmd := fmt.Sprintf("sudo zfs destroy -r -f %s", datasetName)
	return c.Exec(ctx, hosts, cmd)
}

// ZFSSnapshot creates a ZFS snapshot
func (c *Client) ZFSSnapshot(ctx context.Context, hosts []string, dataset, snapshotName string) (*ExecResult, error) {
	c.log().Debug("ZFSSnapshot: creating snapshot",
		zap.Strings("hosts", hosts), zap.String("dataset", dataset), zap.String("snapshot", snapshotName))
	cmd := fmt.Sprintf("sudo zfs snapshot %s@%s", dataset, snapshotName)
	return c.Exec(ctx, hosts, cmd)
}

// ZFSRollback rolls back to a ZFS snapshot
func (c *Client) ZFSRollback(ctx context.Context, hosts []string, dataset, snapshotName string) (*ExecResult, error) {
	c.log().Debug("ZFSRollback: rolling back",
		zap.Strings("hosts", hosts), zap.String("dataset", dataset), zap.String("snapshot", snapshotName))
	cmd := fmt.Sprintf("sudo zfs rollback -r %s@%s", dataset, snapshotName)
	return c.Exec(ctx, hosts, cmd)
}

// ZFSClone creates a clone from a snapshot
func (c *Client) ZFSClone(ctx context.Context, hosts []string, snapshot, clonePath string) (*ExecResult, error) {
	c.log().Debug("ZFSClone: cloning snapshot",
		zap.Strings("hosts", hosts), zap.String("snapshot", snapshot), zap.String("clone", clonePath))
	cmd := fmt.Sprintf("sudo zfs clone %s %s", snapshot, clonePath)
	return c.Exec(ctx, hosts, cmd)
}

// ZFSListSnapshots lists ZFS snapshots for a dataset. -p prints sizes in bytes
// and the creation time as Unix seconds: the human form ("Mon Oct 12 1:30
// 2026") has spaces in it and no time zone.
func (c *Client) ZFSListSnapshots(ctx context.Context, hosts []string, dataset string) (*ExecResult, error) {
	c.log().Debug("ZFSListSnapshots: listing snapshots", zap.Strings("hosts", hosts), zap.String("dataset", dataset))
	cmd := fmt.Sprintf("sudo zfs list -t snapshot -o name,used,refer,creation -H -p %s", dataset)
	return c.Exec(ctx, hosts, cmd)
}

// ZFSDestroySnapshot destroys a ZFS snapshot
func (c *Client) ZFSDestroySnapshot(ctx context.Context, hosts []string, snapshot string) (*ExecResult, error) {
	c.log().Debug("ZFSDestroySnapshot: destroying snapshot", zap.Strings("hosts", hosts), zap.String("snapshot", snapshot))
	cmd := fmt.Sprintf("sudo zfs destroy -r %s", snapshot)
	return c.Exec(ctx, hosts, cmd)
}

// ZFSSetQuota sets a quota on a ZFS dataset
func (c *Client) ZFSSetQuota(ctx context.Context, hosts []string, dataset, quota string) (*ExecResult, error) {
	c.log().Debug("ZFSSetQuota: setting quota", zap.Strings("hosts", hosts), zap.String("dataset", dataset), zap.String("quota", quota))
	cmd := fmt.Sprintf("sudo zfs set quota=%s %s", quota, dataset)
	return c.Exec(ctx, hosts, cmd)
}

// ZFSSetReservation sets a reservation on a ZFS dataset
func (c *Client) ZFSSetReservation(ctx context.Context, hosts []string, dataset, reservation string) (*ExecResult, error) {
	c.log().Debug("ZFSSetReservation: setting reservation",
		zap.Strings("hosts", hosts), zap.String("dataset", dataset), zap.String("reservation", reservation))
	cmd := fmt.Sprintf("sudo zfs set reservation=%s %s", reservation, dataset)
	return c.Exec(ctx, hosts, cmd)
}

// ZFSResizeVolume resizes a ZFS volume
func (c *Client) ZFSResizeVolume(ctx context.Context, hosts []string, volumePath, newSize string) (*ExecResult, error) {
	c.log().Debug("ZFSResizeVolume: resizing zvol",
		zap.Strings("hosts", hosts), zap.String("volume", volumePath), zap.String("size", newSize))
	cmd := fmt.Sprintf("sudo zfs set volsize=%s %s", newSize, volumePath)
	return c.Exec(ctx, hosts, cmd)
}

// ZFSOption configures ZFS pool creation.
type ZFSOption func(*zfsOptions)

type zfsOptions struct {
	compression string
	dedup       bool
}

// zfsCompressionRE is every compression value OpenZFS accepts.
var zfsCompressionRE = regexp.MustCompile(`^(on|off|lz4|lzjb|zle|gzip(-[1-9])?|zstd(-([1-9]|1[0-9]))?|zstd-fast(-[0-9]+)?)$`)

// ValidZFSCompression reports whether value is a compression setting OpenZFS
// accepts. It is checked before anything is created, and it is what keeps the
// value safe to put in a shell command.
func ValidZFSCompression(value string) bool {
	return zfsCompressionRE.MatchString(value)
}

func (o zfsOptions) createFlags() (string, error) {
	var flags string
	if o.compression != "" {
		if !ValidZFSCompression(o.compression) {
			zap.L().Debug("zfsOptions.createFlags: unknown compression", zap.String("compression", o.compression))
			return "", fmt.Errorf("unknown ZFS compression %q (on, off, lz4, zstd, zstd-1..19, gzip, gzip-1..9, lzjb, zle)", o.compression)
		}
		flags += " -O compression=" + o.compression
	}
	if o.dedup {
		flags += " -O dedup=on"
	}
	zap.L().Debug("zfsOptions.createFlags: flags built", zap.String("flags", flags))
	return flags, nil
}

// WithZFSCompression sets the pool's compression algorithm; empty keeps the
// OpenZFS default.
func WithZFSCompression(algorithm string) ZFSOption {
	return func(o *zfsOptions) {
		o.compression = algorithm
	}
}

// WithZFSDedup turns on deduplication for the pool.
func WithZFSDedup(dedup bool) ZFSOption {
	return func(o *zfsOptions) {
		o.dedup = dedup
	}
}

// ZFSPoolPropertiesScript prints each pool's compression algorithm and
// achieved ratio, read from its root dataset: "<pool>\t<property>\t<value>"
// per line. It uses a shell variable, so it has to reach the node
// base64-wrapped (dispatch's sh -c "..." would empty it).
const ZFSPoolPropertiesScript = `p=$(zpool list -H -o name 2>/dev/null)
[ -n "$p" ] && zfs get -H -p -o name,property,value compression,compressratio $p 2>/dev/null
true
`
