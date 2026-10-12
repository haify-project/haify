package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
)

// The files on every node that name a renumbered node by address: /etc/hosts
// and root's known_hosts.

// updateHostsFiles points every node's /etc/hosts entry for the node at its
// new address. Every IPv4 entry naming the host is rewritten, not only the one
// with the address being replaced: registration appends entries and nothing
// ever removed one, so a node renumbered before this existed has several, all
// stale. Loopback lines are left alone — a host names itself on 127.0.1.1 —
// and the duplicates the rewrite produces are dropped. Best effort: a stale
// entry misleads a person, not the controller, which dials addresses.
func (nm *NodeManager) updateHostsFiles(ctx context.Context, hostname, old, address string) {
	log := nm.controller.traceLogger()
	log.Debug("NodeManager.updateHostsFiles: start", zap.String("hostname", hostname),
		zap.String("from", old), zap.String("to", address))
	if hostname == "" || old == address {
		log.Debug("NodeManager.updateHostsFiles: skipped, no hostname or address unchanged",
			zap.String("hostname", hostname), zap.Bool("unchanged", old == address))
		return
	}
	nm.controller.hostsLock.RLock()
	hosts := append([]string(nil), nm.controller.hosts...)
	nm.controller.hostsLock.RUnlock()
	start := time.Now()
	res, err := nm.controller.deployment.Exec(ctx, hosts,
		"echo "+base64Std(hostsFileScript(hostname, address))+" | base64 -d | sudo /bin/sh")
	if err != nil {
		log.Debug("NodeManager.updateHostsFiles: rewrite of /etc/hosts failed to run", zap.Strings("hosts", hosts),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		nm.controller.logger.Warn("Failed to update /etc/hosts for a renumbered node", zap.Error(err))
		return
	}
	log.Debug("NodeManager.updateHostsFiles: rewrote /etc/hosts", zap.Strings("hosts", hosts),
		zap.Bool("success", res.AllSuccess()), zap.Strings("failed_hosts", res.FailedHosts()), zap.Duration("took", time.Since(start)))
}

// updateKnownHosts puts each renumbered node's host keys into every node's
// root known_hosts under its new address, and forgets what was recorded under
// its old one.
//
// Without it the renumbering works from the node running the controller and
// breaks the day another node takes over: when addresses are reshuffled — a
// DHCP lease swap, two nodes trading places — each node still holds the key
// of the machine that used to be at an address, and accept-new refuses a key
// that changed. On a test cluster that surfaced weeks later as "mkdir
// failed" on the first resource created after a controller failover.
//
// The keys are read over the connection that just proved the node answers at
// its new address with its own hostname. Every stale entry is removed before
// any key is added, so a node moving onto another's old address does not have
// its fresh entry removed along with the other's stale one.
func (nm *NodeManager) updateKnownHosts(ctx context.Context, moves []NodeMove) {
	log := nm.controller.traceLogger()
	log.Debug("NodeManager.updateKnownHosts: start", zap.Int("count", len(moves)))
	var forget []string
	var entries strings.Builder
	for _, mv := range moves {
		forget = append(forget, mv.OldAddress, mv.Address)
		start := time.Now()
		res, err := nm.controller.deployment.Exec(ctx, []string{mv.Address}, "cat /etc/ssh/ssh_host_*_key.pub")
		if err != nil {
			log.Debug("NodeManager.updateKnownHosts: reading host keys failed to run", zap.String("node", mv.Node),
				zap.String("host", mv.Address), zap.Duration("took", time.Since(start)), zap.Error(err))
			nm.controller.logger.Warn("Read host keys of a renumbered node", zap.String("node", mv.Node), zap.Error(err))
			continue
		}
		keys := 0
		for _, hr := range res.Hosts {
			if !hr.Success {
				log.Debug("NodeManager.updateKnownHosts: reading host keys failed", zap.String("node", mv.Node),
					zap.String("host", mv.Address))
				continue
			}
			for _, line := range strings.Split(hr.Output, "\n") {
				f := strings.Fields(line)
				if len(f) >= 2 && (strings.HasPrefix(f[0], "ssh-") || strings.HasPrefix(f[0], "ecdsa-")) {
					fmt.Fprintf(&entries, "%s %s %s\n", mv.Address, f[0], f[1])
					keys++
				}
			}
		}
		log.Debug("NodeManager.updateKnownHosts: read host keys", zap.String("node", mv.Node),
			zap.String("host", mv.Address), zap.Int("count", keys), zap.Duration("took", time.Since(start)))
	}
	nm.controller.hostsLock.RLock()
	hosts := append([]string(nil), nm.controller.hosts...)
	nm.controller.hostsLock.RUnlock()
	start := time.Now()
	res, err := nm.controller.deployment.Exec(ctx, hosts,
		"echo "+base64Std(knownHostsScript(forget, entries.String()))+" | base64 -d | sudo /bin/sh")
	if err != nil {
		log.Debug("NodeManager.updateKnownHosts: known_hosts update failed to run", zap.Strings("hosts", hosts),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		nm.controller.logger.Warn("Failed to update known_hosts for renumbered nodes", zap.Error(err))
		return
	}
	log.Debug("NodeManager.updateKnownHosts: updated known_hosts", zap.Strings("hosts", hosts),
		zap.Strings("forget", forget), zap.Bool("success", res.AllSuccess()),
		zap.Strings("failed_hosts", res.FailedHosts()), zap.Duration("took", time.Since(start)))
}

// knownHostsScript removes every address in forget from root's known_hosts,
// then appends entries.
func knownHostsScript(forget []string, entries string) string {
	var b strings.Builder
	b.WriteString("f=/root/.ssh/known_hosts\nmkdir -p /root/.ssh && touch \"$f\"\n")
	seen := map[string]bool{}
	for _, a := range forget {
		if a == "" || seen[a] {
			continue
		}
		seen[a] = true
		fmt.Fprintf(&b, "ssh-keygen -R %q -f \"$f\" >/dev/null 2>&1\n", a)
	}
	if entries != "" {
		fmt.Fprintf(&b, "cat >> \"$f\" <<'HAIFY_KNOWN_HOSTS'\n%sHAIFY_KNOWN_HOSTS\n", entries)
	}
	b.WriteString("rm -f \"$f.old\"\n")
	zap.L().Debug("knownHostsScript: built", zap.Int("forget", len(seen)), zap.Bool("has_entries", entries != ""))
	return b.String()
}

// hostsFileScript rewrites /etc/hosts in place; see updateHostsFiles.
func hostsFileScript(hostname, address string) string {
	return fmt.Sprintf(`f=/etc/hosts
awk -v new=%[1]q -v name=%[2]q '
$1 !~ /^127\./ && $1 !~ /:/ { for (i = 2; i <= NF; i++) if ($i == name) { $1 = new; break } }
!seen[$0]++ { print }' "$f" > "$f.haify-new" && cat "$f.haify-new" > "$f" && rm -f "$f.haify-new"
`, address, hostname)
}
