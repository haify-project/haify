package csi

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// On a node the controller cannot SSH into (RHCOS on OpenShift), this plugin
// is the node's agent. It asks the controller which resources the node is a
// diskless client of, keeps their configuration in the host's /etc/drbd.d and
// applies it with drbdadm, and makes the node Primary and Secondary itself.
// The controller side is pkg/controller/node_local_agent.go.

// AgentBackend is what the agent asks the controller.
type AgentBackend interface {
	GetNodeAgentState(ctx context.Context, node string) ([]*haifypb.AgentResource, error)
}

// agentHeader opens every configuration file the agent writes. It is how the
// agent tells its own files from any others in /etc/drbd.d, and only its own
// are ever taken down and removed.
const agentHeader = "# Written by the Haify CSI node plugin as this node's local agent. Do not edit.\n"

// resourceNamePattern is what a resource name may be before it is used in a
// file name and a drbdadm argument.
var resourceNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.+-]*$`)

// commandRunner runs a command on this node and returns its combined output.
type commandRunner func(ctx context.Context, name string, args ...string) (string, error)

func execRunner(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

// LocalAgent keeps this node's DRBD resources in line with the controller.
type LocalAgent struct {
	backend AgentBackend
	node    string
	confDir string
	run     commandRunner
	log     *zap.Logger
	// mu serialises reconciles: the loop and a NodeStage that needs its
	// resource now.
	mu sync.Mutex
}

// NewLocalAgent returns the agent for node, writing configuration to confDir
// (the host's /etc/drbd.d, mounted into the plugin).
func NewLocalAgent(b AgentBackend, node, confDir string, log *zap.Logger) *LocalAgent {
	return &LocalAgent{backend: b, node: node, confDir: confDir, run: execRunner, log: log}
}

// Run reconciles every interval until ctx ends.
func (a *LocalAgent) Run(ctx context.Context, interval time.Duration) {
	a.log.Debug("LocalAgent.Run: starting", zap.String("node", a.node), zap.Duration("interval", interval))
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := a.Reconcile(ctx); err != nil {
			a.log.Warn("local agent: reconcile failed; will retry", zap.Error(err))
		}
		select {
		case <-ctx.Done():
			a.log.Debug("LocalAgent.Run: stopping", zap.String("node", a.node))
			return
		case <-t.C:
		}
	}
}

// Reconcile brings up or adjusts every resource the controller lists for this
// node and takes down the ones it wrote before that are no longer listed.
func (a *LocalAgent) Reconcile(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	start := time.Now()
	want, err := a.backend.GetNodeAgentState(ctx, a.node)
	a.log.Debug("LocalAgent.Reconcile: asked the controller", zap.String("node", a.node), zap.Int("resources", len(want)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("get agent state: %w", err)
	}
	wanted := map[string]bool{}
	var errs []string
	for _, r := range want {
		if !resourceNamePattern.MatchString(r.GetName()) {
			a.log.Debug("LocalAgent.Reconcile: skipping unusable resource name", zap.String("resource", r.GetName()))
			continue
		}
		wanted[r.GetName()] = true
		if err := a.apply(ctx, r.GetName(), r.GetConfig()); err != nil {
			errs = append(errs, err.Error())
		}
	}
	managed, err := a.managedResources()
	if err != nil {
		errs = append(errs, err.Error())
	}
	for _, name := range managed {
		if wanted[name] {
			continue
		}
		if err := a.remove(ctx, name); err != nil {
			errs = append(errs, err.Error())
		}
	}
	a.log.Debug("LocalAgent.Reconcile: done", zap.Int("wanted", len(wanted)), zap.Int("managed", len(managed)),
		zap.Int("errors", len(errs)), zap.Duration("took", time.Since(start)))
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

func (a *LocalAgent) resPath(name string) string {
	return filepath.Join(a.confDir, name+".res")
}

// apply writes the resource's configuration when it changed and brings the
// resource up, or adjusts it to the configuration.
func (a *LocalAgent) apply(ctx context.Context, name, config string) error {
	content := agentHeader + config
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	path := a.resPath(name)
	current, rerr := os.ReadFile(path)
	if rerr == nil && !bytes.HasPrefix(current, []byte(agentHeader)) {
		// Someone else's file: a node registered for SSH before, or a
		// resource set up by hand. Never overwrite it.
		a.log.Debug("LocalAgent.apply: refusing to replace a file the agent did not write", zap.String("path", path))
		return fmt.Errorf("%s exists and was not written by the Haify local agent", path)
	}
	changed := rerr != nil || string(current) != content
	up := a.isUp(ctx, name)
	a.log.Debug("LocalAgent.apply: state", zap.String("resource", name), zap.Bool("changed", changed), zap.Bool("up", up))
	if !changed && up {
		return nil
	}
	if changed {
		tmp := path + ".haify-tmp"
		if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
			a.log.Debug("LocalAgent.apply: writing config failed", zap.String("path", tmp), zap.Error(err))
			return fmt.Errorf("write %s: %w", tmp, err)
		}
		if err := os.Rename(tmp, path); err != nil {
			a.log.Debug("LocalAgent.apply: replacing config failed", zap.String("path", path), zap.Error(err))
			return fmt.Errorf("replace %s: %w", path, err)
		}
		a.log.Debug("LocalAgent.apply: config written", zap.String("path", path), zap.Int("bytes", len(content)))
	}
	verb := "adjust"
	if !up {
		verb = "up"
	}
	// A live migration allows two Primaries at runtime, and an adjust puts
	// back the configuration's "no" — on every peer, since DRBD passes the
	// option on. Another client attaching mid-migration changes the
	// configuration, so the window is kept across the adjust.
	twoPrimaries := up && a.allowsTwoPrimaries(ctx, name)
	start := time.Now()
	out, err := a.run(ctx, "drbdadm", verb, name)
	a.log.Debug("LocalAgent.apply: drbdadm ran", zap.String("resource", name), zap.String("verb", verb),
		zap.String("output", strings.TrimSpace(out)), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("drbdadm %s %s: %v: %s", verb, name, err, strings.TrimSpace(out))
	}
	if !up {
		a.log.Info("local agent: resource up", zap.String("resource", name))
	}
	if twoPrimaries {
		a.log.Debug("LocalAgent.apply: restoring allow-two-primaries after adjust", zap.String("resource", name))
		if err := a.SetTwoPrimaries(ctx, name, true); err != nil {
			return err
		}
	}
	return nil
}

// remove takes a resource the controller no longer lists down and deletes its
// configuration. A resource still Primary here is in use by a Pod and is left
// alone: the controller's list may be behind, the Pod is not.
func (a *LocalAgent) remove(ctx context.Context, name string) error {
	st, err := a.status(ctx, name)
	if err == nil && st.Role == "Primary" {
		a.log.Warn("local agent: resource no longer listed for this node but still Primary here; leaving it up",
			zap.String("resource", name))
		return nil
	}
	if err == nil {
		start := time.Now()
		out, derr := a.run(ctx, "drbdadm", "down", name)
		a.log.Debug("LocalAgent.remove: drbdadm down ran", zap.String("resource", name),
			zap.String("output", strings.TrimSpace(out)), zap.Duration("took", time.Since(start)), zap.Error(derr))
		if derr != nil {
			return fmt.Errorf("drbdadm down %s: %v: %s", name, derr, strings.TrimSpace(out))
		}
	} else {
		a.log.Debug("LocalAgent.remove: resource not up", zap.String("resource", name), zap.Error(err))
	}
	if err := os.Remove(a.resPath(name)); err != nil && !os.IsNotExist(err) {
		a.log.Debug("LocalAgent.remove: deleting config failed", zap.String("resource", name), zap.Error(err))
		return fmt.Errorf("remove %s: %w", a.resPath(name), err)
	}
	a.log.Info("local agent: resource down", zap.String("resource", name))
	return nil
}

// managedResources lists the resources whose configuration the agent wrote.
func (a *LocalAgent) managedResources() ([]string, error) {
	entries, err := os.ReadDir(a.confDir)
	if err != nil {
		a.log.Debug("LocalAgent.managedResources: reading directory failed", zap.String("dir", a.confDir), zap.Error(err))
		return nil, fmt.Errorf("read %s: %w", a.confDir, err)
	}
	var out []string
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".res")
		if !ok || e.IsDir() {
			continue
		}
		f, err := os.Open(filepath.Join(a.confDir, e.Name()))
		if err != nil {
			a.log.Debug("LocalAgent.managedResources: open failed", zap.String("file", e.Name()), zap.Error(err))
			continue
		}
		first, _ := bufio.NewReader(f).ReadString('\n')
		_ = f.Close()
		if first == agentHeader {
			out = append(out, name)
		}
	}
	a.log.Debug("LocalAgent.managedResources: listed", zap.Strings("resources", out))
	return out, nil
}
