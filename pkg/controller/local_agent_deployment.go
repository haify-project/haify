package controller

import (
	"context"
	"fmt"
	"os"

	"github.com/haify-project/haify/pkg/deployment"
	"go.uber.org/zap"
)

// localAgentDeployment keeps every command the controller runs away from
// local-agent nodes (node_local_agent.go), which it has no SSH access to. A
// call that names such a node runs on the others; the agent node gets a
// successful empty result in its place, because the agent there applies the
// same change itself from GetNodeAgentState. Filtering here, at the one place
// every command passes, is what lets attach, detach, resize and repair stay
// unaware of agents.
//
// A status read is different: an empty answer would read as "nothing here", so
// for an agent node it fails instead, and callers that ask several nodes in turn
// move on to one that can answer.
//
// Promotion and demotion are refused outright rather than skipped: an agent
// node's role is changed by the CSI node plugin on that node, and a promote
// reported as done but never run would leave a caller waiting on a Primary
// that never comes.
type localAgentDeployment struct {
	deploymentClient
	isAgent func(host string) bool
}

// split separates the hosts the controller can reach from the agent ones.
func (d *localAgentDeployment) split(hosts []string) (reachable, agents []string) {
	for _, h := range hosts {
		if d.isAgent(h) {
			agents = append(agents, h)
		} else {
			reachable = append(reachable, h)
		}
	}
	return reachable, agents
}

func skippedHost(h string) *deployment.HostResult {
	return &deployment.HostResult{Host: h, Success: true}
}

func unreadableHost(h string) *deployment.HostResult {
	return &deployment.HostResult{Host: h, Error: fmt.Errorf("%s is a local-agent node: the controller cannot read its status", h)}
}

// execOn runs fn on the reachable hosts and fills in the agent hosts.
func (d *localAgentDeployment) execOn(op string, hosts []string,
	fn func(reachable []string) (*deployment.ExecResult, error)) (*deployment.ExecResult, error) {
	return d.execFill(op, hosts, skippedHost, fn)
}

// execFill is execOn with the agent hosts' results made by fill.
func (d *localAgentDeployment) execFill(op string, hosts []string, fill func(string) *deployment.HostResult,
	fn func(reachable []string) (*deployment.ExecResult, error)) (*deployment.ExecResult, error) {
	reachable, agents := d.split(hosts)
	if len(agents) == 0 {
		return fn(hosts)
	}
	zap.L().Debug("localAgentDeployment: skipping local-agent hosts", zap.String("op", op),
		zap.Strings("agents", agents), zap.Strings("reachable", reachable))
	res := &deployment.ExecResult{Hosts: map[string]*deployment.HostResult{}}
	if len(reachable) > 0 {
		inner, err := fn(reachable)
		if err != nil {
			return inner, err
		}
		if inner != nil {
			res = inner
			if res.Hosts == nil {
				res.Hosts = map[string]*deployment.HostResult{}
			}
		}
	}
	for _, h := range agents {
		res.Hosts[h] = fill(h)
	}
	return res, nil
}

// configOn is execOn for the calls that return a ConfigResult.
func (d *localAgentDeployment) configOn(op string, hosts []string,
	fn func(reachable []string) (*deployment.ConfigResult, error)) (*deployment.ConfigResult, error) {
	reachable, agents := d.split(hosts)
	if len(agents) == 0 {
		return fn(hosts)
	}
	zap.L().Debug("localAgentDeployment: skipping local-agent hosts", zap.String("op", op),
		zap.Strings("agents", agents), zap.Strings("reachable", reachable))
	res := &deployment.ConfigResult{Success: true, Hosts: map[string]*deployment.HostResult{}}
	if len(reachable) > 0 {
		inner, err := fn(reachable)
		if err != nil {
			return inner, err
		}
		if inner != nil {
			res = inner
			if res.Hosts == nil {
				res.Hosts = map[string]*deployment.HostResult{}
			}
		}
	}
	for _, h := range agents {
		res.Hosts[h] = skippedHost(h)
	}
	return res, nil
}

func (d *localAgentDeployment) Exec(ctx context.Context, hosts []string, cmd string, opts ...deployment.ExecOption) (*deployment.ExecResult, error) {
	return d.execOn("Exec", hosts, func(h []string) (*deployment.ExecResult, error) {
		return d.deploymentClient.Exec(ctx, h, cmd, opts...)
	})
}

func (d *localAgentDeployment) DistributeConfig(ctx context.Context, hosts []string, content, remotePath string, opts ...deployment.ConfigOption) (*deployment.ConfigResult, error) {
	return d.configOn("DistributeConfig", hosts, func(h []string) (*deployment.ConfigResult, error) {
		return d.deploymentClient.DistributeConfig(ctx, h, content, remotePath, opts...)
	})
}

func (d *localAgentDeployment) DistributeSecret(ctx context.Context, hosts []string, content, relPath string) (*deployment.ConfigResult, error) {
	return d.configOn("DistributeSecret", hosts, func(h []string) (*deployment.ConfigResult, error) {
		return d.deploymentClient.DistributeSecret(ctx, h, content, relPath)
	})
}

func (d *localAgentDeployment) InstallFile(ctx context.Context, hosts []string, localPath, remotePath string, mode os.FileMode) (*deployment.ConfigResult, error) {
	return d.configOn("InstallFile", hosts, func(h []string) (*deployment.ConfigResult, error) {
		return d.deploymentClient.InstallFile(ctx, h, localPath, remotePath, mode)
	})
}

func (d *localAgentDeployment) DeleteConfig(ctx context.Context, hosts []string, remotePath string) error {
	reachable, agents := d.split(hosts)
	if len(agents) > 0 {
		zap.L().Debug("localAgentDeployment: skipping local-agent hosts", zap.String("op", "DeleteConfig"),
			zap.Strings("agents", agents), zap.Strings("reachable", reachable))
	}
	if len(reachable) == 0 {
		return nil
	}
	return d.deploymentClient.DeleteConfig(ctx, reachable, remotePath)
}

func (d *localAgentDeployment) DRBDUp(ctx context.Context, hosts []string, resource string) (*deployment.ExecResult, error) {
	return d.execOn("DRBDUp", hosts, func(h []string) (*deployment.ExecResult, error) {
		return d.deploymentClient.DRBDUp(ctx, h, resource)
	})
}

func (d *localAgentDeployment) DRBDDown(ctx context.Context, hosts []string, resource string) (*deployment.ExecResult, error) {
	return d.execOn("DRBDDown", hosts, func(h []string) (*deployment.ExecResult, error) {
		return d.deploymentClient.DRBDDown(ctx, h, resource)
	})
}

func (d *localAgentDeployment) DRBDCreateMD(ctx context.Context, hosts []string, resource string, maxPeers int) (*deployment.ExecResult, error) {
	return d.execOn("DRBDCreateMD", hosts, func(h []string) (*deployment.ExecResult, error) {
		return d.deploymentClient.DRBDCreateMD(ctx, h, resource, maxPeers)
	})
}

func (d *localAgentDeployment) DRBDStatus(ctx context.Context, hosts []string, resource string) (*deployment.ExecResult, error) {
	return d.execFill("DRBDStatus", hosts, unreadableHost, func(h []string) (*deployment.ExecResult, error) {
		return d.deploymentClient.DRBDStatus(ctx, h, resource)
	})
}

func (d *localAgentDeployment) DRBDStatusJSON(ctx context.Context, hosts []string, resource string) (*deployment.ExecResult, error) {
	return d.execFill("DRBDStatusJSON", hosts, unreadableHost, func(h []string) (*deployment.ExecResult, error) {
		return d.deploymentClient.DRBDStatusJSON(ctx, h, resource)
	})
}

func (d *localAgentDeployment) ReactorReload(ctx context.Context, hosts []string) (*deployment.ExecResult, error) {
	return d.execOn("ReactorReload", hosts, func(h []string) (*deployment.ExecResult, error) {
		return d.deploymentClient.ReactorReload(ctx, h)
	})
}

func (d *localAgentDeployment) DRBDPrimary(ctx context.Context, host, resource string, force bool) (*deployment.HostResult, error) {
	if d.isAgent(host) {
		zap.L().Debug("localAgentDeployment.DRBDPrimary: refusing, local-agent host", zap.String("host", host),
			zap.String("resource", resource))
		return nil, errLocalAgentRole(host, resource, "Primary")
	}
	return d.deploymentClient.DRBDPrimary(ctx, host, resource, force)
}

func (d *localAgentDeployment) DRBDSecondary(ctx context.Context, host, resource string) (*deployment.HostResult, error) {
	if d.isAgent(host) {
		zap.L().Debug("localAgentDeployment.DRBDSecondary: refusing, local-agent host", zap.String("host", host),
			zap.String("resource", resource))
		return nil, errLocalAgentRole(host, resource, "Secondary")
	}
	return d.deploymentClient.DRBDSecondary(ctx, host, resource)
}

func errLocalAgentRole(host, resource, role string) error {
	return fmt.Errorf("%s is a local-agent node: the Haify CSI node plugin there makes %s %s, the controller cannot",
		host, resource, role)
}
