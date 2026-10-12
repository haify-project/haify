package controller

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"
)

// After an eviction: `node lost` and `node restore`.
//
// An evicted node's replicas were replaced while it was away, but nothing
// could touch the node itself: it still has the old DRBD configs, the backing
// volumes and the promoter configs of resources it is no longer part of. Left
// alone, it would bring them up on boot with the old membership.
//
// `node lost` is for a node that will not come back: every replica it still
// holds is removed the lost way (removereplica_lost.go), and it stays evicted.
// `node restore` is for one that did come back: whatever it holds of
// resources it is no longer a member of is taken down and deleted there, and
// it may take replicas again.

// NodeLost removes every replica node still holds, without contacting it.
func (rm *ResourceManager) NodeLost(ctx context.Context, node string) ([]string, error) {
	log := rm.resourcesLog()
	log.Debug("NodeLost: start", zap.String("node", node))
	addr := rm.controller.nodes.GetNodeAddressByName(node)
	if addr == "" {
		log.Debug("NodeLost: node not registered", zap.String("node", node))
		return nil, fmt.Errorf("node %q is not registered", node)
	}
	if rm.answers(ctx, addr) {
		log.Debug("NodeLost: node answers; refusing", zap.String("node", node))
		return nil, fmt.Errorf("%s answers over SSH; restore it with `haify node restore %s` instead", node, node)
	}
	resources, err := rm.controller.db.ListResources(ctx)
	log.Debug("NodeLost: listed resources", zap.Int("count", len(resources)), zap.Error(err))
	if err != nil {
		return nil, err
	}
	var done, failed []string
	for _, r := range resources {
		if !contains(splitCSV(r.Nodes), node) {
			continue
		}
		if err := rm.RemoveReplicaOptions(ctx, r.Name, node, true); err != nil {
			log.Debug("NodeLost: replica not removed", zap.String("resource", r.Name), zap.Error(err))
			failed = append(failed, fmt.Sprintf("%s: %v", r.Name, err))
			continue
		}
		log.Debug("NodeLost: replica removed", zap.String("resource", r.Name), zap.String("node", node))
		done = append(done, r.Name)
	}
	err = rm.controller.nodes.SetNodeState(ctx, addr, NodeStateEvicted)
	log.Debug("NodeLost: marked evicted", zap.String("node", node), zap.Int("done", len(done)), zap.Int("failed", len(failed)), zap.Error(err))
	if err != nil {
		return done, err
	}
	if len(failed) > 0 {
		return done, fmt.Errorf("not removed: %s", strings.Join(failed, "; "))
	}
	return done, nil
}

var resFileRE = regexp.MustCompile(`^/etc/drbd\.d/([A-Za-z0-9][A-Za-z0-9_.-]*)\.res$`)

// NodeRestore cleans what node holds of resources it is no longer a member of
// and lets it take replicas again. With dryRun it only returns the plan.
func (rm *ResourceManager) NodeRestore(ctx context.Context, node string, dryRun bool) ([]string, error) {
	log := rm.resourcesLog()
	log.Debug("NodeRestore: start", zap.String("node", node), zap.Bool("dry_run", dryRun))
	addr := rm.controller.nodes.GetNodeAddressByName(node)
	if addr == "" {
		log.Debug("NodeRestore: node not registered", zap.String("node", node))
		return nil, fmt.Errorf("node %q is not registered", node)
	}
	if !rm.answers(ctx, addr) {
		log.Debug("NodeRestore: node does not answer", zap.String("node", node))
		return nil, fmt.Errorf("%s does not answer over SSH; it cannot be restored until it does", node)
	}
	start := time.Now()
	held, err := rm.deployment.Exec(ctx, []string{addr}, "ls /etc/drbd.d/*.res 2>/dev/null; true")
	log.Debug("NodeRestore: listed configs on node", zap.String("host", addr), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, err
	}
	resources, err := rm.controller.db.ListResources(ctx)
	log.Debug("NodeRestore: listed resources", zap.Int("count", len(resources)), zap.Error(err))
	if err != nil {
		return nil, err
	}
	member := map[string]bool{}
	for _, r := range resources {
		for _, list := range []string{r.Nodes, r.DisklessNodes, r.DisklessClients} {
			if contains(splitCSV(list), node) {
				member[r.Name] = true
			}
		}
	}
	var stale []string
	for _, line := range execLines(held, addr) {
		if m := resFileRE.FindStringSubmatch(strings.TrimSpace(line)); m != nil && !member[m[1]] {
			stale = append(stale, m[1])
		}
	}
	sort.Strings(stale)
	log.Debug("NodeRestore: stale resources on node", zap.String("node", node), zap.Strings("stale", stale))
	var plan []string
	for _, r := range stale {
		plan = append(plan, fmt.Sprintf("take %s down and delete its config, promoter configs and volumes on %s", r, node))
		if dryRun {
			log.Debug("NodeRestore: dry run; would clean", zap.String("node", node), zap.String("resource", r))
			continue
		}
		// Base64-wrapped: dispatch's sh -c quoting empties the script's $vars.
		cmd := "echo " + base64Std(staleResourceCleanup(r)) + " | base64 -d | sudo /bin/bash"
		start := time.Now()
		if err := rm.execAllSuccess(ctx, []string{addr}, cmd, "clean "+r+" on "+node); err != nil {
			log.Debug("NodeRestore: cleanup failed", zap.String("node", node), zap.String("resource", r),
				zap.Duration("took", time.Since(start)), zap.Error(err))
			return plan, err
		}
		rm.controller.logger.Info("Restore: cleaned a resource the node no longer belongs to",
			zap.String("node", node), zap.String("resource", r))
	}
	if dryRun {
		return plan, nil
	}
	_, rerr := rm.deployment.Exec(ctx, []string{addr}, "sudo systemctl reload drbd-reactor 2>/dev/null; true")
	err = rm.controller.nodes.SetNodeState(ctx, addr, NodeStateOnline)
	log.Debug("NodeRestore: reloaded drbd-reactor and marked online", zap.String("node", node),
		zap.NamedError("reload_error", rerr), zap.Error(err))
	return plan, err
}

// staleResourceCleanup takes a resource down on a node that is no longer its
// member and deletes what it holds there: the config, the promoter configs,
// and the backing volumes haify names after it ("<r>_data", "<r>_vol<K>") with
// their snapshots. An encrypted LV's LUKS container is closed and its key
// destroyed first (the open container would block the removal, and the key
// must not outlive the data); ZFS volumes are never LUKS-encrypted. Nothing
// else is touched.
func staleResourceCleanup(r string) string {
	return fmt.Sprintf(`set -e
drbdadm down %[1]s 2>/dev/null || true
rm -f /etc/drbd.d/%[1]s.res /etc/drbd-reactor.d/haify-*-%[1]s.toml /etc/drbd-reactor.d/haify-*-%[1]s.toml.pending
close_luks() {
	n="haify_$1_$2"
	cryptsetup close "$n" 2>/dev/null || true
	if [ -f "%[2]s/$n.key" ]; then
		shred -u "%[2]s/$n.key" 2>/dev/null || true
	fi
	rm -f "%[2]s/$n.key" "%[2]s/$n.dev"
}
for lv in $(lvs --noheadings -o vg_name,lv_name,origin --separator / 2>/dev/null | tr -d ' ' | \
	awk -F/ '$2 ~ /^%[1]s_(data|vol[0-9]+)$/ || $3 ~ /^%[1]s_(data|vol[0-9]+)$/ {print $1"/"$2}' | sort -r); do
	close_luks "${lv%%%%/*}" "${lv#*/}"
	lvremove -f "$lv"
done
for ds in $(zfs list -H -o name 2>/dev/null | awk -F/ '$NF ~ /^%[1]s_(data|vol[0-9]+)$/'); do
	zfs destroy -r "$ds"
done
`, r, luksKeyDir)
}
