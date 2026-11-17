package controller

import (
	"fmt"
	"strings"
	"time"
)

// The node-side steps of a live move of an HA guest. Each is a small root
// script that says what it did in KEY=value lines, so the controller never
// has to trust an exit code alone.

const (
	// liveStateDir is the libvirt hook's state directory; the hook leaves a
	// resource alone while a marker for it is in liveStateDir/controlled.
	liveStateDir = "/run/haify-libvirt"
	// liveGuardFile is the drop-in that keeps systemd from restarting the
	// guest's agent on the node the guest leaves.
	liveGuardFile = "zz-haify-live.conf"
)

// liveMoveUnit is the transient unit `virsh migrate` runs in.
func liveMoveUnit(resource string) string { return "haify-live-" + resource }

// liveMarkScript tells the libvirt hook on a node that the controller is
// moving the guest of resource until deadline, so the hook does not also
// promote, demote or toggle dual-primary for it.
func liveMarkScript(resource string, deadline time.Time) string {
	return fmt.Sprintf("mkdir -p %[1]s/controlled && echo %[3]d > %[1]s/controlled/%[2]s && echo HAIFY_MARKED=1\n",
		liveStateDir, resource, deadline.Unix())
}

// liveUnmarkScript removes the marker and what the move left in liveStateDir.
func liveUnmarkScript(resource string) string {
	return fmt.Sprintf("rm -f %[1]s/controlled/%[2]s %[1]s/live-%[2]s.sh %[1]s/live-%[2]s.rc %[1]s/live-%[2]s.log\n"+
		"systemctl reset-failed %[3]s 2>/dev/null\ntrue\n", liveStateDir, resource, shellSingleQuote(liveMoveUnit(resource)))
}

// liveGuardScript sets Restart=no on the guest's agent unit.
//
// drbd-reactor runs an OCF agent in ocf.rs@<instance>.service: a wrapper that
// starts the agent and then monitors it, with Restart=always. Once the guest
// has migrated away the monitor finds it gone, the wrapper exits, and systemd
// starts the unit again — which is the agent starting the guest a second time
// on the node it just left, while DRBD still allows two Primaries. With
// Restart=no the unit only fails. The drop-in is in /run, so a reboot of the
// node removes it.
func liveGuardScript(unit string) string {
	return fmt.Sprintf(`unit=%s
d=/run/systemd/system/$unit.d
mkdir -p "$d" && printf '[Service]\nRestart=no\n' > "$d/%s" && systemctl daemon-reload
echo "HAIFY_RESTART=$(systemctl show -p Restart --value "$unit" 2>/dev/null)"
`, shellSingleQuote(unit), liveGuardFile)
}

// liveUnguardScript removes the drop-in again.
func liveUnguardScript(unit string) string {
	return fmt.Sprintf(`unit=%s
f=/run/systemd/system/$unit.d/%s
if [ -e "$f" ]; then rm -f "$f"; systemctl daemon-reload; fi
echo "HAIFY_RESTART=$(systemctl show -p Restart --value "$unit" 2>/dev/null)"
`, shellSingleQuote(unit), liveGuardFile)
}

// liveDisableScript takes the resource away from drbd-reactor on a node the
// way `drbd-reactorctl disable` does: the config file is renamed and the
// daemon reloaded. The services it started are not stopped — the promoter
// only stops them on exit when stop-services-on-exit is set — so the guest
// keeps running. Stopping the target instead would shut the guest down, and
// leaving the config in place would have drbd-reactor on the other replicas
// start the guest the moment DRBD says they may promote.
func liveDisableScript(resource string) string {
	return fmt.Sprintf(`cfg=%s
if [ -f "$cfg" ]; then mv "$cfg" "$cfg.disabled" || exit 1; fi
[ -f "$cfg.disabled" ] || { echo "HAIFY_ERR=no promoter config"; exit 1; }
systemctl reload drbd-reactor || { echo "HAIFY_ERR=drbd-reactor did not reload"; exit 1; }
sleep 2
echo HAIFY_DISABLED=1
`, shellSingleQuote(haTomlPath(resource)))
}

// liveEnableScript gives the resource back to drbd-reactor on a node.
func liveEnableScript(resource string) string {
	return fmt.Sprintf(`cfg=%s
if [ -f "$cfg.disabled" ] && [ ! -f "$cfg" ]; then mv "$cfg.disabled" "$cfg" || exit 1; fi
[ -f "$cfg" ] || { echo "HAIFY_ERR=no promoter config"; exit 1; }
rm -f "$cfg.disabled"
systemctl reload drbd-reactor || systemctl restart drbd-reactor || { echo "HAIFY_ERR=drbd-reactor did not reload"; exit 1; }
echo HAIFY_ENABLED=1
`, shellSingleQuote(haTomlPath(resource)))
}

// liveMoveURIs are the libvirt connection and the migration data address for
// a move to addr. The data address is given so libvirt does not derive it
// from the target's host name, which the source need not resolve — or, on a
// Debian host, resolves to 127.0.1.1.
func liveMoveURIs(addr string) (dest, data string) {
	host := addr
	if strings.Contains(addr, ":") && !strings.HasPrefix(addr, "[") {
		host = "[" + addr + "]"
	}
	return "qemu+ssh://" + host + "/system", "tcp://" + host
}

// liveConnectScript checks, from the source, that libvirt on the target can
// be reached the way the migration will reach it.
func liveConnectScript(addr string) string {
	dest, _ := liveMoveURIs(addr)
	return fmt.Sprintf(`out=$(timeout 20 virsh -c %s uri 2>&1) && echo HAIFY_CONNECTED=1 || echo "HAIFY_ERR=$(printf '%%s' "$out" | tr '\n' ' ')"
`, shellSingleQuote(dest+"?no_tty=1"))
}

// liveStartMoveScript starts `virsh migrate --live` in a transient unit of
// its own, so it neither dies with the SSH session that started it nor with
// the controller, and leaves its exit code and output behind.
func liveStartMoveScript(resource, domain, addr string) string {
	dest, data := liveMoveURIs(addr)
	return fmt.Sprintf(`dir=%[1]s
base=$dir/live-%[2]s
unit=%[3]s
mkdir -p "$dir"
systemctl stop "$unit" 2>/dev/null; systemctl reset-failed "$unit" 2>/dev/null
rm -f "$base.rc" "$base.log"
cat > "$base.sh" <<'HAIFY_EOF'
#!/bin/sh
virsh -c qemu:///system migrate --live %[4]s %[5]s --migrateuri %[6]s > %[1]s/live-%[2]s.log 2>&1
echo $? > %[1]s/live-%[2]s.rc.tmp && mv %[1]s/live-%[2]s.rc.tmp %[1]s/live-%[2]s.rc
HAIFY_EOF
systemd-run --unit="$unit" %[7]s /bin/sh "$base.sh" >/dev/null 2>&1 || { echo "HAIFY_ERR=systemd-run failed"; exit 1; }
echo HAIFY_STARTED=1
`, liveStateDir, resource, shellSingleQuote(liveMoveUnit(resource)), shellSingleQuote(domain),
		shellSingleQuote(dest), shellSingleQuote(data), collectFlag)
}

// livePollScript reports how the migration is doing.
func livePollScript(resource, domain string) string {
	return fmt.Sprintf(`base=%[1]s/live-%[2]s
echo "RC=$(cat "$base.rc" 2>/dev/null)"
echo "UNIT=$(systemctl is-active %[3]s 2>/dev/null)"
job=$(LANG=C virsh -c qemu:///system domjobinfo %[4]s 2>/dev/null)
echo "JOB_PROCESSED=$(printf '%%s\n' "$job" | sed -n 's/^Data processed:[[:space:]]*//p')"
echo "JOB_TOTAL=$(printf '%%s\n' "$job" | sed -n 's/^Data total:[[:space:]]*//p')"
if [ -s "$base.rc" ]; then echo "LOG=$(tail -c 1500 "$base.log" 2>/dev/null | base64 | tr -d '\n')"; fi
`, liveStateDir, resource, shellSingleQuote(liveMoveUnit(resource)), shellSingleQuote(domain))
}

// liveAbortScript asks libvirt to give up the migration; the guest stays on
// the source.
func liveAbortScript(domain string) string {
	return fmt.Sprintf("virsh -c qemu:///system domjobabort %s 2>&1; true\n", shellSingleQuote(domain))
}

// liveRetireScript clears drbd-reactor's units for the resource from a node
// the guest does not run on. It stops the services target only where that is
// a no-op for the guest and for DRBD: the agent's stop finds no guest, and
// the demote in drbd-promote@ finds the resource Secondary already. On a
// node that is still Primary a failing demote would trigger the promoter's
// on-drbd-demote-failure action, so there the target is left alone.
func liveRetireScript(resource string, g *vmGuest) string {
	return fmt.Sprintf(`d=%s
res=%s
target=%s
unit=%s
st=$(LANG=C virsh -c qemu:///system domstate "$d" 2>&1 | head -n1)
case "$st" in
  "shut off"|crashed|*"failed to get domain"*|*"omain not found"*) ;;
  *) echo "HAIFY_KEPT=the guest is $st here"; exit 0 ;;
esac
role=$(drbdsetup status "$res" 2>/dev/null | sed -n '1s/.*role:\([A-Za-z]*\).*/\1/p')
if [ "$role" = Primary ]; then echo "HAIFY_KEPT=$res is still Primary here"; exit 0; fi
systemctl stop "$target" 2>/dev/null
systemctl reset-failed "$target" "$unit" 2>/dev/null
f=/run/systemd/system/$unit.d/%s
if [ -e "$f" ]; then rm -f "$f"; systemctl daemon-reload; fi
echo HAIFY_RETIRED=1
`, shellSingleQuote(g.Domain), shellSingleQuote(resource), shellSingleQuote(reactorTargetUnit(resource)),
		shellSingleQuote(g.agentUnit(resource)), liveGuardFile)
}

// liveAdoptScript has drbd-reactor's units take the running guest on the
// node it runs on. Starting the target promotes (the node is Primary
// already) and starts the agent unit; the VirtualDomain agent's start finds
// the guest running and returns without touching it, and the wrapper goes on
// to monitor it. The agent unit is started by name as well: a target that is
// already active does not restart a member that failed.
func liveAdoptScript(resource string, g *vmGuest) string {
	return fmt.Sprintf(`target=%s
unit=%s
systemctl reset-failed "$unit" 2>/dev/null
systemctl start "$target" 2>&1
systemctl start "$unit" 2>&1
echo "TARGET_ACTIVE=$(systemctl is-active "$target" 2>/dev/null)"
echo "AGENT_ACTIVE=$(systemctl is-active "$unit" 2>/dev/null)"
`, shellSingleQuote(reactorTargetUnit(resource)), shellSingleQuote(g.agentUnit(resource)))
}
