package controller

import (
	"context"
	"encoding/xml"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/haify-project/haify/pkg/database"
	"go.uber.org/zap"
)

// ocfRoot is the OCF root ($OCF_ROOT); ocfResourceDir is the standard on-node
// location for OCF resource agents (ocfRoot + "/resource.d"). The meta-data
// action sources OCF shell libs from $OCF_ROOT/lib, so OCF_ROOT must be the
// parent of resource.d, not resource.d itself.
const ocfRoot = "/usr/lib/ocf"
const ocfResourceDir = ocfRoot + "/resource.d"

// ocfNameRe validates OCF provider/agent names before they are interpolated
// into a shell command or filesystem path, blocking shell/path injection.
var ocfNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// ocfResourceAgent identifies a single OCF resource agent by provider and name.
type ocfResourceAgent struct {
	Provider  string
	Name      string
	Shortdesc string
}

// ocfAgentParameter is one parsed OCF meta-data parameter.
type ocfAgentParameter struct {
	Name      string
	Required  bool
	Unique    bool
	Type      string
	Default   string
	Shortdesc string
	Longdesc  string
}

// ocfAgentMetadata is the parsed OCF meta-data for a single agent.
type ocfAgentMetadata struct {
	Provider   string
	Name       string
	Version    string
	Shortdesc  string
	Longdesc   string
	Parameters []ocfAgentParameter
}

// OcfAgentSpec is one OCF resource agent to compose into an HA promoter's
// start[] list. It renders as "ocf:<provider>:<name> <instance> <k>=<v> ...".
type OcfAgentSpec struct {
	Provider string
	Name     string
	Instance string
	Params   map[string]string
}

// HaStartItem is one entry in an ordered promoter start[] list. Exactly one of
// SystemdUnit or Ocf is set. systemd/mount units and OCF agents are peers, so a
// slice of these preserves the exact order the user arranged (e.g. portblock ->
// Filesystem -> IPaddr2 -> nfsserver -> exportfs -> portunblock).
type HaStartItem struct {
	SystemdUnit string        // a systemd/mount unit name, e.g. "mysql.service"
	Ocf         *OcfAgentSpec // an OCF resource agent
}

// ocfAgentRecords and startItemRecords convert to the database's record of
// an HA config.
func ocfAgentRecords(agents []OcfAgentSpec) []database.HaOcfAgent {
	var out []database.HaOcfAgent
	for _, a := range agents {
		out = append(out, database.HaOcfAgent{Provider: a.Provider, Name: a.Name, Instance: a.Instance, Params: a.Params})
	}
	zap.L().Debug("ocfAgentRecords: converted", zap.Int("count", len(out)))
	return out
}

func startItemRecords(items []HaStartItem) []database.HaStartItem {
	var out []database.HaStartItem
	for _, it := range items {
		rec := database.HaStartItem{SystemdUnit: it.SystemdUnit}
		if it.Ocf != nil {
			rec.Ocf = &database.HaOcfAgent{Provider: it.Ocf.Provider, Name: it.Ocf.Name,
				Instance: it.Ocf.Instance, Params: it.Ocf.Params}
		}
		out = append(out, rec)
	}
	zap.L().Debug("startItemRecords: converted", zap.Int("count", len(out)))
	return out
}

// renderStartItem renders one ordered start item as a promoter start[] entry:
// a bare systemd unit name, or an "ocf:..." agent line. Returns "" for an empty
// item (skipped by the caller).
func renderStartItem(it HaStartItem) string {
	if it.Ocf != nil {
		zap.L().Debug("renderStartItem: OCF agent item", zap.String("provider", it.Ocf.Provider), zap.String("agent", it.Ocf.Name))
		return renderOcfStartEntry(*it.Ocf)
	}
	zap.L().Debug("renderStartItem: systemd unit item", zap.String("unit", it.SystemdUnit))
	return strings.TrimSpace(it.SystemdUnit)
}

// ---- OCF meta-data XML (encoding/xml) ----

type ocfXMLDesc struct {
	Lang string `xml:"lang,attr"`
	Text string `xml:",chardata"`
}

type ocfXMLContent struct {
	Type    string `xml:"type,attr"`
	Default string `xml:"default,attr"`
}

type ocfXMLParameter struct {
	Name      string        `xml:"name,attr"`
	Required  string        `xml:"required,attr"`
	Unique    string        `xml:"unique,attr"`
	Shortdesc []ocfXMLDesc  `xml:"shortdesc"`
	Longdesc  []ocfXMLDesc  `xml:"longdesc"`
	Content   ocfXMLContent `xml:"content"`
}

type ocfXMLResourceAgent struct {
	XMLName    xml.Name          `xml:"resource-agent"`
	Name       string            `xml:"name,attr"`
	Version    string            `xml:"version,attr"`
	VersionEl  string            `xml:"version"`
	Shortdesc  []ocfXMLDesc      `xml:"shortdesc"`
	Longdesc   []ocfXMLDesc      `xml:"longdesc"`
	Parameters []ocfXMLParameter `xml:"parameters>parameter"`
}

// ocfBool interprets the OCF boolean attribute encoding ("1"/"true"/"yes").
func ocfBool(s string) bool {
	s = strings.TrimSpace(s)
	return s == "1" || strings.EqualFold(s, "true") || strings.EqualFold(s, "yes")
}

// pickDesc prefers the English description, falling back to the first present.
func pickDesc(descs []ocfXMLDesc) string {
	for _, d := range descs {
		if strings.EqualFold(d.Lang, "en") {
			if t := strings.TrimSpace(d.Text); t != "" {
				return t
			}
		}
	}
	for _, d := range descs {
		if t := strings.TrimSpace(d.Text); t != "" {
			return t
		}
	}
	return ""
}

// parseOCFMetaData parses an OCF resource-agent meta-data XML document.
func parseOCFMetaData(data []byte) (*ocfAgentMetadata, error) {
	log := zap.L()
	log.Debug("parseOCFMetaData: start", zap.Int("bytes", len(data)))
	var ra ocfXMLResourceAgent
	if err := xml.Unmarshal(data, &ra); err != nil {
		log.Debug("parseOCFMetaData: XML did not parse", zap.Error(err))
		return nil, fmt.Errorf("invalid OCF meta-data XML: %w", err)
	}
	if ra.XMLName.Local != "resource-agent" {
		log.Debug("parseOCFMetaData: unexpected root element", zap.String("root", ra.XMLName.Local))
		return nil, fmt.Errorf("unexpected root element %q, want resource-agent", ra.XMLName.Local)
	}

	version := strings.TrimSpace(ra.Version)
	if version == "" {
		log.Debug("parseOCFMetaData: no version attribute, using the version element")
		version = strings.TrimSpace(ra.VersionEl)
	}

	meta := &ocfAgentMetadata{
		Name:      strings.TrimSpace(ra.Name),
		Version:   version,
		Shortdesc: pickDesc(ra.Shortdesc),
		Longdesc:  pickDesc(ra.Longdesc),
	}
	for _, p := range ra.Parameters {
		meta.Parameters = append(meta.Parameters, ocfAgentParameter{
			Name:      strings.TrimSpace(p.Name),
			Required:  ocfBool(p.Required),
			Unique:    ocfBool(p.Unique),
			Type:      strings.TrimSpace(p.Content.Type),
			Default:   p.Content.Default,
			Shortdesc: pickDesc(p.Shortdesc),
			Longdesc:  pickDesc(p.Longdesc),
		})
	}
	log.Debug("parseOCFMetaData: parsed", zap.String("agent", meta.Name), zap.String("version", version),
		zap.Int("parameters", len(meta.Parameters)))
	return meta, nil
}

// parseResourceAgentPaths turns `find` output of executable OCF agent files
// (one path per line, under /usr/lib/ocf/resource.d/<provider>/<name>) into a
// deduplicated, ordered list of provider/name pairs. Helper scripts and hidden
// files are filtered out.
func parseResourceAgentPaths(output string) []ocfResourceAgent {
	prefix := ocfResourceDir + "/"
	seen := make(map[string]bool)
	var agents []ocfResourceAgent
	skipped := 0
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		rest := strings.TrimPrefix(line, prefix)
		if rest == line {
			// Line did not carry the expected prefix.
			skipped++
			continue
		}
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) != 2 {
			skipped++
			continue
		}
		provider, name := parts[0], parts[1]
		if provider == "" || name == "" || strings.Contains(name, "/") {
			skipped++
			continue
		}
		// Skip shared helper scripts that are not runnable agents.
		if strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".sh") {
			skipped++
			continue
		}
		key := provider + "/" + name
		if seen[key] {
			continue
		}
		seen[key] = true
		agents = append(agents, ocfResourceAgent{Provider: provider, Name: name})
	}
	// One summary per call: a node can carry hundreds of agents.
	zap.L().Debug("parseResourceAgentPaths: parsed", zap.Int("count", len(agents)), zap.Int("skipped", skipped))
	return agents
}

// renderOcfStartEntry renders one OCF agent as a drbd-reactor promoter start[]
// entry: "ocf:<provider>:<name> <instance> <k>=<v> ..." with sorted keys for
// deterministic output.
func renderOcfStartEntry(a OcfAgentSpec) string {
	provider := strings.TrimSpace(a.Provider)
	name := strings.TrimSpace(a.Name)
	// Parameter values are not logged: an agent's params can hold a password.
	if provider == "" || name == "" {
		zap.L().Debug("renderOcfStartEntry: provider or name empty, rendering nothing",
			zap.String("provider", provider), zap.String("agent", name))
		return ""
	}
	zap.L().Debug("renderOcfStartEntry: rendering", zap.String("provider", provider), zap.String("agent", name),
		zap.String("instance", a.Instance), zap.Int("params", len(a.Params)))
	var b strings.Builder
	fmt.Fprintf(&b, "ocf:%s:%s", provider, name)
	if inst := strings.TrimSpace(a.Instance); inst != "" {
		b.WriteString(" ")
		b.WriteString(inst)
	}
	keys := make([]string, 0, len(a.Params))
	for k := range a.Params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, " %s=%s", k, a.Params[k])
	}
	return b.String()
}

// allNodeAddresses returns the reachable node addresses to try for node-local
// OCF discovery, preferring registered nodes and falling back to the resource
// manager's configured hosts. The result is sorted for deterministic behavior.
func (rm *ResourceManager) allNodeAddresses() []string {
	log := loggerOfController(rm.controller)
	var addrs []string
	if rm.controller != nil && rm.controller.nodes != nil {
		start := time.Now()
		nodes, err := rm.controller.nodes.ListNodes(context.Background())
		log.Debug("ResourceManager.allNodeAddresses: listed registered nodes", zap.Int("count", len(nodes)),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		if err == nil {
			for _, n := range nodes {
				if n != nil && strings.TrimSpace(n.Address) != "" {
					log.Debug("ResourceManager.allNodeAddresses: node address", zap.String("node", n.Name), zap.String("host", n.Address))
					addrs = append(addrs, n.Address)
				}
			}
		}
	}
	if len(addrs) == 0 {
		rm.mu.RLock()
		addrs = append(addrs, rm.hosts...)
		rm.mu.RUnlock()
		log.Debug("ResourceManager.allNodeAddresses: no registered node addresses, falling back to configured hosts",
			zap.Strings("hosts", addrs))
	}
	sort.Strings(addrs)
	log.Debug("ResourceManager.allNodeAddresses: done", zap.Strings("hosts", addrs))
	return addrs
}

// ListResourceAgents enumerates the OCF resource agents installed under
// /usr/lib/ocf/resource.d/<provider>/<name> on a reachable node.
func (rm *ResourceManager) ListResourceAgents(ctx context.Context) ([]ocfResourceAgent, error) {
	log := loggerOfController(rm.controller)
	log.Debug("ResourceManager.ListResourceAgents: start")
	if rm.deployment == nil {
		log.Debug("ResourceManager.ListResourceAgents: no deployment client")
		return nil, fmt.Errorf("deployment client not set")
	}
	hosts := rm.allNodeAddresses()
	if len(hosts) == 0 {
		log.Debug("ResourceManager.ListResourceAgents: no nodes to ask")
		return nil, fmt.Errorf("no nodes available to list OCF resource agents")
	}
	cmd := fmt.Sprintf("find %s -mindepth 2 -maxdepth 2 -type f -executable 2>/dev/null | sort", ocfResourceDir)
	var lastErr error
	for _, host := range hosts {
		start := time.Now()
		result, err := rm.deployment.Exec(ctx, []string{host}, cmd)
		log.Debug("ResourceManager.ListResourceAgents: find agents", zap.String("host", host),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			lastErr = err
			continue
		}
		hr := result.Hosts[host]
		if hr == nil || !hr.Success {
			log.Debug("ResourceManager.ListResourceAgents: find failed on node, trying the next", zap.String("host", host),
				zap.Bool("has_result", hr != nil))
			lastErr = fmt.Errorf("failed to list OCF resource agents on node %s", host)
			continue
		}
		return parseResourceAgentPaths(hr.Output), nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no reachable node")
	}
	log.Debug("ResourceManager.ListResourceAgents: no node answered", zap.Error(lastErr))
	return nil, fmt.Errorf("failed to list OCF resource agents: %w", lastErr)
}

// GetResourceAgentMetadata runs the agent's `meta-data` action on a reachable
// node and parses the OCF meta-data XML into a parameter schema.
func (rm *ResourceManager) GetResourceAgentMetadata(ctx context.Context, provider, name string) (*ocfAgentMetadata, error) {
	log := loggerOfController(rm.controller)
	log.Debug("ResourceManager.GetResourceAgentMetadata: start", zap.String("provider", provider), zap.String("agent", name))
	if !ocfNameRe.MatchString(provider) {
		log.Debug("ResourceManager.GetResourceAgentMetadata: invalid provider name", zap.String("provider", provider))
		return nil, fmt.Errorf("invalid OCF provider %q", provider)
	}
	if !ocfNameRe.MatchString(name) {
		log.Debug("ResourceManager.GetResourceAgentMetadata: invalid agent name", zap.String("agent", name))
		return nil, fmt.Errorf("invalid OCF agent name %q", name)
	}
	if rm.deployment == nil {
		log.Debug("ResourceManager.GetResourceAgentMetadata: no deployment client")
		return nil, fmt.Errorf("deployment client not set")
	}
	hosts := rm.allNodeAddresses()
	if len(hosts) == 0 {
		log.Debug("ResourceManager.GetResourceAgentMetadata: no nodes to ask")
		return nil, fmt.Errorf("no nodes available to read OCF meta-data")
	}
	agentPath := fmt.Sprintf("%s/%s/%s", ocfResourceDir, provider, name)
	cmd := fmt.Sprintf("if [ -x %s ]; then OCF_ROOT=%s %s meta-data; else echo __HAIFY_OCF_MISSING__; fi",
		agentPath, ocfRoot, agentPath)

	var lastErr error
	for _, host := range hosts {
		start := time.Now()
		result, err := rm.deployment.Exec(ctx, []string{host}, cmd)
		log.Debug("ResourceManager.GetResourceAgentMetadata: run meta-data", zap.String("host", host),
			zap.String("path", agentPath), zap.Duration("took", time.Since(start)), zap.Error(err))
		if err != nil {
			lastErr = err
			continue
		}
		hr := result.Hosts[host]
		if hr == nil {
			log.Debug("ResourceManager.GetResourceAgentMetadata: no result from node", zap.String("host", host))
			lastErr = fmt.Errorf("no result from node %s", host)
			continue
		}
		if strings.Contains(hr.Output, "__HAIFY_OCF_MISSING__") {
			log.Debug("ResourceManager.GetResourceAgentMetadata: agent not installed on node", zap.String("host", host),
				zap.String("path", agentPath))
			lastErr = fmt.Errorf("OCF resource agent %s:%s not found at %s", provider, name, agentPath)
			continue
		}
		if !hr.Success {
			log.Debug("ResourceManager.GetResourceAgentMetadata: meta-data action failed", zap.String("host", host))
			lastErr = fmt.Errorf("meta-data failed on node %s: %s", host, strings.TrimSpace(hr.Output))
			continue
		}
		meta, err := parseOCFMetaData([]byte(hr.Output))
		if err != nil {
			log.Debug("ResourceManager.GetResourceAgentMetadata: meta-data did not parse", zap.String("host", host), zap.Error(err))
			return nil, fmt.Errorf("parse OCF meta-data for %s:%s: %w", provider, name, err)
		}
		meta.Provider = provider
		if meta.Name == "" {
			meta.Name = name
		}
		log.Debug("ResourceManager.GetResourceAgentMetadata: done", zap.String("host", host),
			zap.String("agent", meta.Name), zap.Int("parameters", len(meta.Parameters)))
		return meta, nil
	}
	log.Debug("ResourceManager.GetResourceAgentMetadata: no node answered", zap.Error(lastErr))
	return nil, lastErr
}

// GetHaToml reads a resource's drbd-reactor promoter TOML from the first node
// that has it. Returns an error naming the missing file if no HA config exists.
func (rm *ResourceManager) GetHaToml(ctx context.Context, resource string) (path, content string, err error) {
	log := loggerOfController(rm.controller)
	log.Debug("ResourceManager.GetHaToml: start", zap.String("resource", resource))
	if rm.deployment == nil {
		log.Debug("ResourceManager.GetHaToml: no deployment client", zap.String("resource", resource))
		return "", "", fmt.Errorf("deployment client not set")
	}
	path = haTomlPath(resource)
	hosts, err := rm.resourceHosts(ctx, resource)
	if err != nil {
		log.Debug("ResourceManager.GetHaToml: resource hosts unknown", zap.String("resource", resource), zap.Error(err))
		return "", "", err
	}
	cmd := fmt.Sprintf("if [ -f %s ]; then cat %s; else echo __HAIFY_HA_TOML_MISSING__; fi", path, path)
	var lastErr error
	for _, host := range hosts {
		start := time.Now()
		result, execErr := rm.deployment.Exec(ctx, []string{host}, cmd)
		log.Debug("ResourceManager.GetHaToml: read promoter config", zap.String("resource", resource),
			zap.String("host", host), zap.String("path", path), zap.Duration("took", time.Since(start)), zap.Error(execErr))
		if execErr != nil {
			lastErr = execErr
			continue
		}
		hr := result.Hosts[host]
		if hr == nil || !hr.Success {
			log.Debug("ResourceManager.GetHaToml: read failed on node, trying the next", zap.String("host", host))
			lastErr = fmt.Errorf("failed to read %s on node %s", path, host)
			continue
		}
		if strings.Contains(hr.Output, "__HAIFY_HA_TOML_MISSING__") {
			log.Debug("ResourceManager.GetHaToml: no promoter config on node", zap.String("host", host))
			lastErr = fmt.Errorf("no HA config for resource %q: %s not found", resource, path)
			continue
		}
		log.Debug("ResourceManager.GetHaToml: found", zap.String("resource", resource), zap.String("host", host),
			zap.Int("bytes", len(hr.Output)))
		return path, hr.Output, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no HA config for resource %q: %s not found", resource, path)
	}
	log.Debug("ResourceManager.GetHaToml: not found on any node", zap.String("resource", resource),
		zap.Strings("hosts", hosts), zap.Error(lastErr))
	return "", "", lastErr
}

// SyncHaToml validates and distributes an edited promoter TOML to the nodes
// that may run it (failoverHosts), then reloads drbd-reactor. It never touches
// DRBD data. A copy on the DR node, written before DR nodes were excluded, is
// retired.
func (rm *ResourceManager) SyncHaToml(ctx context.Context, resource, content string) (string, error) {
	log := loggerOfController(rm.controller)
	// The content is not logged: an OCF agent's params in it can hold a password.
	log.Debug("ResourceManager.SyncHaToml: start", zap.String("resource", resource), zap.Int("bytes", len(content)))
	if rm.deployment == nil {
		log.Debug("ResourceManager.SyncHaToml: no deployment client", zap.String("resource", resource))
		return "", fmt.Errorf("deployment client not set")
	}
	if strings.TrimSpace(content) == "" {
		log.Debug("ResourceManager.SyncHaToml: content empty, refusing", zap.String("resource", resource))
		return "", fmt.Errorf("toml content is empty")
	}
	if !strings.Contains(content, "[[promoter]]") {
		log.Debug("ResourceManager.SyncHaToml: no [[promoter]] table, refusing", zap.String("resource", resource))
		return "", fmt.Errorf("toml content does not contain a [[promoter]] table; refusing to sync")
	}
	hosts, err := rm.failoverHosts(ctx, resource)
	if err != nil {
		log.Debug("ResourceManager.SyncHaToml: failover hosts unknown", zap.String("resource", resource), zap.Error(err))
		return "", err
	}
	if dr := rm.drHost(ctx, resource); dr != "" {
		log.Debug("ResourceManager.SyncHaToml: retiring the promoter on the DR node", zap.String("resource", resource),
			zap.String("host", dr))
		rm.retireHaPromoter(ctx, resource, []string{dr})
	}
	path := haTomlPath(resource)
	start := time.Now()
	if _, err := rm.deployment.DistributeConfig(ctx, hosts, content, path); err != nil {
		log.Debug("ResourceManager.SyncHaToml: distribute failed", zap.String("resource", resource),
			zap.Strings("hosts", hosts), zap.Duration("took", time.Since(start)), zap.Error(err))
		return "", fmt.Errorf("failed to distribute HA toml: %w", err)
	}
	log.Debug("ResourceManager.SyncHaToml: distributed", zap.String("resource", resource), zap.Strings("hosts", hosts),
		zap.String("path", path), zap.Duration("took", time.Since(start)))
	start = time.Now()
	reloadResult, err := rm.deployment.ReactorReload(ctx, hosts)
	if err != nil {
		log.Debug("ResourceManager.SyncHaToml: drbd-reactor reload errored", zap.String("resource", resource),
			zap.Strings("hosts", hosts), zap.Duration("took", time.Since(start)), zap.Error(err))
		return "", fmt.Errorf("failed to reload drbd-reactor: %w", err)
	}
	log.Debug("ResourceManager.SyncHaToml: drbd-reactor reloaded", zap.String("resource", resource),
		zap.Strings("hosts", hosts), zap.Bool("all_success", reloadResult.AllSuccess()), zap.Duration("took", time.Since(start)))
	if !reloadResult.AllSuccess() {
		log.Debug("ResourceManager.SyncHaToml: reload failed on some nodes", zap.String("resource", resource),
			zap.Strings("failed_hosts", reloadResult.FailedHosts()))
		return "", fmt.Errorf("drbd-reactor reload failed on nodes: %s", reloadResult.FailureDetails())
	}
	return fmt.Sprintf("toml synced to %d nodes, drbd-reactor reloaded", len(hosts)), nil
}

// haTomlPath is the on-node path of a resource's HA promoter config.
func haTomlPath(resource string) string {
	return fmt.Sprintf("/etc/drbd-reactor.d/haify-ha-%s.toml", resource)
}
