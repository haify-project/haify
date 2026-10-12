package apptemplate

import (
	"strconv"
	"strings"

	"go.uber.org/zap"
)

// The initialization script runs once, as root, on the node chosen as the
// first Primary, with the resource Primary there and nothing mounted. It
//
//  1. formats the volume ext4 when blkid finds no signature on it, and refuses
//     one that carries another filesystem;
//  2. mounts it at /var/lib/haify-app/<name>;
//  3. when the volume already holds an app of the same engine — an app
//     deleted without --delete-data and created again — keeps it as it is and
//     reports state=reused;
//  4. otherwise refuses a volume holding anything it did not create, moves the
//     staged password onto the volume (root only, 0600), initializes the
//     engine, starts it once without network access to prove it runs, and
//     stops it again;
//  5. unmounts, whatever happened.
//
// The password reaches the node as a file the controller copied over the SSH
// stream (deployment.DistributeSecret), never in a command line: this script
// itself travels base64-encoded inside one, where `ps` and any command log can
// see it. The script reads the password from the file into a shell variable
// and hands it to the engine through a file or stdin, never through argv.

// scriptVars fills the @PLACEHOLDERS@ every script shares.
func scriptVars(s Spec, b Binaries, extra ...string) *strings.Replacer {
	l := LayoutFor(s.Name)
	pairs := []string{
		"@NAME@", s.Name,
		"@ENGINE@", string(s.Engine),
		"@MOUNT@", l.Mount,
		"@RUN@", l.Runtime,
		"@PORT@", strconv.Itoa(s.Port),
		"@PASSWORD@", l.Password,
		"@CLIENTCONF@", l.ClientConf,
		"@SERVER@", b.Server,
		"@CLIENT@", b.Client,
		"@ADMIN@", b.Admin,
		"@INIT@", b.Init,
		"@PGCTL@", b.pgCtl(),
		"@USER@", OSUser(s.Engine),
		"@ADMINUSER@", AdminUser(s.Engine),
		"@HOLD@", strconv.Itoa(int(FreezeWatchdog.Seconds())),
		"@LOCKUNIT@", LockUnit(s.Name),
	}
	return strings.NewReplacer(append(pairs, extra...)...)
}

// InitScript renders the initialization script. device is the DRBD device to
// format and mount; staged is the absolute path of the password file the
// controller copied to the node, which the script always removes.
func InitScript(s Spec, b Binaries, device, staged string) string {
	var body, stop, selinuxType string
	switch s.Engine {
	case Postgres:
		body, stop, selinuxType = postgresInit(s), postgresStop, "postgresql_db_t"
	case MySQL:
		body, stop, selinuxType = mysqlInit(b), mysqlStop, "mysqld_db_t"
	case Redis:
		body, stop, selinuxType = redisInit, redisStop, "redis_var_lib_t"
	case RustFS:
		body, stop, selinuxType = rustfsInit, rustfsStop, "var_lib_t"
	}
	// The script carries the staged password file's path, never the password.
	zap.L().Debug("InitScript: rendering init script",
		zap.String("app", s.Name), zap.String("engine", string(s.Engine)), zap.String("device", device),
		zap.String("staged", staged), zap.String("selinux_type", selinuxType), zap.Bool("vector", s.Vector))
	r := scriptVars(s, b, "@DEV@", device, "@STAGED@", staged, "@SETYPE@", selinuxType)
	return r.Replace(strings.NewReplacer("@ENGINE_INIT@", body, "@STOP_TEMP@", stop).Replace(initFrame))
}

const initFrame = `#!/bin/bash
# haify app create: initialize @NAME@ (@ENGINE@) on this node.
cd /
umask 022
dev=@DEV@
M=@MOUNT@
run=@RUN@
staged=@STAGED@
u=@USER@
mounted=
initializing=
temp_started=
srvpid=

fail() { echo "$(hostname): $*" >&2; exit 3; }

stop_temp() {
@STOP_TEMP@
}

cleanup() {
  rc=$?
  rm -f "$staged"
  if [ -n "$temp_started" ]; then stop_temp; fi
  # A failed first initialization leaves nothing behind, so it can simply be
  # run again; the volume held nothing else (checked before it began).
  if [ $rc -ne 0 ] && [ -n "$initializing" ]; then rm -rf "$M/data" "$M/conf" "$M/haify"; fi
  if [ -n "$mounted" ]; then
    sync
    umount "$M" || echo "$(hostname): could not unmount $M" >&2
  fi
  rm -rf "$run"
  exit $rc
}
trap cleanup EXIT

real=$(readlink -f "$dev")
if [ -z "$real" ] || [ ! -b "$real" ]; then
  fail "$dev is not a block device here; is the resource up and Primary on this node?"
fi
where=$(findmnt -rn -o TARGET -S "$real" 2>/dev/null | head -n1)
if [ -n "$where" ] && [ "$where" != "$M" ]; then
  fail "$dev is mounted at $where; unmount it first"
fi
fstype=$(blkid -p -o value -s TYPE "$real" 2>/dev/null)
pttype=$(blkid -p -o value -s PTTYPE "$real" 2>/dev/null)
if [ -n "$pttype" ]; then
  fail "$dev carries a $pttype partition table; an app needs a blank volume or an ext4 filesystem"
fi
if [ -z "$fstype" ]; then
  mkfs.ext4 -q "$real" || fail "mkfs.ext4 on $dev failed"
  echo "formatted=yes"
elif [ "$fstype" != ext4 ]; then
  fail "$dev carries a $fstype filesystem; an app needs a blank volume or an ext4 filesystem"
fi
mkdir -p "$M"
if [ -z "$where" ]; then
  mount -t ext4 "$real" "$M" || fail "cannot mount $dev at $M"
fi
mounted=1

if [ -f "$M/haify/engine" ]; then
  have=$(cat "$M/haify/engine")
  [ "$have" = "@ENGINE@" ] || fail "the volume already holds a $have app; it is not initialized again as @ENGINE@"
  echo "state=reused"
  exit 0
fi
other=$(ls -A "$M" | grep -vx -e lost+found -e haify | head -n1)
[ -z "$other" ] || fail "the volume holds files haify app did not create ($M/$other); use a blank resource"
[ -s "$staged" ] || fail "the generated password did not reach this node ($staged)"

initializing=1
install -d -m 0700 -o root -g root "$M/haify"
install -m 0600 -o root -g root "$staged" "$M/haify/password"
pw=$(cat "$M/haify/password")
install -d -m 0755 -o "$u" -g "$u" "$run"

@ENGINE_INIT@

# SELinux labels are extended attributes on the volume itself, so they fail
# over with the data. Best effort: without the policy type nothing changes.
if command -v selinuxenabled >/dev/null 2>&1 && selinuxenabled; then
  chcon -R -t @SETYPE@ "$M/data" "$M/conf" 2>/dev/null || echo "$(hostname): could not label $M for SELinux (@SETYPE@)" >&2
fi

echo "@ENGINE@" > "$M/haify/engine"
sync
initializing=
echo "state=initialized"
`

// postgresInit runs initdb with the password from a file, puts the settings
// Haify relies on in conf/postgresql.haify.conf (included last from
// postgresql.conf, so they win), and starts the server once with no TCP
// listener to prove it runs — creating pgvector while it is up.
func postgresInit(s Spec) string {
	zap.L().Debug("postgresInit: rendering postgres init", zap.String("app", s.Name), zap.Bool("vector", s.Vector))
	vector := ""
	if s.Vector {
		vector = `for db in template1 postgres; do
  runuser -u "$u" -- @CLIENT@ -h "$run" -p @PORT@ -d "$db" -Atqc 'CREATE EXTENSION IF NOT EXISTS vector' >/dev/null \
    || fail "CREATE EXTENSION vector failed in $db; is pgvector installed for this PostgreSQL version?"
done
`
	}
	return `install -d -m 0700 -o "$u" -g "$u" "$M/data"
install -d -m 0750 -o "$u" -g "$u" "$M/conf"
install -m 0600 -o "$u" -g "$u" "$M/haify/password" "$M/conf/.initpw"
runuser -u "$u" -- @INIT@ -D "$M/data" -U postgres --pwfile="$M/conf/.initpw" -E UTF8 --data-checksums \
  --auth-local=peer --auth-host=scram-sha-256 >"$run/init.log" 2>&1
rc=$?
rm -f "$M/conf/.initpw"
[ $rc -eq 0 ] || fail "initdb failed: $(tail -n 5 "$run/init.log")"
cat > "$M/conf/postgresql.haify.conf" <<'HAIFYEOF'
# Written by haify app create: the settings Haify relies on. Tune anything else
# in ../data/postgresql.conf; both live on the DRBD volume and fail over
# with the data.
listen_addresses = '*'
port = @PORT@
unix_socket_directories = '@RUN@'
hba_file = '@MOUNT@/conf/pg_hba.conf'
password_encryption = 'scram-sha-256'
HAIFYEOF
cat > "$M/conf/pg_hba.conf" <<'HAIFYEOF'
# Written by haify app create. Local OS users by peer; everything over TCP
# (including clients of the service IP) by password.
# TYPE  DATABASE  USER  ADDRESS       METHOD
local   all       all                 peer
host    all       all   127.0.0.1/32  scram-sha-256
host    all       all   ::1/128       scram-sha-256
host    all       all   0.0.0.0/0     scram-sha-256
host    all       all   ::/0          scram-sha-256
HAIFYEOF
chown "$u:$u" "$M/conf/postgresql.haify.conf" "$M/conf/pg_hba.conf"
printf "\ninclude '%s'\n" "$M/conf/postgresql.haify.conf" >> "$M/data/postgresql.conf"
runuser -u "$u" -- @PGCTL@ -D "$M/data" -o "-c listen_addresses=''" -l "$run/pg.log" -w -t 120 start >/dev/null \
  || fail "postgres did not start: $(tail -n 5 "$run/pg.log")"
temp_started=1
` + vector + `stop_temp
temp_started=`
}

const postgresStop = `  runuser -u "$u" -- @PGCTL@ -D "$M/data" -m fast -w -t 120 stop >/dev/null 2>&1`

// mysqlInit initializes the data directory the way the installed flavor
// wants (mariadb-install-db, or mysqld --initialize-insecure), starts the
// server without networking, sets the root password over the socket through
// stdin, and leaves a root-only client.cnf for the tools Haify runs later
// (health probe, snapshot lock).
func mysqlInit(b Binaries) string {
	initdb := `@INIT@ --defaults-file="$M/conf/my.cnf" --user="$u" >"$run/init.log" 2>&1 \
  || fail "mariadb-install-db failed: $(tail -n 5 "$run/init.log")"`
	// MariaDB has its own install-db tool; MySQL initializes through the server.
	zap.L().Debug("mysqlInit: rendering mysql init", zap.String("flavor", b.Flavor),
		zap.Bool("initialize_through_server", b.Flavor != "mariadb"))
	if b.Flavor != "mariadb" {
		initdb = `@SERVER@ --defaults-file="$M/conf/my.cnf" --initialize-insecure --user="$u" >"$run/init.log" 2>&1 \
  || fail "mysqld --initialize-insecure failed: $(tail -n 5 "$run/init.log")"`
	}
	return `install -d -m 0750 -o "$u" -g "$u" "$M/data"
install -d -m 0750 -o root -g "$u" "$M/conf"
cat > "$M/conf/my.cnf" <<'HAIFYEOF'
# Written by haify app create. It lives on the DRBD volume and fails over with
# the data; tune the server here.
[mysqld]
datadir = @MOUNT@/data
port = @PORT@
bind-address = 0.0.0.0
socket = @RUN@/mysqld.sock
pid-file = @RUN@/mysqld.pid
skip-name-resolve
innodb_flush_log_at_trx_commit = 1
sync_binlog = 1

[client]
socket = @RUN@/mysqld.sock
HAIFYEOF
chown "root:$u" "$M/conf/my.cnf"
chmod 0640 "$M/conf/my.cnf"
` + initdb + `
@SERVER@ --defaults-file="$M/conf/my.cnf" --skip-networking --user="$u" >>"$run/init.log" 2>&1 &
srvpid=$!
temp_started=1
i=0
until @ADMIN@ --socket="$run/mysqld.sock" -uroot ping >/dev/null 2>&1; do
  kill -0 "$srvpid" 2>/dev/null || fail "the database server exited: $(tail -n 5 "$run/init.log")"
  i=$((i+1)); [ $i -lt 120 ] || fail "the database server did not answer within 120s"
  sleep 1
done
@CLIENT@ --socket="$run/mysqld.sock" -uroot <<HAIFYEOF || fail "setting the root password failed"
ALTER USER 'root'@'localhost' IDENTIFIED BY '$pw';
CREATE USER IF NOT EXISTS 'root'@'%' IDENTIFIED BY '$pw';
GRANT ALL PRIVILEGES ON *.* TO 'root'@'%' WITH GRANT OPTION;
DROP DATABASE IF EXISTS test;
FLUSH PRIVILEGES;
HAIFYEOF
(umask 077; printf '[client]\nuser=root\npassword=%s\nsocket=%s\n' "$pw" "$run/mysqld.sock" > "$M/haify/client.cnf")
stop_temp
temp_started=`
}

const mysqlStop = `  if [ -s "$M/haify/client.cnf" ]; then
    @ADMIN@ --defaults-extra-file="$M/haify/client.cnf" shutdown >/dev/null 2>&1 || kill "$srvpid" 2>/dev/null
  else
    @ADMIN@ --socket="$run/mysqld.sock" -uroot shutdown >/dev/null 2>&1 || kill "$srvpid" 2>/dev/null
  fi
  wait "$srvpid" 2>/dev/null`

// redisInit writes redis.conf (with requirepass, so mode 0600 and owned by
// the redis user) and starts the server once on its socket only. setpriv
// execs rather than forks, so $! is the server itself.
const redisInit = `install -d -m 0750 -o "$u" -g "$u" "$M/data" "$M/conf"
(umask 077; cat > "$M/conf/redis.conf" <<HAIFYEOF
# Written by haify app create. It lives on the DRBD volume and fails over with
# the data. appendfsync everysec can lose up to one second of acknowledged
# writes in a failover; set it to always for none.
bind 0.0.0.0
port @PORT@
protected-mode no
unixsocket @RUN@/redis.sock
unixsocketperm 700
daemonize no
supervised no
dir @MOUNT@/data
dbfilename dump.rdb
appendonly yes
appendfsync everysec
logfile ""
requirepass $pw
HAIFYEOF
)
chown "$u:$u" "$M/conf/redis.conf"
setpriv --reuid="$u" --regid="$u" --init-groups @SERVER@ "$M/conf/redis.conf" --port 0 >"$run/init.log" 2>&1 &
srvpid=$!
temp_started=1
i=0
until REDISCLI_AUTH="$pw" @CLIENT@ -s "$run/redis.sock" ping 2>/dev/null | grep -qx PONG; do
  kill -0 "$srvpid" 2>/dev/null || fail "redis-server exited: $(tail -n 5 "$run/init.log")"
  i=$((i+1)); [ $i -lt 60 ] || fail "redis-server did not answer within 60s"
  sleep 1
done
stop_temp
temp_started=`

const redisStop = `  REDISCLI_AUTH="$pw" @CLIENT@ -s "$run/redis.sock" shutdown >/dev/null 2>&1 || kill "$srvpid" 2>/dev/null
  wait "$srvpid" 2>/dev/null`

// rustfsInit writes the keys (the fixed access key, the generated password as
// the secret key) into files only the rustfs user reads, and starts the store
// once on loopback to prove it runs and to let it lay out its own metadata
// (.rustfs.sys) in the data directory.
const rustfsInit = `install -d -m 0750 -o "$u" -g "$u" "$M/data" "$M/conf"
(umask 077; printf '%s' "@ADMINUSER@" > "$M/conf/access_key"; printf '%s' "$pw" > "$M/conf/secret_key")
chown "$u:$u" "$M/conf/access_key" "$M/conf/secret_key"
setpriv --reuid="$u" --regid="$u" --init-groups @SERVER@ server --address 127.0.0.1:@PORT@ \
  --access-key-file "$M/conf/access_key" --secret-key-file "$M/conf/secret_key" "$M/data" >"$run/init.log" 2>&1 &
srvpid=$!
temp_started=1
i=0
until @CLIENT@ -fsS -o /dev/null --max-time 5 http://127.0.0.1:@PORT@/health/ready 2>/dev/null; do
  kill -0 "$srvpid" 2>/dev/null || fail "rustfs exited: $(tail -n 5 "$run/init.log")"
  i=$((i+1)); [ $i -lt 120 ] || fail "rustfs did not answer within 120s"
  sleep 1
done
stop_temp
temp_started=`

const rustfsStop = `  kill "$srvpid" 2>/dev/null
  wait "$srvpid" 2>/dev/null`
