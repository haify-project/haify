package controller

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/deployment"
	"go.uber.org/zap"
)

// Keeping one definition of an HA guest on every replica that may run it.
//
// The definition is taken from a node that has it, kept in the controller's
// database, and defined on the other replicas with `virsh define`, which also
// has that node's libvirt check that it could run the guest at all: a machine
// type or emulator the node lacks fails here and not at the takeover.
//
// A definition is only ever removed from a node Haify defined it on, and
// never while the guest runs there.

// vmDefineStageDir is where a definition waits, in the login user's home at
// mode 0600, for `virsh define`. It goes by scp, not on a command line.
const vmDefineStageDir = ".haify/vm-define"

// runNodeScript runs a root shell script on hosts. The script is
// base64-wrapped: dispatch runs commands through `sh -c "..."`, which would
// expand the script's own $variables first.
func (rm *ResourceManager) runNodeScript(ctx context.Context, hosts []string, script string, opts ...deployment.ExecOption) (*deployment.ExecResult, error) {
	return rm.deployment.Exec(ctx, hosts, "echo "+base64Std(script)+" | base64 -d | sudo /bin/sh", opts...)
}

// vmSource is where a sync takes the definition from.
type vmSource struct {
	// Node is the node whose file is the source; empty with Stored set means
	// the copy the controller keeps.
	Node   string
	Stored bool
}

// chooseVMDefinitionSource picks the definition a sync spreads. order lists
// the replicas by name and facts is keyed the same way.
//
// With from, that node. On the first sync, the node the guest runs on, or
// any node when every copy carries the same UUID. Afterwards the one node
// whose copy was edited since the last sync, or the kept copy when none was.
// Whatever is left is a choice only the operator can make.
func chooseVMDefinitionSource(def *database.VMDefinition, domain string, order []string, facts map[string]*vmNodeFacts, from string) (vmSource, error) {
	usable := func(node string) bool {
		f := facts[node]
		return f != nil && f.Reachable && f.HasConfig && f.ParseErr == "" && f.FileName == domain
	}
	if from != "" {
		f := facts[from]
		switch {
		case f == nil:
			return vmSource{}, fmt.Errorf("%s holds no replica of this resource", from)
		case !f.Reachable:
			return vmSource{}, fmt.Errorf("%s did not answer: %s", from, f.Err)
		case !usable(from):
			return vmSource{}, fmt.Errorf("%s has no usable definition of %s", from, domain)
		}
		return vmSource{Node: from}, nil
	}

	var have []string
	for _, n := range order {
		if usable(n) {
			have = append(have, n)
		}
	}
	if def == nil {
		if len(have) == 0 {
			return vmSource{}, fmt.Errorf("%s is not defined on any replica (%s); define it on one with `virsh define`",
				domain, strings.Join(order, ", "))
		}
		for _, n := range have {
			if facts[n].Running {
				return vmSource{Node: n}, nil
			}
		}
		uuids := map[string][]string{}
		for _, n := range have {
			uuids[facts[n].FileUUID] = append(uuids[facts[n].FileUUID], n)
		}
		if len(uuids) > 1 {
			return vmSource{}, fmt.Errorf("%s is defined under different UUIDs (%s); name the node whose definition to keep",
				domain, describeUUIDs(uuids))
		}
		return vmSource{Node: have[0]}, nil
	}

	var edited []string
	for _, n := range have {
		if rec := def.NodeHashes[n]; rec != "" && rec != facts[n].Hash && facts[n].FileUUID == def.UUID {
			edited = append(edited, n)
		}
	}
	switch {
	case len(edited) == 0:
		return vmSource{Stored: true}, nil
	case len(edited) == 1:
		return vmSource{Node: edited[0]}, nil
	}
	first := facts[edited[0]].Hash
	for _, n := range edited[1:] {
		if facts[n].Hash != first {
			return vmSource{}, fmt.Errorf("the definition of %s was edited on %s; name the node whose definition to keep",
				domain, strings.Join(edited, " and "))
		}
	}
	return vmSource{Node: edited[0]}, nil
}

// describeUUIDs lists which nodes hold which UUID, in a stable order.
func describeUUIDs(uuids map[string][]string) string {
	keys := make([]string, 0, len(uuids))
	for u := range uuids {
		keys = append(keys, u)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, u := range keys {
		parts = append(parts, fmt.Sprintf("%s on %s", u, strings.Join(uuids[u], ", ")))
	}
	return strings.Join(parts, "; ")
}

// vmDefineScript defines the staged file on a node, replacing a shut-off
// definition under another UUID and turning autostart off: drbd-reactor is
// what starts an HA guest.
func vmDefineScript(g *vmGuest, uuid, staged string) string {
	return fmt.Sprintf(`f=%s
d=%s
want=%s
trap 'rm -f "$f"' EXIT
V="virsh -c qemu:///system"
cur=$($V domuuid "$d" 2>/dev/null | head -n1)
if [ -n "$cur" ] && [ "$cur" != "$want" ]; then
  st=$(LANG=C $V domstate "$d" 2>&1 | head -n1)
  if [ "$st" != "shut off" ]; then echo "HAIFY_ERR=$d is $st here under another UUID ($cur)"; exit 3; fi
  $V undefine --keep-nvram "$d" >/dev/null 2>&1 || $V undefine "$d" >/dev/null 2>&1 || { echo "HAIFY_ERR=the definition under UUID $cur could not be removed"; exit 4; }
fi
out=$($V define "$f" 2>&1) || { echo "HAIFY_ERR=$(printf '%%s' "$out" | tr '\n' ' ')"; exit 5; }
$V autostart --disable "$d" >/dev/null 2>&1
echo HAIFY_DEFINED=1
`, shellSingleQuote(staged), shellSingleQuote(g.Domain), shellSingleQuote(uuid))
}

// defineVMOn stages xml on host and defines it there.
func (rm *ResourceManager) defineVMOn(ctx context.Context, resource string, g *vmGuest, uuid, xml, host string) error {
	log := rm.controller.traceLogger()
	log.Debug("ResourceManager.defineVMOn: start", zap.String("resource", resource), zap.String("domain", g.Domain),
		zap.String("host", host), zap.Int("xml_bytes", len(xml)))
	start := time.Now()
	res, err := rm.deployment.Exec(ctx, []string{host},
		fmt.Sprintf(`mkdir -p "$HOME/%[1]s" && chmod 0700 "$HOME/%[1]s" && echo "HAIFY_HOME=$HOME"`, vmDefineStageDir))
	log.Debug("ResourceManager.defineVMOn: staging directory", zap.String("host", host),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("prepare %s: %w", rm.nodeLabel(host), err)
	}
	out, ok := appHostOutput(res, host)
	home := keyValues(out)["HAIFY_HOME"]
	if !ok || !strings.HasPrefix(home, "/") {
		log.Debug("ResourceManager.defineVMOn: no usable home directory", zap.String("host", host), zap.Bool("ok", ok))
		return fmt.Errorf("prepare %s: %s", rm.nodeLabel(host), hostFailure(res.Hosts[host]))
	}
	rel := path.Join(vmDefineStageDir, resource+".xml")
	start = time.Now()
	put, err := rm.deployment.DistributeSecret(ctx, []string{host}, xml, rel)
	log.Debug("ResourceManager.defineVMOn: definition copied", zap.String("host", host), zap.String("path", rel),
		zap.Bool("success", put != nil && put.Success), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("copy the definition to %s: %w", rm.nodeLabel(host), err)
	}
	if put == nil || !put.Success || len(put.FailedHosts()) > 0 {
		return fmt.Errorf("copy the definition to %s failed", rm.nodeLabel(host))
	}
	start = time.Now()
	dres, err := rm.runNodeScript(ctx, []string{host}, vmDefineScript(g, uuid, path.Join(home, rel)))
	log.Debug("ResourceManager.defineVMOn: virsh define", zap.String("host", host),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("define %s on %s: %w", g.Domain, rm.nodeLabel(host), err)
	}
	dout, _ := appHostOutput(dres, host)
	kv := keyValues(dout)
	if kv["HAIFY_DEFINED"] != "1" {
		reason := kv["HAIFY_ERR"]
		if reason == "" {
			reason = "virsh define did not run"
		}
		log.Debug("ResourceManager.defineVMOn: not defined", zap.String("host", host), zap.String("reason", reason))
		return fmt.Errorf("define %s on %s: %s", g.Domain, rm.nodeLabel(host), reason)
	}
	return nil
}

// syncVMDefinition takes the guest's definition from its source and defines
// it on every other reachable host, then records what each node ended up
// with. def is the kept record, nil on the first sync.
func (rm *ResourceManager) syncVMDefinition(ctx context.Context, resource string, g *vmGuest, hosts []string, def *database.VMDefinition, from string) (*VMDefinitionStatus, error) {
	log := rm.controller.traceLogger()
	log.Debug("ResourceManager.syncVMDefinition: start", zap.String("resource", resource), zap.String("domain", g.Domain),
		zap.Strings("hosts", hosts), zap.Bool("has_record", def != nil), zap.String("from", from))
	nodes := rm.nodeNames(hosts)
	hostOf := map[string]string{}
	for i, n := range nodes {
		hostOf[n] = hosts[i]
	}
	if from != "" {
		from = rm.nodeLabel(rm.controller.ResolveHost(from))
	}
	facts := rm.vmFactsByNode(rm.probeVMNodes(ctx, g, hosts, ""))
	src, err := chooseVMDefinitionSource(def, g.Domain, nodes, facts, from)
	if err != nil {
		log.Debug("ResourceManager.syncVMDefinition: no source", zap.String("resource", resource), zap.Error(err))
		return nil, err
	}
	xml, sourceNode := "", src.Node
	if src.Stored {
		xml, sourceNode = def.XML, def.SourceNode
	} else {
		xml = facts[src.Node].XML
	}
	name, uuid, _, err := parseDomainXML(xml)
	if err != nil || name != g.Domain || uuid == "" {
		log.Debug("ResourceManager.syncVMDefinition: source definition unusable", zap.String("resource", resource),
			zap.String("source", sourceNode), zap.Bool("has_uuid", uuid != ""), zap.Error(err))
		return nil, fmt.Errorf("the definition of %s on %s has no <name>%s</name> and <uuid>", g.Domain, sourceNode, g.Domain)
	}
	log.Debug("ResourceManager.syncVMDefinition: source chosen", zap.String("resource", resource),
		zap.String("source", sourceNode), zap.Bool("stored", src.Stored), zap.String("uuid", uuid))
	// A running guest keeps the identity it started with: a definition under
	// another UUID could never be the one it is migrated or restarted with.
	for _, n := range nodes {
		if f := facts[n]; f != nil && f.Running && f.LibvirtUUID != "" && f.LibvirtUUID != uuid {
			log.Debug("ResourceManager.syncVMDefinition: running guest has another UUID", zap.String("node", n))
			return nil, fmt.Errorf("%s runs on %s with UUID %s, but the definition on %s has %s",
				g.Domain, n, f.LibvirtUUID, sourceNode, uuid)
		}
	}

	next := &database.VMDefinition{Resource: resource, Domain: g.Domain, UUID: uuid, XML: xml, SourceNode: sourceNode,
		NodeHashes: map[string]string{}}
	if def != nil {
		next.CreatedAt = def.CreatedAt
		next.Placed = append(next.Placed, def.Placed...)
		for n, h := range def.NodeHashes {
			next.NodeHashes[n] = h
		}
	}
	var failed []string
	for _, n := range nodes {
		f := facts[n]
		if f == nil || !f.Reachable {
			log.Debug("ResourceManager.syncVMDefinition: node did not answer, left for the next sync", zap.String("node", n))
			continue
		}
		if !f.Virsh {
			failed = append(failed, fmt.Sprintf("%s: libvirt is not installed", n))
			continue
		}
		if n == src.Node {
			// The source keeps its file; it only loses autostart.
			_, aerr := rm.runNodeScript(ctx, []string{hostOf[n]},
				fmt.Sprintf("virsh -c qemu:///system autostart --disable %s >/dev/null 2>&1; true", shellSingleQuote(g.Domain)))
			log.Debug("ResourceManager.syncVMDefinition: autostart off on the source", zap.String("node", n), zap.Error(aerr))
			continue
		}
		if err := rm.defineVMOn(ctx, resource, g, uuid, xml, hostOf[n]); err != nil {
			log.Debug("ResourceManager.syncVMDefinition: define failed", zap.String("node", n), zap.Error(err))
			failed = append(failed, err.Error())
			continue
		}
		if !f.HasConfig && f.LibvirtUUID == "" && !containsString(next.Placed, n) {
			log.Debug("ResourceManager.syncVMDefinition: node had no definition; recorded as placed", zap.String("node", n))
			next.Placed = append(next.Placed, n)
		}
	}

	// What each node now holds is what a later comparison is made against.
	after := rm.vmFactsByNode(rm.probeVMNodes(ctx, g, hosts, ""))
	for _, n := range nodes {
		if f := after[n]; f != nil && f.Reachable && f.HasConfig && f.ParseErr == "" && f.FileUUID == uuid {
			next.NodeHashes[n] = f.Hash
		}
	}
	if err := rm.controller.db.SaveVMDefinition(ctx, next); err != nil {
		log.Debug("ResourceManager.syncVMDefinition: saving the record failed", zap.String("resource", resource), zap.Error(err))
		return nil, err
	}
	st := assessVMDefinition(next, nodes, after)
	log.Debug("ResourceManager.syncVMDefinition: done", zap.String("resource", resource), zap.Bool("consistent", st.Consistent),
		zap.Strings("failed", failed))
	if len(failed) > 0 {
		return st, fmt.Errorf("%s", strings.Join(failed, "; "))
	}
	rm.controller.logger.Info("Guest definition synced", zap.String("resource", resource), zap.String("domain", g.Domain),
		zap.String("source", sourceNode), zap.Strings("nodes", nodes))
	return st, nil
}

// SyncVMDefinition is `haify ha vm-sync`.
func (rm *ResourceManager) SyncVMDefinition(ctx context.Context, resource, from string) (*VMDefinitionStatus, error) {
	log := rm.controller.traceLogger()
	log.Debug("ResourceManager.SyncVMDefinition: start", zap.String("resource", resource), zap.String("from", from))
	g, err := rm.haGuest(ctx, resource)
	if err != nil {
		return nil, err
	}
	if m := rm.liveMigrationRunning(resource); m != "" {
		log.Debug("ResourceManager.SyncVMDefinition: live migration in progress", zap.String("resource", resource))
		return nil, fmt.Errorf("%s", m)
	}
	hosts, err := rm.failoverHosts(ctx, resource)
	if err != nil {
		log.Debug("ResourceManager.SyncVMDefinition: no failover hosts", zap.String("resource", resource), zap.Error(err))
		return nil, err
	}
	def, err := rm.controller.db.GetVMDefinition(ctx, resource)
	if err != nil {
		log.Debug("ResourceManager.SyncVMDefinition: record unreadable", zap.String("resource", resource), zap.Error(err))
		return nil, err
	}
	return rm.syncVMDefinition(ctx, resource, g, hosts, def, from)
}

// PrepareVMDefinition runs before an HA config that starts a libvirt guest
// is written: it takes the guest's definition and defines it on every
// replica, so no promoter is ever enabled on a node that could not start the
// guest. A config that runs no guest is left alone.
func (rm *ResourceManager) PrepareVMDefinition(ctx context.Context, resource string, ocfAgents []OcfAgentSpec, startItems []HaStartItem, from string) error {
	log := rm.controller.traceLogger()
	g := vmGuestOf(&database.HaConfig{Resource: resource, OcfAgents: ocfAgentRecords(ocfAgents), StartItems: startItemRecords(startItems)})
	if g == nil {
		log.Debug("ResourceManager.PrepareVMDefinition: no libvirt guest in the start list", zap.String("resource", resource))
		return nil
	}
	log.Debug("ResourceManager.PrepareVMDefinition: start", zap.String("resource", resource), zap.String("domain", g.Domain),
		zap.String("from", from))
	if rm.controller.db == nil || rm.deployment == nil {
		log.Debug("ResourceManager.PrepareVMDefinition: no database or deployment client", zap.String("resource", resource))
		return fmt.Errorf("database or deployment client not available")
	}
	hosts, err := rm.failoverHosts(ctx, resource)
	if err != nil {
		log.Debug("ResourceManager.PrepareVMDefinition: no failover hosts", zap.String("resource", resource), zap.Error(err))
		return err
	}
	def, err := rm.controller.db.GetVMDefinition(ctx, resource)
	if err != nil {
		return err
	}
	if def != nil && def.Domain != g.Domain {
		log.Debug("ResourceManager.PrepareVMDefinition: record is of another guest; starting over",
			zap.String("resource", resource), zap.String("was", def.Domain))
		def = nil
	}
	st, err := rm.syncVMDefinition(ctx, resource, g, hosts, def, from)
	if err != nil {
		return fmt.Errorf("guest %s: %w", g.Domain, err)
	}
	// A replica that did not answer has no definition yet and would be
	// handed a promoter all the same.
	for _, n := range st.Nodes {
		if n.State != vmDefOK {
			log.Debug("ResourceManager.PrepareVMDefinition: a replica lacks the definition", zap.String("node", n.Node), zap.String("state", n.State))
			return fmt.Errorf("guest %s: %s: %s", g.Domain, n.Node, vmDefStateText(n))
		}
	}
	return nil
}

// checkVMDefinitionBeforeMove runs before a guest is moved, live or not. A
// replica that lacks the definition gets it; a copy that differs is the
// operator's to settle, and must names the nodes on which that stops the
// move (every replica when must is empty).
func (rm *ResourceManager) checkVMDefinitionBeforeMove(ctx context.Context, resource string, g *vmGuest, hosts []string, must []string) error {
	log := rm.controller.traceLogger()
	log.Debug("ResourceManager.checkVMDefinitionBeforeMove: start", zap.String("resource", resource), zap.Strings("must", must))
	def, err := rm.controller.db.GetVMDefinition(ctx, resource)
	if err != nil {
		return err
	}
	if def == nil || def.Domain != g.Domain {
		// An HA config from before definitions were kept: take it now.
		log.Debug("ResourceManager.checkVMDefinitionBeforeMove: no record; first sync", zap.String("resource", resource))
		if _, err := rm.syncVMDefinition(ctx, resource, g, hosts, nil, ""); err != nil {
			return fmt.Errorf("guest %s: %w", g.Domain, err)
		}
		return nil
	}
	nodes := rm.nodeNames(hosts)
	st := assessVMDefinition(def, nodes, rm.vmFactsByNode(rm.probeVMNodes(ctx, g, hosts, "")))
	missing := false
	for _, n := range st.Nodes {
		missing = missing || n.State == vmDefMissing
	}
	if missing {
		log.Debug("ResourceManager.checkVMDefinitionBeforeMove: a replica lacks the definition; placing it", zap.String("resource", resource))
		if err := rm.syncVMDefinitionPlacement(ctx, resource, hosts, nil); err != nil {
			return err
		}
		if def, err = rm.controller.db.GetVMDefinition(ctx, resource); err != nil || def == nil {
			return fmt.Errorf("guest %s: its definition record could not be read back", g.Domain)
		}
		st = assessVMDefinition(def, nodes, rm.vmFactsByNode(rm.probeVMNodes(ctx, g, hosts, "")))
	}
	var off []string
	for _, n := range st.Nodes {
		if n.State == vmDefOK || n.State == vmDefUnreachable || (len(must) > 0 && !containsString(must, n.Node)) {
			continue
		}
		off = append(off, fmt.Sprintf("%s: %s", n.Node, vmDefStateText(n)))
	}
	if len(off) > 0 {
		log.Debug("ResourceManager.checkVMDefinitionBeforeMove: definitions differ", zap.String("resource", resource), zap.Strings("off", off))
		return fmt.Errorf("the definition of %s is not the same on every replica (%s). Run `haify ha vm-sync %s`, "+
			"with --from <node> to name the copy to keep, then try again", g.Domain, strings.Join(off, "; "), resource)
	}
	return nil
}

// checkGuestBeforeEvict runs before a plain evict of a resource that runs a
// guest: drbd-reactor picks the replica that takes over, so every one of
// them must be able to start it.
func (rm *ResourceManager) checkGuestBeforeEvict(ctx context.Context, resource string) error {
	log := rm.controller.traceLogger()
	if rm.controller.db == nil {
		return nil
	}
	cfg, err := rm.controller.db.GetHaConfig(ctx, resource)
	if err != nil || cfg == nil {
		// A gateway, an app, the controller's own resource: no guest.
		log.Debug("ResourceManager.checkGuestBeforeEvict: no HA config", zap.String("resource", resource))
		return nil
	}
	g := vmGuestOf(cfg)
	if g == nil {
		log.Debug("ResourceManager.checkGuestBeforeEvict: no libvirt guest", zap.String("resource", resource))
		return nil
	}
	hosts, err := rm.failoverHosts(ctx, resource)
	if err != nil {
		log.Debug("ResourceManager.checkGuestBeforeEvict: no failover hosts", zap.String("resource", resource), zap.Error(err))
		return err
	}
	return rm.checkVMDefinitionBeforeMove(ctx, resource, g, hosts, nil)
}
