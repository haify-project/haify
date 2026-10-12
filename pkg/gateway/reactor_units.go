package gateway

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
	"go.uber.org/zap"
)

// The systemd units drbd-reactor makes out of a promoter's start list, worked
// out the way drbd-reactor itself does it (src/plugin/promoter.rs,
// generate_systemd_templates; src/systemd.rs, escaped_ocf_parse_to_env).
//
// This exists because a running gateway cannot be edited through drbd-reactor.
// A changed promoter config is a different plugin to drbd-reactor: on reload it
// stops the old one and starts a new one, and haify writes every gateway with
// stop-services-on-exit = true, so stopping the old plugin stops the whole
// service chain — filesystem, target, LUNs, service IP — demotes the resource
// and lets every node race to promote it again. Whatever an edit changes has
// to reach the node that runs the gateway some other way, and its units have
// to describe the edited chain, or the next start of a unit there (systemd
// restarts a failed OCF unit by itself) would bring back the old one. These
// are the drop-ins drbd-reactor would have written; on its next reload of that
// config it writes the same files again.

// systemdRunDir is where drbd-reactor writes its unit drop-ins.
const systemdRunDir = "/run/systemd/system"

// defaultOCFRoot is drbd-reactor's default ocf-root.
const defaultOCFRoot = "/usr/lib/ocf"

var ocfStartRE = regexp.MustCompile(`(?s)^ocf:(\S+):(\S+)\s+(.*)$`)

// reactorUnit is one start-list entry as a systemd unit.
type reactorUnit struct {
	// Name is the unit name, e.g. "ocf.rs@target_blk.service".
	Name string
	// Agent is "<vendor>:<agent>" for an OCF entry, empty for a plain unit.
	Agent string
	// Params are the OCF parameters as the agent receives them.
	Params map[string]string
	// Env is the unit's Environment= values, escaped as drbd-reactor does.
	Env []string
}

// promoterUnits is a single-resource promoter config's service chain.
type promoterUnits struct {
	Resource       string
	TargetAs       string
	DependenciesAs string
	Units          []reactorUnit
}

type promoterFile struct {
	Promoter []struct {
		Resources map[string]struct {
			Start          []string `toml:"start"`
			TargetAs       string   `toml:"target-as"`
			DependenciesAs string   `toml:"dependencies-as"`
			OCFRoot        string   `toml:"ocf-root"`
		} `toml:"resources"`
	} `toml:"promoter"`
}

// parsePromoterUnits reads a gateway's promoter config into its units.
func parsePromoterUnits(content string) (*promoterUnits, error) {
	log := zap.L()
	log.Debug("parsePromoterUnits: start", zap.Int("size_bytes", len(content)))
	var f promoterFile
	if _, err := toml.Decode(content, &f); err != nil {
		// Not the error: a TOML error can quote the text around it.
		log.Debug("parsePromoterUnits: not valid TOML")
		return nil, fmt.Errorf("parse promoter config: %w", err)
	}
	if len(f.Promoter) != 1 || len(f.Promoter[0].Resources) != 1 {
		resources := 0
		if len(f.Promoter) > 0 {
			resources = len(f.Promoter[0].Resources)
		}
		log.Debug("parsePromoterUnits: not a single-resource promoter",
			zap.Int("promoters", len(f.Promoter)), zap.Int("resources", resources))
		return nil, fmt.Errorf("promoter config must hold exactly one promoter with one resource")
	}
	p := &promoterUnits{TargetAs: "Requires", DependenciesAs: "Requires"}
	for name, res := range f.Promoter[0].Resources {
		p.Resource = name
		if res.TargetAs != "" {
			p.TargetAs = res.TargetAs
		}
		if res.DependenciesAs != "" {
			p.DependenciesAs = res.DependenciesAs
		}
		root := res.OCFRoot
		if root == "" {
			root = defaultOCFRoot
		}
		log.Debug("parsePromoterUnits: resource",
			zap.String("resource", name), zap.String("target_as", p.TargetAs),
			zap.String("dependencies_as", p.DependenciesAs), zap.String("ocf_root", root), zap.Int("count", len(res.Start)))
		seen := map[string]bool{}
		for idx, action := range res.Start {
			u, err := parseStartAction(name, strings.TrimSpace(action), root)
			if err != nil {
				// Neither the entry nor the error, which quotes it: an iSCSI
				// target entry holds the CHAP password.
				log.Debug("parsePromoterUnits: start entry unparsable", zap.String("resource", name), zap.Int("index", idx))
				return nil, err
			}
			if seen[u.Name] {
				log.Debug("parsePromoterUnits: unit used twice", zap.String("unit", u.Name))
				return nil, fmt.Errorf("unit %s is used twice", u.Name)
			}
			seen[u.Name] = true
			log.Debug("parsePromoterUnits: unit", zap.Int("index", idx), zap.String("unit", u.Name), zap.String("agent", u.Agent))
			p.Units = append(p.Units, u)
		}
	}
	return p, nil
}

func parseStartAction(resource, action, ocfRoot string) (reactorUnit, error) {
	m := ocfStartRE.FindStringSubmatch(action)
	if m == nil {
		zap.L().Debug("parseStartAction: plain systemd unit", zap.String("resource", resource), zap.String("unit", action))
		return reactorUnit{Name: action}, nil
	}
	vendor, agent := m[1], m[2]
	args, err := shellWords(m[3])
	if err != nil {
		zap.L().Debug("parseStartAction: arguments unparsable", zap.String("agent", vendor+":"+agent), zap.Error(err))
		return reactorUnit{}, fmt.Errorf("parse %q: %w", action, err)
	}
	if len(args) == 0 {
		zap.L().Debug("parseStartAction: OCF entry without instance name", zap.String("agent", vendor+":"+agent))
		return reactorUnit{}, fmt.Errorf("parse %q: an OCF agent needs an instance name", action)
	}
	u := reactorUnit{
		Name:   fmt.Sprintf("ocf.rs@%s.service", systemdEscapeName(args[0]+"_"+resource)),
		Agent:  vendor + ":" + agent,
		Params: map[string]string{},
	}
	for _, item := range args[1:] {
		k, v, _ := strings.Cut(item, "=")
		if k == "" {
			continue
		}
		u.Params[k] = v
		u.Env = append(u.Env, fmt.Sprintf("OCF_RESKEY_%s=%s", k, systemdEscapeEnv(v)))
	}
	root := path.Clean(ocfRoot)
	u.Env = append(u.Env,
		"OCF_ROOT="+systemdEscapeEnv(root),
		"AGENT="+systemdEscapeEnv(path.Join(root, "resource.d", vendor, agent)))
	return u, nil
}

// shellWords splits like the shell_words crate drbd-reactor uses: blanks
// separate words, single quotes are literal, double quotes allow \" \\ \$ \`,
// and a backslash outside quotes escapes the next character.
func shellWords(s string) ([]string, error) {
	var words []string
	var cur strings.Builder
	inWord := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case ' ', '\t', '\n':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		case '\'':
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				return nil, fmt.Errorf("unterminated single quote")
			}
			cur.WriteString(s[i+1 : i+1+end])
			i += end + 1
			inWord = true
		case '"':
			i++
			for ; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) && strings.IndexByte("\"\\$`\n", s[i+1]) >= 0 {
					i++
				}
				cur.WriteByte(s[i])
			}
			if i >= len(s) {
				return nil, fmt.Errorf("unterminated double quote")
			}
			inWord = true
		case '\\':
			if i+1 < len(s) {
				i++
				cur.WriteByte(s[i])
			}
			inWord = true
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, nil
}

// systemdEscapeName is drbd-reactor's escape_name (libsystemd's unit name
// escaping): "/" becomes "-", a leading "." and everything outside
// [:_0-9a-zA-Z.] becomes \xNN.
func systemdEscapeName(name string) string {
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c == '/':
			b.WriteByte('-')
		case c == ':' || c == '_' || isAlnum(c), c == '.' && i > 0:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, `\x%02x`, c)
		}
	}
	return b.String()
}

// systemdEscapeEnv is drbd-reactor's escape_env: everything outside
// [./:_0-9a-zA-Z] becomes \xNN.
func systemdEscapeEnv(v string) string {
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c == '.' || c == '/' || c == ':' || c == '_' || isAlnum(c) {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, `\x%02x`, c)
		}
	}
	return b.String()
}

func isAlnum(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// servicesTarget is drbd-reactor's drbd-services@<resource>.target.
func servicesTarget(resource string) string {
	return fmt.Sprintf("drbd-services@%s.target", systemdEscapeName(resource))
}

// dropInHeader marks the files as written on drbd-reactor's behalf.
const dropInHeader = "# Written by haify for an edit of a running gateway; drbd-reactor writes the same file on its next reload of this config\n"

// unitDropIn renders units[i]'s reactor.conf as promote_unit does.
func (p *promoterUnits) unitDropIn(i int) string {
	esc := systemdEscapeName(p.Resource)
	var b strings.Builder
	b.WriteString(dropInHeader)
	fmt.Fprintf(&b, "[Unit]\nDescription=drbd-reactor controlled %%N\nPartOf = drbd-services@%s.target\n\n", esc)
	fmt.Fprintf(&b, "BindsTo = drbd-promote@%s.service\nAfter = drbd-promote@%s.service\n", esc, esc)
	if i > 0 {
		prev := p.Units[i-1].Name
		fmt.Fprintf(&b, "%s = %s\nAfter = %s\n", p.DependenciesAs, prev, prev)
	}
	if env := p.Units[i].Env; len(env) > 0 {
		b.WriteString("\n[Service]\n")
		for _, e := range env {
			fmt.Fprintf(&b, "Environment= %s\n", e)
		}
	}
	return b.String()
}

// targetDropIn renders the services target's reactor.conf.
func (p *promoterUnits) targetDropIn() string {
	var b strings.Builder
	b.WriteString(dropInHeader)
	b.WriteString("[Unit]\n")
	for _, u := range p.Units {
		fmt.Fprintf(&b, "%s = %s\n", p.TargetAs, u.Name)
	}
	return b.String()
}

// unitDiff is what an edit changes in a service chain.
type unitDiff struct {
	Added   []reactorUnit
	Removed []reactorUnit
	// Changed pairs a unit's old and new definition when its parameters differ.
	Changed [][2]reactorUnit
}

func diffUnits(oldP, newP *promoterUnits) unitDiff {
	var d unitDiff
	oldByName := map[string]reactorUnit{}
	for _, u := range oldP.Units {
		oldByName[u.Name] = u
	}
	newByName := map[string]bool{}
	for _, u := range newP.Units {
		newByName[u.Name] = true
		prev, ok := oldByName[u.Name]
		switch {
		case !ok:
			d.Added = append(d.Added, u)
		case strings.Join(prev.Env, "\n") != strings.Join(u.Env, "\n"):
			d.Changed = append(d.Changed, [2]reactorUnit{prev, u})
		}
	}
	for _, u := range oldP.Units {
		if !newByName[u.Name] {
			d.Removed = append(d.Removed, u)
		}
	}
	zap.L().Debug("diffUnits: diffed",
		zap.String("resource", newP.Resource), zap.Int("units_added", len(d.Added)),
		zap.Int("units_removed", len(d.Removed)), zap.Int("units_changed", len(d.Changed)))
	return d
}

// ChainEntry is one entry of a promoter's start list as the node will run it.
type ChainEntry struct {
	// Unit is the systemd unit the entry is, e.g. "var-lib-app.mount".
	Unit string
	// AgentPath is the OCF agent an "ocf:" entry runs; empty for a plain unit.
	AgentPath string
}

// PromoterChain reads a single-resource promoter config's start list.
func PromoterChain(content string) ([]ChainEntry, error) {
	p, err := parsePromoterUnits(content)
	if err != nil {
		// Not the error: it can quote a start entry, CHAP password included.
		zap.L().Debug("PromoterChain: config unparsable")
		return nil, err
	}
	zap.L().Debug("PromoterChain: chain read", zap.String("resource", p.Resource), zap.Int("count", len(p.Units)))
	out := make([]ChainEntry, 0, len(p.Units))
	for _, u := range p.Units {
		e := ChainEntry{Unit: u.Name}
		if u.Agent != "" {
			for _, env := range u.Env {
				if v, ok := strings.CutPrefix(env, "AGENT="); ok {
					e.AgentPath = v
				}
			}
		}
		out = append(out, e)
	}
	return out, nil
}
