package wanproxy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// event records one call against the fake deployment client, in order, so a
// test can assert the exact sequence of file-pushes and commands.
type event struct {
	kind    string // "distribute" or "exec"
	hosts   []string
	path    string // distribute: remote path
	content string // distribute: content
	cmd     string // exec: command
}

// fakeDeploy is an in-memory DeploymentClient: no SSH, no network. It records
// every call in order and lets a test inject a failure at a given step.
type fakeDeploy struct {
	events []event

	// failDistributePath / failExecSubstr trigger a per-host failure Result when
	// set and matched, to exercise the error paths.
	failDistributePath string
	failExecSubstr     string
	// failAll makes every exec return a per-host failure (used to model a node
	// where the proxy is down and the WAN port is unreachable).
	failAll bool

	// execOutput lets a test attach stdout to a matching command, so reads (e.g.
	// `cat` of the metrics snapshot) can return a body. Keyed by substring.
	execOutput map[string]string
	// execFailSubstrOutput marks a matching command as failed AND gives it
	// output, modelling e.g. `cat` of a file that does not exist.
	execFailSubstrOutput map[string]string
}

func (f *fakeDeploy) DistributeConfig(_ context.Context, hosts []string, content, remotePath string) (*Result, error) {
	f.events = append(f.events, event{kind: "distribute", hosts: append([]string(nil), hosts...), path: remotePath, content: content})
	return f.result(hosts, f.failDistributePath != "" && f.failDistributePath == remotePath), nil
}

func (f *fakeDeploy) Exec(_ context.Context, hosts []string, cmd string) (*Result, error) {
	f.events = append(f.events, event{kind: "exec", hosts: append([]string(nil), hosts...), cmd: cmd})

	for sub, out := range f.execFailSubstrOutput {
		if strings.Contains(cmd, sub) {
			return f.resultWithOutput(hosts, false, out), nil
		}
	}
	for sub, out := range f.execOutput {
		if strings.Contains(cmd, sub) {
			return f.resultWithOutput(hosts, true, out), nil
		}
	}
	return f.result(hosts, f.failAll || (f.failExecSubstr != "" && strings.Contains(cmd, f.failExecSubstr))), nil
}

func (f *fakeDeploy) resultWithOutput(hosts []string, success bool, out string) *Result {
	r := &Result{Hosts: map[string]*HostResult{}}
	for _, h := range hosts {
		r.Hosts[h] = &HostResult{Host: h, Success: success, Output: out}
	}
	return r
}

func (f *fakeDeploy) result(hosts []string, fail bool) *Result {
	r := &Result{Hosts: map[string]*HostResult{}}
	for _, h := range hosts {
		r.Hosts[h] = &HostResult{Host: h, Success: !fail}
	}
	return r
}

func newSpecWithTempPKI(t *testing.T) ProxySpec {
	t.Helper()
	// Redirect the package PKI cache to a temp dir so the test never writes to
	// /var/lib/haify.
	prev := PKIDir
	PKIDir = t.TempDir()
	t.Cleanup(func() { PKIDir = prev })
	return sampleSpec()
}

func TestProvisionSequence(t *testing.T) {
	spec := newSpecWithTempPKI(t)
	f := &fakeDeploy{}

	if err := Provision(context.Background(), f, spec); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	primary := spec.PrimaryNodeAddr
	dr := spec.DRNodeAddr
	both := []string{primary, dr}
	cfgPath := NodeConfigPath(spec.Resource)
	instance := UnitInstance(spec.Resource)

	// Expected ordered sequence (binary skipped: BinaryPath empty).
	want := []event{
		{kind: "distribute", hosts: both, path: UnitTemplatePath},
		{kind: "distribute", hosts: both, path: NodeCAPath},
		{kind: "distribute", hosts: both, path: NodeCertPath},
		{kind: "distribute", hosts: both, path: NodeKeyPath},
		{kind: "exec", hosts: both, cmd: "test -x " + NodeBinaryPath}, // pushed or pre-staged
		{kind: "distribute", hosts: []string{primary}, path: cfgPath}, // dialer
		{kind: "distribute", hosts: []string{dr}, path: cfgPath},      // acceptor
		{kind: "exec", hosts: both, cmd: "sudo systemctl daemon-reload"},
		{kind: "exec", hosts: both, cmd: "sudo systemctl enable " + instance},
		{kind: "exec", hosts: both, cmd: "sudo systemctl restart " + instance},
		// Preflight reachability probe from the primary to the DR WAN endpoint.
		{kind: "exec", hosts: []string{primary}, cmd: reachCmd(spec.DRPublicEndpoint, spec.WANPort)},
	}

	if len(f.events) != len(want) {
		t.Fatalf("event count = %d, want %d\ngot: %s", len(f.events), len(want), dumpEvents(f.events))
	}
	for i, w := range want {
		got := f.events[i]
		if got.kind != w.kind {
			t.Fatalf("event %d kind = %q, want %q", i, got.kind, w.kind)
		}
		if !sameHosts(got.hosts, w.hosts) {
			t.Fatalf("event %d hosts = %v, want %v", i, got.hosts, w.hosts)
		}
		if w.kind == "distribute" && got.path != w.path {
			t.Fatalf("event %d path = %q, want %q", i, got.path, w.path)
		}
		if w.kind == "exec" && got.cmd != w.cmd {
			t.Fatalf("event %d cmd = %q, want %q", i, got.cmd, w.cmd)
		}
	}

	// The dialer config went to the primary, the acceptor config to the DR.
	if got := f.events[5].content; got != RenderDialerConfig(spec) {
		t.Fatalf("primary did not receive the dialer config:\n%s", got)
	}
	if got := f.events[6].content; got != RenderAcceptorConfig(spec) {
		t.Fatalf("DR did not receive the acceptor config:\n%s", got)
	}

	// The restart must come after every config/cert push — it is what makes a
	// leg that was already running pick them up.
	restartIdx := lastExecIndex(f.events, "systemctl restart")
	for i, e := range f.events {
		if e.kind == "distribute" && i > restartIdx {
			t.Fatalf("config push at %d happened after the restart at %d", i, restartIdx)
		}
	}
}

func TestProvisionPushesBinaryWhenPathSet(t *testing.T) {
	spec := newSpecWithTempPKI(t)

	// A stand-in "binary" file; Provision just distributes its bytes.
	binPath := filepath.Join(t.TempDir(), "haify-proxy")
	if err := os.WriteFile(binPath, []byte("\x7fELF fake binary"), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	spec.BinaryPath = binPath

	f := &fakeDeploy{}
	if err := Provision(context.Background(), f, spec); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	// The binary must be pushed to NodeBinaryPath on both nodes, then chmod'd.
	var pushed, chmodded bool
	for _, e := range f.events {
		if e.kind == "distribute" && e.path == NodeBinaryPath {
			pushed = true
			if e.content != "\x7fELF fake binary" {
				t.Fatalf("binary content mismatch: %q", e.content)
			}
		}
		if e.kind == "exec" && strings.Contains(e.cmd, "chmod") && strings.Contains(e.cmd, NodeBinaryPath) {
			chmodded = true
		}
	}
	if !pushed {
		t.Fatalf("binary was not pushed to %s", NodeBinaryPath)
	}
	if !chmodded {
		t.Fatalf("binary was not made executable")
	}
}

func TestProvisionSurfacesDistributeFailure(t *testing.T) {
	spec := newSpecWithTempPKI(t)
	f := &fakeDeploy{failDistributePath: NodeCAPath}

	err := Provision(context.Background(), f, spec)
	if err == nil {
		t.Fatal("expected Provision to fail when a distribute step fails")
	}
	if !strings.Contains(err.Error(), "distribute CA") {
		t.Fatalf("error = %v, want it to mention the failing step", err)
	}
}

func TestProvisionSurfacesExecFailure(t *testing.T) {
	spec := newSpecWithTempPKI(t)
	f := &fakeDeploy{failExecSubstr: "systemctl enable"}

	err := Provision(context.Background(), f, spec)
	if err == nil {
		t.Fatal("expected Provision to fail when enable/start fails")
	}
	if !strings.Contains(err.Error(), "enable proxy") {
		t.Fatalf("error = %v, want it to name the failing step", err)
	}
}

func TestProvisionRejectsInvalidSpec(t *testing.T) {
	spec := newSpecWithTempPKI(t)
	spec.WANPort = 0
	f := &fakeDeploy{}
	if err := Provision(context.Background(), f, spec); err == nil {
		t.Fatal("expected Provision to reject an invalid spec")
	}
	if len(f.events) != 0 {
		t.Fatalf("no node should be touched for an invalid spec, got %d events", len(f.events))
	}
}

func TestDeprovisionSequence(t *testing.T) {
	f := &fakeDeploy{}
	resource := "data"
	primary := "10.0.0.1"
	dr := "10.0.0.2"

	if err := Deprovision(context.Background(), f, resource, primary, dr); err != nil {
		t.Fatalf("Deprovision: %v", err)
	}

	both := []string{primary, dr}
	instance := UnitInstance(resource)
	want := []event{
		{kind: "exec", hosts: both, cmd: "sudo systemctl disable --now " + instance + " 2>/dev/null || true"},
		{kind: "exec", hosts: both, cmd: "sudo rm -f " + NodeConfigPath(resource) + " " + NodeMetricsPath(resource)},
	}
	if len(f.events) != len(want) {
		t.Fatalf("event count = %d, want %d\ngot: %s", len(f.events), len(want), dumpEvents(f.events))
	}
	for i, w := range want {
		got := f.events[i]
		if got.kind != w.kind || got.cmd != w.cmd || !sameHosts(got.hosts, w.hosts) {
			t.Fatalf("event %d = %+v, want %+v", i, got, w)
		}
	}

	// Deprovision must NOT remove shared material (binary, certs, unit template).
	for _, e := range f.events {
		for _, shared := range []string{NodeBinaryPath, NodeCAPath, NodeCertPath, NodeKeyPath, UnitTemplatePath} {
			if strings.Contains(e.cmd, shared) {
				t.Fatalf("Deprovision must not touch shared %s (cmd=%q)", shared, e.cmd)
			}
		}
	}
}

func sameHosts(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func lastExecIndex(events []event, substr string) int {
	idx := -1
	for i, e := range events {
		if e.kind == "exec" && strings.Contains(e.cmd, substr) {
			idx = i
		}
	}
	return idx
}

func dumpEvents(events []event) string {
	var b strings.Builder
	for i, e := range events {
		if e.kind == "distribute" {
			b.WriteString("  [" + itoa(i) + "] distribute " + e.path + " -> " + strings.Join(e.hosts, ",") + "\n")
		} else {
			b.WriteString("  [" + itoa(i) + "] exec " + e.cmd + " -> " + strings.Join(e.hosts, ",") + "\n")
		}
	}
	return b.String()
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var digits []byte
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}

// A leg that is already running must be restarted, not merely enabled.
//
// `enable --now` starts a stopped unit and does nothing to a running one, so an
// existing leg keeps whatever it loaded at startup — including its mTLS
// material. Rotate the PKI and the two ends diverge while the files on disk
// agree: the dialer reports "invalid peer certificate: BadSignature", the
// acceptor "received fatal alert: DecryptError", and every certificate is
// provably identical. Nothing in that picture points at process age.
func TestProvisionRestartsSoRotatedPKIIsPickedUp(t *testing.T) {
	spec := newSpecWithTempPKI(t)
	f := &fakeDeploy{}
	if err := Provision(context.Background(), f, spec); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	instance := UnitInstance(spec.Resource)
	if lastExecIndex(f.events, "sudo systemctl restart "+instance) < 0 {
		t.Fatalf("no restart of %s; a running leg would keep its old certificates\n%s",
			instance, dumpEvents(f.events))
	}
	// `enable --now` would mask the bug by looking like it started something.
	for _, e := range f.events {
		if e.kind == "exec" && strings.Contains(e.cmd, "enable --now") {
			t.Fatalf("still using `enable --now`, which no-ops on a running unit: %q", e.cmd)
		}
	}
}

// The same guarantee for a multi-leg resource: every leg gets restarted, or the
// one that was not is the one that silently stops replicating.
func TestProvisionMultiRestartsEveryLeg(t *testing.T) {
	prev := PKIDir
	PKIDir = t.TempDir()
	t.Cleanup(func() { PKIDir = prev })

	f := &fakeDeploy{}
	spec := MultiSpec{
		Resource:              "data",
		PrimaryNodeAddrs:      []string{"10.0.0.1", "10.0.0.2"},
		DRNodeAddr:            "203.0.113.7",
		DRPublicEndpoint:      "203.0.113.7",
		BaseWANPort:           6600,
		BaseDRBDPort:          7300,
		SkipReachabilityCheck: true,
	}
	if err := ProvisionMulti(context.Background(), f, spec); err != nil {
		t.Fatalf("ProvisionMulti: %v", err)
	}

	for _, leg := range spec.Legs() {
		want := "sudo systemctl restart " + UnitInstance(leg.Resource)
		if lastExecIndex(f.events, want) < 0 {
			t.Fatalf("leg %s was never restarted; it would keep its old certificates",
				leg.Resource)
		}
	}
}

// A node without the binary used to get a unit that crash-looped with
// 203/EXEC, reported only as a closed WAN port.
func TestProvisionFailsWhenANodeHasNoBinary(t *testing.T) {
	spec := newSpecWithTempPKI(t)
	f := &fakeDeploy{failExecSubstr: "test -x " + NodeBinaryPath}
	err := Provision(context.Background(), f, spec)
	if err == nil || !strings.Contains(err.Error(), "no executable "+NodeBinaryPath) {
		t.Fatalf("want a missing-binary error, got %v", err)
	}
	if lastExecIndex(f.events, "sudo systemctl restart "+UnitInstance(spec.Resource)) >= 0 {
		t.Fatal("the proxy was started on a node with no binary")
	}
}

// ProvisionMulti used to ignore BinaryFor, so a controller holding the right
// binary for each node pushed none of them.
func TestProvisionMultiPushesThePerNodeBinary(t *testing.T) {
	prev := PKIDir
	PKIDir = t.TempDir()
	t.Cleanup(func() { PKIDir = prev })
	bin := filepath.Join(t.TempDir(), "haify-proxy-arm64")
	if err := os.WriteFile(bin, []byte("binary"), 0755); err != nil {
		t.Fatal(err)
	}

	f := &fakeDeploy{}
	spec := MultiSpec{
		Resource:              "data",
		PrimaryNodeAddrs:      []string{"10.0.0.1"},
		DRNodeAddr:            "203.0.113.7",
		DRPublicEndpoint:      "203.0.113.7",
		BaseWANPort:           6600,
		BaseDRBDPort:          7300,
		SkipReachabilityCheck: true,
		BinaryFor: func(addr string) string {
			if addr == "203.0.113.7" {
				return bin
			}
			return ""
		},
	}
	if err := ProvisionMulti(context.Background(), f, spec); err != nil {
		t.Fatalf("ProvisionMulti: %v", err)
	}
	pushed := false
	for _, e := range f.events {
		if e.kind == "distribute" && e.path == NodeBinaryPath {
			pushed = true
			if !sameHosts(e.hosts, []string{"203.0.113.7"}) {
				t.Fatalf("binary pushed to %v, want only the DR node", e.hosts)
			}
		}
	}
	if !pushed {
		t.Fatal("the DR node's binary was never pushed")
	}
}
