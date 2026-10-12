package gateway

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Users of a running SMB gateway: their passdb entries, kept on the state
// volume, and the local accounts behind them, which must have the same uid on
// every node the gateway can run on.
//
// A password reaches the serving node base64-wrapped inside a script and is
// handed to smbpasswd on stdin; traces here say whether one was given, never
// what it is.

// SetSMBUser adds an SMB user, or changes its password.
func (s *SMBManager) SetSMBUser(ctx context.Context, resource, user, password string) error {
	log := s.log()
	log.Debug("SMBManager.SetSMBUser: start",
		zap.String("resource", resource), zap.String("user", user), zap.Bool("has_password", password != ""))
	if err := validateSMBUser(user); err != nil {
		log.Debug("SMBManager.SetSMBUser: invalid user", zap.String("user", user), zap.Error(err))
		return invalidArgument(err)
	}
	if password == "" || strings.ContainsAny(password, "\n\r\x00") {
		log.Debug("SMBManager.SetSMBUser: password empty or not on one line", zap.String("user", user))
		return invalidArgument(fmt.Errorf("a password is required, on one line"))
	}
	host, hosts, err := s.servingNode(ctx, resource)
	if err != nil {
		log.Debug("SMBManager.SetSMBUser: no serving node", zap.String("resource", resource), zap.Error(err))
		return err
	}
	recorded, err := s.recordedUIDs(ctx, host, resource)
	if err != nil {
		log.Debug("SMBManager.SetSMBUser: reading recorded uids failed", zap.String("host", host), zap.Error(err))
		return err
	}
	uid, err := s.allocateUID(ctx, hosts, user, recorded)
	if err != nil {
		log.Debug("SMBManager.SetSMBUser: no uid", zap.String("user", user), zap.Error(err))
		return err
	}
	log.Debug("SMBManager.SetSMBUser: setting passdb entry", zap.String("host", host), zap.String("user", user), zap.Int("uid", uid))
	conf, users := smbConfPath(resource), smbUsersPath(resource)
	script := fmt.Sprintf(`set -e
grep -q '^%[1]s:' %[2]s || echo '%[1]s:%[3]d' >> %[2]s
%[4]s %[2]s
pw=$(echo %[5]s | base64 -d)
printf '%%s\n%%s\n' "$pw" "$pw" | smbpasswd -c %[6]s -s -a %[1]s >/dev/null
smbpasswd -c %[6]s -e %[1]s >/dev/null
`, user, users, uid, smbUsersHelper, base64Of(password), conf)
	s.logger.Info("Setting SMB user", zap.String("resource", resource), zap.String("user", user))
	return s.runScript(ctx, []string{host}, script)
}

// RemoveSMBUser removes an SMB user. Its local account stays, unused.
func (s *SMBManager) RemoveSMBUser(ctx context.Context, resource, user string) error {
	log := s.log()
	log.Debug("SMBManager.RemoveSMBUser: start", zap.String("resource", resource), zap.String("user", user))
	if err := validateSMBUser(user); err != nil {
		log.Debug("SMBManager.RemoveSMBUser: invalid user", zap.String("user", user), zap.Error(err))
		return invalidArgument(err)
	}
	host, _, err := s.servingNode(ctx, resource)
	if err != nil {
		log.Debug("SMBManager.RemoveSMBUser: no serving node", zap.String("resource", resource), zap.Error(err))
		return err
	}
	log.Debug("SMBManager.RemoveSMBUser: removing passdb entry", zap.String("host", host), zap.String("user", user))
	script := fmt.Sprintf(`set -e
pdbedit -s %[1]s -L | grep -q '^%[2]s:' || { echo "no SMB user %[2]s" >&2; exit 1; }
smbpasswd -c %[1]s -x %[2]s >/dev/null
grep -v '^%[2]s:' %[3]s > %[3]s.new || true
mv %[3]s.new %[3]s
`, smbConfPath(resource), user, smbUsersPath(resource))
	return s.runScript(ctx, []string{host}, script)
}

// ListSMBUsers lists the SMB users of a running gateway.
func (s *SMBManager) ListSMBUsers(ctx context.Context, resource string) ([]string, error) {
	log := s.log()
	log.Debug("SMBManager.ListSMBUsers: start", zap.String("resource", resource))
	host, _, err := s.servingNode(ctx, resource)
	if err != nil {
		log.Debug("SMBManager.ListSMBUsers: no serving node", zap.String("resource", resource), zap.Error(err))
		return nil, err
	}
	out, err := s.readOn(ctx, host, "pdbedit -s "+smbConfPath(resource)+" -L")
	if err != nil {
		log.Debug("SMBManager.ListSMBUsers: pdbedit failed", zap.String("host", host), zap.Error(err))
		return nil, err
	}
	var users []string
	for _, line := range strings.Split(out, "\n") {
		if name, _, ok := strings.Cut(strings.TrimSpace(line), ":"); ok && name != "" {
			users = append(users, name)
		}
	}
	sort.Strings(users)
	log.Debug("SMBManager.ListSMBUsers: done", zap.String("resource", resource), zap.Strings("users", users))
	return users, nil
}

// recordedUIDs reads the gateway's users file.
func (s *SMBManager) recordedUIDs(ctx context.Context, host, resource string) (map[string]int, error) {
	log := s.log()
	out, err := s.readOn(ctx, host, "cat "+smbUsersPath(resource)+" 2>/dev/null; true")
	if err != nil {
		log.Debug("SMBManager.recordedUIDs: read failed", zap.String("host", host), zap.String("resource", resource), zap.Error(err))
		return nil, err
	}
	ids := parseNameIDs(out, 1)
	log.Debug("SMBManager.recordedUIDs: read", zap.String("host", host), zap.String("resource", resource), zap.Int("count", len(ids)))
	return ids, nil
}

// parseNameIDs reads "name:...:id" lines, the id being field idField.
func parseNameIDs(out string, idField int) map[string]int {
	m := map[string]int{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimSpace(line), ":")
		if len(f) <= idField || f[0] == "" {
			continue
		}
		if id, err := strconv.Atoi(f[idField]); err == nil {
			m[f[0]] = id
		}
	}
	return m
}

// allocateUID returns the uid name has, or gets, on every host: the one it
// was recorded with, the one it already has on the hosts (which must agree),
// or the lowest id from smbFirstUID that is free as a uid and a gid on all.
func (s *SMBManager) allocateUID(ctx context.Context, hosts []string, name string, recorded map[string]int) (int, error) {
	log := s.log()
	log.Debug("SMBManager.allocateUID: start", zap.String("user", name), zap.Strings("hosts", hosts), zap.Int("recorded", len(recorded)))
	if uid, ok := recorded[name]; ok {
		log.Debug("SMBManager.allocateUID: uid already recorded", zap.String("user", name), zap.Int("uid", uid))
		return uid, nil
	}
	reader, ok := s.deployment.(HostOutputReader)
	if !ok {
		log.Debug("SMBManager.allocateUID: deployment client cannot return output")
		return 0, fmt.Errorf("deployment client cannot read node output")
	}
	start := time.Now()
	out, err := reader.ExecOutput(ctx, hosts, scriptCmd("getent passwd; echo ---; getent group"))
	if err != nil {
		log.Debug("SMBManager.allocateUID: reading accounts failed",
			zap.Strings("hosts", hosts), zap.Duration("took", time.Since(start)), zap.Error(err))
		return 0, fmt.Errorf("read accounts: %w", err)
	}
	log.Debug("SMBManager.allocateUID: accounts read",
		zap.Int("count", len(out)), zap.Int("hosts", len(hosts)), zap.Duration("took", time.Since(start)))
	if len(out) < len(hosts) {
		log.Debug("SMBManager.allocateUID: not every node answered", zap.Int("answered", len(out)), zap.Int("hosts", len(hosts)))
		return 0, fmt.Errorf("cannot read the accounts on every node of the gateway; retry when they all answer")
	}
	return pickUID(name, out, recorded)
}

// pickUID is allocateUID's decision over each host's getent output.
func pickUID(name string, outputs map[string]string, recorded map[string]int) (int, error) {
	log := zap.L()
	used := map[int]bool{}
	for _, id := range recorded {
		used[id] = true
	}
	have := map[int][]string{}
	takenBy := map[int]string{} // uid -> another account's name holding it somewhere
	for host, out := range outputs {
		passwd, group, _ := strings.Cut(out, "---")
		users := parseNameIDs(passwd, 2)
		if uid, ok := users[name]; ok {
			log.Debug("pickUID: account exists on host", zap.String("user", name), zap.String("host", host), zap.Int("uid", uid))
			have[uid] = append(have[uid], host)
		}
		for n, id := range users {
			used[id] = true
			if n != name {
				takenBy[id] = n + " on " + host
			}
		}
		for _, id := range parseNameIDs(group, 2) {
			used[id] = true
		}
	}
	switch len(have) {
	case 0:
		log.Debug("pickUID: account exists nowhere, allocating", zap.String("user", name))
	case 1:
		for uid := range have {
			if other, ok := takenBy[uid]; ok {
				log.Debug("pickUID: existing uid held by another account", zap.String("user", name), zap.Int("uid", uid), zap.String("taken_by", other))
				return 0, fmt.Errorf("%s has uid %d, which is %s; the gateway could not start there", name, uid, other)
			}
			log.Debug("pickUID: reusing the uid the account has", zap.String("user", name), zap.Int("uid", uid))
			return uid, nil
		}
	default:
		log.Debug("pickUID: account has different uids on the nodes", zap.String("user", name), zap.Int("count", len(have)))
		return 0, fmt.Errorf("%s has different uids on the gateway's nodes (%v); make them agree first", name, have)
	}
	for uid := smbFirstUID; uid < smbFirstUID+4000; uid++ {
		if !used[uid] {
			log.Debug("pickUID: allocated", zap.String("user", name), zap.Int("uid", uid))
			return uid, nil
		}
	}
	log.Debug("pickUID: no free uid", zap.String("user", name), zap.Int("from", smbFirstUID))
	return 0, fmt.Errorf("no free uid from %d on every node", smbFirstUID)
}
