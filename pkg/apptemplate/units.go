package apptemplate

import (
	"fmt"
	"path"
	"strings"

	"go.uber.org/zap"
)

// HealthCommand is a shell command that exits 0 when the database answers on
// the node it runs on. It talks to the local socket, not the service IP, so
// it measures the database rather than the network, and it needs root only
// for Redis and MySQL, whose credentials it reads from the volume.
//
//	postgres  pg_isready on the unix socket (no credentials needed)
//	mysql     mysqladmin ping, which answers 0 whenever the server is up
//	redis     redis-cli PING, expecting PONG
//	rustfs    /health/ready on loopback, which answers once the store can serve
func HealthCommand(s Spec, b Binaries) string {
	l := LayoutFor(s.Name)
	zap.L().Debug("HealthCommand: building health probe", zap.String("app", s.Name), zap.String("engine", string(s.Engine)))
	switch s.Engine {
	case Postgres:
		return fmt.Sprintf("%s -q -h %s -p %d", b.Admin, l.Runtime, s.Port)
	case MySQL:
		return fmt.Sprintf("%s --defaults-extra-file=%s ping >/dev/null 2>&1", b.Admin, l.ClientConf)
	case Redis:
		return fmt.Sprintf(`REDISCLI_AUTH="$(cat %s)" %s -s %s ping 2>/dev/null | grep -qx PONG`,
			l.Password, b.Client, path.Join(l.Runtime, "redis.sock"))
	case RustFS:
		return fmt.Sprintf("%s -fsS -o /dev/null --max-time 5 http://127.0.0.1:%d/health/ready", b.Client, s.Port)
	}
	return "false"
}

// systemdEscape makes a shell command safe to put in an Exec*= line: systemd
// expands $VAR and %-specifiers itself before the shell ever sees them.
func systemdEscape(cmd string) string {
	return strings.NewReplacer("%", "%%", "$", "$$").Replace(cmd)
}

// waitReady is an ExecStartPost= that holds the unit in "activating" until
// the database answers. drbd-reactor starts the next entry of the chain — the
// service IP — only once this unit is started, so without it clients would be
// sent to a database still replaying its log after a failover. The leading
// "+" runs it as root, because the probe may read root-only credentials.
func waitReady(s Spec, b Binaries) string {
	loop := fmt.Sprintf("i=0; until %s; do i=$((i+1)); [ $i -ge %d ] && exit 1; sleep 1; done",
		HealthCommand(s, b), ReadyTimeout)
	return "+/bin/sh -c '" + systemdEscape(loop) + "'"
}

// Unit renders haify-app-<name>.service. It has no [Install] section, so it
// cannot be enabled: only the promoter starts it, after mounting the volume.
// It also refuses to start when the volume is not mounted, which keeps a
// stray `systemctl start` from initializing an empty database on a node's
// root filesystem.
//
// There is no Restart=: the promoter binds the unit to its target, so a
// database that dies takes the target down and drbd-reactor moves the whole
// chain to another node, which is the failover this is for.
func Unit(s Spec, b Binaries) string {
	l := LayoutFor(s.Name)
	user := OSUser(s.Engine)
	zap.L().Debug("Unit: rendering unit", zap.String("app", s.Name), zap.String("engine", string(s.Engine)),
		zap.String("resource", s.Resource), zap.String("user", user), zap.String("server", b.Server), zap.Int("port", s.Port))
	var exec, extra string
	switch s.Engine {
	case Postgres:
		exec = fmt.Sprintf("%s -D %s", b.Server, l.Data)
		// SIGINT is PostgreSQL's fast shutdown: sessions are cut, the
		// checkpoint is written, and the demote that follows is clean.
		extra = "ExecReload=/bin/kill -HUP $MAINPID\nKillMode=mixed\nKillSignal=SIGINT\n"
	case MySQL:
		exec = fmt.Sprintf("%s --defaults-file=%s", b.Server, path.Join(l.Conf, "my.cnf"))
	case Redis:
		exec = fmt.Sprintf("%s %s", b.Server, path.Join(l.Conf, "redis.conf"))
	case RustFS:
		// The keys are read from files on the volume, owned by the rustfs
		// user, so they never appear in a command line.
		exec = fmt.Sprintf("%s server --address :%d --access-key-file %s --secret-key-file %s "+
			"--console-enable --console-address :%d %s", b.Server, s.Port,
			path.Join(l.Conf, "access_key"), path.Join(l.Conf, "secret_key"), ConsolePort(s), l.Data)
	}
	return fmt.Sprintf(`# Haify app %[1]s: %[2]s on DRBD resource %[3]s.
# Written by haify-controller (haify app create). drbd-reactor starts and stops
# this unit through the promoter %[4]s, on the node where %[3]s is
# Primary; never enable or start it by hand.

[Unit]
Description=Haify app %[1]s (%[2]s)
After=network-online.target

[Service]
Type=simple
User=%[5]s
Group=%[5]s
RuntimeDirectory=%[6]s
RuntimeDirectoryMode=0755
ExecStartPre=/bin/sh -c 'mountpoint -q %[7]s'
ExecStart=%[8]s
ExecStartPost=%[9]s
%[10]sTimeoutStartSec=%[11]d
TimeoutStopSec=300
LimitNOFILE=65536
`, s.Name, s.Engine, s.Resource, PromoterName(s.Name), user,
		strings.TrimPrefix(l.Runtime, "/run/"), l.Mount, exec, waitReady(s, b), extra, ReadyTimeout+60)
}

// PromoterConfig renders /etc/drbd-reactor.d/haify-app-<name>.toml. The chain
// is mount, database, service IP: the address comes up last, once the
// database answers, and goes first, so a client never reaches a node whose
// database is still starting or already stopping.
func PromoterConfig(s Spec, device string) (string, error) {
	ip, prefix, err := ParseServiceIP(s.ServiceIP)
	if err != nil {
		zap.L().Debug("PromoterConfig: invalid service IP", zap.String("app", s.Name), zap.String("service_ip", s.ServiceIP), zap.Error(err))
		return "", err
	}
	l := LayoutFor(s.Name)
	zap.L().Debug("PromoterConfig: rendering promoter config",
		zap.String("app", s.Name), zap.String("resource", s.Resource), zap.String("device", device),
		zap.String("mount", l.Mount), zap.String("ip", ip), zap.Int("prefix", prefix))
	return fmt.Sprintf(`# drbd-reactor promoter for the Haify app %[1]s (%[2]s).
# Written by haify-controller (haify app create); remove it with haify app delete.

[[promoter]]
[promoter.resources.%[3]s]
runner = "systemd"
on-drbd-demote-failure = "reboot-immediate"
stop-services-on-exit = true
target-as = "BindsTo"
start = [
  "ocf:heartbeat:Filesystem fs_app device=%[4]s directory=%[5]s fstype=ext4 run_fsck=no force_unmount=false",
  "%[6]s",
  "ocf:heartbeat:IPaddr2 service_ip ip=%[7]s cidr_netmask=%[8]d",
]
`, s.Name, s.Engine, s.Resource, device, l.Mount, UnitName(s.Name), ip, prefix), nil
}

// ConnectionHint tells a client where the app answers.
func ConnectionHint(s Spec) string {
	ip, _, err := ParseServiceIP(s.ServiceIP)
	if err != nil {
		zap.L().Debug("ConnectionHint: no usable service IP", zap.String("app", s.Name), zap.Error(err))
		return ""
	}
	zap.L().Debug("ConnectionHint: building hint", zap.String("app", s.Name), zap.String("engine", string(s.Engine)), zap.String("ip", ip))
	switch s.Engine {
	case Postgres:
		return fmt.Sprintf("postgresql://%s@%s:%d/postgres", AdminUser(s.Engine), ip, s.Port)
	case MySQL:
		return fmt.Sprintf("mysql -h %s -P %d -u %s -p", ip, s.Port, AdminUser(s.Engine))
	case Redis:
		return fmt.Sprintf("redis-cli -h %s -p %d --askpass", ip, s.Port)
	case RustFS:
		return fmt.Sprintf("http://%s:%d (path-style S3; console http://%s:%d/rustfs/console/)",
			ip, s.Port, ip, ConsolePort(s))
	}
	zap.L().Debug("ConnectionHint: no hint for engine", zap.String("engine", string(s.Engine)))
	return ""
}
