package controller

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"strings"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/deployment"
)

// More than one network between replicas. A node can list extra replication
// addresses (one per additional network, in the same order on every node);
// each pair of nodes then gets one DRBD path per network both of them are on,
// and DRBD keeps the connection up over whichever path works when one link or
// switch fails.
//
// DRBD expresses that with explicit `connection { path {...} path {...} }`
// sections, which cannot sit beside the `connection-mesh` the rest of the
// controller builds and edits: drbdadm then creates every peer twice. So the
// mesh stays the form the controller reads and edits, and the explicit form
// exists only on the nodes. resPathsDeployment expands the mesh into paths on
// every write of /etc/drbd.d/*.res and folds them back on every read, so none
// of the code that edits configs has to know paths exist. A resource whose
// pairs all have a single path is written exactly as before.

const (
	pathsMarker    = "# haify-paths:"
	pathsEndMarker = "# haify-paths end"
)

// resAddr is an `on` block's address, split.
type resAddr struct{ ip, port string }

var onAddressRE = regexp.MustCompile(`^address\s+(?:ipv4\s+)?([0-9.]+):(\d+);`)

// onAddresses maps each `on` block's host name to its address.
func onAddresses(content string) map[string]resAddr {
	out := map[string]resAddr{}
	cur := ""
	depth := 0
	for _, line := range strings.Split(content, "\n") {
		t := strings.TrimSpace(line)
		if cur == "" && depth == 1 && strings.HasPrefix(t, "on ") && strings.Contains(t, "{") {
			if f := strings.Fields(t); len(f) >= 2 {
				cur = f[1]
			}
		} else if cur != "" {
			if m := onAddressRE.FindStringSubmatch(t); m != nil {
				out[cur] = resAddr{m[1], m[2]}
			}
		}
		depth += strings.Count(line, "{") - strings.Count(line, "}")
		if cur != "" && depth <= 1 {
			cur = ""
		}
	}
	zap.L().Debug("onAddresses: parsed on-block addresses", zap.Int("count", len(out)))
	return out
}

// meshHosts returns the hosts of the config's connection-mesh, or nil.
func meshHosts(content string) []string {
	idx := strings.Index(content, "connection-mesh")
	if idx < 0 {
		zap.L().Debug("meshHosts: config has no connection-mesh")
		return nil
	}
	rest := content[idx:]
	h := strings.Index(rest, "hosts")
	end := strings.Index(rest, ";")
	if h < 0 || end < h {
		zap.L().Debug("meshHosts: connection-mesh has no hosts line")
		return nil
	}
	hosts := strings.Fields(rest[h+len("hosts") : end])
	zap.L().Debug("meshHosts: mesh hosts", zap.Strings("hosts", hosts))
	return hosts
}

// collapseReplicationPaths turns the explicit path sections written by
// expandReplicationPaths back into the mesh they came from.
func collapseReplicationPaths(content string) string {
	if !strings.Contains(content, pathsMarker) {
		return content
	}
	zap.L().Debug("collapseReplicationPaths: folding explicit paths back into the mesh", zap.Int("bytes", len(content)))
	var out []string
	skipping := false
	for _, line := range strings.Split(content, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, pathsMarker):
			skipping = true
			// The blank lines expansion left before the block go with it, so a
			// config read and written any number of times stays the same.
			for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
				out = out[:len(out)-1]
			}
			if hosts, ok := strings.CutPrefix(strings.TrimSpace(strings.TrimPrefix(t, pathsMarker)), "mesh "); ok {
				out = append(out, strings.Split(strings.TrimRight(buildConnectionMesh(strings.Fields(hosts)), "\n"), "\n")...)
			}
		case t == pathsEndMarker:
			skipping = false
		case !skipping:
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// expandReplicationPaths writes one path per shared network for every pair of
// LAN hosts, when at least one pair shares more than one. extra maps a DRBD
// host name to its addresses beyond the one in its `on` block.
func expandReplicationPaths(content string, extra map[string][]string) string {
	log := zap.L()
	content = collapseReplicationPaths(content)
	if len(extra) == 0 {
		log.Debug("expandReplicationPaths: no node has extra paths; config unchanged")
		return content
	}
	addrs := onAddresses(content)
	hosts := meshHosts(content)
	header := ""
	if hosts != nil {
		header = "mesh " + strings.Join(hosts, " ")
	} else if blocks := parseOnBlocks(content); len(blocks) == 2 && !strings.Contains(content, "connection {") {
		// Two hosts and no connection sections: DRBD pairs them implicitly.
		hosts, header = onHostNames(blocks), "implicit"
	}
	log.Debug("expandReplicationPaths: hosts to pair", zap.Strings("hosts", hosts), zap.String("form", header),
		zap.Int("nodes_with_paths", len(extra)))
	if len(hosts) < 2 {
		log.Debug("expandReplicationPaths: fewer than two hosts; config unchanged")
		return content
	}

	pathsFor := func(a, b string) [][2]string {
		pa, pb := addrs[a], addrs[b]
		if pa.ip == "" || pb.ip == "" || strings.HasPrefix(pa.ip, "127.") || strings.HasPrefix(pb.ip, "127.") {
			return nil
		}
		paths := [][2]string{{pa.ip + ":" + pa.port, pb.ip + ":" + pb.port}}
		ea, eb := extra[a], extra[b]
		for i := 0; i < len(ea) && i < len(eb); i++ {
			paths = append(paths, [2]string{ea[i] + ":" + pa.port, eb[i] + ":" + pb.port})
		}
		return paths
	}
	multi := false
	for i := range hosts {
		for j := i + 1; j < len(hosts); j++ {
			if len(pathsFor(hosts[i], hosts[j])) > 1 {
				multi = true
			}
		}
	}
	if !multi {
		log.Debug("expandReplicationPaths: no pair shares a second network; config unchanged")
		return content
	}

	var b strings.Builder
	fmt.Fprintf(&b, "\n    %s %s\n", pathsMarker, header)
	for i := range hosts {
		for j := i + 1; j < len(hosts); j++ {
			paths := pathsFor(hosts[i], hosts[j])
			log.Debug("expandReplicationPaths: pair", zap.String("node", hosts[i]), zap.String("peer", hosts[j]), zap.Int("paths", len(paths)))
			if len(paths) == 0 {
				log.Debug("expandReplicationPaths: a host's address cannot be read; leaving the mesh alone",
					zap.String("node", hosts[i]), zap.String("peer", hosts[j]))
				return content // a host whose address cannot be read: leave the mesh alone
			}
			b.WriteString("    connection {\n")
			for _, p := range paths {
				fmt.Fprintf(&b, "        path {\n            host %s address %s;\n            host %s address %s;\n        }\n",
					hosts[i], p[0], hosts[j], p[1])
			}
			b.WriteString("    }\n")
		}
	}
	fmt.Fprintf(&b, "    %s\n", pathsEndMarker)
	out, err := insertBeforeResourceClose(stripConnectionMesh(content), b.String())
	if err != nil {
		log.Debug("expandReplicationPaths: could not insert paths; config unchanged", zap.Error(err))
		return content
	}
	log.Debug("expandReplicationPaths: wrote explicit paths", zap.Int("hosts", len(hosts)))
	return out
}

// validateReplicationPaths checks a node's extra replication addresses.
func validateReplicationPaths(addrs []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, a := range addrs {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		ip := net.ParseIP(a)
		if ip == nil || ip.To4() == nil {
			zap.L().Debug("validateReplicationPaths: not IPv4", zap.String("address", a))
			return nil, fmt.Errorf("%q is not an IPv4 address", a)
		}
		if ip.IsLoopback() {
			zap.L().Debug("validateReplicationPaths: loopback", zap.String("address", a))
			return nil, fmt.Errorf("%s is a loopback address", a)
		}
		if seen[a] {
			zap.L().Debug("validateReplicationPaths: duplicate", zap.String("address", a))
			return nil, fmt.Errorf("%s is listed twice", a)
		}
		seen[a] = true
		out = append(out, a)
	}
	zap.L().Debug("validateReplicationPaths: valid", zap.Strings("addresses", out))
	return out, nil
}

// replicationPathsByDRBDName is every node's extra replication addresses,
// keyed by the host name its .res `on` blocks use.
func (nm *NodeManager) replicationPathsByDRBDName() map[string][]string {
	nm.mu.RLock()
	defer nm.mu.RUnlock()
	out := map[string][]string{}
	for _, n := range nm.nodes {
		if len(n.ReplicationPaths) == 0 {
			continue
		}
		name := n.Hostname
		if name == "" {
			name = n.Name
		}
		out[name] = append([]string(nil), n.ReplicationPaths...)
	}
	nm.replicationPathsLog().Debug("NodeManager.replicationPathsByDRBDName: nodes with extra paths", zap.Int("count", len(out)))
	return out
}

// resPathsDeployment is the deployment client every DRBD config passes
// through; see the top of this file.
type resPathsDeployment struct {
	deploymentClient
	extra func() map[string][]string
}

func isResConfigPath(p string) bool {
	return strings.HasPrefix(p, "/etc/drbd.d/") && strings.HasSuffix(p, ".res")
}

func (d *resPathsDeployment) DistributeConfig(ctx context.Context, hosts []string, content, remotePath string, opts ...deployment.ConfigOption) (*deployment.ConfigResult, error) {
	if isResConfigPath(remotePath) {
		zap.L().Debug("resPathsDeployment.DistributeConfig: expanding replication paths", zap.String("path", remotePath),
			zap.Strings("hosts", hosts))
		content = expandReplicationPaths(content, d.extra())
	}
	return d.deploymentClient.DistributeConfig(ctx, hosts, content, remotePath, opts...)
}

func (d *resPathsDeployment) Exec(ctx context.Context, hosts []string, cmd string, opts ...deployment.ExecOption) (*deployment.ExecResult, error) {
	res, err := d.deploymentClient.Exec(ctx, hosts, cmd, opts...)
	if res != nil && strings.Contains(cmd, "cat ") && strings.Contains(cmd, "/etc/drbd.d/") {
		zap.L().Debug("resPathsDeployment.Exec: collapsing replication paths in config read", zap.Strings("hosts", hosts))
		for _, h := range res.Hosts {
			if h != nil {
				h.Output = collapseReplicationPaths(h.Output)
			}
		}
	}
	return res, err
}
