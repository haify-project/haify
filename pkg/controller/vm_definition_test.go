package controller

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/deployment"
	"github.com/haify-project/haify/pkg/gateway"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const testUUID = "6f1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"

// testDomainXML is a definition as libvirt writes the file, VNC password
// included.
func testDomainXML(name, uuid string, memory int) string {
	return fmt.Sprintf(`<!--
WARNING: THIS IS AN AUTO-GENERATED FILE.
  virsh edit %[1]s
-->

<domain type='kvm'>
  <name>%[1]s</name>
  <uuid>%[2]s</uuid>
  <memory unit='KiB'>%[3]d</memory>
  <devices>
    <disk type='block' device='disk'><source dev='/dev/drbd/by-res/%[1]s/0'/><target dev='vda' bus='virtio'/></disk>
    <graphics type='vnc' port='-1' passwd='s3cr3t-vnc'/>
  </devices>
</domain>
`, name, uuid, memory)
}

func factsFor(xml string, running bool) *vmNodeFacts {
	f := &vmNodeFacts{Reachable: true, Virsh: true, StateKnown: true, Running: running, State: "shut off"}
	if xml != "" {
		f.HasConfig, f.XML = true, xml
		f.FileName, f.FileUUID, f.Hash, _ = parseDomainXML(xml)
	}
	return f
}

func TestVMGuestOfFindsTheGuestInEitherStartList(t *testing.T) {
	agent := database.HaOcfAgent{Provider: "heartbeat", Name: "VirtualDomain", Instance: "vm_web_1",
		Params: map[string]string{"config": "/etc/libvirt/qemu/web-1.xml", "hypervisor": "qemu:///system"}}

	g := vmGuestOf(&database.HaConfig{Resource: "web-1", OcfAgents: []database.HaOcfAgent{agent}})
	require.NotNil(t, g)
	assert.Equal(t, "web-1", g.Domain)
	assert.Equal(t, "/etc/libvirt/qemu/web-1.xml", g.ConfigPath)

	g = vmGuestOf(&database.HaConfig{Resource: "web-1", StartItems: []database.HaStartItem{{SystemdUnit: "x.service"}, {Ocf: &agent}}})
	require.NotNil(t, g)
	assert.Equal(t, "web-1", g.Domain)

	assert.Nil(t, vmGuestOf(&database.HaConfig{Resource: "db", Services: []string{"mysql"}}))
	// A definition file outside libvirt's own directory is not one Haify manages.
	agent.Params = map[string]string{"config": "/srv/vms/web-1.xml"}
	assert.Nil(t, vmGuestOf(&database.HaConfig{Resource: "web-1", OcfAgents: []database.HaOcfAgent{agent}}))
}

// The unit the live move guards and adopts must be the one drbd-reactor
// generates; the gateway package derives the same name from the promoter
// config, independently.
func TestAgentUnitMatchesWhatThePromoterConfigYields(t *testing.T) {
	for _, tc := range []struct{ resource, instance, domain string }{
		{"web1", "vm_web1", "web1"},
		{"web-1", "vm_web_1", "web-1"},
		{"app_db2", "vm_app_db_2", "app.db+2"},
	} {
		g := &vmGuest{Domain: tc.domain, ConfigPath: "/etc/libvirt/qemu/" + tc.domain + ".xml", Instance: tc.instance}
		rm := &ResourceManager{}
		toml := rm.generatePromoterConfig(tc.resource, nil, "", "", []OcfAgentSpec{{Provider: "heartbeat", Name: "VirtualDomain",
			Instance: tc.instance, Params: map[string]string{"config": g.ConfigPath, "hypervisor": "qemu:///system"}}})
		chain, err := gateway.PromoterChain(toml)
		require.NoError(t, err)
		require.Len(t, chain, 1)
		assert.Equal(t, chain[0].Unit, g.agentUnit(tc.resource), tc.resource)
		assert.True(t, strings.HasSuffix(chain[0].AgentPath, "/heartbeat/VirtualDomain"))
	}
	assert.Equal(t, `drbd-services@web\x2d1.target`, reactorTargetUnit("web-1"))
}

func TestParseDomainXMLIgnoresTheFileHeader(t *testing.T) {
	xml := testDomainXML("web1", strings.ToUpper(testUUID), 2097152)
	name, uuid, hash, err := parseDomainXML(xml)
	require.NoError(t, err)
	assert.Equal(t, "web1", name)
	assert.Equal(t, testUUID, uuid, "UUIDs compare case-insensitively")

	_, _, bare, err := parseDomainXML(xml[strings.Index(xml, "<domain"):])
	require.NoError(t, err)
	assert.Equal(t, hash, bare, "the comment libvirt heads the file with is not part of the definition")

	_, _, other, _ := parseDomainXML(testDomainXML("web1", testUUID, 4194304))
	assert.NotEqual(t, hash, other)

	_, _, _, err = parseDomainXML("not xml at all")
	require.Error(t, err)
	_, _, _, err = parseDomainXML("<domain><name>x</name passwd='s3cr3t-vnc'")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "s3cr3t", "a parse error must not quote the definition")
}

func TestVMStateRunning(t *testing.T) {
	for state, want := range map[string][2]bool{
		"running":                            {true, true},
		"paused":                             {true, true},
		"in shutdown":                        {true, true},
		"shut off":                           {false, true},
		"error: failed to get domain 'web1'": {false, true},
		// libvirt did not answer: not a node the guest is absent from.
		"error: failed to connect to the hypervisor": {false, false},
		"": {false, false},
	} {
		running, known := vmStateRunning(state)
		assert.Equal(t, want, [2]bool{running, known}, state)
	}
}

func TestChooseVMDefinitionSource(t *testing.T) {
	order := []string{"k1", "k2", "k3"}
	xml := testDomainXML("web1", testUUID, 2097152)
	edited := testDomainXML("web1", testUUID, 4194304)
	otherUUID := testDomainXML("web1", "11111111-2222-4333-8444-555555555555", 2097152)
	_, _, hash, _ := parseDomainXML(xml)
	kept := &database.VMDefinition{Resource: "web1", Domain: "web1", UUID: testUUID, XML: xml, SourceNode: "k1",
		NodeHashes: map[string]string{"k1": hash, "k2": hash, "k3": hash}}

	t.Run("first sync takes the only definition", func(t *testing.T) {
		src, err := chooseVMDefinitionSource(nil, "web1", order, map[string]*vmNodeFacts{
			"k1": factsFor("", false), "k2": factsFor(xml, false), "k3": factsFor("", false)}, "")
		require.NoError(t, err)
		assert.Equal(t, vmSource{Node: "k2"}, src)
	})
	t.Run("first sync prefers the node the guest runs on", func(t *testing.T) {
		src, err := chooseVMDefinitionSource(nil, "web1", order, map[string]*vmNodeFacts{
			"k1": factsFor(otherUUID, false), "k2": factsFor(xml, true), "k3": factsFor("", false)}, "")
		require.NoError(t, err)
		assert.Equal(t, "k2", src.Node)
	})
	t.Run("first sync refuses to pick between UUIDs", func(t *testing.T) {
		_, err := chooseVMDefinitionSource(nil, "web1", order, map[string]*vmNodeFacts{
			"k1": factsFor(otherUUID, false), "k2": factsFor(xml, false), "k3": factsFor("", false)}, "")
		require.ErrorContains(t, err, "different UUIDs")
		assert.Contains(t, err.Error(), testUUID+" on k2")
	})
	t.Run("defined nowhere", func(t *testing.T) {
		_, err := chooseVMDefinitionSource(nil, "web1", order, map[string]*vmNodeFacts{
			"k1": factsFor("", false), "k2": factsFor("", false), "k3": {Err: "timeout"}}, "")
		require.ErrorContains(t, err, "not defined on any replica")
	})
	t.Run("nothing edited repairs from the kept copy", func(t *testing.T) {
		src, err := chooseVMDefinitionSource(kept, "web1", order, map[string]*vmNodeFacts{
			"k1": factsFor(xml, true), "k2": factsFor("", false), "k3": factsFor(xml, false)}, "")
		require.NoError(t, err)
		assert.Equal(t, vmSource{Stored: true}, src)
	})
	t.Run("the one edited node is the source", func(t *testing.T) {
		src, err := chooseVMDefinitionSource(kept, "web1", order, map[string]*vmNodeFacts{
			"k1": factsFor(xml, true), "k2": factsFor(edited, false), "k3": factsFor(xml, false)}, "")
		require.NoError(t, err)
		assert.Equal(t, vmSource{Node: "k2"}, src)
	})
	t.Run("two different edits need a decision", func(t *testing.T) {
		_, err := chooseVMDefinitionSource(kept, "web1", order, map[string]*vmNodeFacts{
			"k1": factsFor(testDomainXML("web1", testUUID, 1048576), true), "k2": factsFor(edited, false), "k3": factsFor(xml, false)}, "")
		require.ErrorContains(t, err, "k1 and k2")
	})
	t.Run("a copy under another UUID is not an edit", func(t *testing.T) {
		src, err := chooseVMDefinitionSource(kept, "web1", order, map[string]*vmNodeFacts{
			"k1": factsFor(xml, true), "k2": factsFor(otherUUID, false), "k3": factsFor(xml, false)}, "")
		require.NoError(t, err)
		assert.True(t, src.Stored)
	})
	t.Run("from names the source", func(t *testing.T) {
		src, err := chooseVMDefinitionSource(kept, "web1", order, map[string]*vmNodeFacts{
			"k1": factsFor(xml, true), "k2": factsFor(edited, false), "k3": factsFor("", false)}, "k1")
		require.NoError(t, err)
		assert.Equal(t, "k1", src.Node)
		_, err = chooseVMDefinitionSource(kept, "web1", order, map[string]*vmNodeFacts{"k3": factsFor("", false)}, "k3")
		require.ErrorContains(t, err, "no usable definition")
		_, err = chooseVMDefinitionSource(kept, "web1", order, map[string]*vmNodeFacts{}, "k9")
		require.ErrorContains(t, err, "holds no replica")
	})
}

func TestAssessVMDefinition(t *testing.T) {
	xml := testDomainXML("web1", testUUID, 2097152)
	_, _, hash, _ := parseDomainXML(xml)
	def := &database.VMDefinition{Resource: "web1", Domain: "web1", UUID: testUUID, XML: xml,
		NodeHashes: map[string]string{"k1": hash, "k2": hash, "k3": hash, "k4": hash}}
	libvirtOther := factsFor(xml, false)
	libvirtOther.LibvirtUUID = "11111111-2222-4333-8444-555555555555"

	st := assessVMDefinition(def, []string{"k1", "k2", "k3", "k4", "k5", "k6"}, map[string]*vmNodeFacts{
		"k1": factsFor(xml, true),
		"k2": factsFor("", false),
		"k3": factsFor(testDomainXML("web1", testUUID, 4194304), false),
		"k4": libvirtOther,
		"k5": {Err: "ssh: connection refused"},
		"k6": factsFor(xml, false),
	})
	states := map[string]string{}
	for _, n := range st.Nodes {
		states[n.Node] = n.State
	}
	assert.Equal(t, map[string]string{"k1": vmDefOK, "k2": vmDefMissing, "k3": vmDefChanged, "k4": vmDefUUIDMismatch,
		"k5": vmDefUnreachable, "k6": vmDefChanged}, states, "k6 was never synced, so its copy is not known to match")
	assert.False(t, st.Consistent)
	assert.True(t, st.Nodes[0].Running)
	assert.NotContains(t, st.Message, "s3cr3t")

	ok := assessVMDefinition(def, []string{"k1", "k2"}, map[string]*vmNodeFacts{"k1": factsFor(xml, true), "k2": factsFor(xml, false)})
	assert.True(t, ok.Consistent)
}

// A guest's definition can hold a VNC password. Nothing on the way from a
// node's file to another node's `virsh define` may put it in a log or on a
// command line.
func TestSyncVMDefinitionKeepsTheDefinitionOutOfLogsAndCommands(t *testing.T) {
	xml := testDomainXML("web1", testUUID, 2097152)
	hosts := map[string]string{"10.0.0.1": xml, "10.0.0.2": "", "10.0.0.3": ""}
	var secrets []string
	dep := &fakeDeploymentClient{}
	dep.distributeSecretFunc = func(ctx context.Context, h []string, content, relPath string) (*deployment.ConfigResult, error) {
		secrets = append(secrets, h[0]+":"+relPath)
		hosts[h[0]] = "staged:" + content
		return &deployment.ConfigResult{Path: relPath, Success: true, Hosts: map[string]*deployment.HostResult{}}, nil
	}
	dep.execFunc = func(ctx context.Context, h []string, cmd string, opts ...deployment.ExecOption) (*deployment.ExecResult, error) {
		script := decodeNodeScript(cmd)
		res := &deployment.ExecResult{Hosts: map[string]*deployment.HostResult{}}
		for _, host := range h {
			out := ""
			switch {
			case strings.Contains(cmd, "HAIFY_HOME"):
				out = "HAIFY_HOME=/home/haify\n"
			case strings.Contains(script, "HAIFY_DEFINED"):
				hosts[host] = strings.TrimPrefix(hosts[host], "staged:")
				out = "HAIFY_DEFINED=1\n"
			case strings.Contains(script, "LIBVIRT_UUID="):
				out = "VIRSH=yes\nSTATE=shut off\nLIBVIRT_UUID=\nDOMID=-\nCONFIG="
				if x := hosts[host]; x != "" && !strings.HasPrefix(x, "staged:") {
					out += base64.StdEncoding.EncodeToString([]byte(x))
				}
				out += "\n"
			}
			res.Hosts[host] = &deployment.HostResult{Host: host, Success: true, Output: out}
		}
		return res, nil
	}
	ctrl := newBasicTestController(dep)
	var logs bytes.Buffer
	ctrl.logger = zap.New(zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(&logs), zapcore.DebugLevel))
	ctrl.db = newTestDB(t)
	ctx := context.Background()
	require.NoError(t, ctrl.db.SaveResource(ctx, &database.Resource{Name: "web1", Port: 7001, Nodes: "k1,k2,k3", Protocol: "C", Replicas: 3}))
	for i, n := range []string{"k1", "k2", "k3"} {
		addr := fmt.Sprintf("10.0.0.%d", i+1)
		ctrl.nodes.nodes[addr] = &NodeInfo{Name: n, Address: addr}
		ctrl.hostsMap[n] = addr
	}

	agents := []OcfAgentSpec{{Provider: "heartbeat", Name: "VirtualDomain", Instance: "vm_web1",
		Params: map[string]string{"config": "/etc/libvirt/qemu/web1.xml", "hypervisor": "qemu:///system"}}}
	require.NoError(t, ctrl.resources.PrepareVMDefinition(ctx, "web1", agents, nil, ""))

	def, err := ctrl.db.GetVMDefinition(ctx, "web1")
	require.NoError(t, err)
	require.NotNil(t, def)
	assert.Equal(t, testUUID, def.UUID)
	assert.Equal(t, "k1", def.SourceNode)
	assert.ElementsMatch(t, []string{"k2", "k3"}, def.Placed, "only the nodes Haify defined the guest on")
	assert.Len(t, def.NodeHashes, 3)
	assert.Len(t, secrets, 2, "the definition travels by scp to the two nodes that lacked it")
	assert.Equal(t, xml, hosts["10.0.0.2"])

	assert.NotContains(t, logs.String(), "s3cr3t-vnc")
	encoded := base64.StdEncoding.EncodeToString([]byte(xml))
	for _, call := range dep.execCalls {
		assert.NotContains(t, call.cmd, "s3cr3t-vnc")
		assert.NotContains(t, call.cmd, encoded[:60], "the definition must not ride on a command line")
		assert.NotContains(t, decodeNodeScript(call.cmd), "s3cr3t-vnc")
	}

	// A config that runs no guest is none of this code's business.
	require.NoError(t, ctrl.resources.PrepareVMDefinition(ctx, "db", nil, []HaStartItem{{SystemdUnit: "mysql.service"}}, ""))
}

// decodeNodeScript returns the script runNodeScript wrapped in cmd, or "".
func decodeNodeScript(cmd string) string {
	rest, ok := strings.CutPrefix(cmd, "echo ")
	if !ok {
		return ""
	}
	enc, _, ok := strings.Cut(rest, " | base64 -d")
	if !ok {
		return ""
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return ""
	}
	return string(raw)
}
