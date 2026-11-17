package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Where an HA guest's definition is, as the replica set and the HA config
// change: a new replica is given it, and a node that stops being one — or
// every node, when the HA config is deleted — loses the copy Haify placed.
//
// Removing is the dangerous half. `virsh undefine` on a node where the guest
// runs turns it into a guest that is gone at its next shutdown, so the node
// itself checks, in the same script that would remove it, that the guest is
// not running there, that libvirt answered at all, and that the definition
// is the one Haify placed. A definition the operator made is never removed.

// vmUndefineScript removes a definition Haify placed, unless the guest runs
// on the node, libvirt could not say whether it does, or the definition there
// is not the one Haify placed.
func vmUndefineScript(g *vmGuest, uuid string) string {
	return fmt.Sprintf(`d=%s
cfg=%s
want=%s
V="virsh -c qemu:///system"
st=$(LANG=C $V domstate "$d" 2>&1 | head -n1)
case "$st" in
  "shut off"|crashed|*"failed to get domain"*|*"omain not found"*) ;;
  *) echo "HAIFY_KEPT=the guest is $st here"; exit 0 ;;
esac
if pgrep -f "guest=$d[, ]" >/dev/null 2>&1; then echo "HAIFY_KEPT=a qemu process of the guest runs here"; exit 0; fi
cur=$($V domuuid "$d" 2>/dev/null | head -n1)
if [ -n "$cur" ]; then
  [ "$cur" = "$want" ] || { echo "HAIFY_KEPT=the definition here has another UUID ($cur)"; exit 0; }
  $V undefine --keep-nvram "$d" >/dev/null 2>&1 || $V undefine "$d" >/dev/null 2>&1 || { echo "HAIFY_ERR=virsh undefine failed"; exit 4; }
fi
if [ -f "$cfg" ]; then
  if grep -qi "<uuid>$want</uuid>" "$cfg"; then rm -f "$cfg"; else echo "HAIFY_KEPT=the definition file here has another UUID"; exit 0; fi
fi
echo HAIFY_REMOVED=1
`, shellSingleQuote(g.Domain), shellSingleQuote(g.ConfigPath), shellSingleQuote(uuid))
}

// syncVMDefinitionPlacement follows a change of the replica set: a replica
// that lacks the guest's definition gets it, and a node that stopped being
// one loses the definition Haify placed there.
func (rm *ResourceManager) syncVMDefinitionPlacement(ctx context.Context, resource string, run, former []string) error {
	log := rm.controller.traceLogger()
	if rm.controller.db == nil {
		return nil
	}
	def, err := rm.controller.db.GetVMDefinition(ctx, resource)
	if err != nil || def == nil {
		log.Debug("ResourceManager.syncVMDefinitionPlacement: no guest definition kept", zap.String("resource", resource), zap.Error(err))
		return err
	}
	g := &vmGuest{Domain: def.Domain, ConfigPath: "/etc/libvirt/qemu/" + def.Domain + ".xml"}
	log.Debug("ResourceManager.syncVMDefinitionPlacement: start", zap.String("resource", resource), zap.String("domain", def.Domain),
		zap.Strings("hosts", run), zap.Strings("former", former))
	facts := rm.probeVMNodes(ctx, g, run, "")
	var errs []string
	changed := false
	for _, h := range run {
		f, node := facts[h], rm.nodeLabel(h)
		if f == nil || !f.Reachable || f.HasConfig || f.LibvirtUUID != "" {
			continue
		}
		log.Debug("ResourceManager.syncVMDefinitionPlacement: replica lacks the definition", zap.String("node", node))
		if !f.Virsh {
			errs = append(errs, fmt.Sprintf("%s holds a replica of %s but has no libvirt to run %s", node, resource, def.Domain))
			continue
		}
		if err := rm.defineVMOn(ctx, resource, g, def.UUID, def.XML, h); err != nil {
			errs = append(errs, err.Error())
			continue
		}
		if after := rm.probeVMNodes(ctx, g, []string{h}, "")[h]; after != nil && after.HasConfig && after.ParseErr == "" {
			if def.NodeHashes == nil {
				def.NodeHashes = map[string]string{}
			}
			def.NodeHashes[node] = after.Hash
		}
		if !containsString(def.Placed, node) {
			def.Placed = append(def.Placed, node)
		}
		changed = true
		rm.controller.logger.Info("Guest defined on a new replica", zap.String("resource", resource),
			zap.String("domain", def.Domain), zap.String("node", node))
	}
	runSet := map[string]bool{}
	for _, h := range run {
		runSet[h] = true
	}
	for _, h := range rm.resolveAll(former) {
		node := rm.nodeLabel(h)
		if runSet[h] || !containsString(def.Placed, node) {
			log.Debug("ResourceManager.syncVMDefinitionPlacement: former node keeps what it has", zap.String("node", node),
				zap.Bool("still_replica", runSet[h]))
			continue
		}
		if rm.undefineVMOn(ctx, resource, g, def.UUID, h) {
			def.Placed = without(def.Placed, node)
			delete(def.NodeHashes, node)
			changed = true
		}
	}
	if changed {
		if err := rm.controller.db.SaveVMDefinition(ctx, def); err != nil {
			log.Debug("ResourceManager.syncVMDefinitionPlacement: saving the record failed", zap.Error(err))
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("guest %s: %s", def.Domain, strings.Join(errs, "; "))
	}
	return nil
}

// undefineVMOn removes the definition Haify placed on host and reports
// whether it is gone. The node decides, in one script, whether that is safe.
func (rm *ResourceManager) undefineVMOn(ctx context.Context, resource string, g *vmGuest, uuid, host string) bool {
	log := rm.controller.traceLogger()
	start := time.Now()
	res, err := rm.runNodeScript(ctx, []string{host}, vmUndefineScript(g, uuid))
	out, _ := appHostOutput(res, host)
	kv := keyValues(out)
	log.Debug("ResourceManager.undefineVMOn: ran", zap.String("resource", resource), zap.String("domain", g.Domain),
		zap.String("host", host), zap.String("kept", kv["HAIFY_KEPT"]), zap.Bool("removed", kv["HAIFY_REMOVED"] == "1"),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if kv["HAIFY_REMOVED"] == "1" {
		rm.controller.logger.Info("Guest definition removed", zap.String("resource", resource),
			zap.String("domain", g.Domain), zap.String("node", rm.nodeLabel(host)))
		return true
	}
	reason := kv["HAIFY_KEPT"]
	if reason == "" {
		reason = "the node did not answer"
	}
	rm.controller.logger.Warn("Guest definition left in place", zap.String("resource", resource),
		zap.String("domain", g.Domain), zap.String("node", rm.nodeLabel(host)), zap.String("reason", reason))
	return false
}

// removeVMDefinition follows `ha delete`: the definitions Haify placed are
// removed, the ones that were there before stay, and so does the one on the
// node the guest runs on.
func (rm *ResourceManager) removeVMDefinition(ctx context.Context, resource string) {
	log := rm.controller.traceLogger()
	if rm.controller.db == nil {
		return
	}
	def, err := rm.controller.db.GetVMDefinition(ctx, resource)
	if err != nil || def == nil {
		log.Debug("ResourceManager.removeVMDefinition: no guest definition kept", zap.String("resource", resource), zap.Error(err))
		return
	}
	g := &vmGuest{Domain: def.Domain, ConfigPath: "/etc/libvirt/qemu/" + def.Domain + ".xml"}
	log.Debug("ResourceManager.removeVMDefinition: start", zap.String("resource", resource), zap.String("domain", def.Domain),
		zap.Strings("placed", def.Placed))
	for _, node := range def.Placed {
		rm.undefineVMOn(ctx, resource, g, def.UUID, rm.controller.ResolveHost(node))
	}
	if err := rm.controller.db.DeleteVMDefinition(ctx, resource); err != nil {
		rm.controller.logger.Warn("Failed to delete the guest definition record", zap.String("resource", resource), zap.Error(err))
	}
}

// dropUnusedVMDefinition removes a guest definition record, and the copies
// Haify placed, when the HA config it was taken for was not created after
// all.
func (rm *ResourceManager) dropUnusedVMDefinition(ctx context.Context, resource string) {
	if rm.controller.db == nil {
		return
	}
	if cfg, err := rm.controller.db.GetHaConfig(ctx, resource); err == nil && cfg != nil {
		rm.controller.traceLogger().Debug("ResourceManager.dropUnusedVMDefinition: an HA config exists; record kept",
			zap.String("resource", resource))
		return
	}
	rm.removeVMDefinition(ctx, resource)
}
