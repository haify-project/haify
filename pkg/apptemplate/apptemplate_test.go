package apptemplate

import (
	"errors"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalize(t *testing.T) {
	tests := []struct {
		name    string
		in      Spec
		want    Spec
		wantErr string
	}{
		{name: "defaults", in: Spec{Name: "orders", Engine: "Postgres", ServiceIP: "10.0.0.50/24"},
			want: Spec{Name: "orders", Engine: Postgres, Resource: "orders", ServiceIP: "10.0.0.50/24", Port: 5432}},
		{name: "mysql default port", in: Spec{Name: "db", Engine: "mysql", Resource: "r1", ServiceIP: "10.0.0.50/24"},
			want: Spec{Name: "db", Engine: MySQL, Resource: "r1", ServiceIP: "10.0.0.50/24", Port: 3306}},
		{name: "redis custom port", in: Spec{Name: "cache", Engine: "redis", ServiceIP: "10.0.0.50/24", Port: 7000},
			want: Spec{Name: "cache", Engine: Redis, Resource: "cache", ServiceIP: "10.0.0.50/24", Port: 7000}},
		{name: "rustfs default port", in: Spec{Name: "objects", Engine: "rustfs", ServiceIP: "10.0.0.50/24"},
			want: Spec{Name: "objects", Engine: RustFS, Resource: "objects", ServiceIP: "10.0.0.50/24", Port: 9000}},
		{name: "rustfs console needs the next port", in: Spec{Name: "objects", Engine: "rustfs", ServiceIP: "10.0.0.50/24", Port: 65535},
			wantErr: "console"},
		{name: "vector", in: Spec{Name: "emb", Engine: "postgres", ServiceIP: "10.0.0.50/24", Vector: true},
			want: Spec{Name: "emb", Engine: Postgres, Resource: "emb", ServiceIP: "10.0.0.50/24", Port: 5432, Vector: true}},
		{name: "upper case name", in: Spec{Name: "Orders", Engine: "postgres", ServiceIP: "10.0.0.50/24"}, wantErr: "name"},
		{name: "name with slash", in: Spec{Name: "a/b", Engine: "postgres", ServiceIP: "10.0.0.50/24"}, wantErr: "name"},
		{name: "trailing hyphen", in: Spec{Name: "db-", Engine: "postgres", ServiceIP: "10.0.0.50/24"}, wantErr: "name"},
		{name: "name too long", in: Spec{Name: strings.Repeat("a", 41), Engine: "postgres", ServiceIP: "10.0.0.50/24"}, wantErr: "name"},
		{name: "unknown engine", in: Spec{Name: "db", Engine: "oracle", ServiceIP: "10.0.0.50/24"}, wantErr: "unknown engine"},
		{name: "vector on mysql", in: Spec{Name: "db", Engine: "mysql", ServiceIP: "10.0.0.50/24", Vector: true}, wantErr: "pgvector"},
		{name: "bad resource", in: Spec{Name: "db", Engine: "mysql", Resource: "r;rm", ServiceIP: "10.0.0.50/24"}, wantErr: "resource"},
		{name: "port out of range", in: Spec{Name: "db", Engine: "redis", ServiceIP: "10.0.0.50/24", Port: 70000}, wantErr: "port"},
		{name: "no service ip", in: Spec{Name: "db", Engine: "redis"}, wantErr: "service IP"},
		{name: "bare ip", in: Spec{Name: "db", Engine: "redis", ServiceIP: "10.0.0.50"}, wantErr: "CIDR"},
		{name: "ipv6", in: Spec{Name: "db", Engine: "redis", ServiceIP: "fd00::5/64"}, wantErr: "IPv4"},
		{name: "network address", in: Spec{Name: "db", Engine: "redis", ServiceIP: "10.0.0.0/24"}, wantErr: "network or broadcast"},
		{name: "broadcast address", in: Spec{Name: "db", Engine: "redis", ServiceIP: "10.0.0.255/24"}, wantErr: "network or broadcast"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.in
			err := s.Normalize()
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.True(t, errors.Is(err, ErrInvalid), "validation errors are ErrInvalid: %v", err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, s)
		})
	}
}

func TestParseServiceIP(t *testing.T) {
	ip, prefix, err := ParseServiceIP("192.168.1.60/24")
	require.NoError(t, err)
	assert.Equal(t, "192.168.1.60", ip)
	assert.Equal(t, 24, prefix)

	ip, prefix, err = ParseServiceIP("192.168.1.60/32")
	require.NoError(t, err)
	assert.Equal(t, "192.168.1.60", ip)
	assert.Equal(t, 32, prefix)
}

func specFor(t *testing.T, e Engine) Spec {
	t.Helper()
	s := Spec{Name: "orders", Engine: e, Resource: "res1", ServiceIP: "10.0.0.50/24"}
	require.NoError(t, s.Normalize())
	return s
}

func binariesFor(e Engine, flavor string) Binaries {
	switch e {
	case Postgres:
		return Binaries{Server: "/usr/lib/postgresql/16/bin/postgres", Client: "/usr/lib/postgresql/16/bin/psql",
			Admin: "/usr/lib/postgresql/16/bin/pg_isready", Init: "/usr/lib/postgresql/16/bin/initdb"}
	case MySQL:
		if flavor == "mariadb" {
			return Binaries{Server: "/usr/sbin/mariadbd", Client: "/usr/bin/mariadb", Admin: "/usr/bin/mariadb-admin",
				Init: "/usr/bin/mariadb-install-db", Flavor: "mariadb"}
		}
		return Binaries{Server: "/usr/sbin/mysqld", Client: "/usr/bin/mysql", Admin: "/usr/bin/mysqladmin", Flavor: "mysql"}
	case RustFS:
		return Binaries{Server: "/usr/local/bin/rustfs", Client: "/usr/bin/curl"}
	}
	return Binaries{Server: "/usr/bin/redis-server", Client: "/usr/bin/redis-cli"}
}

// The chain must mount, then start the database, then raise the service IP:
// clients only reach a node whose database answers, and lose the address
// before the database stops.
func TestPromoterConfigStartsTheServiceIPLast(t *testing.T) {
	for _, e := range []Engine{Postgres, MySQL, Redis, RustFS} {
		t.Run(string(e), func(t *testing.T) {
			s := specFor(t, e)
			cfg, err := PromoterConfig(s, DataDevice("res1", 0))
			require.NoError(t, err)

			assert.Contains(t, cfg, "[promoter.resources.res1]")
			fs := strings.Index(cfg, `"ocf:heartbeat:Filesystem fs_app device=/dev/drbd/by-res/res1/0 directory=/var/lib/haify-app/orders fstype=ext4`)
			unit := strings.Index(cfg, `"haify-app-orders.service"`)
			vip := strings.Index(cfg, `"ocf:heartbeat:IPaddr2 service_ip ip=10.0.0.50 cidr_netmask=24"`)
			require.True(t, fs >= 0 && unit >= 0 && vip >= 0, cfg)
			assert.Less(t, fs, unit, "the volume is mounted before the database starts")
			assert.Less(t, unit, vip, "the service IP comes after the database")
			assert.Equal(t, vip, strings.LastIndex(cfg, `"ocf:`), "nothing follows the service IP")
			assert.Contains(t, cfg, `target-as = "BindsTo"`)
			assert.Contains(t, cfg, `on-drbd-demote-failure = "reboot-immediate"`)
			// force_unmount=true resolves a no-longer-mounted directory to the
			// root filesystem and kills every process on the node.
			assert.Contains(t, cfg, "fstype=ext4 run_fsck=no force_unmount=safe")
		})
	}
}

func TestUnit(t *testing.T) {
	tests := []struct {
		engine   Engine
		flavor   string
		exec     string
		user     string
		probeHas string
	}{
		{Postgres, "", "ExecStart=/usr/lib/postgresql/16/bin/postgres -D /var/lib/haify-app/orders/data", "postgres",
			"/usr/lib/postgresql/16/bin/pg_isready -q -h /run/haify-app-orders -p 5432"},
		{MySQL, "mariadb", "ExecStart=/usr/sbin/mariadbd --defaults-file=/var/lib/haify-app/orders/conf/my.cnf", "mysql",
			"/usr/bin/mariadb-admin --defaults-extra-file=/var/lib/haify-app/orders/haify/client.cnf ping"},
		{Redis, "", "ExecStart=/usr/bin/redis-server /var/lib/haify-app/orders/conf/redis.conf", "redis",
			`REDISCLI_AUTH="$$(cat /var/lib/haify-app/orders/haify/password)" /usr/bin/redis-cli -s /run/haify-app-orders/redis.sock ping`},
		{RustFS, "", "ExecStart=/usr/local/bin/rustfs server --address :9000 " +
			"--access-key-file /var/lib/haify-app/orders/conf/access_key --secret-key-file /var/lib/haify-app/orders/conf/secret_key " +
			"--console-enable --console-address :9001 /var/lib/haify-app/orders/data", "rustfs",
			"/usr/bin/curl -fsS -o /dev/null --max-time 5 http://127.0.0.1:9000/health/ready"},
	}
	for _, tc := range tests {
		t.Run(string(tc.engine), func(t *testing.T) {
			s := specFor(t, tc.engine)
			unit := Unit(s, binariesFor(tc.engine, tc.flavor))
			assert.Contains(t, unit, tc.exec+"\n")
			assert.Contains(t, unit, "User="+tc.user+"\nGroup="+tc.user+"\n")
			assert.Contains(t, unit, "RuntimeDirectory=haify-app-orders\n")
			assert.Contains(t, unit, "ExecStartPre=/bin/sh -c 'mountpoint -q /var/lib/haify-app/orders'")
			assert.Contains(t, unit, "ExecStartPost=+/bin/sh -c '")
			assert.Contains(t, unit, tc.probeHas, "the unit waits for the database to answer")
			assert.NotContains(t, unit, "[Install]", "only the promoter may start the unit")
			assert.NotContains(t, unit, "Restart=", "a dead database fails over instead of restarting in place")
			// In an Exec line, systemd expands $X and %x itself: every shell
			// dollar in the wait loop must be doubled.
			post := unit[strings.Index(unit, "ExecStartPost="):]
			post = post[:strings.Index(post, "\n")]
			assert.NotRegexp(t, regexp.MustCompile(`[^$]\$[^$]`), strings.ReplaceAll(post, "$$", ""), post)
		})
	}
}

func TestHealthCommand(t *testing.T) {
	assert.Contains(t, HealthCommand(specFor(t, Postgres), binariesFor(Postgres, "")), "pg_isready")
	assert.Contains(t, HealthCommand(specFor(t, MySQL), binariesFor(MySQL, "mysql")), "mysqladmin --defaults-extra-file=")
	assert.Contains(t, HealthCommand(specFor(t, MySQL), binariesFor(MySQL, "mysql")), " ping")
	redis := HealthCommand(specFor(t, Redis), binariesFor(Redis, ""))
	assert.Contains(t, redis, "redis-cli -s /run/haify-app-orders/redis.sock ping")
	assert.Contains(t, redis, "grep -qx PONG")
	assert.NotContains(t, redis, " -a ", "a password on the command line is visible in ps")
	assert.Equal(t, "/usr/bin/curl -fsS -o /dev/null --max-time 5 http://127.0.0.1:9000/health/ready",
		HealthCommand(specFor(t, RustFS), binariesFor(RustFS, "")))
}

// Every generated script must at least parse; a syntax error would only show
// on a node, half-way through an initialization.
func TestScriptsParse(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	type variant struct {
		engine Engine
		flavor string
		vector bool
	}
	for _, v := range []variant{{Postgres, "", false}, {Postgres, "", true}, {MySQL, "mariadb", false},
		{MySQL, "mysql", false}, {Redis, "", false}, {RustFS, "", false}} {
		s := specFor(t, v.engine)
		s.Vector = v.vector
		b := binariesFor(v.engine, v.flavor)
		scripts := map[string]string{
			"init":   InitScript(s, b, DataDevice("res1", 0), "/root/.haify-app/orders.pw"),
			"freeze": FreezeForSnapshot(s, b),
			"thaw":   ThawAfterSnapshot(s, b),
			"probe":  ProbeScript(s),
			"status": StatusScript(s, b),
		}
		for kind, script := range scripts {
			t.Run(string(v.engine)+"/"+v.flavor+"/"+kind, func(t *testing.T) {
				assert.NotRegexp(t, regexp.MustCompile(`@[A-Z_]+@`), script, "a placeholder was left unfilled")
				cmd := exec.Command(bash, "-n")
				cmd.Stdin = strings.NewReader(script)
				out, err := cmd.CombinedOutput()
				require.NoError(t, err, "%s\n%s", out, script)
			})
		}
	}
}

func TestInitScript(t *testing.T) {
	const staged = "/home/haify/.haify-app/orders.pw"
	dev := DataDevice("res1", 0)

	pg := specFor(t, Postgres)
	pg.Vector = true
	script := InitScript(pg, binariesFor(Postgres, ""), dev, staged)
	assert.Contains(t, script, "dev="+dev+"\n")
	assert.Contains(t, script, "staged="+staged+"\n")
	assert.Contains(t, script, `rm -f "$staged"`, "the staged password is always removed")
	assert.Contains(t, script, "blkid -p -o value -s TYPE")
	assert.Contains(t, script, `mkfs.ext4 -q "$real"`)
	assert.Contains(t, script, "state=reused", "a volume already holding the app is kept")
	assert.Contains(t, script, "/usr/lib/postgresql/16/bin/initdb -D \"$M/data\" -U postgres --pwfile=")
	assert.Contains(t, script, "CREATE EXTENSION IF NOT EXISTS vector")
	assert.Contains(t, script, "unix_socket_directories = '/run/haify-app-orders'")
	assert.Contains(t, script, `-o "-c listen_addresses=''"`, "the smoke test listens on no network")
	assert.Less(t, strings.Index(script, "mount -t ext4"), strings.Index(script, "initdb"))

	noVector := InitScript(specFor(t, Postgres), binariesFor(Postgres, ""), dev, staged)
	assert.NotContains(t, noVector, "CREATE EXTENSION")

	maria := InitScript(specFor(t, MySQL), binariesFor(MySQL, "mariadb"), dev, staged)
	assert.Contains(t, maria, "/usr/bin/mariadb-install-db --defaults-file=")
	assert.Contains(t, maria, "ALTER USER 'root'@'localhost' IDENTIFIED BY '$pw'")
	assert.Contains(t, maria, `"$M/haify/client.cnf"`)
	mysql := InitScript(specFor(t, MySQL), binariesFor(MySQL, "mysql"), dev, staged)
	assert.Contains(t, mysql, "/usr/sbin/mysqld --defaults-file=\"$M/conf/my.cnf\" --initialize-insecure")
	assert.NotContains(t, mysql, "install-db")

	redis := InitScript(specFor(t, Redis), binariesFor(Redis, ""), dev, staged)
	assert.Contains(t, redis, "requirepass $pw")
	assert.Contains(t, redis, "appendonly yes")
	assert.Contains(t, redis, "setpriv --reuid=\"$u\"")

	rustfs := InitScript(specFor(t, RustFS), binariesFor(RustFS, ""), dev, staged)
	assert.Contains(t, rustfs, `printf '%s' "admin" > "$M/conf/access_key"`)
	assert.Contains(t, rustfs, `printf '%s' "$pw" > "$M/conf/secret_key"`)
	assert.Contains(t, rustfs, "--address 127.0.0.1:9000", "the smoke test listens on loopback only")
	assert.NotContains(t, rustfs, "--secret-key ", "the secret key never goes on a command line")
}

func TestFreezeArmsTheWatchdogBeforeFreezing(t *testing.T) {
	for _, tc := range []struct {
		engine Engine
		flavor string
		step   string
	}{
		{Postgres, "", "-Atqc CHECKPOINT"},
		{MySQL, "mysql", "FLUSH TABLES WITH READ LOCK; SELECT SLEEP(60) AS haify_app_freeze"},
		{Redis, "", "BGSAVE SCHEDULE"},
	} {
		t.Run(string(tc.engine), func(t *testing.T) {
			s := specFor(t, tc.engine)
			b := binariesFor(tc.engine, tc.flavor)
			script := FreezeForSnapshot(s, b)
			arm := strings.Index(script, "systemd-run --unit=haify-app-thaw-orders $(systemd-run --help 2>/dev/null | grep -q -e --collect && echo --collect) --quiet --on-active=60 /bin/bash /run/haify-app-orders-thaw.sh")
			step := strings.Index(script, tc.step)
			freeze := strings.Index(script, "fsfreeze -f /var/lib/haify-app/orders")
			require.True(t, arm >= 0 && step >= 0 && freeze >= 0, script)
			assert.Less(t, arm, step, "the watchdog is armed before the database is touched")
			assert.Less(t, step, freeze)
			assert.Contains(t, script, "fsfreeze -u /var/lib/haify-app/orders", "the thaw steps are written for the watchdog")

			thaw := ThawAfterSnapshot(s, b)
			assert.Contains(t, thaw, "systemctl stop haify-app-thaw-orders.timer")
			assert.Contains(t, thaw, "fsfreeze -u /var/lib/haify-app/orders")
			if tc.engine == MySQL {
				assert.Contains(t, thaw, `KILL $id`, "the lock session is killed, not left sleeping")
				assert.Contains(t, thaw, "systemctl stop haify-app-lock-orders.service")
			}
		})
	}
}

func TestStatusScript(t *testing.T) {
	script := StatusScript(specFor(t, Postgres), binariesFor(Postgres, ""))
	assert.Contains(t, script, "systemctl is-active haify-app-orders.service")
	assert.Contains(t, script, "health=ok")
}

func TestConnectionHint(t *testing.T) {
	assert.Equal(t, "postgresql://postgres@10.0.0.50:5432/postgres", ConnectionHint(specFor(t, Postgres)))
	assert.Contains(t, ConnectionHint(specFor(t, MySQL)), "-h 10.0.0.50 -P 3306 -u root")
	assert.Contains(t, ConnectionHint(specFor(t, Redis)), "-h 10.0.0.50 -p 6379")
	assert.Equal(t, "http://10.0.0.50:9000 (path-style S3; console http://10.0.0.50:9001/rustfs/console/)",
		ConnectionHint(specFor(t, RustFS)))
}
