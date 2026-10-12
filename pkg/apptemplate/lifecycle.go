package apptemplate

import (
	"regexp"
	"strings"

	"go.uber.org/zap"
)

// EvictScript moves the app off the node it runs on, through drbd-reactor:
// the node stops the chain (service IP, database, mount), demotes, and
// another replica's promoter takes over. drbd-reactorctl exits 0 even when no
// other node took over — it then re-enables the resource where it was and
// says so only in its output — so the output is checked.
func EvictScript(name string) string {
	zap.L().Debug("EvictScript: rendering evict script", zap.String("app", name), zap.String("promoter", PromoterName(name)))
	return strings.NewReplacer("@CFG@", PromoterPath(name), "@PROM@", PromoterName(name)).Replace(`#!/bin/bash
[ -f @CFG@ ] || { echo "$(hostname): no promoter config @CFG@ here" >&2; exit 3; }
out=$(drbd-reactorctl evict @PROM@ 2>&1); rc=$?
printf '%s\n' "$out"
[ $rc -eq 0 ] || exit $rc
printf '%s\n' "$out" | grep -qE "Node '[^']+' took over" && exit 0
echo "$(hostname): no other node took over @PROM@; it is still running here" >&2
exit 4
`)
}

var tookOverRe = regexp.MustCompile(`Node '([^']+)' took over`)

// TookOver names the node drbd-reactorctl evict reported taking over.
func TookOver(out string) string {
	if m := tookOverRe.FindStringSubmatch(out); m != nil {
		zap.L().Debug("TookOver: evict named the new node", zap.String("node", m[1]))
		return m[1]
	}
	zap.L().Debug("TookOver: evict output names no node", zap.Int("output_len", len(out)))
	return ""
}

// RemoveScript takes the app off one node for good. drbd-reactor re-promotes
// within seconds as long as the promoter config is in place, so the config is
// disabled first (as `drbd-reactorctl disable` does) and only then is the
// chain stopped — stopping the drbd-services target stops the service IP, the
// database and the mount, and demotes. What is left of the chain is then
// stopped one by one, the files removed, and a node still Primary is demoted.
// The volume's contents are not touched. It fails only when the volume stays
// mounted, which would keep the resource from being used anywhere else.
func RemoveScript(s Spec) string {
	l := LayoutFor(s.Name)
	target := "drbd-services@" + strings.ReplaceAll(s.Resource, "-", `\x2d`) + ".target"
	zap.L().Debug("RemoveScript: rendering remove script",
		zap.String("app", s.Name), zap.String("resource", s.Resource), zap.String("target", target), zap.String("mount", l.Mount))
	return strings.NewReplacer(
		"@CFG@", PromoterPath(s.Name),
		"@UNITFILE@", UnitPath(s.Name),
		"@UNIT@", UnitName(s.Name),
		"@TARGET@", target,
		"@THAWUNIT@", ThawUnit(s.Name),
		"@LOCKUNIT@", LockUnit(s.Name),
		"@THAWFILE@", thawFile(s.Name),
		"@MOUNT@", l.Mount,
		"@RES@", s.Resource,
	).Replace(`#!/bin/bash
cd /
f=@CFG@
had=
for x in "$f" "$f.disabled"; do [ -e "$x" ] && had=1; done
if [ -f "$f" ]; then
  mv -f "$f" "$f.disabled"
  if systemctl cat drbd-reactor.service >/dev/null 2>&1; then
    systemctl reload drbd-reactor || systemctl restart drbd-reactor
  fi
fi
if [ -n "$had" ]; then
  systemctl stop '@TARGET@' 2>/dev/null
fi
systemctl stop @UNIT@ @LOCKUNIT@.service @THAWUNIT@.timer 2>/dev/null
[ -f @THAWFILE@ ] && /bin/bash @THAWFILE@ >/dev/null 2>&1
if mountpoint -q @MOUNT@; then
  umount @MOUNT@ || { echo "$(hostname): @MOUNT@ is still mounted and in use" >&2; exit 3; }
fi
rmdir @MOUNT@ 2>/dev/null
rm -f "$f" "$f.disabled" @THAWFILE@
if [ -e @UNITFILE@ ]; then
  rm -f @UNITFILE@
  systemctl daemon-reload
fi
if [ -n "$had" ] && drbdsetup status @RES@ 2>/dev/null | head -n1 | grep -q 'role:Primary'; then
  drbdadm secondary @RES@ || echo "$(hostname): @RES@ is still Primary here" >&2
fi
echo "removed=$had"
true
`)
}

// PresenceScript reports whether a node holds the app's promoter config
// (live or disabled) and its unit: "promoter=yes|no unit=yes|no".
func PresenceScript(name string) string {
	zap.L().Debug("PresenceScript: rendering presence probe", zap.String("app", name))
	return strings.NewReplacer("@CFG@", PromoterPath(name), "@UNITFILE@", UnitPath(name)).Replace(`p=no; u=no
{ [ -f @CFG@ ] || [ -f @CFG@.disabled ]; } && p=yes
[ -f @UNITFILE@ ] && u=yes
echo "promoter=$p"
echo "unit=$u"
`)
}
