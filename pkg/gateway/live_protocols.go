package gateway

import (
	"fmt"
	"strings"

	"go.uber.org/zap"
)

// What a changed OCF parameter means for the target that is already running.
//
// An OCF unit reads its parameters only when it starts, and restarting one is
// not a way to apply them: the units of a gateway are chained with Requires=,
// so a restart of the iSCSITarget unit restarts every LUN and the service IP
// after it, and its stop deletes the whole LIO target with every session on
// it. The parameters an operator edits on a running gateway — the initiator
// allow-list, CHAP, the NVMe host allow-list — are instead set on the live
// target here, with the same commands the agents run at start
// (resource-agents heartbeat/iSCSITarget and heartbeat/nvmet-subsystem), so
// the live state ends up what a fresh start from the edited config produces.
//
// Anything else changed on a running unit is refused before any side effect:
// there is no way to apply it without restarting the gateway, and that is the
// operator's decision (haify gateway stop, then start), not an edit's.

// Where the agents find the running targets; tests point them elsewhere.
var (
	lioConfigfs   = "/sys/kernel/config/target/iscsi"
	nvmetConfigfs = "/sys/kernel/config/nvmet"
)

// shq quotes s for /bin/sh.
func shq(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// liveParamScript returns the script that applies a changed unit's parameters
// to the running target, or an error when the change cannot be applied live.
func liveParamScript(oldU, newU reactorUnit) (string, error) {
	zap.L().Debug("liveParamScript: start", zap.String("unit", newU.Name), zap.String("agent", newU.Agent))
	switch newU.Agent {
	case "heartbeat:iSCSITarget":
		return iscsiTargetLiveScript(oldU.Params, newU.Params)
	case "heartbeat:nvmet-subsystem":
		return nvmeSubsystemLiveScript(oldU.Params, newU.Params)
	}
	zap.L().Debug("liveParamScript: agent has no live edit", zap.String("unit", newU.Name), zap.String("agent", newU.Agent))
	return "", fmt.Errorf("%s cannot be changed on a running gateway; stop and start the gateway to apply it", newU.Name)
}

// fixedParamsChanged returns the keys whose values differ between old and
// new, ignoring the ones listed in editable.
func fixedParamsChanged(oldP, newP map[string]string, editable ...string) []string {
	skip := map[string]bool{}
	for _, k := range editable {
		skip[k] = true
	}
	var changed []string
	for k, v := range newP {
		if !skip[k] && oldP[k] != v {
			changed = append(changed, k)
		}
	}
	for k := range oldP {
		if _, ok := newP[k]; !ok && !skip[k] {
			changed = append(changed, k)
		}
	}
	return changed
}

// iscsiTargetLiveScript brings the running LIO target's ACLs and CHAP
// settings to what iSCSITarget's start would make of p.
//
// Order matters for the clients already connected: ACLs are created before
// the TPG leaves demo mode, so a listed initiator is never locked out in
// between, and ACLs are deleted last. Deleting an ACL ends its sessions —
// that is what removing an initiator is for. A new ACL is created with
// add_mapped_luns=true, which maps every existing LUN to it the way the LUN
// agents' targetcli calls would have at start.
func iscsiTargetLiveScript(oldP, p map[string]string) (string, error) {
	log := zap.L()
	if changed := fixedParamsChanged(oldP, p, "allowed_initiators", "incoming_username", "incoming_password"); len(changed) > 0 {
		log.Debug("iscsiTargetLiveScript: fixed parameters changed", zap.Strings("changed", changed))
		return "", fmt.Errorf("iSCSI target parameter(s) %s cannot be changed on a running gateway; stop and start the gateway to apply them", strings.Join(changed, ", "))
	}
	if impl := p["implementation"]; impl != "" && impl != "lio-t" {
		log.Debug("iscsiTargetLiveScript: implementation not editable live", zap.String("implementation", impl))
		return "", fmt.Errorf("iSCSI implementation %q cannot be edited live; only lio-t", impl)
	}
	user, pass := p["incoming_username"], p["incoming_password"]
	if user == "" && oldP["incoming_username"] != "" {
		log.Debug("iscsiTargetLiveScript: removing CHAP live refused")
		return "", fmt.Errorf("removing CHAP from a running gateway is not supported; stop and start the gateway to apply it")
	}
	// LIO stores node names lower-cased (rtslib normalize_wwn); compare that way.
	var want []string
	for _, i := range parseAllowedList(p["allowed_initiators"]) {
		want = append(want, strings.ToLower(i))
	}
	iqn := p["iqn"]
	// The script carries the CHAP password; only whether there is one is traced.
	log.Debug("iscsiTargetLiveScript: rendering",
		zap.String("iqn", iqn), zap.Strings("allowed", want), zap.String("username", user),
		zap.Bool("has_password", pass != ""), zap.Bool("acl_mode", len(want) > 0))
	var b strings.Builder
	fmt.Fprintf(&b, `t=%s
c=%s
want=%s
user=%s
pass=%s
[ -d "$c" ] || { echo "$(hostname): iSCSI target %s is not running here" >&2; exit 3; }
`, shq("/iscsi/"+iqn+"/tpg1"), shq(lioConfigfs+"/"+iqn+"/tpgt_1"),
		shq(strings.Join(want, " ")), shq(user), shq(pass), iqn)
	b.WriteString(`if [ -n "$want" ]; then
  for i in $want; do
    [ -d "$c/acls/$i" ] || targetcli "$t/acls" create "$i" add_mapped_luns=true || exit 1
    if [ -n "$user" ]; then
      targetcli "$t/acls/$i" set auth "userid=$user" || exit 1
      targetcli "$t/acls/$i" set auth "password=$pass" || exit 1
    fi
  done
  if [ -n "$user" ]; then targetcli "$t" set attribute authentication=1 || exit 1; fi
  targetcli "$t" set attribute generate_node_acls=0 || exit 1
  for d in "$c"/acls/*; do
    [ -d "$d" ] || continue
    i=${d##*/}
    case " $want " in *" $i "*) ;; *) targetcli "$t/acls" delete "$i" || exit 1 ;; esac
  done
else
  if [ -n "$user" ]; then
    targetcli "$t" set attribute authentication=1 demo_mode_write_protect=0 generate_node_acls=1 cache_dynamic_acls=1 || exit 1
    targetcli "$t" set auth "userid=$user" || exit 1
    targetcli "$t" set auth "password=$pass" || exit 1
  else
    targetcli "$t" set attribute authentication=0 demo_mode_write_protect=0 generate_node_acls=1 cache_dynamic_acls=1 || exit 1
  fi
  for d in "$c"/acls/*; do
    [ -d "$d" ] || continue
    targetcli "$t/acls" delete "${d##*/}" || exit 1
  done
fi
`)
	return b.String(), nil
}

// nvmeSubsystemLiveScript brings the running nvmet subsystem's host
// allow-list to what nvmet-subsystem's start would make of p. Links are added
// before allow_any_host is cleared and removed after, so a listed host is
// never refused in between. nvmet checks the list when a host connects: a
// removed host keeps a connection it already has until it disconnects.
func nvmeSubsystemLiveScript(oldP, p map[string]string) (string, error) {
	if changed := fixedParamsChanged(oldP, p, "allowed_initiators"); len(changed) > 0 {
		zap.L().Debug("nvmeSubsystemLiveScript: fixed parameters changed", zap.Strings("changed", changed))
		return "", fmt.Errorf("NVMe subsystem parameter(s) %s cannot be changed on a running gateway; stop and start the gateway to apply them", strings.Join(changed, ", "))
	}
	nqn := p["nqn"]
	zap.L().Debug("nvmeSubsystemLiveScript: rendering",
		zap.String("nqn", nqn), zap.Strings("allowed", parseAllowedList(p["allowed_initiators"])))
	var b strings.Builder
	fmt.Fprintf(&b, `s=%s
h=%s
want=%s
[ -d "$s" ] || { echo "$(hostname): NVMe subsystem %s is not running here" >&2; exit 3; }
`, shq(nvmetConfigfs+"/subsystems/"+nqn), shq(nvmetConfigfs+"/hosts"), shq(strings.Join(parseAllowedList(p["allowed_initiators"]), " ")), nqn)
	b.WriteString(`if [ -n "$want" ]; then
  for n in $want; do
    mkdir -p "$h/$n" || exit 1
    [ -L "$s/allowed_hosts/$n" ] || ln -s "$h/$n" "$s/allowed_hosts/$n" || exit 1
  done
  echo 0 > "$s/attr_allow_any_host" || exit 1
  for l in "$s"/allowed_hosts/*; do
    [ -L "$l" ] || continue
    case " $want " in *" ${l##*/} "*) ;; *) rm -f "$l" || exit 1 ;; esac
  done
else
  echo 1 > "$s/attr_allow_any_host" || exit 1
  for l in "$s"/allowed_hosts/*; do
    [ -L "$l" ] || continue
    rm -f "$l" || exit 1
  done
fi
`)
	return b.String(), nil
}
