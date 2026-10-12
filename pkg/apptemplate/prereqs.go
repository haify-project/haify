package apptemplate

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"go.uber.org/zap"
)

// Before anything is written, every diskful node must be able to run the
// app, and run it the same way. A promoter config is accepted whatever it
// names, so a node that lacks the engine or an OCF agent shows up only when
// the database fails over to it — and fails to start there.
//
// Three things are compared rather than merely checked:
//
//   - the daemon user's uid and gid. The data files belong to numeric ids;
//     on a node where "postgres" is uid 114 instead of 113, the files the
//     last Primary wrote belong to somebody else, and the database refuses to
//     start (or worse, starts as the wrong user).
//   - the binary paths, because one unit file is written to every node.
//   - the version, because a data directory written by PostgreSQL 16 does not
//     start under 15, and MySQL/MariaDB and Redis refuse newer on-disk formats.

// NodeProbe is what ProbeScript reports about one node.
type NodeProbe struct {
	Binaries
	Version string
	UID     int // -1 when the user is missing
	GID     int
	Missing []string
	// PortBusy is the listener already on the app's port, if any.
	PortBusy string
}

// ProbeScript prints key=value lines describing one node. It always exits 0:
// what is missing is reported, not signalled, so one call collects every
// problem. Run it as root.
func ProbeScript(s Spec) string {
	zap.L().Debug("ProbeScript: rendering prerequisite probe",
		zap.String("app", s.Name), zap.String("engine", string(s.Engine)), zap.Int("port", s.Port), zap.Bool("vector", s.Vector))
	var engine string
	switch s.Engine {
	case Postgres:
		engine = postgresProbe
		if s.Vector {
			engine += vectorProbe
		}
		engine += "fi\n"
	case MySQL:
		engine = mysqlProbe
	case Redis:
		engine = redisProbe
	case RustFS:
		engine = rustfsProbe
	}
	// Two passes: a Replacer does not rescan what it inserted, and the engine
	// part carries placeholders of its own.
	script := strings.Replace(probeFrame, "@ENGINE@", engine, 1)
	return strings.NewReplacer("@USER@", OSUser(s.Engine),
		"@PORT@", strconv.Itoa(s.Port), "@CONSOLE@", strconv.Itoa(ConsolePort(s))).
		Replace(script)
}

const probeFrame = `#!/bin/bash
cd /
missing=
have() { command -v "$1" >/dev/null 2>&1 || [ -x "/usr/sbin/$1" ] || [ -x "/sbin/$1" ]; }
pick() { for c in "$@"; do p=$(command -v "$c" 2>/dev/null) && { echo "$p"; return 0; }; done; return 1; }
for a in Filesystem IPaddr2; do
  [ -x /usr/lib/ocf/resource.d/heartbeat/$a ] || missing="$missing ocf:heartbeat:$a"
done
for t in drbd-reactorctl blkid mkfs.ext4 findmnt fsfreeze runuser setpriv systemd-run; do
  have $t || missing="$missing $t"
done
@ENGINE@
if id -u @USER@ >/dev/null 2>&1; then
  echo "uid=$(id -u @USER@)"
  echo "gid=$(id -g @USER@)"
else
  missing="$missing user:@USER@"
fi
if have ss; then
  busy=$(ss -Hltn "sport = :@PORT@" 2>/dev/null | head -n1)
  [ -z "$busy" ] || echo "portbusy=$busy"
fi
echo "missing=$missing"
exit 0
`

// postgresProbe picks the newest PostgreSQL that has both postgres and
// initdb: Debian/Ubuntu keep them in /usr/lib/postgresql/<major>/bin, PGDG
// builds for EL in /usr/pgsql-<major>/bin, EL's own packages in /usr/bin.
const postgresProbe = `bin=
for d in $(ls -d /usr/lib/postgresql/*/bin /usr/pgsql-*/bin 2>/dev/null | sort -V -r) /usr/bin; do
  if [ -x "$d/postgres" ] && [ -x "$d/initdb" ]; then bin=$d; break; fi
done
if [ -z "$bin" ]; then
  missing="$missing postgres"
else
  for t in pg_ctl psql pg_isready; do [ -x "$bin/$t" ] || missing="$missing $t"; done
  echo "server=$bin/postgres"
  echo "client=$bin/psql"
  echo "admin=$bin/pg_isready"
  echo "init=$bin/initdb"
  echo "version=$("$bin/postgres" --version 2>/dev/null | head -n1)"
`

// vectorProbe looks for pgvector where the chosen PostgreSQL loads
// extensions from.
const vectorProbe = `  case "$bin" in
    /usr/lib/postgresql/*) share=/usr/share/postgresql/$(basename "$(dirname "$bin")") ;;
    /usr/bin) share=/usr/share/pgsql ;;
    *) share=$(dirname "$bin")/share ;;
  esac
  [ -f "$share/extension/vector.control" ] || missing="$missing pgvector"
`

const mysqlProbe = `server=
for c in /usr/sbin/mariadbd /usr/sbin/mysqld /usr/libexec/mariadbd /usr/libexec/mysqld; do
  if [ -x "$c" ]; then server=$c; break; fi
done
if [ -z "$server" ]; then
  missing="$missing mysqld"
else
  v=$("$server" --version 2>/dev/null | head -n1)
  case "$v" in *MariaDB*) flavor=mariadb ;; *) flavor=mysql ;; esac
  echo "server=$server"
  echo "version=$v"
  echo "flavor=$flavor"
  if c=$(pick mariadb mysql); then echo "client=$c"; else missing="$missing mysql-client"; fi
  if c=$(pick mariadb-admin mysqladmin); then echo "admin=$c"; else missing="$missing mysqladmin"; fi
  if [ "$flavor" = mariadb ]; then
    if c=$(pick mariadb-install-db mysql_install_db); then echo "init=$c"; else missing="$missing mariadb-install-db"; fi
  fi
fi
`

// rustfsProbe finds the server, the curl its health probe uses, and whether
// the console port, the one after the S3 port, is free too.
const rustfsProbe = `if c=$(pick rustfs); then
  echo "server=$c"
  echo "version=$("$c" --version 2>/dev/null | head -n1)"
else
  missing="$missing rustfs"
fi
if c=$(pick curl); then echo "client=$c"; else missing="$missing curl"; fi
if have ss; then
  busy=$(ss -Hltn "sport = :@CONSOLE@" 2>/dev/null | head -n1)
  [ -z "$busy" ] || echo "portbusy=$busy (the console port)"
fi
`

const redisProbe = `if c=$(pick redis-server); then
  echo "server=$c"
  echo "version=$("$c" --version 2>/dev/null | head -n1)"
else
  missing="$missing redis-server"
fi
if c=$(pick redis-cli); then echo "client=$c"; else missing="$missing redis-cli"; fi
`

// ParseProbe reads ProbeScript's output. Lines it does not know are ignored:
// the transport may add notices to the output.
func ParseProbe(out string) NodeProbe {
	p := NodeProbe{UID: -1, GID: -1}
	for _, line := range strings.Split(out, "\n") {
		key, val, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch key {
		case "server":
			p.Server = val
		case "client":
			p.Client = val
		case "admin":
			p.Admin = val
		case "init":
			p.Init = val
		case "flavor":
			p.Flavor = val
		case "version":
			p.Version = val
		case "uid":
			if n, err := strconv.Atoi(val); err == nil {
				p.UID = n
			}
		case "gid":
			if n, err := strconv.Atoi(val); err == nil {
				p.GID = n
			}
		case "missing":
			p.Missing = strings.Fields(val)
		case "portbusy":
			p.PortBusy = val
		}
	}
	zap.L().Debug("ParseProbe: probe parsed",
		zap.String("server", p.Server), zap.String("version", p.Version), zap.String("flavor", p.Flavor),
		zap.Int("uid", p.UID), zap.Int("gid", p.GID), zap.Strings("missing", p.Missing), zap.String("port_busy", p.PortBusy))
	return p
}

// NodeResult is one node's probe, by the node's name.
type NodeResult struct {
	Node  string
	Probe NodeProbe
}

// Agreed is what every node has in common, recorded with the app.
type Agreed struct {
	Binaries Binaries
	Version  string
	UID, GID int
}

var versionRe = regexp.MustCompile(`(\d+)\.(\d+)`)

// compatKey is the part of a version string that must match across nodes for
// one node to run what another wrote: the major version for PostgreSQL,
// major.minor (and the flavor) for MySQL/MariaDB and Redis, whose on-disk
// formats change between minor releases.
func compatKey(e Engine, flavor, version string) string {
	m := versionRe.FindStringSubmatch(version)
	if m == nil {
		zap.L().Debug("compatKey: no x.y in version, comparing it whole", zap.String("version", version))
		return strings.TrimSpace(version)
	}
	switch e {
	case Postgres:
		return m[1]
	case MySQL:
		return flavor + " " + m[1] + "." + m[2]
	}
	return m[1] + "." + m[2]
}

// CheckNodes turns the probes of every diskful node into one verdict. All
// problems are reported together, each naming its nodes, so one round of
// fixes is enough.
func CheckNodes(s Spec, nodes []NodeResult) (Agreed, error) {
	log := zap.L()
	log.Debug("CheckNodes: comparing node probes",
		zap.String("app", s.Name), zap.String("engine", string(s.Engine)), zap.Int("count", len(nodes)))
	if len(nodes) == 0 {
		log.Debug("CheckNodes: no node to check", zap.String("app", s.Name))
		return Agreed{}, fmt.Errorf("no node to check")
	}
	user := OSUser(s.Engine)
	var problems []string

	var missing []string
	for _, n := range nodes {
		log.Debug("CheckNodes: node probe",
			zap.String("node", n.Node), zap.String("server", n.Probe.Server), zap.String("version", n.Probe.Version),
			zap.Int("uid", n.Probe.UID), zap.Int("gid", n.Probe.GID), zap.Strings("missing", n.Probe.Missing),
			zap.String("port_busy", n.Probe.PortBusy))
		if len(n.Probe.Missing) > 0 {
			missing = append(missing, fmt.Sprintf("%s lacks %s", n.Node, strings.Join(n.Probe.Missing, " ")))
		}
	}
	if len(missing) > 0 {
		log.Debug("CheckNodes: nodes lack prerequisites, skipping the comparisons", zap.Int("count", len(missing)))
		problems = append(problems, fmt.Sprintf("%s (install %s; the OCF agents come from "+
			"resource-agents-extra on Ubuntu, resource-agents on Debian and EL)", strings.Join(missing, "; "), Packages(s.Engine)))
	}
	for _, n := range nodes {
		if n.Probe.PortBusy != "" {
			problems = append(problems, fmt.Sprintf("port %d is already in use on %s (%s): stop and disable the "+
				"distribution's own %s service there, or choose another --port", s.Port, n.Node, n.Probe.PortBusy, s.Engine))
		}
	}
	if len(missing) == 0 {
		problems = append(problems, differs(nodes, "the engine is installed in different places",
			"one unit file runs it on every node; install the same packages everywhere", func(p NodeProbe) string {
				return strings.TrimSpace(fmt.Sprintf("%s %s %s %s %s", p.Server, p.Client, p.Admin, p.Init, p.Flavor))
			})...)
		problems = append(problems, differs(nodes, "the engine versions differ",
			"a failover would hand the data to a version that cannot read it; install the same version everywhere",
			func(p NodeProbe) string { return compatKey(s.Engine, p.Flavor, p.Version) })...)
		problems = append(problems, differs(nodes, fmt.Sprintf("user %s has different ids", user),
			"after a failover the data files would belong to another user; give it the same uid and gid on every "+
				"node (usermod -u / groupmod -g, then chown its files)", func(p NodeProbe) string {
				return fmt.Sprintf("uid=%d gid=%d", p.UID, p.GID)
			})...)
	}
	if len(problems) > 0 {
		log.Debug("CheckNodes: nodes cannot run the app", zap.String("app", s.Name), zap.Strings("problems", problems))
		return Agreed{}, fmt.Errorf("the nodes cannot run %s app %s: %s", s.Engine, s.Name, strings.Join(problems, "; "))
	}
	first := nodes[0].Probe
	log.Debug("CheckNodes: nodes agree",
		zap.String("app", s.Name), zap.String("server", first.Server), zap.String("version", first.Version),
		zap.Int("uid", first.UID), zap.Int("gid", first.GID))
	return Agreed{Binaries: first.Binaries, Version: first.Version, UID: first.UID, GID: first.GID}, nil
}

// differs reports when key is not the same on every node, grouping the nodes
// by the value they have.
func differs(nodes []NodeResult, what, why string, key func(NodeProbe) string) []string {
	byValue := map[string][]string{}
	for _, n := range nodes {
		k := key(n.Probe)
		byValue[k] = append(byValue[k], n.Node)
	}
	if len(byValue) < 2 {
		zap.L().Debug("differs: every node agrees", zap.String("what", what))
		return nil
	}
	zap.L().Debug("differs: nodes disagree", zap.String("what", what), zap.Int("count", len(byValue)))
	values := make([]string, 0, len(byValue))
	for v := range byValue {
		values = append(values, v)
	}
	sort.Strings(values)
	parts := make([]string, 0, len(values))
	for _, v := range values {
		parts = append(parts, fmt.Sprintf("%s: %s", strings.Join(byValue[v], ", "), v))
	}
	return []string{fmt.Sprintf("%s (%s) — %s", what, strings.Join(parts, "; "), why)}
}
