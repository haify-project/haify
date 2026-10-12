package controller

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/gateway"
)

// Preferred nodes for an HA resource (`haify ha set-preferred`).
//
// drbd-reactor's promoter can be told which nodes to prefer: `preferred-nodes`
// lists them in order, and each node waits a moment longer the further down
// the list it is before it tries to promote, so the first one that can wins.
// `preferred-nodes-policy = "start-only"` (drbd-reactor 1.9+) uses the order
// only to pick where the resource starts; "always" also moves it back to a
// more preferred node when that one returns, which is a failover of its own.
//
// It is a preference, not a fence: it does not stop any node from promoting
// and plays no part in split-brain avoidance, which is DRBD quorum's job.
// drbd-reactor compares the entries with each node's `uname -n`, so haify node
// names are written as the hostnames recorded at registration.
//
// The new config is written on every node that runs the promoter, and
// drbd-reactor is reloaded on all of them except the one where the resource is
// Primary: the order only matters to the nodes that would take over, and
// reloading a changed promoter where it runs can restart what it started. The
// Primary's drbd-reactor picks the file up at its next reload.

var preferredLineRE = regexp.MustCompile(`(?m)^preferred-nodes(-policy)?\s*=.*\n`)

// withPreferredNodes sets (or, with no nodes, removes) the preferred-nodes
// lines of a promoter config.
func withPreferredNodes(content string, hostnames []string, policy string) string {
	log := zap.L()
	out := preferredLineRE.ReplaceAllString(content, "")
	if len(hostnames) == 0 {
		log.Debug("withPreferredNodes: no nodes, preference removed")
		return out
	}
	quoted := make([]string, len(hostnames))
	for i, h := range hostnames {
		quoted[i] = fmt.Sprintf("%q", h)
	}
	lines := "preferred-nodes = [" + strings.Join(quoted, ", ") + "]\n"
	if policy != "" {
		lines += fmt.Sprintf("preferred-nodes-policy = %q\n", policy)
	}
	header := regexp.MustCompile(`(?m)^\[promoter\.resources\.[^\]]+\]\n`)
	if loc := header.FindStringIndex(out); loc != nil {
		log.Debug("withPreferredNodes: inserted under the promoter resource header",
			zap.Strings("hostnames", hostnames), zap.String("policy", policy))
		return out[:loc[1]] + lines + out[loc[1]:]
	}
	log.Debug("withPreferredNodes: no promoter resource header, appended at the end",
		zap.Strings("hostnames", hostnames), zap.String("policy", policy))
	return out + lines
}

// SetHaPreferredNodes records and applies the preferred nodes of resource's
// HA config. An empty list removes the preference.
func (rm *ResourceManager) SetHaPreferredNodes(ctx context.Context, resource string, nodes []string, policy string) error {
	log := rm.controller.traceLogger()
	log.Debug("ResourceManager.SetHaPreferredNodes: start", zap.String("resource", resource),
		zap.Strings("nodes", nodes), zap.String("policy", policy))
	switch policy {
	case "", "start-only", "always":
	default:
		log.Debug("ResourceManager.SetHaPreferredNodes: unknown policy", zap.String("policy", policy))
		return fmt.Errorf("the policy is start-only or always, not %q", policy)
	}
	start := time.Now()
	ha, err := rm.controller.db.GetHaConfig(ctx, resource)
	log.Debug("ResourceManager.SetHaPreferredNodes: database read HA config", zap.String("resource", resource),
		zap.Bool("found", err == nil && ha != nil), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil || ha == nil {
		return fmt.Errorf("%s has no HA config; create it with `haify ha create` first", resource)
	}
	start = time.Now()
	dbRes, err := rm.controller.db.GetResource(ctx, resource)
	log.Debug("ResourceManager.SetHaPreferredNodes: database read resource", zap.String("resource", resource),
		zap.Bool("found", err == nil && dbRes != nil), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil || dbRes == nil {
		return fmt.Errorf("resource %q not found", resource)
	}
	members := splitCSV(dbRes.Nodes)
	hostnames := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if !contains(members, n) || (dbRes.WANMode && n == dbRes.DRNode) {
			log.Debug("ResourceManager.SetHaPreferredNodes: node holds no primary-site replica", zap.String("resource", resource),
				zap.String("node", n), zap.Strings("members", members), zap.Bool("wan", dbRes.WANMode), zap.String("dr_node", dbRes.DRNode))
			return fmt.Errorf("%s holds no primary-site replica of %s, so it cannot be preferred", n, resource)
		}
		hostname := rm.uname(n)
		log.Debug("ResourceManager.SetHaPreferredNodes: preferred node", zap.String("node", n), zap.String("hostname", hostname))
		hostnames = append(hostnames, hostname)
	}
	hosts, err := rm.failoverHosts(ctx, resource)
	if err != nil {
		log.Debug("ResourceManager.SetHaPreferredNodes: failover hosts unknown", zap.String("resource", resource), zap.Error(err))
		return err
	}
	cfg := haTomlPath(resource)
	start = time.Now()
	dump, err := rm.deployment.Exec(ctx, hosts, gateway.ConfigDumpCommand(cfg, cfg+".disabled"))
	log.Debug("ResourceManager.SetHaPreferredNodes: dumped promoter configs", zap.String("resource", resource),
		zap.Strings("hosts", hosts), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return err
	}
	primary := ""
	if info, err := rm.GetResource(ctx, resource); err == nil {
		for node, st := range info.NodeStates {
			if strings.EqualFold(st.Role, "Primary") {
				primary = rm.controller.ResolveHost(node)
			}
		}
	} else {
		log.Debug("ResourceManager.SetHaPreferredNodes: resource state unknown, reloading every node",
			zap.String("resource", resource), zap.Error(err))
	}
	log.Debug("ResourceManager.SetHaPreferredNodes: Primary found", zap.String("resource", resource), zap.String("primary", primary))
	var reload []string
	for _, h := range hosts {
		hr := dump.Hosts[h]
		if hr == nil || !hr.Success {
			log.Debug("ResourceManager.SetHaPreferredNodes: node did not answer the dump", zap.String("host", h),
				zap.Bool("has_result", hr != nil))
			return fmt.Errorf("%s did not answer; nothing was changed there", rm.nodeLabel(h))
		}
		files, err := gateway.ParseConfigDump(hr.Output)
		if err != nil {
			log.Debug("ResourceManager.SetHaPreferredNodes: config dump did not parse", zap.String("host", h), zap.Error(err))
			return err
		}
		for path, content := range files {
			start := time.Now()
			if _, err := rm.deployment.DistributeConfig(ctx, []string{h}, withPreferredNodes(content, hostnames, policy), path); err != nil {
				log.Debug("ResourceManager.SetHaPreferredNodes: write failed", zap.String("host", h), zap.String("path", path),
					zap.Duration("took", time.Since(start)), zap.Error(err))
				return fmt.Errorf("write %s on %s: %w", path, rm.nodeLabel(h), err)
			}
			log.Debug("ResourceManager.SetHaPreferredNodes: wrote config", zap.String("host", h), zap.String("path", path),
				zap.Duration("took", time.Since(start)))
		}
		if h != primary {
			reload = append(reload, h)
		} else {
			log.Debug("ResourceManager.SetHaPreferredNodes: not reloading the Primary", zap.String("host", h))
		}
	}
	if len(reload) > 0 {
		start := time.Now()
		_, err := rm.deployment.ReactorReload(ctx, reload)
		log.Debug("ResourceManager.SetHaPreferredNodes: drbd-reactor reload", zap.Strings("hosts", reload),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			rm.controller.logger.Warn("Reloading drbd-reactor failed", zap.String("resource", resource), zap.Error(err))
		}
	}
	ha.PreferredNodes, ha.PreferredNodesPolicy = nodes, policy
	start = time.Now()
	err = rm.controller.db.SaveHaConfig(ctx, ha)
	log.Debug("ResourceManager.SetHaPreferredNodes: database save HA config", zap.String("resource", resource),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	return err
}

// uname is the hostname drbd-reactor knows node by.
func (rm *ResourceManager) uname(node string) string {
	log := rm.controller.traceLogger()
	if nodes, err := rm.controller.nodes.ListNodes(context.Background()); err == nil {
		for _, n := range nodes {
			if n.Name == node && n.Hostname != "" {
				log.Debug("ResourceManager.uname: registered hostname", zap.String("node", node), zap.String("hostname", n.Hostname))
				return n.Hostname
			}
		}
	} else {
		log.Debug("ResourceManager.uname: listing nodes failed", zap.String("node", node), zap.Error(err))
	}
	log.Debug("ResourceManager.uname: no hostname recorded, using the node name", zap.String("node", node))
	return node
}
