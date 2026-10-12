package gateway

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	v1 "github.com/haify-project/haify/api/proto/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestCreateSMBGateway(t *testing.T) {
	dep := &MockDeploymentClient{HostOutputs: map[string]string{
		"node1": "root:x:0:0::/root:/bin/sh\nalice:x:61000:61000::/:/bin/false\n---\nroot:x:0:\n",
		"node2": "root:x:0:0::/root:/bin/sh\n---\nroot:x:0:\n",
	}}
	m := New(diskfulResources("files"), dep, zap.NewNop(), []string{"node1", "node2", "tb"})
	resp, err := NewSMBManager(m).CreateSMBGateway(context.Background(), &v1.CreateSMBGatewayRequest{
		Resource: "files", ServiceIp: "10.0.0.50/24", Workgroup: "office",
	})
	require.NoError(t, err)
	require.True(t, resp.Success, resp.Message)
	assert.Contains(t, resp.Message, `\\10.0.0.50\files`)

	cfg := dep.Configs["/etc/drbd-reactor.d/haify-smb-files.toml"]
	require.NotEmpty(t, cfg)
	assert.Equal(t, []string{"node1", "node2"}, dep.ConfigHosts["/etc/drbd-reactor.d/haify-smb-files.toml"],
		"the promoter goes to the diskful replicas only")
	ip := strings.Index(cfg, "IPaddr2 service_ip ip=10.0.0.50 cidr_netmask=24")
	unit := strings.Index(cfg, `"haify-smbd@files.service"`)
	share := strings.Index(cfg, "fs_share")
	require.True(t, ip > 0 && unit > 0 && share > 0, cfg)
	assert.Less(t, share, ip)
	assert.Less(t, ip, unit, "smbd binds the service IP, so the IP must be up first")

	// The unit and helper are installed on the replicas before the seed.
	assert.Equal(t, []string{"node1", "node2"}, findScript(t, dep, smbUnitPath).hosts)
	seed := findScript(t, dep, "shares.conf")
	assert.Equal(t, []string{"node1"}, seed.hosts)
	// alice holds 61000 on node1, so haify-smb gets the next free id.
	assert.Contains(t, seed.script, "haify-smb:61001")
	assert.Contains(t, seed.script, "mount /dev/drbd")
}

func TestCreateSMBGatewayRejectsBadInput(t *testing.T) {
	m := New(diskfulResources("files"), &MockDeploymentClient{}, zap.NewNop(), []string{"node1"})
	for _, req := range []*v1.CreateSMBGatewayRequest{
		{Resource: "files", ServiceIp: "10.0.0.50"},
		{Resource: "files", ServiceIp: "10.0.0.50/24", Workgroup: "has space"},
		{Resource: "files", ServiceIp: "10.0.0.50/24", ShareName: "IPC$"},
		{Resource: "files", ServiceIp: "10.0.0.50/24", ValidUsers: []string{"Root"}},
	} {
		_, err := NewSMBManager(m).CreateSMBGateway(context.Background(), req)
		assert.Error(t, err, "%+v", req)
	}
}

func TestSMBGlobalConfig(t *testing.T) {
	ip, err := parseServiceIP("10.0.0.50/24")
	require.NoError(t, err)
	conf := smbGlobalConfig("files", "OFFICE", ip)
	for _, want := range []string{
		"workgroup = OFFICE",
		"interfaces = 10.0.0.50/24",
		"bind interfaces only = yes",
		"passdb backend = tdbsam:/var/lib/haify-gateway/files/smb/private/passdb.tdb",
		"lock directory = /var/lib/haify-gateway/files/smb/lock",
		"include = /var/lib/haify-gateway/files/smb/shares.conf",
	} {
		assert.Contains(t, conf, want)
	}
	assert.Equal(t, "A-VERY-LONG-RES", smbNetbiosName("a_very_long_resource_name"))
}

func TestSMBShareValidateAndRoundTrip(t *testing.T) {
	bad := []SMBShare{
		{Name: "global"}, {Name: "a b"}, {Name: "ok", Path: "../etc"},
		{Name: "ok", Path: "/abs"}, {Name: "ok", ValidUsers: []string{"haify-smb"}},
	}
	for _, sh := range bad {
		assert.Error(t, sh.validate(), "%+v", sh)
	}
	shares := []SMBShare{
		{Name: "all"},
		{Name: "projects", Path: "projects/a", ReadOnly: true, ValidUsers: []string{"alice", "bob"}},
	}
	for i := range shares {
		require.NoError(t, shares[i].validate())
	}
	got := parseSMBShares("files", renderSMBShares("files", shares))
	assert.Equal(t, shares, got)
	assert.Contains(t, shares[1].section("files"), "path = /srv/gateway-exports/files/projects/a")
	assert.Contains(t, shares[1].section("files"), "force user = haify-smb")
}

func TestPickUID(t *testing.T) {
	// An account that exists keeps its uid when the nodes agree.
	uid, err := pickUID("alice", map[string]string{
		"n1": "alice:x:61005:61005::/:/bin/false\n---\n",
		"n2": "---\n",
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, 61005, uid)

	// Disagreeing nodes are refused.
	_, err = pickUID("alice", map[string]string{
		"n1": "alice:x:61005:61005::/:/bin/false\n---\n",
		"n2": "alice:x:61006:61006::/:/bin/false\n---\n",
	}, nil)
	assert.Error(t, err)

	// Its uid held by someone else on another node is refused.
	_, err = pickUID("alice", map[string]string{
		"n1": "alice:x:61005:61005::/:/bin/false\n---\n",
		"n2": "carol:x:61005:61005::/:/bin/false\n---\n",
	}, nil)
	assert.Error(t, err)

	// A new account skips uids and gids in use anywhere, and recorded ones.
	uid, err = pickUID("bob", map[string]string{
		"n1": "x:x:61000:1::/:/bin/false\n---\n",
		"n2": "---\ng:x:61001:\n",
	}, map[string]int{"haify-smb": 61002})
	require.NoError(t, err)
	assert.Equal(t, 61003, uid)
}

func TestGatewayTypesCoverSMB(t *testing.T) {
	assert.Contains(t, promoterConfigPaths("r", ".disabled"), "/etc/drbd-reactor.d/haify-smb-r.toml.disabled")
	gw, live := parseGatewayConfigName("haify-smb-r.toml")
	require.NotNil(t, gw)
	assert.True(t, live)
	assert.Equal(t, "smb", gw.Type)
}

// The account helper runs as a shell script on the nodes; it must at least
// parse, and must do nothing for a gateway without a users file.
func TestSMBUsersHelperIsValidShell(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	out, err := exec.Command("sh", "-n", "-c", smbUsersHelperContent).CombinedOutput()
	require.NoError(t, err, string(out))
	out, err = exec.Command("sh", "-c", smbUsersHelperContent, "sh", "/nonexistent/users").CombinedOutput()
	require.NoError(t, err, string(out))
	assert.True(t, strings.HasPrefix(smbPrereqs().checks[0], "! systemctl is-active -q smbd"))
}

func TestSMBSharesNeedARunningGateway(t *testing.T) {
	dep := &MockDeploymentClient{}
	m := New(diskfulResources("files"), dep, zap.NewNop(), []string{"node1", "node2"})
	err := NewSMBManager(m).AddSMBShare(context.Background(), "files", SMBShare{Name: "x"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not running")
}

func TestAddSMBShareOnServingNode(t *testing.T) {
	existing := renderSMBShares("files", []SMBShare{{Name: "files"}})
	dep := &MockDeploymentClient{
		TargetStates: map[string]string{"node1": "inactive", "node2": "active"},
		HostOutputs:  map[string]string{"node2": existing},
	}
	m := New(diskfulResources("files"), dep, zap.NewNop(), []string{"node1", "node2"})
	smb := NewSMBManager(m)
	require.Error(t, smb.AddSMBShare(context.Background(), "files", SMBShare{Name: "FILES"}),
		"share names are case-insensitive in SMB")
	require.NoError(t, smb.AddSMBShare(context.Background(), "files", SMBShare{Name: "proj", Path: "p"}))
	run := findScript(t, dep, "reload-config")
	assert.Equal(t, []string{"node2"}, run.hosts)
	assert.Contains(t, run.script, "mkdir -p '/srv/gateway-exports/files/p'")
	assert.Contains(t, run.script, "chown haify-smb:haify-smb")
}

// A share restricted to some users must not lie inside a share that lets in
// others: Samba checks valid users per share, so they would reach it through
// the outer one.
func TestSMBNestedExposure(t *testing.T) {
	root := SMBShare{Name: "files"}
	rootAlice := SMBShare{Name: "files", ValidUsers: []string{"alice"}}
	proj := SMBShare{Name: "projects", Path: "projects", ValidUsers: []string{"alice"}}

	err := smbNestedExposure([]SMBShare{root}, proj)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "lets in every user")

	err = smbNestedExposure([]SMBShare{{Name: "files", ValidUsers: []string{"alice", "bob"}}}, proj)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "lets in bob")

	assert.NoError(t, smbNestedExposure([]SMBShare{rootAlice}, proj), "the outer share lets in no one extra")
	assert.NoError(t, smbNestedExposure([]SMBShare{root}, SMBShare{Name: "pub", Path: "pub"}), "an open share inside an open one")
	assert.NoError(t, smbNestedExposure([]SMBShare{{Name: "a", Path: "a", ValidUsers: []string{"bob"}}}, proj), "siblings do not overlap")
	assert.NoError(t, smbNestedExposure([]SMBShare{{Name: "pa", Path: "projects-archive"}}, proj), "a shared name prefix is not nesting")

	// Adding the wide share around an existing restricted one is the same hole.
	err = smbNestedExposure([]SMBShare{proj}, SMBShare{Name: "all", Path: ""})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "share projects (/projects) lies inside share all")
}

// With force_unmount=true the Filesystem agent finds holders with
// `fuser -m <dir>`; when <dir> is no longer a mount point that names the root
// filesystem, and every process on the node is killed — init included. Seen on
// a test node: drbd-reactor died with the rest and the node silently stopped
// taking failovers. "safe" walks every entry under /proc before its first
// unmount attempt; on a host with 1200 threads that took 14 s, past the
// 10 s the agent allows itself, so the unmount never ran and every move
// failed. Every generated Filesystem line must use "false".
func assertSafeUnmount(t *testing.T, config string) {
	t.Helper()
	n := 0
	for _, line := range strings.Split(config, "\n") {
		if strings.Contains(line, "ocf:heartbeat:Filesystem") {
			n++
			assert.Contains(t, line, "force_unmount=false", line)
		}
	}
	assert.NotZero(t, n, "the config mounts nothing")
}

func TestSMBConfigUnmountsSafely(t *testing.T) {
	ip, err := parseServiceIP("192.168.1.50/24")
	require.NoError(t, err)
	config, err := generateSMBGatewayConfig("files", "/dev/drbd/by-res/files/0", "/dev/drbd/by-res/files/1", ip)
	require.NoError(t, err)
	assertSafeUnmount(t, config)
}
