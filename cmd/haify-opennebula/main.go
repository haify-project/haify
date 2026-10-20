// haify-opennebula is the OpenNebula storage driver for Haify. OpenNebula
// runs it through the one-line action scripts in deploy/opennebula, as
//
//	haify-opennebula ds <action> [args]   datastore driver (DS_MAD=haify)
//	haify-opennebula tm <action> [args]   transfer driver (TM_MAD=haify)
//
// on the frontend, as oneadmin. A datastore action reads its data from stdin
// and prints its result on stdout; errors go to stderr, where OpenNebula
// shows them.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/haify-project/haify/pkg/client"
	"github.com/haify-project/haify/pkg/logging"
	"github.com/haify-project/haify/pkg/opennebula"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const usage = `usage: haify-opennebula [--remotes DIR] [--debug] ds|tm <action> [args]
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("haify-opennebula", flag.ContinueOnError)
	fl.SetOutput(stderr)
	remotes := fl.String("remotes", "/var/lib/one/remotes", "OpenNebula's remotes directory")
	debug := fl.Bool("debug", false, "trace every step on stderr (or HAIFY_LOG_LEVEL=debug)")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	level := logging.ParseLevel(os.Getenv(logging.EnvLevel), zapcore.WarnLevel)
	if *debug {
		level = zapcore.DebugLevel
	}
	log := logging.Console(level)
	defer func() { _ = log.Sync() }()
	rest := fl.Args()
	if len(rest) < 2 {
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}
	kind, action, actionArgs := rest[0], rest[1], rest[2:]
	log.Debug("run: starting", zap.String("kind", kind), zap.String("action", action), zap.Strings("args", actionArgs),
		zap.String("remotes", *remotes))

	timeout := 30 * time.Minute
	if action == "cp" || action == "clone" || action == "cpds" {
		timeout = 6 * time.Hour // a large image copied over the network
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	d := &opennebula.Driver{
		Connect: connect,
		One:     execOne{},
		Shell:   shell{},
		Remotes: *remotes,
		Delegate: func(ctx context.Context, tm, action string, args []string, data []byte) error {
			return delegate(ctx, *remotes, tm, action, args, data, stdin, stdout, stderr)
		},
	}
	out, err := dispatch(ctx, d, kind, action, actionArgs, stdin)
	if err != nil {
		log.Debug("run: action failed", zap.String("kind", kind), zap.String("action", action), zap.Error(err))
		_, _ = fmt.Fprintf(stderr, "haify: %s %s: %v\n", kind, action, err)
		return 1
	}
	if out != "" {
		_, _ = fmt.Fprintln(stdout, out)
	}
	log.Debug("run: done", zap.String("kind", kind), zap.String("action", action), zap.String("output", out))
	return 0
}

// dispatch runs one action and returns what it prints.
func dispatch(ctx context.Context, d *opennebula.Driver, kind, action string, args []string, stdin io.Reader) (string, error) {
	readAll := func() ([]byte, error) {
		data, err := io.ReadAll(stdin)
		zap.L().Debug("dispatch: read stdin", zap.Int("bytes", len(data)), zap.Error(err))
		return data, err
	}
	switch kind {
	case "ds":
		data, err := readAll()
		if err != nil {
			return "", err
		}
		switch action {
		case "cp":
			return d.DSCp(ctx, data)
		case "mkfs":
			return d.DSMkfs(ctx, data)
		case "rm":
			return "", d.DSRm(ctx, data)
		case "clone":
			return d.DSClone(ctx, data)
		case "monitor":
			return d.DSMonitor(ctx, data)
		case "snap_delete", "snap_revert", "snap_flatten":
			return "", d.DSSnapshot(ctx, action, data)
		}
	case "tm":
		switch action {
		case "ln":
			return "", d.TMLink(ctx, false, args)
		case "clone":
			return "", d.TMLink(ctx, true, args)
		case "delete":
			return "", d.TMDelete(ctx, args)
		case "mv":
			return "", d.TMMv(ctx, args)
		case "mvds":
			return "", d.TMMvds(ctx, args)
		case "cpds":
			return "", d.TMCpds(ctx, args)
		case "resize":
			return "", d.TMResize(ctx, args)
		case "snap_create", "snap_create_live", "snap_revert", "snap_delete":
			return "", d.TMSnapshot(ctx, action, args)
		case "premigrate", "postmigrate", "failmigrate":
			data, err := readAll()
			if err != nil {
				return "", err
			}
			return "", d.TMMigrate(ctx, action, args, data)
		}
	}
	zap.L().Debug("dispatch: unknown action", zap.String("kind", kind), zap.String("action", action))
	return "", fmt.Errorf("unsupported action")
}

// connect dials the controller at addr (host or host:port, port 3374) with
// the API token from HAIFY_TOKEN, ~/.haify/token or /etc/haify/token.
func connect(addr string) (opennebula.Backend, error) {
	if !strings.Contains(addr, ":") {
		addr += ":3374"
	}
	token := client.ResolveToken("")
	zap.L().Debug("connect: dialing", zap.String("controller", addr), zap.Bool("has_token", token != ""))
	return client.NewHaifyClient(addr, client.WithToken(token))
}

// delegate runs another transfer driver's action script, passing data (or,
// when nil, this process's stdin) to it.
func delegate(ctx context.Context, remotes, tm, action string, args []string, data []byte, stdin io.Reader,
	stdout, stderr io.Writer) error {
	script := filepath.Join(remotes, "tm", tm, action)
	cmd := exec.CommandContext(ctx, script, args...)
	cmd.Stdin = stdin
	if data != nil {
		cmd.Stdin = bytes.NewReader(data)
	}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	start := time.Now()
	err := cmd.Run()
	zap.L().Debug("delegate: ran", zap.String("script", script), zap.Strings("args", args),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("%s/%s: %w", tm, action, err)
	}
	return nil
}

// execOne runs OpenNebula's CLI.
type execOne struct{}

func (execOne) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// shell runs bash locally or on a host over SSH.
type shell struct{}

func (shell) Remote(ctx context.Context, host, script string) (string, error) {
	cmd := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=15", host, "bash", "-s")
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (shell) Local(ctx context.Context, env []string, script string) (string, error) {
	cmd := exec.CommandContext(ctx, "bash", "-c", script)
	cmd.Env = append(os.Environ(), env...)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		err = fmt.Errorf("exit status %d", exit.ExitCode())
	}
	return buf.String(), err
}
