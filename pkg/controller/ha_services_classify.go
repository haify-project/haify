package controller

import (
	"fmt"
	"path"
	"strconv"
	"strings"

	"go.uber.org/zap"
)

// Naming the units of an HA chain.
//
// drbd-reactor reports a promoter's chain as bare systemd unit names, and
// puts "drbd-reactor controlled <name>" over every unit's own Description.
// What a person needs to see is what each unit does — the mount, the service
// IP, the NFS export, the database — so each unit gets a kind and a label,
// worked out from its name, its unit file's own description and, for the
// ocf.rs@ units drbd-reactor runs OCF agents through, the agent and its
// parameters from the unit's Environment.

// Kinds of chain unit.
const (
	haKindPromote   = "promote"
	haKindMount     = "mount"
	haKindServiceIP = "service_ip"
	haKindNFS       = "nfs"
	haKindISCSI     = "iscsi"
	haKindNVMeoF    = "nvmeof"
	haKindSMB       = "smb"
	haKindDatabase  = "database"
	haKindVM        = "vm"
	haKindHaify     = "haify"
	haKindTarget    = "target"
	haKindService   = "service"
)

// reactorDescPrefix is what drbd-reactor's drop-in sets as every chain
// unit's Description (src/plugin/promoter.rs: "drbd-reactor controlled %N").
const reactorDescPrefix = "drbd-reactor controlled "

// haUnitFacts is what systemd says about one unit on one node.
type haUnitFacts struct {
	ID            string
	Names         []string
	Description   string // the effective Description=
	OwnDesc       string // the first Description= of the unit's own file
	LoadState     string
	UnitFileState string
	ActiveState   string
	SubState      string
	ActiveEnter   string // RFC 3339, empty when systemd has none
	Restarts      int
	MainPID       int
	FragmentPath  string
	// Env holds an ocf.rs@ unit's AGENT and the OCF_RESKEY_ parameters the
	// probe passes through, keyed without the OCF_RESKEY_ prefix; nil for
	// every other unit.
	Env map[string]string
}

// systemdUnescape undoes systemd's unit-name escaping: "-" stands for "/" and
// \xNN for a byte.
func systemdUnescape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '-':
			b.WriteByte('/')
		case c == '\\' && i+3 < len(s) && s[i+1] == 'x':
			if v, err := strconv.ParseUint(s[i+2:i+4], 16, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// splitUnitName splits "drbd-promote@openclaw.service" into its prefix
// ("drbd-promote"), instance ("openclaw", still escaped; empty for a unit that
// is no template instance) and suffix ("service").
func splitUnitName(unit string) (prefix, instance, suffix string) {
	base := unit
	if i := strings.LastIndexByte(unit, '.'); i > 0 {
		base, suffix = unit[:i], unit[i+1:]
	}
	prefix, instance, _ = strings.Cut(base, "@")
	return prefix, instance, suffix
}

// mountUnitPath is the path a .mount unit mounts: "var-lib-app.mount" is
// /var/lib/app, "-.mount" is /.
func mountUnitPath(unit string) string {
	name := strings.TrimSuffix(unit, ".mount")
	if name == "-" || name == "" {
		return "/"
	}
	return "/" + systemdUnescape(name)
}

// unitBaseName is a unit's name without its type suffix.
func unitBaseName(unit string) string {
	prefix, instance, _ := splitUnitName(unit)
	if instance != "" {
		return prefix + "@" + instance
	}
	return prefix
}

// classifyHaUnit says what unit is and labels it. facts may be nil when the
// node could not describe the unit.
func classifyHaUnit(unit string, facts *haUnitFacts) (kind, label string) {
	defer func() {
		zap.L().Debug("classifyHaUnit: classified", zap.String("unit", unit), zap.Bool("has_facts", facts != nil),
			zap.String("kind", kind), zap.String("label", label))
	}()
	if facts != nil && facts.Env != nil {
		if agent := facts.Env["AGENT"]; agent != "" {
			zap.L().Debug("classifyHaUnit: OCF unit, classifying by its agent", zap.String("unit", unit), zap.String("agent", agent))
			return classifyOCFAgent(agent, facts.Env)
		}
	}
	prefix, instance, suffix := splitUnitName(unit)
	inst := systemdUnescape(instance)
	switch {
	case suffix == "mount":
		return haKindMount, mountUnitPath(unit)
	case suffix == "target" && prefix == "drbd-services":
		return haKindTarget, "services of " + inst
	case prefix == "drbd-promote":
		return haKindPromote, "promote " + inst
	case prefix == "service-ip":
		return haKindServiceIP, inst
	case prefix == "haify-smbd":
		return haKindSMB, "Samba for " + inst
	case prefix == "ocf.rs":
		// An ocf.rs@ unit whose Environment the node did not report.
		return haKindService, "OCF " + inst
	}
	desc := unitOwnDescription(facts)
	name := strings.ToLower(prefix)
	switch {
	case strings.HasPrefix(name, "haify-app-"):
		return classifyHaifyApp(strings.TrimPrefix(prefix, "haify-app-"), desc)
	case isDatabaseUnit(name), describesDatabase(desc):
		return haKindDatabase, labelOr(desc, unitBaseName(unit))
	case name == "haify-controller", name == "haify-ai", strings.HasPrefix(name, "haify-mcp"):
		return haKindHaify, labelOr(desc, unitBaseName(unit))
	case name == "nfs-server", name == "nfs-kernel-server":
		return haKindNFS, labelOr(desc, unitBaseName(unit))
	case name == "smbd", name == "smb":
		return haKindSMB, labelOr(desc, unitBaseName(unit))
	case name == "target", name == "tgt", name == "rtslib-fb-targetctl":
		return haKindISCSI, labelOr(desc, unitBaseName(unit))
	}
	return haKindService, labelOr(desc, unitBaseName(unit))
}

// databasePrefixes are the unit names of the database servers Haify knows,
// as distributions package them (postgresql@16-main, mariadb, redis-server).
var databasePrefixes = []string{"postgresql", "postgres", "mysql", "mysqld", "mariadb", "redis", "valkey"}

func isDatabaseUnit(name string) bool {
	for _, p := range databasePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// describesDatabase reports whether a unit's description names a database
// server, for units named after what they serve rather than what runs them
// ("pgha.service", "PostgreSQL HA instance on DRBD").
func describesDatabase(desc string) bool {
	words := strings.FieldsFunc(strings.ToLower(desc), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
	for _, w := range words {
		for _, p := range databasePrefixes {
			if w == p {
				return true
			}
		}
	}
	return false
}

// classifyHaifyApp labels haify-app-<name>.service, whose own description is
// "Haify app <name> (<engine>)" (apptemplate.Unit).
func classifyHaifyApp(name, desc string) (kind, label string) {
	defer func() {
		zap.L().Debug("classifyHaifyApp: classified", zap.String("app", name), zap.String("kind", kind), zap.String("label", label))
	}()
	engine := ""
	if i := strings.LastIndexByte(desc, '('); i >= 0 && strings.HasSuffix(desc, ")") {
		engine = strings.ToLower(desc[i+1 : len(desc)-1])
	}
	label = "app " + name
	if engine != "" {
		label = fmt.Sprintf("%s app %s", engine, name)
	}
	if engine == "" || isDatabaseUnit(engine) {
		return haKindDatabase, label
	}
	return haKindService, label
}

// classifyOCFAgent labels an OCF agent's unit by the agent and the
// parameters that say what this instance is for.
func classifyOCFAgent(agentPath string, env map[string]string) (kind, label string) {
	// The parameters are not logged as a whole: an agent's can hold a password.
	defer func() {
		zap.L().Debug("classifyOCFAgent: classified", zap.String("agent", agentPath), zap.Int("params", len(env)),
			zap.String("kind", kind), zap.String("label", label))
	}()
	agent := path.Base(agentPath)
	p := func(k string) string { return env[k] }
	switch agent {
	case "Filesystem":
		return haKindMount, labelOr(p("directory"), "Filesystem")
	case "IPaddr2", "IPaddr":
		if p("cidr_netmask") != "" {
			return haKindServiceIP, p("ip") + "/" + p("cidr_netmask")
		}
		return haKindServiceIP, labelOr(p("ip"), agent)
	case "nfsserver":
		return haKindNFS, "NFS server"
	case "exportfs":
		return haKindNFS, joinNonEmpty("export", p("directory"), arrowed(p("clientspec")))
	case "iSCSITarget":
		return haKindISCSI, joinNonEmpty("target", p("iqn"))
	case "iSCSILogicalUnit":
		return haKindISCSI, joinNonEmpty("LUN", p("lun"), p("path"))
	case "nvmet-subsystem":
		return haKindNVMeoF, joinNonEmpty("subsystem", p("nqn"))
	case "nvmet-namespace":
		return haKindNVMeoF, joinNonEmpty("namespace", p("namespace_id"), p("backing_path"))
	case "nvmet-port":
		port := p("addr")
		if t := p("type"); t != "" {
			port = joinNonEmpty(port, "("+t+")")
		}
		return haKindNVMeoF, joinNonEmpty("port", port)
	case "portblock":
		return portblockKind(p("portno")), joinNonEmpty(p("action"), strings.Trim(p("protocol")+"/"+p("portno"), "/"))
	case "VirtualDomain":
		if c := p("config"); c != "" {
			return haKindVM, strings.TrimSuffix(path.Base(c), ".xml")
		}
		return haKindVM, "VirtualDomain"
	case "pgsql", "mysql", "redis":
		return haKindDatabase, agent
	}
	return haKindService, path.Base(path.Dir(agentPath)) + ":" + agent
}

// portblockKind tells which gateway a portblock entry guards by its port.
func portblockKind(port string) string {
	switch port {
	case "2049":
		return haKindNFS
	case "3260":
		return haKindISCSI
	case "4420":
		return haKindNVMeoF
	case "445":
		return haKindSMB
	}
	return haKindService
}

// unitOwnDescription is the description of the unit's own file when it has
// one, else the effective one, without drbd-reactor's prefix either way.
func unitOwnDescription(facts *haUnitFacts) string {
	if facts == nil {
		return ""
	}
	d := facts.OwnDesc
	if strings.Contains(d, "%") {
		// A template's description: expand the instance specifiers.
		_, instance, _ := splitUnitName(facts.ID)
		d = strings.NewReplacer("%i", instance, "%I", systemdUnescape(instance)).Replace(d)
	}
	if d == "" || strings.Contains(d, "%") {
		d = facts.Description
	}
	return strings.TrimSpace(strings.TrimPrefix(d, reactorDescPrefix))
}

func labelOr(v, fallback string) string {
	if strings.TrimSpace(v) != "" {
		return v
	}
	return fallback
}

func arrowed(v string) string {
	if v == "" {
		return ""
	}
	return "to " + v
}

func joinNonEmpty(parts ...string) string {
	out := parts[:0:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " ")
}

// cgroupUnit is the systemd unit a cgroup path belongs to, as systemd works
// it out: the first path element that is not a slice.
// "/system.slice/openclaw.service" is openclaw.service;
// "/user.slice/user-0.slice/session-3.scope" is session-3.scope; "/" (where
// kernel threads live) is no unit.
func cgroupUnit(cg string) string {
	for _, part := range strings.Split(cg, "/") {
		if part == "" || strings.HasSuffix(part, ".slice") {
			continue
		}
		return part
	}
	return ""
}

// isSessionUnit reports whether a unit is a login session (or a user's
// service manager), which is a person at a shell rather than a service.
func isSessionUnit(unit string) bool {
	if strings.HasPrefix(unit, "session-") && strings.HasSuffix(unit, ".scope") {
		return true
	}
	return strings.HasPrefix(unit, "user@") && strings.HasSuffix(unit, ".service")
}

// systemdWords splits a systemctl show value the way systemd prints lists:
// blank-separated words, double-quoted when they hold blanks, with
// backslash escapes inside quotes.
func systemdWords(s string) []string {
	var words []string
	var cur strings.Builder
	in, quoted := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quoted && c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
		case c == '"':
			quoted = !quoted
			in = true
		case !quoted && (c == ' ' || c == '\t'):
			if in {
				words = append(words, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteByte(c)
			in = true
		}
	}
	if in {
		words = append(words, cur.String())
	}
	return words
}

// parseOCFEnv reads an ocf.rs@ unit's Environment into AGENT and its OCF
// parameters (without the OCF_RESKEY_ prefix). drbd-reactor writes the values
// \xNN-escaped; systemd normally hands them back unescaped, and a value that
// still holds an escape is unescaped here.
func parseOCFEnv(s string) map[string]string {
	env := map[string]string{}
	skipped := 0
	for _, w := range systemdWords(s) {
		k, v, ok := strings.Cut(w, "=")
		if !ok {
			skipped++
			continue
		}
		v = unescapeHexOnly(v)
		switch {
		case k == "AGENT":
			env["AGENT"] = v
		case strings.HasPrefix(k, "OCF_RESKEY_"):
			env[strings.TrimPrefix(k, "OCF_RESKEY_")] = v
		}
	}
	// Values are not logged: an OCF parameter can be a password.
	zap.L().Debug("parseOCFEnv: parsed", zap.String("agent", env["AGENT"]), zap.Int("entries", len(env)),
		zap.Int("skipped", skipped))
	return env
}

// unescapeHexOnly turns \xNN into the byte and leaves everything else, "-"
// included, alone.
func unescapeHexOnly(s string) string {
	if !strings.Contains(s, `\x`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && s[i+1] == 'x' {
			if v, err := strconv.ParseUint(s[i+2:i+4], 16, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
