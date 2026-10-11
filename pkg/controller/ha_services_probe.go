package controller

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/haify-project/haify/pkg/deployment"
)

// Probing one node for GetHaStatus.
//
// One script per node, read-only, answers everything the HA status needs from
// that node: drbd-reactor's view of every promoter there, whether
// drbd-reactor runs, the local DRBD role and disk of every resource, what
// systemd says about every unit of every chain there, whether the OCF agents
// the chains run are installed, and which processes use a DRBD-backed mount.
//
// The process scan reads /proc/<pid>/{cwd,root,fd/*} links only, with one
// find(1). fuser -m and lsof are never used: fuser -m once resolved a mount to
// the root filesystem and its kill took down a whole node.
//
// The script is base64-wrapped (dispatch's sh -c quoting empties $vars) and
// every line it prints for the parser starts with haProbeMarker, so whatever
// else sudo or a login shell writes is ignored.

const haProbeMarker = "@@haify-ha"

// haProbeOCFParams are the OCF parameters the probe passes on: the ones that
// label a unit. Everything else in an agent's Environment (CHAP passwords
// among it) never leaves the node.
const haProbeOCFParams = "directory|ip|cidr_netmask|iqn|target_iqn|lun|path|nqn|namespace_id|backing_path|addr|type|portno|action|protocol|clientspec|config"

// haProbeScript is the per-node probe. It changes nothing on the node.
var haProbeScript = strings.NewReplacer("@@MARK@@", haProbeMarker, "@@PARAMS@@", haProbeOCFParams).Replace(`M=@@MARK@@
T=$(printf '\t')
NL='
'
j=$(drbd-reactorctl status --json 2>/dev/null)
printf '%s reactor %s\n' "$M" "$(printf '%s' "$j" | base64 -w0)"
printf '%s reactord %s\n' "$M" "$(systemctl is-active drbd-reactor 2>/dev/null)"
echo "$M drbd-begin"
drbdsetup status 2>/dev/null
echo "$M drbd-end"
units=$(printf '%s' "$j" | grep -o '"name":"[^"]*"' | sed 's/^"name":"//; s/"$//; s/\\\\/\\/g' | sort -u)
if [ -n "$units" ]; then
  show=$(TZ=UTC systemctl show -p Id,Names,Description,LoadState,UnitFileState,ActiveState,SubState,ActiveEnterTimestamp,NRestarts,MainPID,FragmentPath -- $units 2>/dev/null)
  echo "$M units-begin"
  printf '%s\n' "$show"
  echo "$M units-end"
  for f in $(printf '%s\n' "$show" | sed -n 's/^FragmentPath=//p' | sort -u); do
    [ -f "$f" ] || continue
    printf '%s frag %s%s%s\n' "$M" "$f" "$T" "$(sed -n 's/^[[:space:]]*Description[[:space:]]*=[[:space:]]*//p' "$f" | head -n1)"
  done
  ocf=$(printf '%s\n' $units | grep '^ocf\.rs@')
  if [ -n "$ocf" ]; then
    oenv=$(systemctl show -p Id,Environment -- $ocf 2>/dev/null)
    echo "$M ocf-begin"
    printf '%s\n' "$oenv" | while IFS= read -r l; do
      case $l in
      Environment=*) printf 'Environment=%s\n' "$(printf '%s\n' "${l#Environment=}" | tr ' ' '\n' |
        grep -E '^"?(AGENT|OCF_RESKEY_(@@PARAMS@@))=' | tr '\n' ' ')" ;;
      *) printf '%s\n' "$l" ;;
      esac
    done
    echo "$M ocf-end"
    for a in $(printf '%s\n' "$oenv" | sed -n 's/^Environment=//p' | tr ' ' '\n' | sed -n 's/^AGENT=//p' | sort -u); do
      if [ -x "$a" ]; then s=ok; else s=missing; fi
      printf '%s agent %s %s\n' "$M" "$s" "$a"
    done
  fi
fi
mnts=$(awk '$1 ~ /^\/dev\/drbd/ && $2 != "/" { gsub(/\\040/, " ", $2); print $2 }' /proc/mounts | sort -u)
if [ -n "$mnts" ]; then
  set --
  IFS=$NL
  for m in $mnts; do
    printf '%s mount %s\n' "$M" "$m"
    p=$(printf '%s' "$m" | sed 's/[][*?\\]/\\&/g')
    set -- "$@" -o -lname "$p" -o -lname "$p/*"
  done
  unset IFS
  shift
  hits=$(find /proc/[0-9]*/cwd /proc/[0-9]*/root /proc/[0-9]*/fd -maxdepth 1 \( "$@" \) -printf '%p\t%l\n' 2>/dev/null |
    MNTS=$mnts awk -F'\t' 'BEGIN { n = split(ENVIRON["MNTS"], ms, "\n") }
      { split($1, p, "/"); best = ""
        for (i = 1; i <= n; i++) { m = ms[i]; if (($2 == m || index($2, m "/") == 1) && length(m) > length(best)) best = m }
        if (best != "" && !seen[p[3] SUBSEP best]++) { if (!(p[3] in on)) order[++k] = p[3]; on[p[3]] = on[p[3]] "\t" best } }
      END { for (i = 1; i <= k; i++) print order[i] on[order[i]] }')
  procs=$(printf '%s\n' "$hits" | while IFS= read -r line; do
    pid=${line%%"$T"*}
    [ -n "$pid" ] || continue
    cg= v1=
    while IFS= read -r l; do
      case $l in
      0::*) cg=${l#0::} ;;
      *:name=systemd:*) v1=${l#*:name=systemd:} ;;
      esac
    done 2>/dev/null </proc/$pid/cgroup
    [ -n "$v1" ] && cg=$v1
    [ -n "$cg" ] && [ "$cg" != / ] && printf '%s%s\n' "$cg" "${line#"$pid"}"
  done | sort | uniq -c)
  if [ -n "$procs" ]; then
    printf '%s\n' "$procs" | while IFS= read -r l; do printf '%s procs %s\n' "$M" "$l"; done
    ou=$(printf '%s\n' "$procs" | awk -F'\t' '{ sub(/^ *[0-9]+ /, "", $1); n = split($1, c, "/"); for (i = 1; i <= n; i++) if (c[i] != "" && c[i] !~ /\.slice$/) { print c[i]; break } }' |
      grep -v '^session-.*\.scope$' | grep -vxF "${units:-@@none@@}" | sort -u)
    if [ -n "$ou" ]; then
      echo "$M outside-begin"
      systemctl show -p Id,Names,Description -- $ou 2>/dev/null
      echo "$M outside-end"
    fi
  fi
fi
true
`)

// haProbeCommand is haProbeScript wrapped for dispatch.
func haProbeCommand() string {
	return "echo " + base64Std(haProbeScript) + " | base64 -d | sudo /bin/sh"
}

// haDrbdLocal is a node's own view of one DRBD resource.
type haDrbdLocal struct {
	Role string
	// Disk is the local disk state, the worst over the volumes.
	Disk string
}

// haProcGroup is a set of processes in one cgroup that use the same mounts.
type haProcGroup struct {
	Count  int
	Cgroup string
	Mounts []string
}

// haNodeFacts is one node's probe, parsed.
type haNodeFacts struct {
	Reactor      *deployment.ReactorStatus
	ReactorState string // systemctl is-active drbd-reactor
	Drbd         map[string]haDrbdLocal
	Units        map[string]*haUnitFacts // by every name of the unit
	Agents       map[string]bool         // OCF agent path -> installed
	Mounts       []string                // DRBD-backed mount points
	Procs        []haProcGroup
	Outside      map[string]string // unit -> description, for units seen only in the scan
}

// unit returns the facts for a unit name, or nil.
func (f *haNodeFacts) unit(name string) *haUnitFacts {
	if f == nil {
		return nil
	}
	return f.Units[name]
}

// parseHaNodeProbe reads haProbeScript's output.
func parseHaNodeProbe(out string) (*haNodeFacts, error) {
	f := &haNodeFacts{
		Drbd:    map[string]haDrbdLocal{},
		Units:   map[string]*haUnitFacts{},
		Agents:  map[string]bool{},
		Outside: map[string]string{},
	}
	frags := map[string]string{}
	var section string
	var body []string
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		rest, ok := strings.CutPrefix(line, haProbeMarker+" ")
		if !ok {
			if section != "" {
				body = append(body, line)
			}
			continue
		}
		key, val, _ := strings.Cut(rest, " ")
		switch {
		case strings.HasSuffix(key, "-begin"):
			section, body = strings.TrimSuffix(key, "-begin"), nil
		case strings.HasSuffix(key, "-end"):
			f.applySection(section, body)
			section, body = "", nil
		case key == "reactor":
			if err := f.parseReactor(val); err != nil {
				return nil, err
			}
		case key == "reactord":
			f.ReactorState = strings.TrimSpace(val)
		case key == "frag":
			if p, d, ok := strings.Cut(val, "\t"); ok {
				frags[p] = strings.TrimSpace(d)
			}
		case key == "agent":
			state, agent, _ := strings.Cut(val, " ")
			f.Agents[agent] = state == "ok"
		case key == "mount":
			f.Mounts = append(f.Mounts, val)
		case key == "procs":
			if g, ok := parseProcGroup(val); ok {
				f.Procs = append(f.Procs, g)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read HA probe output: %w", err)
	}
	for _, u := range f.Units {
		if d, ok := frags[u.FragmentPath]; ok {
			u.OwnDesc = d
		}
	}
	return f, nil
}

func (f *haNodeFacts) parseReactor(b64 string) error {
	b64 = strings.TrimSpace(b64)
	if b64 == "" {
		return nil
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return fmt.Errorf("decode drbd-reactor status: %w", err)
	}
	if strings.TrimSpace(string(raw)) == "" {
		return nil
	}
	var rs deployment.ReactorStatus
	if err := json.Unmarshal(raw, &rs); err != nil {
		return fmt.Errorf("parse drbd-reactor status: %w", err)
	}
	f.Reactor = &rs
	return nil
}

func (f *haNodeFacts) applySection(section string, body []string) {
	switch section {
	case "drbd":
		f.Drbd = parseDrbdLocalStatus(body)
	case "units":
		for _, blk := range parseShowBlocks(body) {
			u := unitFactsFromShow(blk)
			for _, n := range u.Names {
				f.Units[n] = u
			}
		}
	case "ocf":
		for _, blk := range parseShowBlocks(body) {
			if u := f.Units[blk["Id"]]; u != nil {
				u.Env = parseOCFEnv(blk["Environment"])
			}
		}
	case "outside":
		for _, blk := range parseShowBlocks(body) {
			for _, n := range showNames(blk) {
				f.Outside[n] = blk["Description"]
			}
		}
	}
}

// parseShowBlocks splits systemctl show output for several units into one
// property map per unit; systemctl separates the units by a blank line.
func parseShowBlocks(lines []string) []map[string]string {
	var blocks []map[string]string
	cur := map[string]string{}
	flush := func() {
		if len(cur) > 0 {
			blocks = append(blocks, cur)
			cur = map[string]string{}
		}
	}
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			flush()
			continue
		}
		if k, v, ok := strings.Cut(l, "="); ok {
			cur[k] = v
		}
	}
	flush()
	return blocks
}

// showNames is every name a unit answers to: its Names= and its Id=.
func showNames(blk map[string]string) []string {
	names := systemdWords(blk["Names"])
	if id := blk["Id"]; id != "" {
		names = append(names, id)
	}
	return names
}

func unitFactsFromShow(blk map[string]string) *haUnitFacts {
	u := &haUnitFacts{
		ID:            blk["Id"],
		Names:         showNames(blk),
		Description:   blk["Description"],
		LoadState:     blk["LoadState"],
		UnitFileState: blk["UnitFileState"],
		ActiveState:   blk["ActiveState"],
		SubState:      blk["SubState"],
		ActiveEnter:   systemdTimestamp(blk["ActiveEnterTimestamp"]),
		FragmentPath:  blk["FragmentPath"],
	}
	u.Restarts, _ = strconv.Atoi(blk["NRestarts"])
	u.MainPID, _ = strconv.Atoi(blk["MainPID"])
	return u
}

// systemdTimestamp turns a timestamp systemctl show printed under TZ=UTC
// ("Thu 2026-10-08 08:06:41 UTC") into RFC 3339, or "" when there is none.
func systemdTimestamp(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || s == "n/a" {
		return ""
	}
	for _, layout := range []string{"Mon 2006-01-02 15:04:05 MST", "2006-01-02 15:04:05 MST"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
	}
	return ""
}

// parseProcGroup reads "<count> <cgroup>\t<mount>[\t<mount>...]", uniq -c's
// output of the scan.
func parseProcGroup(s string) (haProcGroup, bool) {
	s = strings.TrimLeft(s, " ")
	countStr, rest, ok := strings.Cut(s, " ")
	if !ok {
		return haProcGroup{}, false
	}
	n, err := strconv.Atoi(countStr)
	if err != nil {
		return haProcGroup{}, false
	}
	fields := strings.Split(rest, "\t")
	if len(fields) < 2 || fields[0] == "" {
		return haProcGroup{}, false
	}
	return haProcGroup{Count: n, Cgroup: fields[0], Mounts: fields[1:]}, true
}

// parseDrbdLocalStatus reads plain `drbdsetup status` for every resource's
// local role and disk. A resource starts at column 0 ("data role:Primary");
// its own disk lines are indented by two ("  disk:UpToDate" or
// "  volume:0 disk:UpToDate"); peers and their volumes are indented further.
func parseDrbdLocalStatus(lines []string) map[string]haDrbdLocal {
	out := map[string]haDrbdLocal{}
	cur := ""
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if l[0] != ' ' {
			fields := strings.Fields(l)
			cur = fields[0]
			st := haDrbdLocal{}
			for _, fld := range fields[1:] {
				if v, ok := strings.CutPrefix(fld, "role:"); ok {
					st.Role = v
				}
			}
			out[cur] = st
			continue
		}
		if cur == "" || strings.HasPrefix(l, "   ") {
			continue
		}
		for _, fld := range strings.Fields(l) {
			v, ok := strings.CutPrefix(fld, "disk:")
			if !ok {
				continue
			}
			st := out[cur]
			if st.Disk == "" || (st.Disk == "UpToDate" && v != "UpToDate") {
				st.Disk = v
			}
			out[cur] = st
		}
	}
	return out
}
