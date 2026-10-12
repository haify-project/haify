package wanproxy

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"
)

// HostResult is the per-host outcome of a deploy/exec operation.
type HostResult struct {
	Host    string
	Output  string
	Success bool
	Err     error
}

// Result aggregates per-host outcomes. It mirrors the AllSuccess/FailedHosts
// contract of pkg/deployment so orchestration here checks multi-host results the
// same way pkg/controller does, without importing the deployment package's
// concrete types (keeping the interface trivially mockable).
type Result struct {
	Hosts map[string]*HostResult
}

// AllSuccess reports whether every host succeeded.
func (r *Result) AllSuccess() bool {
	for _, h := range r.Hosts {
		if h == nil || !h.Success {
			return false
		}
	}
	return true
}

// FailedHosts lists the hosts that did not succeed.
func (r *Result) FailedHosts() []string {
	var failed []string
	for host, h := range r.Hosts {
		if h == nil || !h.Success {
			failed = append(failed, host)
		}
	}
	return failed
}

// FailureDetails renders the failed hosts with the reason each one gave, so a
// WAN provisioning error names the problem instead of only the addresses. See
// deployment.ExecResult.FailureDetails.
func (r *Result) FailureDetails() string {
	failed := r.FailedHosts()
	if len(failed) == 0 {
		zap.L().Debug("Result.FailureDetails: no failed hosts", zap.Int("hosts", len(r.Hosts)))
		return ""
	}
	zap.L().Debug("Result.FailureDetails: rendering failures", zap.Strings("failed_hosts", failed), zap.Int("hosts", len(r.Hosts)))
	sort.Strings(failed)

	parts := make([]string, 0, len(failed))
	for _, host := range failed {
		reason := ""
		if h := r.Hosts[host]; h != nil {
			reason = strings.TrimSpace(h.Output)
			if reason == "" && h.Err != nil {
				reason = strings.TrimSpace(h.Err.Error())
			}
		}
		if reason == "" {
			reason = "no output"
		}
		parts = append(parts, fmt.Sprintf("%s: %s", host, strings.Join(strings.Fields(reason), " ")))
	}
	return strings.Join(parts, "; ")
}

// ensureBinary reads the controller-local haify-proxy binary and installs it at
// NodeBinaryPath on every host, then marks it executable. Distributing the same
// bytes is idempotent, so re-provisioning simply overwrites with identical
// content.
// ensureBinaries pushes the right binary to each host.
//
// Pushing one file to every node is correct only while the fleet is uniform.
// The moment it is not — an arm64 box at home and an x86_64 rental off-site —
// the same push installs an unrunnable file on half of them, and the failure
// surfaces far from the cause: systemd reports 203/EXEC on the node, while the
// operator sees a DRBD connection that never forms.
func ensureBinaries(ctx context.Context, deploy DeploymentClient, hosts []string, spec ProxySpec) error {
	// Group hosts by the local file they need, so a uniform fleet still gets a
	// single push rather than one per node.
	log := zap.L()
	log.Debug("ensureBinaries: start", zap.String("resource", spec.Resource), zap.Strings("hosts", hosts),
		zap.String("binary_path", spec.BinaryPath), zap.Bool("per_node", spec.BinaryFor != nil))
	byPath := map[string][]string{}
	for _, h := range hosts {
		path := spec.BinaryPath
		if spec.BinaryFor != nil {
			path = spec.BinaryFor(h)
		}
		if path == "" {
			log.Debug("ensureBinaries: no local binary for host, assuming pre-staged", zap.String("host", h))
			continue // pre-staged on this node
		}
		log.Debug("ensureBinaries: host gets binary", zap.String("host", h), zap.String("path", path))
		byPath[path] = append(byPath[path], h)
	}
	for path, group := range byPath {
		if err := ensureBinary(ctx, deploy, group, path); err != nil {
			log.Debug("ensureBinaries: push failed", zap.String("path", path), zap.Strings("hosts", group), zap.Error(err))
			return err
		}
	}
	log.Debug("ensureBinaries: done", zap.Int("binaries", len(byPath)))
	return nil
}

func ensureBinary(ctx context.Context, deploy DeploymentClient, hosts []string, localPath string) error {
	start := time.Now()
	data, err := os.ReadFile(localPath)
	zap.L().Debug("ensureBinary: read local binary", zap.String("path", localPath), zap.Int("bytes", len(data)),
		zap.Strings("hosts", hosts), zap.Error(err), zap.Duration("took", time.Since(start)))
	if err != nil {
		return fmt.Errorf("wanproxy: read haify-proxy binary %q: %w", localPath, err)
	}
	if err := distribute(ctx, deploy, hosts, string(data), NodeBinaryPath, "distribute binary"); err != nil {
		return err
	}
	return run(ctx, deploy, hosts, fmt.Sprintf("sudo chmod 0755 %s", NodeBinaryPath), "chmod binary")
}

// distribute pushes content to remotePath on hosts and surfaces per-host
// failures, matching pkg/controller's error style.
//
// content is never traced: it can be the shared private key.
func distribute(ctx context.Context, deploy DeploymentClient, hosts []string, content, remotePath, desc string) error {
	log := zap.L()
	log.Debug("distribute: start", zap.String("step", desc), zap.Strings("hosts", hosts),
		zap.String("path", remotePath), zap.Int("content_bytes", len(content)))
	start := time.Now()
	res, err := deploy.DistributeConfig(ctx, hosts, content, remotePath)
	if err != nil {
		log.Debug("distribute: call failed", zap.String("step", desc), zap.Strings("hosts", hosts), zap.Error(err),
			zap.Duration("took", time.Since(start)))
		return fmt.Errorf("wanproxy: %s: %w", desc, err)
	}
	if res != nil && !res.AllSuccess() {
		log.Debug("distribute: some hosts failed", zap.String("step", desc), zap.Strings("failed_hosts", res.FailedHosts()),
			zap.Duration("took", time.Since(start)))
		return fmt.Errorf("wanproxy: %s failed: %s", desc, res.FailureDetails())
	}
	log.Debug("distribute: done", zap.String("step", desc), zap.Strings("hosts", hosts), zap.Duration("took", time.Since(start)))
	return nil
}

// requireBinaries fails unless every host now has a runnable haify-proxy, pushed
// by ensureBinaries or staged beforehand. Without one the unit crash-loops with
// 203/EXEC on the node, and the only symptom anywhere else is a WAN port that
// never opens — which the reachability probe used to report as a firewall.
func requireBinaries(ctx context.Context, deploy DeploymentClient, hosts []string) error {
	log := zap.L()
	start := time.Now()
	res, err := deploy.Exec(ctx, hosts, "test -x "+NodeBinaryPath)
	log.Debug("requireBinaries: checked for the binary", zap.Strings("hosts", hosts), zap.String("path", NodeBinaryPath),
		zap.Error(err), zap.Duration("took", time.Since(start)))
	if err != nil {
		return fmt.Errorf("wanproxy: check for %s: %w", NodeBinaryPath, err)
	}
	if res != nil && !res.AllSuccess() {
		missing := res.FailedHosts()
		sort.Strings(missing)
		log.Debug("requireBinaries: binary missing", zap.Strings("failed_hosts", missing))
		return fmt.Errorf("wanproxy: no executable %s on %s, and the controller has none for that node's architecture "+
			"(it looks for %s-<arch>, or %s for its own); install haify-proxy there",
			NodeBinaryPath, strings.Join(missing, ", "), NodeBinaryPath, NodeBinaryPath)
	}
	return nil
}

// run executes cmd on hosts and surfaces per-host failures.
func run(ctx context.Context, deploy DeploymentClient, hosts []string, cmd, desc string) error {
	log := zap.L()
	log.Debug("run: start", zap.String("step", desc), zap.Strings("hosts", hosts), zap.String("cmd", cmd))
	start := time.Now()
	res, err := deploy.Exec(ctx, hosts, cmd)
	if err != nil {
		log.Debug("run: call failed", zap.String("step", desc), zap.Strings("hosts", hosts), zap.Error(err),
			zap.Duration("took", time.Since(start)))
		return fmt.Errorf("wanproxy: %s: %w", desc, err)
	}
	if res != nil && !res.AllSuccess() {
		log.Debug("run: some hosts failed", zap.String("step", desc), zap.Strings("failed_hosts", res.FailedHosts()),
			zap.Duration("took", time.Since(start)))
		return fmt.Errorf("wanproxy: %s failed: %s", desc, res.FailureDetails())
	}
	log.Debug("run: done", zap.String("step", desc), zap.Strings("hosts", hosts), zap.Duration("took", time.Since(start)))
	return nil
}
