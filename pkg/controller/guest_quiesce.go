package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/event"
)

// Freezing a libvirt guest's filesystems around a snapshot.
//
// A snapshot of a volume a guest is writing to holds what a power cut would
// have left: crash consistent. With the guest's filesystems frozen through
// its guest agent (`virsh domfsfreeze`) for the moment the snapshot is taken,
// it holds flushed filesystems with nothing half-written: filesystem
// consistent. Every snapshot of a resource carrying the libvirt guest label
// is taken that way when the guest runs and its agent answers, and as a crash
// consistent snapshot otherwise. Which of the two it was is reported with the
// snapshot and recorded on it (snapshot_consistency.go).
//
// A frozen guest blocks on every write, so it must thaw whatever happens to
// the controller. Three things see to that:
//
//   - Before anything is frozen, the node arms a transient systemd timer that
//     thaws the guest guestFreezeWatchdog later. The freeze is refused when
//     the timer cannot be armed. A controller that is killed, loses the node
//     or just takes too long is not needed for the thaw: the node does it,
//     and keeps trying for minutes if the agent is slow to answer. That is
//     also the upper bound on how long a guest stays frozen.
//   - The controller thaws as soon as the snapshots exist, on a context of
//     its own, so a cancelled request thaws at once rather than at the timer.
//   - A freeze command whose outcome is unknown (the node did not answer in
//     time) is treated as a freeze: the thaw runs anyway.
//
// The thaw tells whether the freeze held to the end: when the watchdog had
// already fired, or the guest turned out not to be frozen any more, the
// snapshot is reported as crash consistent, because that is all it is known
// to be.
//
// A freeze is the guest's, not the resource's: it stops the filesystems on
// all of the guest's disks, on whichever resources they are. Snapshots of two
// resources of one guest would otherwise thaw the guest under each other, so
// they take turns (guestQuiesceLocks). The turn covers DRBD's suspend-io as
// well: freezing flushes every filesystem of the guest, which would hang on a
// resource another snapshot holds suspended.

const (
	// guestFreezeWatchdog is the longest a guest stays frozen. It is longer
	// than resourceSnapshotWatchdog so that, when both run out, I/O resumes
	// before the guest thaws into it.
	guestFreezeWatchdog = 90 * time.Second
	// guestFreezeCommand bounds `virsh domfsfreeze`: libvirt waits without
	// limit for an agent that is connected but does not answer.
	guestFreezeCommand = 20 * time.Second
	// The dispatch timeouts of the two scripts, above the sum of what each
	// may wait for on the node.
	guestFreezeScriptTimeout = 60 * time.Second
	guestThawScriptTimeout   = 60 * time.Second
)

// guestQuiesceLocks is one lock per guest: one snapshot at a time freezes and
// thaws it.
var guestQuiesceLocks keyedLocks

// guestRuntime names what a guest's freeze keeps on its node. Guest names
// may hold characters a systemd unit name may not, so they are hashed.
func guestRuntime(domain string) (unit, thawFile, expiredFile string) {
	sum := sha256.Sum256([]byte(domain))
	id := hex.EncodeToString(sum[:8])
	return "haify-guest-thaw-" + id, "/run/haify-guest-thaw-" + id + ".sh", "/run/haify-guest-thaw-" + id + ".expired"
}

// guestThawSteps is the node's copy of the thaw. The watchdog runs it with
// "expired": it then leaves a marker, so the controller's own thaw can tell
// the freeze ended early, and keeps trying for five minutes. It is on /run, a
// tmpfs no frozen guest can block.
const guestThawSteps = `#!/bin/bash
export LC_ALL=C
dom='@DOM@'
tries=2
if [ "$1" = expired ]; then : > @EXPIRED@; tries=60; fi
i=0
while :; do
  # A guest that was shut off has no frozen filesystems left. A virsh that
  # does not answer says nothing about the guest, so that is retried.
  if state=$(virsh domstate "$dom" 2>/dev/null); then
    case "$(echo $state)" in "shut off"|crashed) echo "Thawed 0 filesystem(s): the guest is $(echo $state)"; exit 0 ;; esac
  fi
  out=$(timeout 15 virsh domfsthaw "$dom" 2>&1) && { echo "$out"; exit 0; }
  i=$((i+1))
  [ $i -lt $tries ] || { echo "$out"; exit 1; }
  sleep 5
done
`

// guestFreezeScript freezes the guest, or says why not. It always exits 0:
// "frozen=yes" means frozen with the watchdog armed, "guest=<why>" means
// running on as before with nothing armed. The one exception is a freeze
// command that timed out: the agent may still be freezing, so the watchdog is
// left armed when the thaw that follows fails too.
const guestFreezeScript = `#!/bin/bash
cd /
export LC_ALL=C
dom='@DOM@'
say() { echo "guest=$1"; [ -z "$2" ] || echo "detail=$(echo "$2" | tr '\n' ' ')"; exit 0; }
command -v virsh >/dev/null 2>&1 || say no-virsh
state=$(virsh domstate "$dom" 2>&1) || say undefined "$state"
state=$(echo $state)
[ "$state" = running ] || say not-running "$state"
cat > @THAWFILE@ <<'HAIFYGUESTTHAW'
@THAW@HAIFYGUESTTHAW
chmod 0700 @THAWFILE@
systemctl stop @UNIT@.timer @UNIT@.service 2>/dev/null
systemctl reset-failed @UNIT@.timer @UNIT@.service 2>/dev/null
rm -f @EXPIRED@
systemd-run --unit=@UNIT@ @COLLECT@ --quiet --timer-property=AccuracySec=1s --on-active=@WATCHDOG@ /bin/bash @THAWFILE@ expired \
  || { rm -f @THAWFILE@; say no-watchdog; }
out=$(timeout @FREEZETIMEOUT@ virsh domfsfreeze "$dom" 2>&1)
rc=$?
if [ $rc -eq 0 ]; then
  echo "frozen=yes"
  exit 0
fi
if /bin/bash @THAWFILE@ >/dev/null 2>&1 || [ $rc -ne 124 ]; then
  systemctl stop @UNIT@.timer 2>/dev/null
  rm -f @THAWFILE@ @EXPIRED@
fi
[ $rc -eq 124 ] && say freeze-timeout
say no-agent "$out"
`

// guestThawScript thaws the guest and disarms the watchdog, and says whether
// the freeze held until now. Safe to run when nothing is frozen. When the
// thaw fails the watchdog stays armed: the node goes on trying.
const guestThawScript = `#!/bin/bash
cd /
export LC_ALL=C
[ -e @THAWFILE@ ] || { echo "thawed=nothing"; exit 0; }
if [ -e @EXPIRED@ ]; then
  rm -f @EXPIRED@ @THAWFILE@
  echo "thawed=expired"
  exit 0
fi
if out=$(/bin/bash @THAWFILE@ 2>&1); then
  systemctl stop @UNIT@.timer 2>/dev/null
  rm -f @THAWFILE@
  if [ -e @EXPIRED@ ]; then
    rm -f @EXPIRED@
    echo "thawed=expired"
  else
    case "$out" in *"Thawed 0 "*) echo "thawed=not-frozen" ;; *) echo "thawed=yes" ;; esac
  fi
else
  echo "thawed=failed"
fi
echo "detail=$(echo "$out" | tr '\n' ' ')"
`

func renderGuestScript(script, domain string) string {
	unit, thawFile, expired := guestRuntime(domain)
	vars := strings.NewReplacer(
		"@DOM@", domain,
		"@UNIT@", unit,
		"@THAWFILE@", thawFile,
		"@EXPIRED@", expired,
		"@COLLECT@", collectFlag,
		"@WATCHDOG@", strconv.Itoa(int(guestFreezeWatchdog.Seconds())),
		"@FREEZETIMEOUT@", strconv.Itoa(int(guestFreezeCommand.Seconds())),
	)
	// The thaw steps are a heredoc inside the freeze script, with variables
	// of their own to fill in.
	return vars.Replace(strings.ReplaceAll(script, "@THAW@", guestThawSteps))
}

// quiescedGuest is one guest a snapshot tried to freeze.
type quiescedGuest struct {
	domain string
	host   string // where it was frozen, or may have been
	// thawNeeded: it was frozen, or the freeze did not report back.
	thawNeeded bool
	frozen     bool
	// why is the reason a snapshot of it is crash consistent.
	why    string
	unlock func()
}

// guestQuiesce is the freeze around one snapshot operation: the guests of the
// resources it covers, and the snapshots taken while they were frozen.
type guestQuiesce struct {
	rm         *ResourceManager
	guests     []*quiescedGuest
	byResource map[string]*quiescedGuest
	taken      []takenSnapshot
	once       sync.Once
}

// quiesceGuests freezes the guest of each of resources that has one, and
// returns what thaws them. It never fails: a guest that cannot be frozen is
// snapshotted as it is, and the result says so. The caller must call finish,
// on every path, once its snapshots exist and I/O it suspended has resumed.
//
// Guests are frozen in the order of their names, so two operations covering
// the same guests cannot wait for each other.
func (rm *ResourceManager) quiesceGuests(ctx context.Context, resources ...string) *guestQuiesce {
	log := rm.resourcesLog()
	q := &guestQuiesce{rm: rm, byResource: map[string]*quiescedGuest{}}
	first := map[string]string{}    // guest -> the first of its resources named
	domainOf := map[string]string{} // resource -> its guest
	var domains []string
	for _, r := range resources {
		d := rm.guestDomain(ctx, r)
		if d == "" {
			continue
		}
		domainOf[r] = d
		if _, seen := first[d]; !seen {
			first[d] = r
			domains = append(domains, d)
		}
	}
	if len(domains) == 0 {
		log.Debug("quiesceGuests: no guests to freeze", zap.Strings("resources", resources))
		return q
	}
	sort.Strings(domains)
	log.Debug("quiesceGuests: freezing guests", zap.Strings("resources", resources), zap.Strings("domains", domains))
	frozen := map[string]*quiescedGuest{}
	for _, d := range domains {
		frozen[d] = rm.freezeGuest(ctx, d, first[d])
		q.guests = append(q.guests, frozen[d])
	}
	for r, d := range domainOf {
		q.byResource[r] = frozen[d]
	}
	return q
}

// freezeGuest takes domain's turn and freezes it on the node where resource
// is Primary. The turn is kept only while a thaw is owed.
func (rm *ResourceManager) freezeGuest(ctx context.Context, domain, resource string) *quiescedGuest {
	log := rm.resourcesLog()
	g := &quiescedGuest{domain: domain}
	start := time.Now()
	unlock, err := guestQuiesceLocks.lock(ctx, domain)
	log.Debug("freezeGuest: took the guest's turn", zap.String("domain", domain), zap.String("resource", resource),
		zap.Duration("waited", time.Since(start)), zap.Error(err))
	if err != nil {
		g.why = fmt.Sprintf("the request ended while waiting for another snapshot of guest %s", domain)
		return g
	}
	hosts := rm.guestHosts(ctx, resource)
	if len(hosts) != 1 {
		// Nowhere: the guest is off. Two: a live migration is under way, and
		// there is no telling which side the agent answers on.
		unlock()
		g.why = fmt.Sprintf("guest %s is not running", domain)
		if len(hosts) > 1 {
			g.why = fmt.Sprintf("guest %s is being migrated", domain)
		}
		log.Debug("freezeGuest: not freezing", zap.String("domain", domain), zap.Strings("primary_hosts", hosts), zap.String("why", g.why))
		return g
	}
	g.host = hosts[0]
	node := rm.nodeLabel(g.host)
	kv, err := rm.runGuestScript(ctx, g.host, renderGuestScript(guestFreezeScript, domain), guestFreezeScriptTimeout)
	switch {
	case kv["frozen"] == "yes":
		g.frozen, g.thawNeeded = true, true
		g.unlock = unlock
		rm.controller.logger.Info("Guest frozen for a snapshot", zap.String("domain", domain), zap.String("node", node),
			zap.String("resource", resource), zap.Duration("watchdog", guestFreezeWatchdog))
		return g
	case kv["guest"] == "not-running", kv["guest"] == "undefined":
		g.why = fmt.Sprintf("guest %s is not running on %s", domain, node)
	case kv["guest"] == "no-virsh":
		g.why = fmt.Sprintf("%s has no virsh to freeze guest %s with", node, domain)
	case kv["guest"] == "no-watchdog":
		g.why = fmt.Sprintf("%s could not arm the timer that thaws guest %s, so it was not frozen", node, domain)
	case kv["guest"] == "no-agent":
		g.why = fmt.Sprintf("the guest agent of %s did not freeze its filesystems (%s)", domain, kv["detail"])
	default:
		// A freeze that timed out, or a script that did not report back: the
		// guest may be frozen, so the thaw is owed and the turn is kept.
		g.thawNeeded = true
		g.unlock = unlock
		g.why = fmt.Sprintf("the guest agent of %s did not freeze its filesystems in time", domain)
		log.Debug("freezeGuest: outcome unknown; a thaw is owed", zap.String("domain", domain), zap.String("host", g.host),
			zap.String("outcome", kv["guest"]), zap.Error(err))
		return g
	}
	unlock()
	log.Debug("freezeGuest: not frozen", zap.String("domain", domain), zap.String("host", g.host), zap.String("why", g.why))
	return g
}

// finish thaws every guest that is, or may be, frozen, gives their turns
// back, and records on the snapshots taken which consistency they have. It is
// safe to call more than once and on a nil receiver.
//
// It runs on a context of its own: the request's may be what ended, and the
// guest must not wait for the watchdog because of that.
func (q *guestQuiesce) finish() {
	if q == nil {
		return
	}
	q.once.Do(func() {
		for i := len(q.guests) - 1; i >= 0; i-- {
			q.thaw(q.guests[i])
		}
		q.recordConsistency()
	})
}

func (q *guestQuiesce) thaw(g *quiescedGuest) {
	if !g.thawNeeded {
		return
	}
	defer g.unlock()
	rm := q.rm
	log := rm.resourcesLog()
	ctx, cancel := context.WithTimeout(context.Background(), guestThawScriptTimeout+15*time.Second)
	defer cancel()
	node := rm.nodeLabel(g.host)
	kv, err := rm.runGuestScript(ctx, g.host, renderGuestScript(guestThawScript, g.domain), guestThawScriptTimeout)
	outcome := kv["thawed"]
	log.Debug("guestQuiesce.thaw: thawed", zap.String("domain", g.domain), zap.String("host", g.host),
		zap.String("outcome", outcome), zap.Bool("was_frozen", g.frozen), zap.Error(err))
	switch outcome {
	case "yes":
		if g.frozen {
			rm.controller.logger.Info("Guest thawed after a snapshot", zap.String("domain", g.domain), zap.String("node", node))
		}
		return
	case "nothing":
		// Nothing was armed, so nothing was frozen.
	case "expired":
		if g.frozen {
			g.why = fmt.Sprintf("the snapshot took longer than the %s guest %s may stay frozen, and it thawed first",
				guestFreezeWatchdog, g.domain)
		}
	case "not-frozen":
		if g.frozen {
			g.why = fmt.Sprintf("guest %s was no longer frozen when the snapshot finished", g.domain)
		}
	default:
		// The thaw failed or did not report back. The node's watchdog is
		// still armed and thaws the guest; someone should know meanwhile.
		why := kv["detail"]
		if why == "" && err != nil {
			why = err.Error()
		}
		msg := fmt.Sprintf("guest %s on %s did not thaw after a snapshot (%s); the node thaws it within %s of the freeze and keeps trying",
			g.domain, node, why, guestFreezeWatchdog)
		rm.controller.logger.Warn("Thawing a guest after its snapshot failed; the node's watchdog thaws it",
			zap.String("domain", g.domain), zap.String("node", node), zap.Duration("within", guestFreezeWatchdog),
			zap.String("reason", why))
		for r, rg := range q.byResource {
			if rg == g {
				rm.publishGuestEvent(event.TypeGuestThawFailed, event.SeverityCritical, r, g.host, g.domain, msg)
				break
			}
		}
		if g.frozen {
			g.why = fmt.Sprintf("guest %s did not thaw when asked, so the freeze cannot be vouched for", g.domain)
		}
	}
	g.frozen = false
}
