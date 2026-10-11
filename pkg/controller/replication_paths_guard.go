package controller

import (
	"context"
	"fmt"
)

// The path guard. DRBD's TCP transport will not connect at all while any one
// path's local address is missing — "Configured local address not found,
// retrying every 10 sec" — so a second NIC that is down at boot, or whose
// address the network manager dropped with the link, cut a node off on the
// network that still worked: the opposite of what the second path is for.
// Seen on a lab node rebooted without its second address.
//
// On every node with extra replication addresses a timer drops, every ten
// seconds, each path whose local address is gone (never a connection's last
// path), and puts back the paths the config has whose addresses are there
// again. It changes only paths, so the connection stays up throughout.

const (
	pathsGuardScript = "/usr/local/sbin/haify-paths-guard"
	pathsGuardUnit   = "haify-paths-guard"
)

const pathsGuardSh = `#!/bin/sh
# Keep DRBD connections up when a replication network's address is missing:
# drop such paths, restore them when the address returns. Installed by Haify.
addrs=" $(ip -4 -o addr show | awk '{split($4,a,"/"); printf "%s ", a[1]}') "
has() { case "$addrs" in *" $1 "*) return 0;; esac; return 1; }
ipof() { x=${1#ipv4:}; echo "${x%:*}"; }
ev=$(drbdsetup events2 --now 2>/dev/null | awk '$1=="exists" && $2=="path"')
echo "$ev" | awk '{for(i=3;i<=NF;i++){split($i,kv,":"); if(kv[1]=="name")n=kv[2]; if(kv[1]=="peer-node-id")p=kv[2]}; print n, p}' |
  sort | uniq -c | while read -r count name peer; do
  [ "$count" -gt 1 ] || continue
  echo "$ev" | grep " name:$name " | grep " peer-node-id:$peer " | while read -r line; do
    l=$(echo "$line" | sed -n 's/.* local:\([^ ]*\).*/\1/p'); r=$(echo "$line" | sed -n 's/.* peer:\([^ ]*\).*/\1/p')
    has "$(ipof "$l")" && continue
    drbdsetup del-path "$name" "$peer" "$l" "$r" && logger -t haify-paths "$name: dropped path $l -> $r, the local address is gone"
  done
done
for res in $(drbdsetup status 2>/dev/null | awk '/^[^ ]/{print $1}'); do
  drbdadm -d adjust "$res" 2>/dev/null | awk '$1=="drbdsetup" && $2=="new-path"' | while read -r _ _ name peer l r; do
    has "$(ipof "$l")" || continue
    drbdsetup new-path "$name" "$peer" "$l" "$r" && logger -t haify-paths "$name: restored path $l -> $r"
  done
done
true
`

const pathsGuardService = `[Unit]
Description=Haify: keep DRBD connections up when a replication network is missing
After=drbd.service

[Service]
Type=oneshot
ExecStart=/bin/sh ` + pathsGuardScript + `
`

const pathsGuardTimer = `[Unit]
Description=Haify: check DRBD replication paths every 10 seconds

[Timer]
OnBootSec=5s
OnUnitActiveSec=10s
AccuracySec=1s

[Install]
WantedBy=timers.target
`

// setPathsGuard installs (on) or removes (off) the guard on host.
func (rm *ResourceManager) setPathsGuard(ctx context.Context, host string, on bool) error {
	hosts := []string{host}
	if !on {
		_, err := rm.deployment.Exec(ctx, hosts, fmt.Sprintf(
			"sudo systemctl disable --now %[1]s.timer >/dev/null 2>&1; sudo rm -f /etc/systemd/system/%[1]s.timer /etc/systemd/system/%[1]s.service %[2]s; sudo systemctl daemon-reload; true",
			pathsGuardUnit, pathsGuardScript))
		return err
	}
	for path, content := range map[string]string{
		pathsGuardScript: pathsGuardSh,
		"/etc/systemd/system/" + pathsGuardUnit + ".service": pathsGuardService,
		"/etc/systemd/system/" + pathsGuardUnit + ".timer":   pathsGuardTimer,
	} {
		if _, err := rm.deployment.DistributeConfig(ctx, hosts, content, path); err != nil {
			return fmt.Errorf("install %s: %w", path, err)
		}
	}
	return execFailure(rm.deployment.Exec(ctx, hosts,
		"sudo systemctl daemon-reload && sudo systemctl enable --now "+pathsGuardUnit+".timer"))
}
