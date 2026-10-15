package deployment

import (
	"context"
	"fmt"

	"go.uber.org/zap"
)

// ============ DRBD Operations ============

// DRBDDetach takes a node's local disk out of a resource, leaving it connected
// but diskless. The peers keep serving throughout.
func (c *Client) DRBDDetach(ctx context.Context, host, resource string) (*ExecResult, error) {
	c.log().Debug("DRBDDetach: detaching local disk", zap.String("host", host), zap.String("resource", resource))
	return c.Exec(ctx, []string{host}, fmt.Sprintf("sudo drbdadm detach %s", resource))
}

// DRBDAttach puts a rebuilt backing device back into a resource, which starts a
// full resync from the peers.
func (c *Client) DRBDAttach(ctx context.Context, host, resource string) (*ExecResult, error) {
	c.log().Debug("DRBDAttach: attaching backing disk", zap.String("host", host), zap.String("resource", resource))
	return c.Exec(ctx, []string{host}, fmt.Sprintf("sudo drbdadm attach %s", resource))
}

// DRBDUp brings up a DRBD resource
func (c *Client) DRBDUp(ctx context.Context, hosts []string, resource string) (*ExecResult, error) {
	c.log().Debug("DRBDUp: bringing resource up", zap.Strings("hosts", hosts), zap.String("resource", resource))
	return c.Exec(ctx, hosts, fmt.Sprintf("sudo drbdadm up %s", resource))
}

// DRBDDown brings down a DRBD resource
func (c *Client) DRBDDown(ctx context.Context, hosts []string, resource string) (*ExecResult, error) {
	c.log().Debug("DRBDDown: taking resource down", zap.Strings("hosts", hosts), zap.String("resource", resource))
	return c.Exec(ctx, hosts, fmt.Sprintf("sudo drbdadm down %s", resource))
}

// DRBDPrimary sets resource to Primary
func (c *Client) DRBDPrimary(ctx context.Context, host, resource string, force bool) (*HostResult, error) {
	log := c.log()
	log.Debug("DRBDPrimary: promoting", zap.String("host", host), zap.String("resource", resource), zap.Bool("force", force))
	// Honor the force flag. A plain `drbdadm primary` refuses to promote when a
	// peer still holds Primary or is unreachable (no quorum) — this is the SAFE
	// default for graceful moves. `--force` overrides that and MUST only be used
	// once the caller has confirmed it is safe (e.g. this node holds DRBD
	// quorum); forcing blindly can create a dual-Primary split-brain.
	cmd := fmt.Sprintf("sudo drbdadm primary %s", resource)
	if force {
		cmd = fmt.Sprintf("sudo drbdadm primary --force %s", resource)
	}
	result, err := c.Exec(ctx, []string{host}, cmd)
	if err != nil {
		log.Debug("DRBDPrimary: exec errored", zap.String("host", host), zap.String("resource", resource), zap.Error(err))
		return nil, err
	}
	// Find result - the returned host key may differ (IP vs hostname)
	for _, r := range result.Hosts {
		log.Debug("DRBDPrimary: result", zap.String("host", host), zap.String("resource", resource), zap.Bool("success", r.Success))
		return r, nil
	}
	log.Debug("DRBDPrimary: no result for host", zap.String("host", host), zap.String("resource", resource))
	return nil, fmt.Errorf("no result returned for host %s", host)
}

// DRBDSecondary sets resource to Secondary
func (c *Client) DRBDSecondary(ctx context.Context, host, resource string) (*HostResult, error) {
	log := c.log()
	log.Debug("DRBDSecondary: demoting", zap.String("host", host), zap.String("resource", resource))
	result, err := c.Exec(ctx, []string{host}, fmt.Sprintf("sudo drbdadm secondary %s", resource))
	if err != nil {
		log.Debug("DRBDSecondary: exec errored", zap.String("host", host), zap.String("resource", resource), zap.Error(err))
		return nil, err
	}
	// Find result - the returned host key may differ (IP vs hostname)
	for _, r := range result.Hosts {
		log.Debug("DRBDSecondary: result", zap.String("host", host), zap.String("resource", resource), zap.Bool("success", r.Success))
		return r, nil
	}
	log.Debug("DRBDSecondary: no result for host", zap.String("host", host), zap.String("resource", resource))
	return nil, fmt.Errorf("no result returned for host %s", host)
}

// DRBDCreateMD creates DRBD metadata with room for maxPeers peers.
//
// The peer count is not cosmetic: DRBD allocates one bitmap slot per peer when
// metadata is created and there is no way to add slots afterwards. Sizing to the
// peer count of the moment means the first node added later — an off-site DR, a
// third replica, a diskless client — fails with "Not enough free bitmap slots",
// and the only fix is to recreate metadata on every replica and resync. Passing
// a maxPeers of 0 uses the slot floor the volume was already sized for.
func (c *Client) DRBDCreateMD(ctx context.Context, hosts []string, resource string, maxPeers int) (*ExecResult, error) {
	if maxPeers <= 0 {
		maxPeers = DefaultMaxPeers
	}
	c.log().Debug("DRBDCreateMD: creating metadata",
		zap.Strings("hosts", hosts), zap.String("resource", resource), zap.Int("max_peers", maxPeers))
	return c.Exec(ctx, hosts,
		fmt.Sprintf("sudo drbdadm create-md --max-peers=%d --force %s", maxPeers, resource))
}

// DefaultMaxPeers is the bitmap-slot count metadata is created with when the
// caller does not care. It matches the peer count backing volumes are sized for,
// so the slots always fit in the space already reserved.
const DefaultMaxPeers = 7

// DRBDAdjust adjusts DRBD configuration
func (c *Client) DRBDAdjust(ctx context.Context, hosts []string, resource string) (*ExecResult, error) {
	c.log().Debug("DRBDAdjust: adjusting resource", zap.Strings("hosts", hosts), zap.String("resource", resource))
	return c.Exec(ctx, hosts, fmt.Sprintf("sudo drbdadm adjust %s", resource))
}

// DRBDStatus gets DRBD resource status
func (c *Client) DRBDStatus(ctx context.Context, hosts []string, resource string) (*ExecResult, error) {
	c.log().Debug("DRBDStatus: reading status", zap.Strings("hosts", hosts), zap.String("resource", resource))
	cmd := fmt.Sprintf("sudo drbdadm status %s --verbose 2>/dev/null || sudo drbdadm status %s", resource, resource)
	return c.Exec(ctx, hosts, cmd, WithReadOnly())
}

// DRBDStatusJSON gets structured DRBD resource status via
// `drbdsetup status <res> --json`. The JSON is keyed by node name / node-id
// and carries per-peer replication state and resync completion (the `done`
// field), which the plain-text `drbdadm status` output does not expose in a
// machine-parseable way.
func (c *Client) DRBDStatusJSON(ctx context.Context, hosts []string, resource string) (*ExecResult, error) {
	c.log().Debug("DRBDStatusJSON: reading status", zap.Strings("hosts", hosts), zap.String("resource", resource))
	return c.Exec(ctx, hosts, fmt.Sprintf("sudo drbdsetup status %s --json", resource), WithReadOnly())
}
