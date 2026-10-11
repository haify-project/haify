package controller

import (
	"encoding/base64"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// openclawReactorJSON is drbd-reactorctl status --json for the openclaw chain
// as node-e printed it while running the chain.
const openclawReactorJSON = `{"promoter":[{"drbd_resource":"openclaw","path":"/etc/drbd-reactor.d/haify-ha-openclaw.toml","enabled":true,"primary_on":"sds-e","target":{"name":"drbd-services@openclaw.target","status":"@S@","freezer":"running"},"dependencies":[{"name":"drbd-promote@openclaw.service","status":"@S@","freezer":"running"},{"name":"var-lib-openclaw.mount","status":"@S@","freezer":"running"},{"name":"service-ip@192.168.123.252-24.service","status":"@S@","freezer":"running"},{"name":"cortexdb-grpc.service","status":"@S@","freezer":"running"},{"name":"openclaw.service","status":"@S@","freezer":"running"}],"status":"@S@"}],"prometheus":[],"debugger":[],"umh":[],"agentx":[]}`

func reactorLine(json, status string) string {
	j := strings.ReplaceAll(json, "@S@", status)
	return haProbeMarker + " reactor " + base64.StdEncoding.EncodeToString([]byte(j)) + "\n"
}

// showBlock is one unit's systemctl show block as the probe asks for it.
func showBlock(id, desc, load, fileState, active, sub, since, frag string, pid int) string {
	return strings.Join([]string{
		"MainPID=" + strconv.Itoa(pid), "NRestarts=0", "Id=" + id, "Names=" + id, "Description=" + desc,
		"LoadState=" + load, "ActiveState=" + active, "SubState=" + sub, "FragmentPath=" + frag,
		"UnitFileState=" + fileState, "ActiveEnterTimestamp=" + since,
	}, "\n") + "\n"
}

const since = "Thu 2026-10-08 08:06:41 UTC"

// openclawActiveProbe is node-e's probe while it runs the openclaw chain,
// with a backup agent and a login shell using the mount besides.
func openclawActiveProbe() string {
	m := haProbeMarker
	var b strings.Builder
	b.WriteString("sudo: unable to resolve host sds-e\n")
	b.WriteString(reactorLine(openclawReactorJSON, "active"))
	b.WriteString(m + " reactord active\n")
	b.WriteString(m + " drbd-begin\nopenclaw role:Primary\n  disk:UpToDate open:yes\n  sds-b role:Secondary\n    peer-disk:UpToDate\n\n" + m + " drbd-end\n")
	b.WriteString(m + " units-begin\n")
	b.WriteString(showBlock("cortexdb-grpc.service", "drbd-reactor controlled cortexdb-grpc", "loaded", "disabled", "active", "running", since, "/etc/systemd/system/cortexdb-grpc.service", 2965835))
	b.WriteString("\n" + showBlock("drbd-promote@openclaw.service", "Promotion of DRBD resource openclaw", "loaded", "static", "active", "exited", since, "/usr/lib/systemd/system/drbd-promote@.service", 0))
	b.WriteString("\n" + showBlock("drbd-services@openclaw.target", "Services for DRBD resource openclaw", "loaded", "static", "active", "active", since, "/usr/lib/systemd/system/drbd-services@.target", 0))
	b.WriteString("\n" + showBlock("openclaw.service", "drbd-reactor controlled openclaw", "loaded", "disabled", "active", "running", since, "/etc/systemd/system/openclaw.service", 2965837))
	b.WriteString("\n" + showBlock("service-ip@192.168.123.252-24.service", "drbd-reactor controlled service-ip@192.168.123.252-24", "loaded", "disabled", "active", "exited", since, "/etc/systemd/system/service-ip@.service", 0))
	b.WriteString("\n" + showBlock("var-lib-openclaw.mount", "drbd-reactor controlled var-lib-openclaw", "loaded", "disabled", "active", "mounted", since, "/etc/systemd/system/var-lib-openclaw.mount", 0))
	b.WriteString(m + " units-end\n")
	b.WriteString(m + " frag /etc/systemd/system/cortexdb-grpc.service\tCortexDB shared brain (gRPC)\n")
	b.WriteString(m + " frag /etc/systemd/system/openclaw.service\tOpenClaw Gateway\n")
	b.WriteString(m + " frag /etc/systemd/system/service-ip@.service\tHigh Availability Service IP %i\n")
	b.WriteString(m + " mount /var/lib/haify\n" + m + " mount /var/lib/openclaw\n")
	b.WriteString(m + " procs       1 /system.slice/cortexdb-grpc.service\t/var/lib/openclaw\n")
	b.WriteString(m + " procs      25 /system.slice/openclaw.service\t/var/lib/openclaw\n")
	b.WriteString(m + " procs       2 /system.slice/backup-agent.service\t/var/lib/openclaw\t/var/lib/haify\n")
	b.WriteString(m + " procs       1 /system.slice/backup-agent.service\t/var/lib/haify\n")
	b.WriteString(m + " procs       3 /user.slice/user-0.slice/session-45301.scope\t/var/lib/openclaw\n")
	b.WriteString(m + " procs       1 /user.slice/user-0.slice/session-45302.scope\t/var/lib/openclaw\n")
	b.WriteString(m + " procs       4 /system.slice/haify-controller.service\t/var/lib/haify\n")
	b.WriteString(m + " outside-begin\nId=backup-agent.service\nNames=backup-agent.service\nDescription=Nightly backup agent\n" + m + " outside-end\n")
	return b.String()
}

func TestParseHaNodeProbeActive(t *testing.T) {
	f, err := parseHaNodeProbe(openclawActiveProbe())
	require.NoError(t, err)
	require.NotNil(t, f.Reactor)
	require.Len(t, f.Reactor.Promoter, 1)
	assert.Equal(t, "active", f.Reactor.Promoter[0].Status)
	assert.Equal(t, "active", f.ReactorState)
	assert.Equal(t, haDrbdLocal{Role: "Primary", Disk: "UpToDate"}, f.Drbd["openclaw"])

	u := f.unit("openclaw.service")
	require.NotNil(t, u)
	assert.Equal(t, "OpenClaw Gateway", u.OwnDesc)
	assert.Equal(t, "running", u.SubState)
	assert.Equal(t, 2965837, u.MainPID)
	assert.Equal(t, "2026-10-08T08:06:41Z", u.ActiveEnter)
	assert.Nil(t, u.Env)

	assert.Equal(t, []string{"/var/lib/haify", "/var/lib/openclaw"}, f.Mounts)
	require.Len(t, f.Procs, 7)
	assert.Equal(t, haProcGroup{Count: 2, Cgroup: "/system.slice/backup-agent.service",
		Mounts: []string{"/var/lib/openclaw", "/var/lib/haify"}}, f.Procs[2])
	assert.Equal(t, "Nightly backup agent", f.Outside["backup-agent.service"])
}

func TestParseHaNodeProbeEscapedNamesAndOCF(t *testing.T) {
	m := haProbeMarker
	out := m + " reactor \n" + m + " reactord failed\n" +
		m + " units-begin\nId=drbd-promote@haify\\x2dmeta.service\nNames=\"drbd-promote@haify\\\\x2dmeta.service\"\nLoadState=loaded\n\n" +
		"Id=ocf.rs@fs_export_data.service\nNames=ocf.rs@fs_export_data.service\nLoadState=loaded\nFragmentPath=/run/systemd/system/ocf.rs@.service\n" +
		m + " units-end\n" +
		m + " ocf-begin\nId=ocf.rs@fs_export_data.service\nEnvironment=AGENT=/usr/lib/ocf/resource.d/heartbeat/Filesystem OCF_RESKEY_directory=/srv/data \n" + m + " ocf-end\n" +
		m + " agent missing /usr/lib/ocf/resource.d/heartbeat/Filesystem\n"
	f, err := parseHaNodeProbe(out)
	require.NoError(t, err)
	assert.Nil(t, f.Reactor, "an empty drbd-reactorctl answer is no status")
	assert.Equal(t, "failed", f.ReactorState)
	assert.NotNil(t, f.unit(`drbd-promote@haify\x2dmeta.service`), "found by its unquoted name")
	u := f.unit("ocf.rs@fs_export_data.service")
	require.NotNil(t, u)
	assert.Equal(t, "/srv/data", u.Env["directory"])
	assert.False(t, f.Agents["/usr/lib/ocf/resource.d/heartbeat/Filesystem"])
	kind, label := classifyHaUnit(u.ID, u)
	assert.Equal(t, haKindMount, kind)
	assert.Equal(t, "/srv/data", label)
}

func TestParseHaNodeProbeBadReactor(t *testing.T) {
	_, err := parseHaNodeProbe(haProbeMarker + " reactor !!!\n")
	assert.Error(t, err)
	_, err = parseHaNodeProbe(haProbeMarker + " reactor " + base64.StdEncoding.EncodeToString([]byte("{not json")) + "\n")
	assert.Error(t, err)
	f, err := parseHaNodeProbe("")
	require.NoError(t, err)
	assert.Nil(t, f.Reactor)
}

func TestParseDrbdLocalStatus(t *testing.T) {
	lines := strings.Split(`haify-meta role:Secondary
  disk:UpToDate open:no
  lima-sds-a role:Secondary
    peer-disk:UpToDate
  sds-e role:Primary
    peer-disk:Outdated

data role:Secondary
  volume:0 disk:UpToDate
  volume:1 disk:Outdated
  sds-b connection:Connecting

client role:Secondary
  disk:Diskless
  sds-b role:Primary
    peer-disk:UpToDate`, "\n")
	got := parseDrbdLocalStatus(lines)
	assert.Equal(t, haDrbdLocal{Role: "Secondary", Disk: "UpToDate"}, got["haify-meta"], "peer disks are not the local disk")
	assert.Equal(t, haDrbdLocal{Role: "Secondary", Disk: "Outdated"}, got["data"], "the worst volume")
	assert.Equal(t, haDrbdLocal{Role: "Secondary", Disk: "Diskless"}, got["client"])
}

func TestParseProcGroup(t *testing.T) {
	g, ok := parseProcGroup("     12 /system.slice/x.service\t/a\t/b")
	require.True(t, ok)
	assert.Equal(t, haProcGroup{Count: 12, Cgroup: "/system.slice/x.service", Mounts: []string{"/a", "/b"}}, g)
	for _, bad := range []string{"", "x /s.service\t/a", "3 /s.service", "3 \t/a"} {
		_, ok := parseProcGroup(bad)
		assert.False(t, ok, bad)
	}
}

func TestSystemdTimestamp(t *testing.T) {
	assert.Equal(t, "2026-10-08T08:06:41Z", systemdTimestamp("Thu 2026-10-08 08:06:41 UTC"))
	assert.Equal(t, "", systemdTimestamp(""))
	assert.Equal(t, "", systemdTimestamp("n/a"))
	assert.Equal(t, "", systemdTimestamp("yesterday"))
}

func TestHaProbeScriptIsReadOnly(t *testing.T) {
	s := haProbeScript
	for _, forbidden := range []string{"fuser", "lsof", "kill", "systemctl start", "systemctl stop",
		"systemctl restart", "systemctl reload", "drbdadm", "rm "} {
		assert.NotContains(t, s, forbidden)
	}
	assert.Contains(t, s, "M="+haProbeMarker)
	assert.Contains(t, s, "OCF_RESKEY_("+haProbeOCFParams+")")
	assert.NotContains(t, s, "@@MARK@@")
	assert.NotContains(t, s, "@@PARAMS@@")
	assert.True(t, strings.HasPrefix(haProbeCommand(), "echo "))
	assert.True(t, strings.HasSuffix(haProbeCommand(), "| base64 -d | sudo /bin/sh"))
}
