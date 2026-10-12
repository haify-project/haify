package wanproxy

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
)

// reachCmd builds a node-side command that succeeds (exit 0) iff a TCP
// connection to host:port can be opened within reachTimeoutSecs. It uses bash's
// /dev/tcp pseudo-device (present on all supported distros) guarded by timeout,
// so no extra tooling (nc/ncat) is required on the node.
func reachCmd(host string, port int) string {
	return fmt.Sprintf("timeout %d bash -c 'exec 3<>/dev/tcp/%s/%d'", reachTimeoutSecs, host, port)
}

// VerifyReachability confirms the primary node can open a TCP connection to the
// DR site's public WAN endpoint — i.e. the DR firewall / cloud security group
// actually permits the inbound mTLS port. It retries to absorb the acceptor's
// bind race after enable --now. Returning an error here is the intended
// fail-fast: without this probe a blocked port lets Provision "succeed" while
// the DRBD resource never reaches Connected.
func VerifyReachability(ctx context.Context, deploy DeploymentClient, spec ProxySpec) error {
	log := zap.L()
	log.Debug("VerifyReachability: start", zap.String("resource", spec.Resource), zap.String("primary", spec.PrimaryNodeAddr),
		zap.String("dr_endpoint", spec.DRPublicEndpoint), zap.Int("wan_port", spec.WANPort), zap.Int("attempts", reachAttempts))
	if deploy == nil {
		log.Debug("VerifyReachability: no deployment client", zap.String("resource", spec.Resource))
		return fmt.Errorf("wanproxy: deployment client is nil")
	}
	if err := spec.Validate(); err != nil {
		log.Debug("VerifyReachability: spec invalid", zap.String("resource", spec.Resource), zap.Error(err))
		return err
	}

	cmd := reachCmd(spec.DRPublicEndpoint, spec.WANPort)
	var last string
	for attempt := 1; attempt <= reachAttempts; attempt++ {
		start := time.Now()
		res, err := deploy.Exec(ctx, []string{spec.PrimaryNodeAddr}, cmd)
		ok := err == nil && res != nil && res.AllSuccess()
		log.Debug("VerifyReachability: probe returned", zap.String("resource", spec.Resource), zap.String("host", spec.PrimaryNodeAddr),
			zap.Int("attempt", attempt), zap.Bool("reachable", ok), zap.Error(err), zap.Duration("took", time.Since(start)))
		switch {
		case err != nil:
			last = err.Error()
		case res != nil && res.AllSuccess():
			return nil
		default:
			last = "connection refused or timed out"
		}
		if attempt < reachAttempts {
			select {
			case <-ctx.Done():
				log.Debug("VerifyReachability: context done while waiting to retry", zap.String("resource", spec.Resource),
					zap.Error(ctx.Err()))
				return ctx.Err()
			case <-time.After(reachRetryDelay):
			}
		}
	}
	// A closed port is a firewall only if something is listening behind it.
	unit := UnitInstance(spec.Resource)
	start := time.Now()
	res, err := deploy.Exec(ctx, []string{spec.DRNodeAddr}, "systemctl is-active "+unit)
	log.Debug("VerifyReachability: checked acceptor unit", zap.String("host", spec.DRNodeAddr), zap.String("unit", unit),
		zap.Bool("active", err == nil && res != nil && res.AllSuccess()), zap.Error(err), zap.Duration("took", time.Since(start)))
	if err == nil && res != nil && !res.AllSuccess() {
		state := ""
		for _, h := range res.Hosts {
			if h != nil {
				state = strings.TrimSpace(h.Output)
			}
		}
		log.Debug("VerifyReachability: acceptor not running", zap.String("host", spec.DRNodeAddr), zap.String("state", state))
		return fmt.Errorf("wanproxy: the acceptor %s on the DR node %s is not running (%s), so nothing listens on :%d; see journalctl -u %s there",
			unit, spec.DRNodeAddr, state, spec.WANPort, unit)
	}
	log.Debug("VerifyReachability: acceptor running but port closed from the primary", zap.String("resource", spec.Resource),
		zap.String("last_error", last))
	return fmt.Errorf(
		"wanproxy: DR endpoint %s:%d is not reachable over TCP from the primary node %s after %d attempts — open inbound TCP :%d on the DR firewall/security group (last error: %s)",
		spec.DRPublicEndpoint, spec.WANPort, spec.PrimaryNodeAddr, reachAttempts, spec.WANPort, last)
}

// Reachable does a single TCP reachability probe from the primary node to the
// DR WAN endpoint and reports whether it succeeded. Unlike VerifyReachability it
// never retries or blocks, so it is safe on a hot status path.
func Reachable(ctx context.Context, deploy DeploymentClient, spec ProxySpec) bool {
	if deploy == nil {
		zap.L().Debug("Reachable: no deployment client", zap.String("resource", spec.Resource))
		return false
	}
	start := time.Now()
	r, err := deploy.Exec(ctx, []string{spec.PrimaryNodeAddr}, reachCmd(spec.DRPublicEndpoint, spec.WANPort))
	ok := err == nil && r != nil && r.AllSuccess()
	zap.L().Debug("Reachable: probe returned", zap.String("resource", spec.Resource), zap.String("host", spec.PrimaryNodeAddr),
		zap.String("dr_endpoint", spec.DRPublicEndpoint), zap.Int("wan_port", spec.WANPort), zap.Bool("reachable", ok),
		zap.Error(err), zap.Duration("took", time.Since(start)))
	return ok
}
