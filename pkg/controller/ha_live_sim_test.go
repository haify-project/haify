package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/haify-project/haify/pkg/database"
	"github.com/haify-project/haify/pkg/deployment"
	"github.com/haify-project/haify/pkg/inspect"
	"github.com/stretchr/testify/require"
)

// liveSim is three KVM hosts as a live move sees them: libvirt, DRBD,
// drbd-reactor and systemd, each reduced to the behaviour the move relies on
// or must guard against. It answers the scripts ha_live_scripts.go sends and
// records, as violations, what must never happen on real hosts:
//
//   - the guest running twice (an enabled promoter on a node that is Primary
//     without the guest, or an agent unit that restarts after the guest left);
//   - the guest being stopped (a services target stopped where it runs);
//   - a demote of the node the guest runs on.
type liveSim struct {
	t  *testing.T
	mu sync.Mutex

	hosts    []string
	name     map[string]string
	xml      string
	guestOn  string
	domID    map[string]int
	nextID   int
	role     map[string]string
	disk     map[string]string
	promoter map[string]string
	twoPrim  map[string]bool
	guard    map[string]bool
	target   map[string]string
	agent    map[string]string
	down     map[string]bool
	marked   map[string]bool

	// How the migration goes.
	moveFails    bool
	promoteFails bool
	moveState    string
	movePolls    int
	moveRC       string
	moveTarget   string

	events     []string
	violations []string
}

var simDestRe = regexp.MustCompile(`qemu\+ssh://([0-9.]+)/system`)

func newLiveSim(t *testing.T) *liveSim {
	s := &liveSim{t: t, hosts: []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"},
		name:  map[string]string{"10.0.0.1": "k1", "10.0.0.2": "k2", "10.0.0.3": "k3"},
		xml:   testDomainXML("web1", testUUID, 2097152),
		domID: map[string]int{"10.0.0.1": 7}, nextID: 20, guestOn: "10.0.0.1",
		role: map[string]string{}, disk: map[string]string{}, promoter: map[string]string{}, twoPrim: map[string]bool{},
		guard: map[string]bool{}, target: map[string]string{}, agent: map[string]string{}, down: map[string]bool{},
		marked: map[string]bool{}}
	for _, h := range s.hosts {
		s.role[h], s.disk[h], s.promoter[h], s.target[h], s.agent[h] = "Secondary", "UpToDate", "enabled", "inactive", "inactive"
	}
	s.role["10.0.0.1"], s.target["10.0.0.1"], s.agent["10.0.0.1"] = "Primary", "active", "active"
	return s
}

func (s *liveSim) event(format string, args ...any) {
	s.events = append(s.events, fmt.Sprintf(format, args...))
}

func (s *liveSim) violate(format string, args ...any) {
	s.violations = append(s.violations, fmt.Sprintf(format, args...))
}

// index returns where the first event with prefix is, or -1.
func (s *liveSim) index(prefix string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, e := range s.events {
		if strings.HasPrefix(e, prefix) {
			return i
		}
	}
	return -1
}

func (s *liveSim) primaries() []string {
	var out []string
	for _, h := range s.hosts {
		if s.role[h] == "Primary" {
			out = append(out, s.name[h])
		}
	}
	return out
}

// reactorStarts is what drbd-reactor does when its promoter for the
// resource is loaded on h, or its target is started there: on a node that is
// Primary it starts the chain. The VirtualDomain agent's start is a no-op
// for a guest that runs on the node, and starts the guest otherwise.
func (s *liveSim) reactorStarts(h string) {
	if s.role[h] != "Primary" {
		if len(s.primaries()) > 0 || s.guestOn != "" {
			return
		}
		// Nothing is Primary and nothing runs: this is a takeover.
		s.role[h] = "Primary"
	}
	s.target[h], s.agent[h] = "active", "active"
	switch s.guestOn {
	case h:
	case "":
		s.nextID++
		s.guestOn, s.domID[h] = h, s.nextID
		s.event("guest-start:%s", s.name[h])
	default:
		s.violate("the guest was started on %s while it runs on %s", s.name[h], s.name[s.guestOn])
	}
}

func (s *liveSim) drbdJSON(h string) string {
	r := inspect.DRBDResource{Name: "web1", Role: s.role[h]}
	yes := true
	r.Devices = []inspect.DRBDDevice{{Volume: 0, DiskState: s.disk[h], Quorum: &yes}}
	for _, o := range s.hosts {
		if o == h || s.down[o] {
			continue
		}
		r.Connections = append(r.Connections, inspect.DRBDConnection{Name: "host-" + s.name[o], ConnectionState: "Connected",
			PeerRole: s.role[o], PeerDevices: []inspect.DRBDPeerDevice{{Volume: 0, ReplicationState: "Established", PeerDiskState: s.disk[o]}}})
	}
	raw, _ := json.Marshal([]inspect.DRBDResource{r})
	return base64.StdEncoding.EncodeToString(raw)
}

func (s *liveSim) probe(h string) string {
	state, id := "shut off", "-"
	if s.guestOn == h {
		state, id = "running", fmt.Sprint(s.domID[h])
	}
	restart, two, move := "always", "0", "inactive"
	if s.guard[h] {
		restart = "no"
	}
	if s.twoPrim[h] {
		two = "1"
	}
	if s.moveState == "running" && s.guestOn == h {
		move = "active"
	}
	return fmt.Sprintf("VIRSH=yes\nSTATE=%s\nLIBVIRT_UUID=%s\nDOMID=%s\nCONFIG=%s\nREACTOR=active\nTARGET_ACTIVE=%s\n"+
		"AGENT_ACTIVE=%s\nAGENT_RESTART=%s\nMOVE_UNIT=%s\nPROMOTER=%s\nOCF_AGENT=yes\nTWO_PRIMARIES=%s\nDRBD=%s\n",
		state, testUUID, id, base64.StdEncoding.EncodeToString([]byte(s.xml)), s.target[h], s.agent[h], restart, move,
		s.promoter[h], two, s.drbdJSON(h))
}

// poll advances the migration and reports it.
func (s *liveSim) poll(h string) string {
	if s.moveState == "running" {
		if s.movePolls--; s.movePolls <= 0 {
			s.moveState = "done"
			switch {
			case s.moveFails:
				s.moveRC = "1"
			case s.role[s.moveTarget] != "Primary" || !s.twoPrim[h] || !s.twoPrim[s.moveTarget]:
				s.violate("the migration ran without two Primaries allowed between %s and %s", s.name[h], s.name[s.moveTarget])
				s.moveRC = "1"
			default:
				s.moveRC = "0"
				s.nextID++
				s.guestOn, s.domID[s.moveTarget] = s.moveTarget, s.nextID
				s.event("guest-arrived:%s", s.name[s.moveTarget])
				// The wrapper's monitor finds the guest gone from the source.
				if s.guard[h] {
					s.agent[h] = "failed"
				} else {
					s.violate("the agent unit on %s restarted after the guest left, starting it a second time", s.name[h])
				}
			}
		}
	}
	unit := "inactive"
	if s.moveState == "running" {
		unit = "active"
	}
	out := fmt.Sprintf("RC=%s\nUNIT=%s\nJOB_PROCESSED=1.000 GiB\nJOB_TOTAL=2.000 GiB\n", s.moveRC, unit)
	if s.moveRC != "" && s.moveRC != "0" {
		out += "LOG=" + base64.StdEncoding.EncodeToString([]byte("error: operation failed: migration out job: unexpectedly failed\n")) + "\n"
	}
	return out
}

// run answers one command on one host.
func (s *liveSim) run(h, cmd string) (string, bool) {
	script := decodeNodeScript(cmd)
	switch {
	case strings.Contains(cmd, "HAIFY_HA_TOML_MISSING"):
		rm := &ResourceManager{}
		return rm.generatePromoterConfig("web1", nil, "", "", []OcfAgentSpec{{Provider: "heartbeat", Name: "VirtualDomain",
			Instance: "vm_web1", Params: map[string]string{"config": "/etc/libvirt/qemu/web1.xml", "hypervisor": "qemu:///system"}}}), true
	case strings.Contains(cmd, "--allow-two-primaries=yes"):
		s.twoPrim[h] = true
		s.event("two-primaries-on:%s", s.name[h])
	case strings.Contains(cmd, "--allow-two-primaries=no"):
		if len(s.primaries()) > 1 {
			return "Can not clear allow_two_primaries as node is primary and peer is primary", false
		}
		s.twoPrim[h] = false
		s.event("two-primaries-off:%s", s.name[h])
	case strings.HasPrefix(cmd, "sudo drbdsetup show"):
		if s.twoPrim[h] {
			return "net {\n    allow-two-primaries yes;\n}\n", true
		}
	case strings.Contains(script, "LIBVIRT_UUID="):
		return s.probe(h), true
	case strings.Contains(script, "HAIFY_MARKED"):
		s.marked[h] = true
		return "HAIFY_MARKED=1\n", true
	case strings.Contains(script, "/controlled/") && strings.Contains(script, "rm -f"):
		s.marked[h] = false
	case strings.Contains(script, "printf '[Service]"):
		s.guard[h] = true
		s.event("guard:%s", s.name[h])
		return "HAIFY_RESTART=no\n", true
	case strings.Contains(script, "HAIFY_RETIRED"):
		switch {
		case s.guestOn == h:
			return "HAIFY_KEPT=the guest is running here\n", true
		case s.role[h] == "Primary":
			return "HAIFY_KEPT=web1 is still Primary here\n", true
		}
		s.target[h], s.agent[h], s.guard[h] = "inactive", "inactive", false
		s.event("retire:%s", s.name[h])
		return "HAIFY_RETIRED=1\n", true
	case strings.Contains(script, liveGuardFile):
		s.guard[h] = false
		s.event("unguard:%s", s.name[h])
		return "HAIFY_RESTART=always\n", true
	case strings.Contains(script, "HAIFY_DISABLED"):
		s.promoter[h] = "disabled"
		s.event("disable:%s", s.name[h])
		return "HAIFY_DISABLED=1\n", true
	case strings.Contains(script, "HAIFY_ENABLED"):
		s.promoter[h] = "enabled"
		s.event("enable:%s", s.name[h])
		s.reactorStarts(h)
		return "HAIFY_ENABLED=1\n", true
	case strings.Contains(script, "HAIFY_CONNECTED"):
		return "HAIFY_CONNECTED=1\n", true
	case strings.Contains(script, "HAIFY_STARTED"):
		m := simDestRe.FindStringSubmatch(script)
		require.NotNil(s.t, m, "the migration names its destination")
		s.moveState, s.movePolls, s.moveRC, s.moveTarget = "running", 2, "", m[1]
		s.event("move-start:%s->%s", s.name[h], s.name[m[1]])
		return "HAIFY_STARTED=1\n", true
	case strings.Contains(script, "JOB_PROCESSED"):
		return s.poll(h), true
	case strings.Contains(script, "domjobabort"):
		s.moveState, s.moveRC = "done", "1"
		s.event("abort:%s", s.name[h])
	case strings.Contains(script, `systemctl start "$target"`):
		s.event("adopt:%s", s.name[h])
		s.reactorStarts(h)
		return fmt.Sprintf("TARGET_ACTIVE=%s\nAGENT_ACTIVE=%s\n", s.target[h], s.agent[h]), true
	}
	return "", true
}

func (s *liveSim) exec(_ context.Context, hosts []string, cmd string, _ ...deployment.ExecOption) (*deployment.ExecResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res := &deployment.ExecResult{Hosts: map[string]*deployment.HostResult{}}
	for _, h := range hosts {
		if s.down[h] {
			res.Hosts[h] = &deployment.HostResult{Host: h, Success: false, Output: "ssh: connect to host: no route to host"}
			continue
		}
		out, ok := s.run(h, cmd)
		res.Hosts[h] = &deployment.HostResult{Host: h, Success: ok, Output: out}
	}
	return res, nil
}

func (s *liveSim) primary(_ context.Context, h, _ string, force bool) (*deployment.HostResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if force {
		s.violate("a forced promote on %s", s.name[h])
	}
	if s.promoteFails {
		return &deployment.HostResult{Host: h, Output: "State change failed: (-2) Need access to UpToDate data"}, nil
	}
	for _, o := range s.hosts {
		if o != h && s.role[o] == "Primary" && (!s.twoPrim[h] || !s.twoPrim[o]) {
			return &deployment.HostResult{Host: h, Output: "State change failed: (-1) Multiple primaries not allowed by config"}, nil
		}
	}
	s.role[h] = "Primary"
	s.event("promote:%s", s.name[h])
	return &deployment.HostResult{Host: h, Success: true}, nil
}

func (s *liveSim) secondary(_ context.Context, h, _ string) (*deployment.HostResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.guestOn == h {
		s.violate("a demote of %s, where the guest runs", s.name[h])
		return &deployment.HostResult{Host: h, Output: "State change failed: (-12) Device is held open by someone"}, nil
	}
	s.role[h] = "Secondary"
	s.event("demote:%s", s.name[h])
	return &deployment.HostResult{Host: h, Success: true}, nil
}

// controller builds a controller over the simulated hosts, with web1 an HA
// guest whose definition is kept and in sync.
func (s *liveSim) controller() *Controller {
	dep := &fakeDeploymentClient{execFunc: s.exec, drbdPrimaryFunc: s.primary, drbdSecondaryFunc: s.secondary}
	ctrl := newBasicTestController(dep)
	ctrl.db = newTestDB(s.t)
	ctx := context.Background()
	require.NoError(s.t, ctrl.db.SaveResource(ctx, &database.Resource{Name: "web1", Port: 7001, Nodes: "k1,k2,k3", Protocol: "C", Replicas: 3}))
	for _, h := range s.hosts {
		ctrl.nodes.nodes[h] = &NodeInfo{Name: s.name[h], Address: h, Hostname: "host-" + s.name[h]}
		ctrl.hostsMap[s.name[h]] = h
	}
	require.NoError(s.t, ctrl.db.SaveHaConfig(ctx, &database.HaConfig{Resource: "web1", OcfAgents: []database.HaOcfAgent{{
		Provider: "heartbeat", Name: "VirtualDomain", Instance: "vm_web1",
		Params: map[string]string{"config": "/etc/libvirt/qemu/web1.xml", "hypervisor": "qemu:///system"}}}}))
	_, _, hash, err := parseDomainXML(s.xml)
	require.NoError(s.t, err)
	require.NoError(s.t, ctrl.db.SaveVMDefinition(ctx, &database.VMDefinition{Resource: "web1", Domain: "web1", UUID: testUUID,
		XML: s.xml, SourceNode: "k1", NodeHashes: map[string]string{"k1": hash, "k2": hash, "k3": hash}}))
	return ctrl
}
