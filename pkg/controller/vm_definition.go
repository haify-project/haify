package controller

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/haify-project/haify/pkg/database"
	"go.uber.org/zap"
)

// The libvirt definition of an HA guest, on the nodes.
//
// `ha create --vm` runs a guest through the VirtualDomain agent, which reads
// the guest's definition from the file libvirt keeps for it,
// /etc/libvirt/qemu/<domain>.xml. That file is the definition on a node, in
// every state the agent leaves a guest in: defined and shut off on a standby,
// running on the active node (where the agent starts it with `virsh create`,
// so libvirt lists it as transient while the file stays), or stopped after a
// run. Every replica that may take the guest over needs the file, with the
// same UUID: a takeover on a node without it starts nothing, and a live
// migration onto a node that knows the name under another UUID is refused by
// libvirt.
//
// Reading the file and comparing is all this file does. The XML can hold a
// VNC or SPICE password, so it travels base64-encoded, is never traced, and
// only its digest and size are ever logged.

// libvirtConfigRe matches the definition file libvirt keeps for a guest.
var libvirtConfigRe = regexp.MustCompile(`^/etc/libvirt/qemu/([A-Za-z0-9][A-Za-z0-9_.+-]{0,62})\.xml$`)

// vmGuest is the libvirt guest an HA config runs.
type vmGuest struct {
	Domain     string
	ConfigPath string
	// Instance is the OCF instance name, which drbd-reactor builds the
	// agent's unit name from.
	Instance string
	// Unit, when set, is the agent's unit name as it was read earlier; it
	// then stands in for the one built from Instance.
	Unit string
}

// vmGuestOf returns the guest an HA config runs through the VirtualDomain
// agent, or nil when it runs none — or runs one from a definition file that
// is not libvirt's own, which Haify does not manage.
func vmGuestOf(cfg *database.HaConfig) *vmGuest {
	if cfg == nil {
		return nil
	}
	agents := append([]database.HaOcfAgent{}, cfg.OcfAgents...)
	for _, it := range cfg.StartItems {
		if it.Ocf != nil {
			agents = append(agents, *it.Ocf)
		}
	}
	for _, a := range agents {
		if a.Name != "VirtualDomain" {
			continue
		}
		m := libvirtConfigRe.FindStringSubmatch(strings.TrimSpace(a.Params["config"]))
		if m == nil {
			zap.L().Debug("vmGuestOf: VirtualDomain agent does not use libvirt's definition file",
				zap.String("resource", cfg.Resource), zap.String("instance", a.Instance))
			return nil
		}
		return &vmGuest{Domain: m[1], ConfigPath: m[0], Instance: a.Instance}
	}
	return nil
}

// agentUnit is the systemd unit drbd-reactor runs the guest's agent in.
func (g *vmGuest) agentUnit(resource string) string {
	if g.Unit != "" {
		return g.Unit
	}
	return "ocf.rs@" + reactorEscape(g.Instance+"_"+resource) + ".service"
}

// reactorTargetUnit is the target drbd-reactor starts a resource's chain as.
func reactorTargetUnit(resource string) string {
	return "drbd-services@" + reactorEscape(resource) + ".target"
}

// reactorEscape escapes a name the way drbd-reactor does when it builds unit
// names (systemd's own escaping): '/' becomes '-', letters, digits, ':' and
// '_' stay, a '.' stays unless it leads, and every other byte becomes \xNN.
func reactorEscape(name string) string {
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c == '/':
			b.WriteByte('-')
		case c == ':' || c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
			b.WriteByte(c)
		case c == '.' && i > 0:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, `\x%02x`, c)
		}
	}
	return b.String()
}

// domainIdentity is what parseDomainXML reads of a definition.
type domainIdentity struct {
	Name string `xml:"name"`
	UUID string `xml:"uuid"`
}

// parseDomainXML returns a definition's name, UUID and digest. The digest is
// over the <domain> element alone: libvirt heads the file with a comment that
// names the file itself.
func parseDomainXML(text string) (name, uuid, hash string, err error) {
	i := strings.Index(text, "<domain")
	if i < 0 {
		return "", "", "", fmt.Errorf("no <domain> element")
	}
	body := strings.TrimSpace(text[i:])
	var id domainIdentity
	if err := xml.Unmarshal([]byte(body), &id); err != nil {
		// Not the decoder's message: it can quote the text around an error.
		return "", "", "", fmt.Errorf("the definition is not well-formed XML")
	}
	sum := sha256.Sum256([]byte(body))
	return strings.TrimSpace(id.Name), strings.ToLower(strings.TrimSpace(id.UUID)), hex.EncodeToString(sum[:]), nil
}

// vmNodeFacts is one node's view of a guest: what libvirt says of it and the
// definition file it holds.
type vmNodeFacts struct {
	Reachable bool
	Err       string
	Virsh     bool
	// State is `virsh domstate`, Running whether that is a guest with a
	// process, and StateKnown whether libvirt gave an answer at all: a node
	// whose libvirt did not answer is not a node the guest is absent from.
	State       string
	Running     bool
	StateKnown  bool
	LibvirtUUID string
	DomID       string
	HasConfig   bool
	// XML is the definition file. Never logged.
	XML      string
	FileName string
	FileUUID string
	Hash     string
	ParseErr string
	// Extra holds the other KEY=value lines of the probe, for callers that
	// add their own to the script.
	Extra map[string]string
}

// vmProbeScript prints a node's view of a guest as KEY=value lines.
func vmProbeScript(g *vmGuest) string {
	return fmt.Sprintf(`d=%s
cfg=%s
V="virsh -c qemu:///system"
if command -v virsh >/dev/null 2>&1; then echo VIRSH=yes; else echo VIRSH=no; fi
echo "STATE=$(LANG=C $V domstate "$d" 2>&1 | head -n1)"
echo "LIBVIRT_UUID=$($V domuuid "$d" 2>/dev/null | head -n1)"
echo "DOMID=$($V domid "$d" 2>/dev/null | head -n1)"
if [ -r "$cfg" ]; then echo "CONFIG=$(base64 "$cfg" | tr -d '\n')"; else echo "CONFIG="; fi
`, shellSingleQuote(g.Domain), shellSingleQuote(g.ConfigPath))
}

// vmStateRunning says whether a `virsh domstate` line is a guest with a
// process, and whether it is an answer about the guest at all.
func vmStateRunning(state string) (running, known bool) {
	s := strings.ToLower(strings.TrimSpace(state))
	switch {
	case s == "running", s == "paused", s == "idle", s == "blocked", s == "in shutdown", s == "pmsuspended":
		return true, true
	case s == "shut off", s == "crashed":
		return false, true
	case strings.Contains(s, "failed to get domain"), strings.Contains(s, "domain not found"):
		return false, true
	}
	return false, false
}

// parseVMProbe reads vmProbeScript's output.
func parseVMProbe(out string) *vmNodeFacts {
	kv := keyValues(out)
	f := &vmNodeFacts{Reachable: true, Virsh: kv["VIRSH"] == "yes", State: kv["STATE"],
		LibvirtUUID: strings.ToLower(kv["LIBVIRT_UUID"]), DomID: kv["DOMID"], Extra: kv}
	f.Running, f.StateKnown = vmStateRunning(f.State)
	if !f.Virsh {
		f.StateKnown = false
	}
	if enc := kv["CONFIG"]; enc != "" {
		raw, err := base64.StdEncoding.DecodeString(enc)
		if err != nil {
			f.ParseErr = "the definition file could not be read"
		} else {
			f.HasConfig = true
			f.XML = string(raw)
			f.FileName, f.FileUUID, f.Hash, err = parseDomainXML(f.XML)
			if err != nil {
				f.ParseErr = err.Error()
			}
		}
	}
	// The encoded file must not ride along in Extra, which callers log from.
	delete(kv, "CONFIG")
	return f
}

// probeVMNodes reads every host's view of a guest in one round. extra is
// appended to the probe script and its KEY=value lines land in Extra.
func (rm *ResourceManager) probeVMNodes(ctx context.Context, g *vmGuest, hosts []string, extra string) map[string]*vmNodeFacts {
	log := rm.controller.traceLogger()
	log.Debug("ResourceManager.probeVMNodes: start", zap.String("domain", g.Domain), zap.Strings("hosts", hosts))
	out := make(map[string]*vmNodeFacts, len(hosts))
	start := time.Now()
	res, err := rm.runNodeScript(ctx, hosts, vmProbeScript(g)+extra)
	log.Debug("ResourceManager.probeVMNodes: probed", zap.String("domain", g.Domain), zap.Strings("hosts", hosts),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	for _, h := range hosts {
		if err != nil || res == nil || res.Hosts[h] == nil || !res.Hosts[h].Success {
			reason := "no answer"
			if err != nil {
				reason = err.Error()
			} else if res != nil {
				reason = hostFailure(res.Hosts[h])
			}
			log.Debug("ResourceManager.probeVMNodes: host did not answer", zap.String("host", h), zap.String("reason", reason))
			out[h] = &vmNodeFacts{Err: reason}
			continue
		}
		f := parseVMProbe(res.Hosts[h].Output)
		log.Debug("ResourceManager.probeVMNodes: host facts", zap.String("host", h), zap.Bool("virsh", f.Virsh),
			zap.String("state", f.State), zap.Bool("running", f.Running), zap.Bool("has_config", f.HasConfig),
			zap.String("file_uuid", f.FileUUID), zap.String("libvirt_uuid", f.LibvirtUUID), zap.Int("xml_bytes", len(f.XML)),
			zap.String("parse_error", f.ParseErr))
		out[h] = f
	}
	return out
}

// VMDefinitionNode is how one node's copy of a guest's definition compares
// with the one the controller keeps.
type VMDefinitionNode struct {
	Node    string
	State   string
	UUID    string
	Running bool
	Detail  string
}

// The states of a node's copy.
const (
	vmDefOK           = "ok"
	vmDefMissing      = "missing"
	vmDefChanged      = "changed"
	vmDefUUIDMismatch = "uuid_mismatch"
	vmDefUnreachable  = "unreachable"
)

// VMDefinitionStatus is a guest's definition across its replicas.
type VMDefinitionStatus struct {
	Resource   string
	Domain     string
	UUID       string
	SourceNode string
	UpdatedAt  time.Time
	Consistent bool
	Nodes      []VMDefinitionNode
	Message    string
}

// assessVMNode compares one node's copy with the kept definition.
func assessVMNode(def *database.VMDefinition, node string, f *vmNodeFacts) VMDefinitionNode {
	n := VMDefinitionNode{Node: node}
	switch {
	case f == nil || !f.Reachable:
		n.State = vmDefUnreachable
		if f != nil {
			n.Detail = f.Err
		}
		return n
	case !f.Virsh:
		n.State, n.Detail = vmDefMissing, "libvirt is not installed"
		return n
	}
	n.Running = f.Running
	n.UUID = f.FileUUID
	if n.UUID == "" {
		n.UUID = f.LibvirtUUID
	}
	switch {
	case !f.HasConfig:
		n.State, n.Detail = vmDefMissing, "no definition"
	case f.ParseErr != "":
		n.State, n.Detail = vmDefChanged, f.ParseErr
	case f.FileName != def.Domain:
		n.State, n.Detail = vmDefChanged, fmt.Sprintf("the definition names the guest %q", f.FileName)
	case f.FileUUID != def.UUID:
		n.State, n.Detail = vmDefUUIDMismatch, fmt.Sprintf("UUID %s, expected %s", f.FileUUID, def.UUID)
	case f.LibvirtUUID != "" && f.LibvirtUUID != def.UUID:
		n.State, n.Detail = vmDefUUIDMismatch, fmt.Sprintf("libvirt knows the guest as %s, expected %s", f.LibvirtUUID, def.UUID)
	case def.NodeHashes[node] == "":
		n.State, n.Detail = vmDefChanged, "not synced to this node yet"
	case def.NodeHashes[node] != f.Hash:
		n.State, n.Detail = vmDefChanged, "edited on this node since the last sync"
	default:
		n.State = vmDefOK
	}
	return n
}

// assessVMDefinition compares every node's copy with the kept definition.
// facts is keyed by node name.
func assessVMDefinition(def *database.VMDefinition, nodes []string, facts map[string]*vmNodeFacts) *VMDefinitionStatus {
	st := &VMDefinitionStatus{Resource: def.Resource, Domain: def.Domain, UUID: def.UUID, SourceNode: def.SourceNode,
		UpdatedAt: def.UpdatedAt, Consistent: true}
	var off []string
	for _, node := range nodes {
		n := assessVMNode(def, node, facts[node])
		st.Nodes = append(st.Nodes, n)
		if n.State != vmDefOK {
			st.Consistent = false
			off = append(off, fmt.Sprintf("%s: %s", node, vmDefStateText(n)))
		}
	}
	if st.Consistent {
		st.Message = fmt.Sprintf("%s is defined with UUID %s on %s", def.Domain, def.UUID, strings.Join(nodes, ", "))
	} else {
		st.Message = strings.Join(off, "; ")
	}
	zap.L().Debug("assessVMDefinition: assessed", zap.String("resource", def.Resource), zap.String("domain", def.Domain),
		zap.Bool("consistent", st.Consistent), zap.Strings("off", off))
	return st
}

// vmDefStateText says a node's state in words.
func vmDefStateText(n VMDefinitionNode) string {
	switch n.State {
	case vmDefMissing:
		return "the guest is not defined"
	case vmDefUUIDMismatch:
		return "defined under another UUID (" + n.Detail + ")"
	case vmDefChanged:
		return "the definition differs (" + n.Detail + ")"
	case vmDefUnreachable:
		return "did not answer"
	}
	return n.State
}

// vmFactsByNode re-keys probe results from host addresses to node names.
func (rm *ResourceManager) vmFactsByNode(byHost map[string]*vmNodeFacts) map[string]*vmNodeFacts {
	out := make(map[string]*vmNodeFacts, len(byHost))
	for h, f := range byHost {
		out[rm.nodeLabel(h)] = f
	}
	return out
}

// haGuest returns the guest resource's HA config runs, or an error saying
// that it runs none.
func (rm *ResourceManager) haGuest(ctx context.Context, resource string) (*vmGuest, error) {
	log := rm.controller.traceLogger()
	if rm.controller.db == nil {
		log.Debug("ResourceManager.haGuest: no database", zap.String("resource", resource))
		return nil, fmt.Errorf("database not available")
	}
	cfg, err := rm.controller.db.GetHaConfig(ctx, resource)
	if err != nil || cfg == nil {
		log.Debug("ResourceManager.haGuest: no HA config", zap.String("resource", resource), zap.Error(err))
		return nil, fmt.Errorf("%s has no HA configuration", resource)
	}
	g := vmGuestOf(cfg)
	if g == nil {
		log.Debug("ResourceManager.haGuest: HA config runs no libvirt guest", zap.String("resource", resource))
		return nil, fmt.Errorf("%s runs no libvirt guest: its HA configuration was not made with --vm", resource)
	}
	log.Debug("ResourceManager.haGuest: guest", zap.String("resource", resource), zap.String("domain", g.Domain))
	return g, nil
}

// GetVMDefinition reports how each replica's copy of resource's guest
// definition compares with the one the controller keeps.
func (rm *ResourceManager) GetVMDefinition(ctx context.Context, resource string) (*VMDefinitionStatus, error) {
	log := rm.controller.traceLogger()
	log.Debug("ResourceManager.GetVMDefinition: start", zap.String("resource", resource))
	g, err := rm.haGuest(ctx, resource)
	if err != nil {
		return nil, err
	}
	def, err := rm.controller.db.GetVMDefinition(ctx, resource)
	if err != nil {
		log.Debug("ResourceManager.GetVMDefinition: record unreadable", zap.String("resource", resource), zap.Error(err))
		return nil, err
	}
	if def == nil {
		log.Debug("ResourceManager.GetVMDefinition: no record", zap.String("resource", resource))
		return nil, fmt.Errorf("no definition of %s is kept yet; run `haify ha vm-sync %s`", g.Domain, resource)
	}
	hosts, err := rm.failoverHosts(ctx, resource)
	if err != nil {
		log.Debug("ResourceManager.GetVMDefinition: no failover hosts", zap.String("resource", resource), zap.Error(err))
		return nil, err
	}
	facts := rm.vmFactsByNode(rm.probeVMNodes(ctx, g, hosts, ""))
	st := assessVMDefinition(def, rm.nodeNames(hosts), facts)
	log.Debug("ResourceManager.GetVMDefinition: done", zap.String("resource", resource), zap.Bool("consistent", st.Consistent))
	return st, nil
}

// nodeNames names hosts by their registered node names, in order.
func (rm *ResourceManager) nodeNames(hosts []string) []string {
	out := make([]string, len(hosts))
	for i, h := range hosts {
		out[i] = rm.nodeLabel(h)
	}
	return out
}
