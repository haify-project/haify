package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// choices is what the wizard sends: the same decisions the script's options
// carry.
type choices struct {
	Env          string              `json:"env"`
	Nodes        map[string]string   `json:"nodes"` // name -> address
	StorageNodes []string            `json:"storageNodes"`
	Devices      map[string][]string `json:"devices"` // node -> disks
	Pool         string              `json:"pool"`
	PoolType     string              `json:"poolType"`
	Replicas     int                 `json:"replicas"`
	SelfHA       bool                `json:"selfHA"`
	VIP          string              `json:"vip"`
}

// safeWord keeps what reaches the script's command line to the characters
// names, addresses and device paths are made of; the script quotes again
// whatever it sends to a node.
var safeWord = regexp.MustCompile(`^[A-Za-z0-9_.:/@%+=-]+$`)

func (c choices) args() ([]string, error) {
	check := func(what, v string) error {
		if !safeWord.MatchString(v) {
			return fmt.Errorf("%s %q has characters the installer does not accept", what, v)
		}
		return nil
	}
	if err := check("platform", c.Env); err != nil {
		return nil, err
	}
	args := []string{"--env", c.Env}
	names := make([]string, 0, len(c.Nodes))
	for n := range c.Nodes {
		names = append(names, n)
	}
	sort.Strings(names)
	var nodes []string
	for _, n := range names {
		if err := check("node", n); err != nil {
			return nil, err
		}
		if err := check("address", c.Nodes[n]); err != nil {
			return nil, err
		}
		nodes = append(nodes, n+"="+c.Nodes[n])
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("no nodes")
	}
	// Proxmox reads its members from the cluster itself.
	if c.Env != "proxmox" {
		args = append(args, "--nodes", strings.Join(nodes, ","))
	}
	if len(c.StorageNodes) > 0 {
		for _, n := range c.StorageNodes {
			if err := check("node", n); err != nil {
				return nil, err
			}
		}
		args = append(args, "--storage-nodes", strings.Join(c.StorageNodes, ","))
	}
	for _, n := range sortedKeys(c.Devices) {
		for _, d := range c.Devices[n] {
			if err := check("disk", d); err != nil {
				return nil, err
			}
		}
		if len(c.Devices[n]) > 0 {
			args = append(args, "--node-devices", n+"="+strings.Join(c.Devices[n], ","))
		}
	}
	if c.Pool != "" {
		if err := check("pool", c.Pool); err != nil {
			return nil, err
		}
		args = append(args, "--pool", c.Pool)
	}
	switch c.PoolType {
	case "", "lvm-thin", "lvm":
		if c.PoolType != "" {
			args = append(args, "--pool-type", c.PoolType)
		}
	default:
		return nil, fmt.Errorf("pool type %q", c.PoolType)
	}
	if c.Replicas > 0 {
		args = append(args, "--replicas", strconv.Itoa(c.Replicas))
	}
	if c.SelfHA {
		if err := check("floating address", c.VIP); err != nil {
			return nil, err
		}
		args = append(args, "--vip", c.VIP)
	} else {
		args = append(args, "--no-self-ha")
	}
	return args, nil
}

func sortedKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// job is the one install a wizard runs. Its output is kept in memory for the
// page and appended to a log file for after.
type job struct {
	mu      sync.Mutex
	state   string // idle, running, done, failed
	exit    int
	env     string
	lines   []string
	step    int
	failed  int
	started time.Time
}

func newJob() *job { return &job{state: "idle"} }

var stepLine = regexp.MustCompile(`^=== Step (\d+): `)
var failedLine = regexp.MustCompile(`^Step (\d+) failed`)

func (j *job) start(script string, args []string, env string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.state == "running" {
		return fmt.Errorf("an install is already running")
	}
	cmd := exec.Command("bash", append([]string{script}, args...)...)
	cmd.Dir = workDir()
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		return err
	}
	// Field by field: assigning a whole job would overwrite the held mutex.
	j.state, j.exit, j.env, j.started = "running", 0, env, time.Now()
	j.lines, j.step, j.failed = nil, 0, 0
	_ = os.MkdirAll("/var/log/haify", 0o755)
	logf, _ := os.OpenFile("/var/log/haify/install.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			j.add(sc.Text(), logf)
		}
		if err := sc.Err(); err != nil {
			j.add("reading the installer's output: "+err.Error(), logf)
			_, _ = io.Copy(io.Discard, pr) // the script must not block on a full pipe
		}
	}()
	go func() {
		err := cmd.Wait()
		_ = pw.Close()
		time.Sleep(200 * time.Millisecond) // let the reader drain the pipe
		j.mu.Lock()
		defer j.mu.Unlock()
		j.state, j.exit = "done", 0
		if err != nil {
			j.state, j.exit = "failed", cmd.ProcessState.ExitCode()
		}
		if logf != nil {
			_ = logf.Close()
		}
	}()
	return nil
}

// ansi matches terminal colour and cursor sequences (apt, dkms).
var ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// screenLine is what a terminal would end up showing for line: progress
// counters rewrite it after each \r, so only the last version is kept.
func screenLine(line string) string {
	line = ansi.ReplaceAllString(strings.TrimRight(line, "\r"), "")
	if i := strings.LastIndexByte(line, '\r'); i >= 0 {
		line = line[i+1:]
	}
	return line
}

func (j *job) add(line string, logf *os.File) {
	line = screenLine(line)
	j.mu.Lock()
	defer j.mu.Unlock()
	j.lines = append(j.lines, line)
	if m := stepLine.FindStringSubmatch(line); m != nil {
		j.step, _ = strconv.Atoi(m[1])
	}
	if m := failedLine.FindStringSubmatch(line); m != nil {
		j.failed, _ = strconv.Atoi(m[1])
	}
	if logf != nil {
		_, _ = fmt.Fprintln(logf, line)
	}
}

func (j *job) status(from int) map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	if from < 0 || from > len(j.lines) {
		from = 0
	}
	return map[string]any{
		"state": j.state, "exit": j.exit, "env": j.env, "step": j.step, "failedStep": j.failed,
		"lines": j.lines[from:], "next": len(j.lines), "elapsed": int(time.Since(j.started).Seconds()),
	}
}
