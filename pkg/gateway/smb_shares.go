package gateway

import (
	"bufio"
	"context"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Shares and users of a running SMB gateway.
//
// Both live on the gateway's state volume (smb.go), which only the node
// serving the gateway has mounted, so every change here is made there and
// applied with `smbcontrol reload-config` — smbd re-reads its shares without
// dropping a session. A stopped gateway cannot be changed: start it first.
// Users are in smb_users.go.

// SMBShare is one share of an SMB gateway.
type SMBShare struct {
	Name string
	// Path is the share's directory relative to the data volume's root; ""
	// shares the whole volume.
	Path       string
	ReadOnly   bool
	ValidUsers []string
}

var (
	smbShareNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	smbUserRE      = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	smbPathRE      = regexp.MustCompile(`^[A-Za-z0-9_.][A-Za-z0-9_./-]*$`)
	smbWorkgroupRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,14}$`)
	smbReserved    = map[string]bool{"global": true, "homes": true, "printers": true, "print$": true, "ipc$": true}
)

func (sh *SMBShare) validate() error {
	log := zap.L()
	if !smbShareNameRE.MatchString(sh.Name) || smbReserved[strings.ToLower(sh.Name)] {
		log.Debug("SMBShare.validate: invalid name", zap.String("share", sh.Name))
		return fmt.Errorf("invalid share name %q: letters, digits, '_', '.', '-', up to 64, not a Samba reserved name", sh.Name)
	}
	if sh.Path != "" {
		clean := path.Clean(sh.Path)
		if !smbPathRE.MatchString(sh.Path) || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(sh.Path, "/") {
			log.Debug("SMBShare.validate: invalid path", zap.String("share", sh.Name), zap.String("path", sh.Path))
			return fmt.Errorf("invalid share path %q: a directory under the gateway's volume, like \"projects/a\"", sh.Path)
		}
		sh.Path = clean
		if sh.Path == "." {
			sh.Path = ""
		}
	}
	for _, u := range sh.ValidUsers {
		if err := validateSMBUser(u); err != nil {
			log.Debug("SMBShare.validate: invalid valid user", zap.String("share", sh.Name), zap.String("user", u))
			return err
		}
	}
	log.Debug("SMBShare.validate: valid",
		zap.String("share", sh.Name), zap.String("path", sh.Path), zap.Bool("read_only", sh.ReadOnly), zap.Strings("valid_users", sh.ValidUsers))
	return nil
}

func validateSMBUser(u string) error {
	if !smbUserRE.MatchString(u) || u == smbOwner || u == "root" {
		zap.L().Debug("validateSMBUser: invalid", zap.String("user", u))
		return fmt.Errorf("invalid SMB user %q: lower-case letters, digits, '_' and '-', up to 32, starting with a letter", u)
	}
	return nil
}

func validateSMBWorkgroup(w string) (string, error) {
	if w == "" {
		zap.L().Debug("validateSMBWorkgroup: none given, using WORKGROUP")
		return "WORKGROUP", nil
	}
	if !smbWorkgroupRE.MatchString(w) {
		zap.L().Debug("validateSMBWorkgroup: invalid", zap.String("workgroup", w))
		return "", fmt.Errorf("invalid workgroup %q: up to 15 letters, digits, '_' or '-'", w)
	}
	return strings.ToUpper(w), nil
}

// section renders the share for shares.conf.
func (sh SMBShare) section(resource string) string {
	ro := "no"
	if sh.ReadOnly {
		ro = "yes"
	}
	lines := []string{
		"[" + sh.Name + "]",
		"  path = " + path.Join(smbShareRoot(resource), sh.Path),
		"  read only = " + ro,
		"  force user = " + smbOwner,
		"  force group = " + smbOwner,
		"  create mask = 0664",
		"  directory mask = 0775",
	}
	if len(sh.ValidUsers) > 0 {
		lines = append(lines, "  valid users = "+strings.Join(sh.ValidUsers, " "))
	}
	return strings.Join(lines, "\n") + "\n"
}

// renderSMBShares renders a whole shares.conf.
func renderSMBShares(resource string, shares []SMBShare) string {
	zap.L().Debug("renderSMBShares: rendering", zap.String("resource", resource), zap.Int("count", len(shares)))
	var b strings.Builder
	b.WriteString("# Haify SMB gateway shares (managed by haify gateway smb share)\n")
	for _, sh := range shares {
		b.WriteString("\n" + sh.section(resource))
	}
	return b.String()
}

// parseSMBShares reads shares.conf back.
func parseSMBShares(resource, content string) []SMBShare {
	var out []SMBShare
	var cur *SMBShare
	root := smbShareRoot(resource)
	sc := bufio.NewScanner(strings.NewReader(content))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";"):
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			out = append(out, SMBShare{Name: strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")})
			cur = &out[len(out)-1]
		case cur != nil:
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
			switch k {
			case "path":
				cur.Path = strings.TrimPrefix(strings.TrimPrefix(v, root), "/")
			case "read only":
				cur.ReadOnly = v == "yes" || v == "true"
			case "valid users":
				cur.ValidUsers = strings.Fields(v)
			}
		}
	}
	zap.L().Debug("parseSMBShares: parsed", zap.String("resource", resource), zap.Int("count", len(out)))
	return out
}

// servingNode returns the node serving resource's gateway, and every node
// that may serve it.
func (s *SMBManager) servingNode(ctx context.Context, resource string) (string, []string, error) {
	log := s.log()
	log.Debug("SMBManager.servingNode: start", zap.String("resource", resource))
	res, err := s.resources.GetResource(ctx, resource)
	if err != nil || res == nil {
		log.Debug("SMBManager.servingNode: resource lookup failed", zap.String("resource", resource), zap.Error(err))
		return "", nil, fmt.Errorf("look up %s: %v", resource, err)
	}
	hosts := gatewayNodes(res)
	states, err := s.gatewayTargetStates(ctx, hosts, resource)
	if err != nil {
		log.Debug("SMBManager.servingNode: target states unknown", zap.String("resource", resource), zap.Error(err))
		return "", nil, err
	}
	host, err := runningGatewayHost(hosts, states, resource)
	if err != nil {
		log.Debug("SMBManager.servingNode: serving node unclear", zap.String("resource", resource), zap.Error(err))
		return "", nil, err
	}
	if host == "" {
		log.Debug("SMBManager.servingNode: gateway not running", zap.String("resource", resource))
		return "", nil, fmt.Errorf("SMB gateway %s is not running; start it first: its shares and users live on its state volume, which only the serving node has mounted", resource)
	}
	log.Debug("SMBManager.servingNode: found", zap.String("resource", resource), zap.String("host", host), zap.Strings("hosts", hosts))
	return host, hosts, nil
}

// readOn returns what cmd prints on host.
func (s *SMBManager) readOn(ctx context.Context, host, cmd string) (string, error) {
	log := s.log()
	reader, ok := s.deployment.(HostOutputReader)
	if !ok {
		log.Debug("SMBManager.readOn: deployment client cannot return output")
		return "", fmt.Errorf("deployment client cannot read node output")
	}
	start := time.Now()
	out, err := reader.ExecOutput(ctx, []string{host}, scriptCmd(cmd))
	if err != nil {
		log.Debug("SMBManager.readOn: exec failed", zap.String("host", host), zap.Duration("took", time.Since(start)), zap.Error(err))
		return "", err
	}
	text, ok := out[host]
	if !ok {
		log.Debug("SMBManager.readOn: host did not answer", zap.String("host", host), zap.Duration("took", time.Since(start)))
		return "", fmt.Errorf("%s did not answer", host)
	}
	log.Debug("SMBManager.readOn: read", zap.String("host", host), zap.Int("output_len", len(text)), zap.Duration("took", time.Since(start)))
	return text, nil
}

// ListSMBShares lists the shares of a running SMB gateway.
func (s *SMBManager) ListSMBShares(ctx context.Context, resource string) ([]SMBShare, error) {
	log := s.log()
	log.Debug("SMBManager.ListSMBShares: start", zap.String("resource", resource))
	host, _, err := s.servingNode(ctx, resource)
	if err != nil {
		log.Debug("SMBManager.ListSMBShares: no serving node", zap.String("resource", resource), zap.Error(err))
		return nil, err
	}
	content, err := s.readOn(ctx, host, "cat "+smbSharesPath(resource))
	if err != nil {
		log.Debug("SMBManager.ListSMBShares: reading shares failed", zap.String("host", host), zap.Error(err))
		return nil, err
	}
	return parseSMBShares(resource, content), nil
}

// writeShares replaces shares.conf on host, creates the directory of share
// (when given) and has smbd re-read its shares.
func (s *SMBManager) writeShares(ctx context.Context, host, resource string, shares []SMBShare, created *SMBShare) error {
	log := s.log()
	mk := ""
	if created != nil {
		dir := path.Join(smbShareRoot(resource), created.Path)
		log.Debug("SMBManager.writeShares: creating share directory", zap.String("host", host), zap.String("dir", dir))
		mk = fmt.Sprintf("mkdir -p %[1]s && chown %[2]s:%[2]s %[1]s\n", shq(dir), smbOwner)
	}
	log.Debug("SMBManager.writeShares: writing shares and reloading smbd",
		zap.String("host", host), zap.String("resource", resource), zap.Int("count", len(shares)))
	script := fmt.Sprintf(`set -e
%[1]secho %[2]s | base64 -d > %[3]s.new && mv %[3]s.new %[3]s
smbcontrol -s %[4]s smbd reload-config
`, mk, base64Of(renderSMBShares(resource, shares)), smbSharesPath(resource), smbConfPath(resource))
	return s.runScript(ctx, []string{host}, script)
}

// AddSMBShare adds a share to a running SMB gateway.
func (s *SMBManager) AddSMBShare(ctx context.Context, resource string, share SMBShare) error {
	log := s.log()
	log.Debug("SMBManager.AddSMBShare: start",
		zap.String("resource", resource), zap.String("share", share.Name), zap.String("path", share.Path))
	if err := share.validate(); err != nil {
		return invalidArgument(err)
	}
	host, _, err := s.servingNode(ctx, resource)
	if err != nil {
		log.Debug("SMBManager.AddSMBShare: no serving node", zap.String("resource", resource), zap.Error(err))
		return err
	}
	content, err := s.readOn(ctx, host, "cat "+smbSharesPath(resource))
	if err != nil {
		log.Debug("SMBManager.AddSMBShare: reading shares failed", zap.String("host", host), zap.Error(err))
		return err
	}
	shares := parseSMBShares(resource, content)
	for _, sh := range shares {
		if strings.EqualFold(sh.Name, share.Name) {
			log.Debug("SMBManager.AddSMBShare: share exists", zap.String("resource", resource), zap.String("share", share.Name))
			return fmt.Errorf("share %s already exists on %s", share.Name, resource)
		}
	}
	if err := smbNestedExposure(shares, share); err != nil {
		log.Debug("SMBManager.AddSMBShare: nesting would widen access", zap.String("share", share.Name), zap.Error(err))
		return invalidArgument(err)
	}
	shares = append(shares, share)
	s.logger.Info("Adding SMB share", zap.String("resource", resource), zap.String("share", share.Name))
	return s.writeShares(ctx, host, resource, shares, &share)
}

// smbNestedExposure refuses a share whose directory overlaps another share's
// with a wider set of users. Samba checks valid users per share, not per
// directory, so the files of a share restricted to alice are open to everyone
// a share of its parent directory lets in — the gateway's first share covers
// the whole volume and lets in every user unless it was given --valid-users.
func smbNestedExposure(existing []SMBShare, added SMBShare) error {
	for _, sh := range existing {
		outer, inner := sh, added
		if !smbPathWithin(inner.Path, outer.Path) {
			outer, inner = added, sh
			if !smbPathWithin(inner.Path, outer.Path) {
				continue
			}
		}
		extra := smbUsersBeyond(outer.ValidUsers, inner.ValidUsers)
		if extra == "" {
			zap.L().Debug("smbNestedExposure: nested shares, no wider access",
				zap.String("outer", outer.Name), zap.String("inner", inner.Name))
			continue
		}
		zap.L().Debug("smbNestedExposure: outer share lets in more users",
			zap.String("outer", outer.Name), zap.String("inner", inner.Name), zap.String("extra", extra))
		return fmt.Errorf("share %s (/%s) lies inside share %s (/%s), which lets in %s: they would reach %s's files "+
			"through %s. Give %s --valid-users that %s also has (remove and re-add it), or put %s outside it",
			inner.Name, inner.Path, outer.Name, outer.Path, extra, inner.Name, outer.Name, outer.Name, inner.Name, inner.Name)
	}
	return nil
}

// smbPathWithin reports whether dir is base or under it ("" is the volume root).
func smbPathWithin(dir, base string) bool {
	return base == "" || dir == base || strings.HasPrefix(dir, base+"/")
}

// smbUsersBeyond describes who outer lets in that inner does not, or "" when
// outer lets in no one inner does not. An empty list means every user.
func smbUsersBeyond(outer, inner []string) string {
	if len(inner) == 0 {
		return ""
	}
	if len(outer) == 0 {
		return "every user"
	}
	allowed := map[string]bool{}
	for _, u := range inner {
		allowed[u] = true
	}
	var extra []string
	for _, u := range outer {
		if !allowed[u] {
			extra = append(extra, u)
		}
	}
	return strings.Join(extra, ", ")
}

// RemoveSMBShare removes a share; its directory and data are left in place.
func (s *SMBManager) RemoveSMBShare(ctx context.Context, resource, name string) error {
	log := s.log()
	log.Debug("SMBManager.RemoveSMBShare: start", zap.String("resource", resource), zap.String("share", name))
	host, _, err := s.servingNode(ctx, resource)
	if err != nil {
		log.Debug("SMBManager.RemoveSMBShare: no serving node", zap.String("resource", resource), zap.Error(err))
		return err
	}
	content, err := s.readOn(ctx, host, "cat "+smbSharesPath(resource))
	if err != nil {
		log.Debug("SMBManager.RemoveSMBShare: reading shares failed", zap.String("host", host), zap.Error(err))
		return err
	}
	shares := parseSMBShares(resource, content)
	kept := shares[:0]
	for _, sh := range shares {
		if !strings.EqualFold(sh.Name, name) {
			kept = append(kept, sh)
		}
	}
	if len(kept) == len(shares) {
		log.Debug("SMBManager.RemoveSMBShare: share not found", zap.String("resource", resource), zap.String("share", name))
		return fmt.Errorf("share %s not found on %s", name, resource)
	}
	s.logger.Info("Removing SMB share", zap.String("resource", resource), zap.String("share", name))
	return s.writeShares(ctx, host, resource, kept, nil)
}
