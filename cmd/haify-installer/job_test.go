package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestChoicesArgs(t *testing.T) {
	c := choices{
		Env:          "libvirt",
		Nodes:        map[string]string{"ins2": "10.0.0.12", "ins1": "10.0.0.11"},
		StorageNodes: []string{"ins1", "ins2"},
		Devices:      map[string][]string{"ins2": {"/dev/sdb"}, "ins1": {"/dev/sdb", "/dev/sdc"}},
		Pool:         "vg0",
		PoolType:     "lvm-thin",
		Replicas:     2,
		SelfHA:       true,
		VIP:          "10.0.0.250/24",
	}
	args, err := c.args()
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(args, " ")
	want := "--env libvirt --nodes ins1=10.0.0.11,ins2=10.0.0.12 --storage-nodes ins1,ins2 " +
		"--node-devices ins1=/dev/sdb,/dev/sdc --node-devices ins2=/dev/sdb --pool vg0 --pool-type lvm-thin " +
		"--replicas 2 --vip 10.0.0.250/24"
	if got != want {
		t.Fatalf("args\n got: %s\nwant: %s", got, want)
	}

	c.SelfHA = false
	c.Env = "proxmox"
	args, _ = c.args()
	got = strings.Join(args, " ")
	if strings.Contains(got, "--nodes") || !strings.HasSuffix(got, "--no-self-ha") {
		t.Fatalf("proxmox without Self-HA: %s", got)
	}
}

func TestChoicesArgsRefusesShellText(t *testing.T) {
	for _, c := range []choices{
		{Env: "libvirt", Nodes: map[string]string{"a;rm -rf /": "10.0.0.1"}},
		{Env: "libvirt", Nodes: map[string]string{"a": "10.0.0.1"}, Devices: map[string][]string{"a": {"/dev/sdb $(id)"}}},
		{Env: "libvirt", Nodes: map[string]string{"a": "10.0.0.1"}, SelfHA: true, VIP: "10.0.0.250/24 --force-wipe"},
		{Env: "libvirt", Nodes: map[string]string{"a": "10.0.0.1"}, PoolType: "zfs"},
		{Env: "libvirt"},
	} {
		if _, err := c.args(); err == nil {
			t.Fatalf("accepted %+v", c)
		}
	}
}

func TestJobStartRunsScript(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "s.sh")
	body := "echo '=== Step 1: nodes ==='\necho \"args $*\"\necho '=== Step 2: drbd ==='\necho 'Step 2 failed. Fix the cause above' >&2\nexit 3\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	j := newJob()
	if err := j.start(script, []string{"--yes"}, "libvirt"); err != nil {
		t.Fatal(err)
	}
	if err := j.start(script, nil, "libvirt"); err == nil {
		t.Fatal("a second install started while the first was running")
	}
	deadline := time.Now().Add(5 * time.Second)
	for j.status(0)["state"] == "running" && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	st := j.status(0)
	if st["state"] != "failed" || st["exit"] != 3 || st["step"] != 2 || st["failedStep"] != 2 {
		t.Fatalf("status %+v", st)
	}
	if lines := st["lines"].([]string); len(lines) != 4 || lines[1] != "args --yes" {
		t.Fatalf("lines %q", lines)
	}
}

func TestScreenLine(t *testing.T) {
	for in, want := range map[string]string{
		"(Reading database ... \r(Reading database ... 5%\r(Reading database ... 100%\r": "(Reading database ... 100%",
		"\x1b[1;32mok\x1b[0m done": "ok done",
		"plain":                    "plain",
	} {
		if got := screenLine(in); got != want {
			t.Errorf("screenLine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestJobTracksSteps(t *testing.T) {
	j := newJob()
	for _, l := range []string{"=== Step 1: nodes ===", "x", "=== Step 2: drbd ===", "Step 2 failed. Fix the cause above"} {
		j.add(l, nil)
	}
	st := j.status(1)
	if st["step"] != 2 || st["failedStep"] != 2 || len(st["lines"].([]string)) != 3 || st["next"] != 4 {
		t.Fatalf("status %+v", st)
	}
}
