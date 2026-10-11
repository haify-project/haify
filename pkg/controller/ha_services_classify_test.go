package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSystemdUnescape(t *testing.T) {
	cases := map[string]string{
		`haify\x2dmeta`:      "haify-meta",
		`var-lib-haify`:      "var/lib/haify",
		`192.168.123.252-24`: "192.168.123.252/24",
		`a\x20b`:             "a b",
		`bad\xZZ`:            `bad\xZZ`,
		`trailing\x2`:        `trailing\x2`,
		``:                   ``,
	}
	for in, want := range cases {
		assert.Equal(t, want, systemdUnescape(in), in)
	}
}

func TestMountUnitPath(t *testing.T) {
	assert.Equal(t, "/var/lib/openclaw", mountUnitPath("var-lib-openclaw.mount"))
	assert.Equal(t, "/var/lib/haify", mountUnitPath("var-lib-haify.mount"))
	assert.Equal(t, "/srv/my-data", mountUnitPath(`srv-my\x2ddata.mount`))
	assert.Equal(t, "/", mountUnitPath("-.mount"))
}

func TestClassifyHaUnitRealChains(t *testing.T) {
	desc := func(own string) *haUnitFacts { return &haUnitFacts{OwnDesc: own} }
	cases := []struct {
		unit        string
		facts       *haUnitFacts
		kind, label string
	}{
		// haify-meta, the Self-HA chain.
		{`drbd-promote@haify\x2dmeta.service`, nil, haKindPromote, "promote haify-meta"},
		{"var-lib-haify.mount", nil, haKindMount, "/var/lib/haify"},
		{"service-ip@192.168.123.251-24.service", nil, haKindServiceIP, "192.168.123.251/24"},
		{"haify-controller.service", desc("drbd-reactor controlled haify-controller"), haKindHaify, "haify-controller"},
		{"haify-ai.service", desc("Haify AI Copilot"), haKindHaify, "Haify AI Copilot"},
		{"haify-mcp-http.service", nil, haKindHaify, "haify-mcp-http"},
		{`drbd-services@haify\x2dmeta.target`, nil, haKindTarget, "services of haify-meta"},
		// openclaw, an `ha create` chain of plain services.
		{"drbd-promote@openclaw.service", nil, haKindPromote, "promote openclaw"},
		{"var-lib-openclaw.mount", nil, haKindMount, "/var/lib/openclaw"},
		{"service-ip@192.168.123.252-24.service", nil, haKindServiceIP, "192.168.123.252/24"},
		{"cortexdb-grpc.service", &haUnitFacts{Description: "drbd-reactor controlled cortexdb-grpc",
			OwnDesc: "CortexDB shared brain (gRPC)"}, haKindService, "CortexDB shared brain (gRPC)"},
		{"openclaw.service", &haUnitFacts{Description: "drbd-reactor controlled openclaw"}, haKindService, "openclaw"},
		{"mystery.service", nil, haKindService, "mystery"},
		// Gateways, apps, databases.
		{"haify-smbd@files.service", nil, haKindSMB, "Samba for files"},
		{"haify-app-orders.service", desc("Haify app orders (postgres)"), haKindDatabase, "postgres app orders"},
		{"haify-app-blobs.service", desc("Haify app blobs (rustfs)"), haKindService, "rustfs app blobs"},
		{"postgresql@16-main.service", &haUnitFacts{ID: "postgresql@16-main.service", OwnDesc: "PostgreSQL Cluster %i"},
			haKindDatabase, "PostgreSQL Cluster 16-main"},
		{"mariadb.service", desc("MariaDB 10.11 database server"), haKindDatabase, "MariaDB 10.11 database server"},
		{"redis-server.service", nil, haKindDatabase, "redis-server"},
		{"nfs-server.service", nil, haKindNFS, "nfs-server"},
		{"ocf.rs@fs_x.service", nil, haKindService, "OCF fs_x"},
	}
	for _, c := range cases {
		kind, label := classifyHaUnit(c.unit, c.facts)
		assert.Equal(t, c.kind, kind, c.unit)
		assert.Equal(t, c.label, label, c.unit)
	}
}

func TestClassifyHaUnitTemplateDescription(t *testing.T) {
	f := &haUnitFacts{ID: "service-ip@192.168.123.252-24.service",
		Description: "drbd-reactor controlled service-ip@192.168.123.252-24",
		OwnDesc:     "High Availability Service IP %I"}
	assert.Equal(t, "High Availability Service IP 192.168.123.252/24", unitOwnDescription(f))
	f.OwnDesc = "Odd %n"
	assert.Equal(t, "service-ip@192.168.123.252-24", unitOwnDescription(f))
	assert.Equal(t, "", unitOwnDescription(nil))
}

func TestClassifyOCFUnits(t *testing.T) {
	const root = "/usr/lib/ocf/resource.d/heartbeat/"
	ocf := func(agent string, kv ...string) *haUnitFacts {
		env := map[string]string{"AGENT": root + agent}
		for i := 0; i+1 < len(kv); i += 2 {
			env[kv[i]] = kv[i+1]
		}
		return &haUnitFacts{Env: env}
	}
	cases := []struct {
		facts       *haUnitFacts
		kind, label string
	}{
		{ocf("Filesystem", "directory", "/var/lib/haify-gateway/data"), haKindMount, "/var/lib/haify-gateway/data"},
		{ocf("IPaddr2", "ip", "10.0.0.9", "cidr_netmask", "24"), haKindServiceIP, "10.0.0.9/24"},
		{ocf("IPaddr2", "ip", "10.0.0.9"), haKindServiceIP, "10.0.0.9"},
		{ocf("nfsserver"), haKindNFS, "NFS server"},
		{ocf("exportfs", "directory", "/data", "clientspec", "10.0.0.0/24"), haKindNFS, "export /data to 10.0.0.0/24"},
		{ocf("portblock", "action", "unblock", "protocol", "tcp", "portno", "3260"), haKindISCSI, "unblock tcp/3260"},
		{ocf("portblock", "action", "block", "protocol", "tcp", "portno", "2049"), haKindNFS, "block tcp/2049"},
		{ocf("iSCSITarget", "iqn", "iqn.2024-01.com.example:blk"), haKindISCSI, "target iqn.2024-01.com.example:blk"},
		{ocf("iSCSILogicalUnit", "lun", "1", "path", "/dev/drbd1001"), haKindISCSI, "LUN 1 /dev/drbd1001"},
		{ocf("nvmet-subsystem", "nqn", "nqn.2024-01.com.example:nvm"), haKindNVMeoF, "subsystem nqn.2024-01.com.example:nvm"},
		{ocf("nvmet-namespace", "namespace_id", "1", "backing_path", "/dev/drbd1002"), haKindNVMeoF, "namespace 1 /dev/drbd1002"},
		{ocf("nvmet-port", "addr", "10.0.0.9", "type", "tcp"), haKindNVMeoF, "port 10.0.0.9 (tcp)"},
		{ocf("VirtualDomain", "config", "/etc/libvirt/qemu/web01.xml"), haKindVM, "web01"},
		{ocf("pgsql"), haKindDatabase, "pgsql"},
		{&haUnitFacts{Env: map[string]string{"AGENT": "/usr/lib/ocf/resource.d/linbit/drbd"}}, haKindService, "linbit:drbd"},
	}
	for _, c := range cases {
		kind, label := classifyHaUnit("ocf.rs@x_res.service", c.facts)
		assert.Equal(t, c.kind, kind, c.facts.Env["AGENT"])
		assert.Equal(t, c.label, label, c.facts.Env["AGENT"])
	}
}

func TestParseOCFEnv(t *testing.T) {
	env := parseOCFEnv(`AGENT=/usr/lib/ocf/resource.d/heartbeat/Filesystem "OCF_RESKEY_directory=/srv/a b" OCF_RESKEY_device=/dev/drbd1 OCF_ROOT=/usr/lib/ocf OCF_RESKEY_clientspec=10.0.0.0\x2f24 `)
	assert.Equal(t, map[string]string{
		"AGENT":      "/usr/lib/ocf/resource.d/heartbeat/Filesystem",
		"directory":  "/srv/a b",
		"device":     "/dev/drbd1",
		"clientspec": "10.0.0.0/24",
	}, env)
	assert.Empty(t, parseOCFEnv(""))
}

func TestSystemdWords(t *testing.T) {
	assert.Equal(t, []string{`drbd-promote@haify\x2dmeta.service`}, systemdWords(`"drbd-promote@haify\\x2dmeta.service"`))
	assert.Equal(t, []string{"mysql.service", "mariadb.service"}, systemdWords("mysql.service mariadb.service"))
	assert.Equal(t, []string{"A=1", "B=x y"}, systemdWords(`A=1 "B=x y"`))
	assert.Empty(t, systemdWords("  "))
}

func TestCgroupUnit(t *testing.T) {
	cases := map[string]string{
		"/system.slice/openclaw.service":                                    "openclaw.service",
		"/system.slice/docker.service/sub/cgroup":                           "docker.service",
		"/user.slice/user-0.slice/session-45292.scope":                      "session-45292.scope",
		"/user.slice/user-1000.slice/user@1000.service/app.slice/x.service": "user@1000.service",
		"/kubepods.slice/kubepods-pod1.slice/cri-containerd-ab.scope":       "cri-containerd-ab.scope",
		"/init.scope": "init.scope",
		"/":           "",
		"":            "",
	}
	for in, want := range cases {
		assert.Equal(t, want, cgroupUnit(in), in)
	}
	assert.True(t, isSessionUnit("session-3.scope"))
	assert.True(t, isSessionUnit("user@0.service"))
	assert.False(t, isSessionUnit("cron.service"))
	assert.False(t, isSessionUnit("docker-1.scope"))
}
