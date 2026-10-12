package gateway

import (
	"context"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Reading a gateway's promoter config back from the nodes that hold it.
//
// Every edit of an existing gateway — a LUN, an initiator, CHAP, a namespace,
// a host NQN, an NFS export — is read-modify-write of the promoter config. The
// config lives in /etc/drbd-reactor.d on the resource's diskful nodes and
// nowhere else; the controller is usually not one of them. Reading the
// controller's own filesystem found nothing there (the edit failed) or, worse,
// a stale copy left from an earlier placement (the edit silently reverted
// everything changed since).
//
// The nodes are written together, so they normally hold identical copies. When
// they do not — a node was down for the last write, or someone edited a file by
// hand — one copy is chosen deterministically and the others are named in a
// warning; the write that follows an edit then makes them identical again.

// configDumpMarker starts every line the dump script prints, so the parser can
// pick them out of whatever else sudo or the login shell writes.
const configDumpMarker = "haify-gateway-config"

// disabledSuffix marks a stopped gateway's config: drbd-reactor ignores it.
const disabledSuffix = ".disabled"

// gatewayConfig is a gateway's promoter config as read from its nodes.
type gatewayConfig struct {
	content string
	// disabled is set when the gateway is stopped and the nodes hold only the
	// .toml.disabled copy. An edit must go back there: writing the live .toml
	// would start the gateway as a side effect of adding a LUN.
	disabled bool
}

// nodeConfigCopy is one node's copy of a gateway config.
type nodeConfigCopy struct {
	host     string
	disabled bool
	content  string
}

// scriptCmd wraps a shell script for dispatch. Dispatch runs commands as
// sh -c "..." (double quotes), so $variables are expanded — that is, emptied —
// by the outer shell before the script runs; base64 makes the script opaque to
// that quoting chain.
func scriptCmd(script string) string {
	encoded := base64.StdEncoding.EncodeToString([]byte(script))
	return fmt.Sprintf("echo %s | base64 -d | sudo /bin/sh", encoded)
}

// configDumpScript prints every existing file matching patterns as
// "<marker> <path> <base64 content>". Base64 keeps the content one line and
// byte-exact, trailing newline included, whatever dispatch does to whitespace.
// A file that cannot be read fails the host rather than reading as empty.
func configDumpScript(patterns ...string) string {
	return fmt.Sprintf(`for f in %s; do
  [ -f "$f" ] || continue
  b=$(base64 -w0 <"$f") || exit 1
  printf '%%s %%s %%s\n' '%s' "$f" "$b"
done
true
`, strings.Join(patterns, " "), configDumpMarker)
}

// parseConfigDump returns the files a configDumpScript run printed, by path.
func parseConfigDump(out string) (map[string]string, error) {
	files := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != configDumpMarker {
			continue
		}
		content := ""
		if len(fields) > 2 {
			raw, err := base64.StdEncoding.DecodeString(fields[2])
			if err != nil {
				zap.L().Debug("parseConfigDump: undecodable file", zap.String("file", fields[1]), zap.Error(err))
				return nil, fmt.Errorf("decode %s: %w", fields[1], err)
			}
			content = string(raw)
		}
		// Only the size: a gateway config can hold CHAP credentials.
		zap.L().Debug("parseConfigDump: file", zap.String("file", fields[1]), zap.Int("size_bytes", len(content)))
		files[fields[1]] = content
	}
	zap.L().Debug("parseConfigDump: done", zap.Int("count", len(files)))
	return files, nil
}

// dumpNodeConfigs runs configDumpScript on hosts and returns, per host that
// answered, the files it holds. Hosts that did not answer are left out.
func (m *Manager) dumpNodeConfigs(ctx context.Context, hosts []string, patterns ...string) (map[string]map[string]string, error) {
	log := m.log()
	log.Debug("dumpNodeConfigs: start", zap.Strings("hosts", hosts), zap.Strings("patterns", patterns))
	if len(hosts) == 0 {
		log.Debug("dumpNodeConfigs: no hosts")
		return nil, fmt.Errorf("no nodes to read gateway configs from")
	}
	reader, ok := m.deployment.(HostOutputReader)
	if !ok {
		log.Debug("dumpNodeConfigs: deployment client cannot return output")
		return nil, fmt.Errorf("deployment client cannot read node output")
	}
	start := time.Now()
	outputs, err := reader.ExecOutput(ctx, hosts, scriptCmd(configDumpScript(patterns...)))
	if err != nil {
		log.Debug("dumpNodeConfigs: read failed", zap.Strings("hosts", hosts), zap.Duration("took", time.Since(start)), zap.Error(err))
		return nil, fmt.Errorf("failed to read gateway configs on %s: %w", strings.Join(hosts, ","), err)
	}
	log.Debug("dumpNodeConfigs: nodes answered", zap.Int("count", len(outputs)), zap.Duration("took", time.Since(start)))
	if len(outputs) == 0 {
		log.Debug("dumpNodeConfigs: no node answered", zap.Strings("hosts", hosts))
		return nil, fmt.Errorf("no node answered when reading gateway configs on %s", strings.Join(hosts, ","))
	}
	byHost := make(map[string]map[string]string, len(outputs))
	for host, out := range outputs {
		files, err := parseConfigDump(out)
		if err != nil {
			m.logger.Warn("Unreadable gateway config dump", zap.String("host", host), zap.Error(err))
			continue
		}
		log.Debug("dumpNodeConfigs: host files", zap.String("host", host), zap.Int("count", len(files)))
		byHost[host] = files
	}
	return byHost, nil
}

// readGatewayConfig reads a gateway's promoter config from the nodes that may
// run it (see promoterHosts). A live .toml (or its .toml.pending successor) is
// preferred over a .toml.disabled copy on any node: the gateway is stopped
// only when no node holds it live.
func (m *Manager) readGatewayConfig(ctx context.Context, resource, pluginID string) (*gatewayConfig, error) {
	log := m.log()
	path := gatewayConfigPath(pluginID)
	log.Debug("readGatewayConfig: start", zap.String("resource", resource), zap.String("plugin", pluginID), zap.String("path", path))
	hosts, _ := m.promoterHosts(ctx, resource)
	byHost, err := m.dumpNodeConfigs(ctx, hosts, path, path+pendingSuffix, path+disabledSuffix)
	if err != nil {
		log.Debug("readGatewayConfig: dump failed", zap.String("resource", resource), zap.Error(err))
		return nil, err
	}

	var copies []nodeConfigCopy
	var missing []string
	for _, host := range sortedKeys(byHost) {
		files := byHost[host]
		// A pending copy is the config the node running the gateway was last
		// edited to (see live_edit.go); its .toml is what drbd-reactor loaded.
		if content, ok := files[path+pendingSuffix]; ok {
			log.Debug("readGatewayConfig: host holds a pending copy", zap.String("host", host))
			copies = append(copies, nodeConfigCopy{host: host, content: content})
		} else if content, ok := files[path]; ok {
			log.Debug("readGatewayConfig: host holds a live copy", zap.String("host", host))
			copies = append(copies, nodeConfigCopy{host: host, content: content})
		} else if content, ok := files[path+disabledSuffix]; ok {
			log.Debug("readGatewayConfig: host holds a disabled copy", zap.String("host", host))
			copies = append(copies, nodeConfigCopy{host: host, disabled: true, content: content})
		} else {
			log.Debug("readGatewayConfig: host holds no copy", zap.String("host", host))
			missing = append(missing, host)
		}
	}
	if len(copies) == 0 {
		log.Debug("readGatewayConfig: no copy anywhere", zap.String("path", path), zap.Strings("hosts", hosts))
		return nil, fmt.Errorf("gateway config %s not found on %s", path, strings.Join(hosts, ","))
	}

	chosen, from, differing := chooseConfigCopy(copies)
	log.Debug("readGatewayConfig: copy chosen",
		zap.String("path", path), zap.Strings("from", from), zap.Strings("differing", differing),
		zap.Strings("missing", missing), zap.Bool("disabled", chosen.disabled), zap.Int("size_bytes", len(chosen.content)))
	if len(differing) > 0 || len(missing) > 0 {
		m.logger.Warn("Gateway config differs between nodes; using one copy, the next write makes them identical",
			zap.String("config", path),
			zap.Strings("used_copy_from", from),
			zap.Strings("differing_nodes", differing),
			zap.Strings("missing_on", missing))
	}
	return &gatewayConfig{content: chosen.content, disabled: chosen.disabled}, nil
}

// chooseConfigCopy picks the copy to edit from copies sorted by host. Live
// copies win over disabled ones; among those, the content most nodes hold, and
// on a tie the one held by the first host in name order. It returns the copy,
// the hosts holding it, and the hosts holding anything else.
func chooseConfigCopy(copies []nodeConfigCopy) (chosen nodeConfigCopy, from, differing []string) {
	candidates := copies
	var live []nodeConfigCopy
	for _, c := range copies {
		if !c.disabled {
			live = append(live, c)
		}
	}
	if len(live) > 0 {
		candidates = live
	}
	zap.L().Debug("chooseConfigCopy: candidates",
		zap.Int("copies", len(copies)), zap.Int("live", len(live)), zap.Int("count", len(candidates)))

	counts := map[string]int{}
	for _, c := range candidates {
		counts[c.content]++
	}
	// Candidates are in host order, so with a strict comparison the first
	// host holding a content stands for it and wins a tie.
	chosen = candidates[0]
	for _, c := range candidates[1:] {
		if counts[c.content] > counts[chosen.content] {
			chosen = c
		}
	}
	for _, c := range copies {
		if c.disabled == chosen.disabled && c.content == chosen.content {
			from = append(from, c.host)
		} else {
			differing = append(differing, c.host)
		}
	}
	return chosen, from, differing
}

// readAllNodeConfigs returns the content of every live config matching
// pattern on any managed node, de-duplicated by node-identical content. Used
// by listings that span gateways, where a union across nodes is the answer.
func (m *Manager) readAllNodeConfigs(ctx context.Context, pattern string) ([]string, error) {
	m.log().Debug("readAllNodeConfigs: start", zap.String("pattern", pattern), zap.Strings("hosts", m.hosts))
	byHost, err := m.dumpNodeConfigs(ctx, m.hosts, filepath.Join(DrbdReactorConfigDir, pattern))
	if err != nil {
		m.log().Debug("readAllNodeConfigs: dump failed", zap.String("pattern", pattern), zap.Error(err))
		return nil, err
	}
	seen := map[string]bool{}
	var contents []string
	for _, host := range sortedKeys(byHost) {
		for _, path := range sortedKeys(byHost[host]) {
			content := byHost[host][path]
			if !seen[content] {
				seen[content] = true
				contents = append(contents, content)
			}
		}
	}
	m.log().Debug("readAllNodeConfigs: done",
		zap.String("pattern", pattern), zap.Int("hosts", len(byHost)), zap.Int("count", len(contents)))
	return contents, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ConfigDumpCommand is the dispatch command that prints every existing file
// matching patterns on a node, byte-exact, for ParseConfigDump. The controller
// reads its own promoter configs (`ha create`) with it.
func ConfigDumpCommand(patterns ...string) string {
	return scriptCmd(configDumpScript(patterns...))
}

// ParseConfigDump returns the files one node's ConfigDumpCommand output holds,
// by path.
func ParseConfigDump(out string) (map[string]string, error) {
	return parseConfigDump(out)
}
