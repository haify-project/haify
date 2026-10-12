package apptemplate

import (
	"strconv"
	"strings"

	"go.uber.org/zap"
)

// Consistent snapshots.
//
// A resource snapshot (pkg/controller/resource_snapshot.go) suspends DRBD I/O
// on every replica and snapshots the backing volumes, which is crash
// consistent: what the snapshot holds is what a power cut would have left.
// Every engine here recovers from that. Freezing first makes the snapshot
// cleaner than a crash — nothing half-written, little or no recovery on
// restore — at the cost of stalling writes for the seconds it takes:
//
//	postgres  CHECKPOINT, then fsfreeze. A checkpoint flushes every dirty
//	          buffer, so a restore replays almost no WAL. (pg_backup_start /
//	          pg_backup_stop would need one session held open across the
//	          snapshot, and the WAL it guards is on the same volume anyway.)
//	mysql     FLUSH TABLES WITH READ LOCK, held by a session in a transient
//	          unit while fsfreeze runs; non-transactional tables are closed and
//	          no statement is half-applied.
//	redis     BGSAVE and wait for it, then fsfreeze: the snapshot carries a
//	          fresh RDB beside the AOF.
//
// fsfreeze flushes the filesystem and blocks new writes until thawed. A
// frozen database must never be left that way because the controller died
// between freeze and thaw, so FreezeForSnapshot first arms a transient
// systemd timer on the node that thaws it after FreezeWatchdog — the same
// pattern the resource snapshot uses for suspend-io. ThawAfterSnapshot thaws
// and disarms it.

// thawFile is the node-local copy of the thaw steps the watchdog runs. It is
// on /run (tmpfs), which a frozen filesystem cannot block.
func thawFile(name string) string { return RuntimeBase + name + "-thaw.sh" }

// thawSteps undo every freeze step, each tolerating that it was never done.
func thawSteps(s Spec, b Binaries) string {
	zap.L().Debug("thawSteps: rendering thaw steps", zap.String("app", s.Name), zap.String("engine", string(s.Engine)),
		zap.Bool("releases_mysql_lock", s.Engine == MySQL))
	steps := "fsfreeze -u @MOUNT@ 2>/dev/null\n"
	if s.Engine == MySQL {
		steps += `ids=$(@CLIENT@ --defaults-extra-file=@CLIENTCONF@ -N -e "SELECT ID FROM information_schema.PROCESSLIST WHERE TRIM(INFO) = '` + mysqlLockQuery + `'" 2>/dev/null)
for id in $ids; do
  @CLIENT@ --defaults-extra-file=@CLIENTCONF@ -e "KILL $id" >/dev/null 2>&1
done
systemctl stop @LOCKUNIT@.service 2>/dev/null
`
	}
	return scriptVars(s, b).Replace(steps + "true\n")
}

// mysqlLockQuery is the statement the lock session sleeps in. It is
// distinctive so the thaw can find that session, and only it, to kill: a
// client disconnecting does not end a server-side SLEEP, so stopping the unit
// alone would keep the lock until the sleep ran out.
const mysqlLockQuery = "SELECT SLEEP(@HOLD@) AS haify_app_freeze"

func freezeSteps(s Spec, b Binaries) string {
	// The engine decides how the app is quiesced before fsfreeze: a
	// checkpoint, a held read lock, or a fresh RDB. Others only fsfreeze.
	zap.L().Debug("freezeSteps: rendering freeze steps", zap.String("app", s.Name), zap.String("engine", string(s.Engine)))
	var steps string
	switch s.Engine {
	case Postgres:
		steps = `runuser -u @USER@ -- @CLIENT@ -h @RUN@ -p @PORT@ -d postgres -Atqc CHECKPOINT >/dev/null || fail "CHECKPOINT failed"
`
	case MySQL:
		steps = `q="SELECT ID FROM information_schema.PROCESSLIST WHERE TRIM(INFO) = '` + mysqlLockQuery + `'"
systemd-run --unit=@LOCKUNIT@ $(systemd-run --help 2>/dev/null | grep -q -e --collect && echo --collect) --quiet @CLIENT@ --defaults-extra-file=@CLIENTCONF@ \
  -e "FLUSH TABLES WITH READ LOCK; ` + mysqlLockQuery + `" || fail "could not start the read-lock session"
i=0
until [ -n "$(@CLIENT@ --defaults-extra-file=@CLIENTCONF@ -N -e "$q" 2>/dev/null)" ]; do
  systemctl is-active -q @LOCKUNIT@.service || fail "the read-lock session ended before it held the lock"
  i=$((i+1)); [ $i -lt 30 ] || fail "FLUSH TABLES WITH READ LOCK did not complete within 30s"
  sleep 1
done
`
	case Redis:
		steps = `export REDISCLI_AUTH="$(cat @PASSWORD@)"
cli() { @CLIENT@ -s @RUN@/redis.sock "$@"; }
before=$(cli LASTSAVE 2>/dev/null)
[ -n "$before" ] || fail "redis does not answer"
while [ "$(date +%s)" -le "$before" ]; do sleep 0.2; done
cli BGSAVE SCHEDULE >/dev/null 2>&1 || fail "BGSAVE failed"
i=0
until [ "$(cli LASTSAVE 2>/dev/null)" -gt "$before" ] 2>/dev/null; do
  i=$((i+1)); [ $i -lt 40 ] || fail "BGSAVE did not finish within 40s"
  sleep 1
done
cli INFO persistence 2>/dev/null | grep -q '^rdb_last_bgsave_status:ok' || fail "BGSAVE failed"
`
	}
	return scriptVars(s, b).Replace(steps) + "sync\nfsfreeze -f " + LayoutFor(s.Name).Mount + ` || fail "fsfreeze failed"` + "\n"
}

// FreezeForSnapshot renders the script run on the Primary before the
// resource snapshot. It arms the thaw watchdog BEFORE freezing — a freeze
// without a watchdog is refused — and thaws at once when any step fails, so
// it either exits 0 with the app frozen or non-zero with it running.
func FreezeForSnapshot(s Spec, b Binaries) string {
	l := LayoutFor(s.Name)
	unit := ThawUnit(s.Name)
	tf := thawFile(s.Name)
	zap.L().Debug("FreezeForSnapshot: rendering freeze script",
		zap.String("app", s.Name), zap.String("engine", string(s.Engine)), zap.String("mount", l.Mount),
		zap.String("thaw_unit", unit), zap.Duration("watchdog", FreezeWatchdog))
	return strings.NewReplacer(
		"@MOUNT@", l.Mount,
		"@THAWFILE@", tf,
		"@THAWUNIT@", unit,
		"@WATCHDOG@", strconv.Itoa(int(FreezeWatchdog.Seconds())),
		"@THAW@", thawSteps(s, b),
		"@FREEZE@", freezeSteps(s, b),
	).Replace(`#!/bin/bash
cd /
fail() {
  echo "$(hostname): $*" >&2
  /bin/bash @THAWFILE@ >/dev/null 2>&1
  systemctl stop @THAWUNIT@.timer 2>/dev/null
  rm -f @THAWFILE@
  exit 3
}
mountpoint -q @MOUNT@ || { echo "$(hostname): @MOUNT@ is not mounted; the app does not run here" >&2; exit 3; }
cat > @THAWFILE@ <<'HAIFYTHAW'
@THAW@HAIFYTHAW
chmod 0700 @THAWFILE@
systemctl stop @THAWUNIT@.timer @THAWUNIT@.service 2>/dev/null
systemctl reset-failed @THAWUNIT@.timer @THAWUNIT@.service 2>/dev/null
systemd-run --unit=@THAWUNIT@ $(systemd-run --help 2>/dev/null | grep -q -e --collect && echo --collect) --quiet --on-active=@WATCHDOG@ /bin/bash @THAWFILE@ \
  || { echo "$(hostname): could not arm the thaw watchdog; not freezing" >&2; rm -f @THAWFILE@; exit 3; }
@FREEZE@echo "frozen=yes"
`)
}

// ThawAfterSnapshot renders the script that thaws the app and disarms the
// watchdog. Safe to run when nothing is frozen.
func ThawAfterSnapshot(s Spec, b Binaries) string {
	tf := thawFile(s.Name)
	zap.L().Debug("ThawAfterSnapshot: rendering thaw script", zap.String("app", s.Name), zap.String("engine", string(s.Engine)))
	return strings.NewReplacer("@THAWFILE@", tf, "@THAWUNIT@", ThawUnit(s.Name), "@THAW@", thawSteps(s, b)).
		Replace(`#!/bin/bash
cd /
systemctl stop @THAWUNIT@.timer 2>/dev/null
@THAW@rm -f @THAWFILE@
true
`)
}

// StatusScript reports, on one node, whether the app's unit is active there
// and whether the database answers: "active=<state>" and "health=ok|fail".
func StatusScript(s Spec, b Binaries) string {
	zap.L().Debug("StatusScript: rendering status script", zap.String("app", s.Name), zap.String("engine", string(s.Engine)))
	return "#!/bin/bash\ncd /\necho \"active=$(systemctl is-active " + UnitName(s.Name) + " 2>/dev/null)\"\n" +
		"if " + HealthCommand(s, b) + "; then echo health=ok; else echo health=fail; fi\n"
}
