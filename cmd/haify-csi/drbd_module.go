package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os/signal"
	"syscall"
	"time"

	"github.com/haify-project/haify/pkg/csi/drbdmodule"
	"go.uber.org/zap"
)

// drbdModuleUsage is printed for `haify-csi drbd-module` without a valid
// command.
const drbdModuleUsage = `usage: haify-csi drbd-module <command> [flags]

Commands used by the haify-csi chart's DRBD module loader and node plugin:
  check     print the loaded DRBD version; exit 1 if it is older than --min-version
  wait      poll until DRBD --min-version or newer is loaded
  prepare   before the loader runs: block the host's own drbd module and unload
            an older, unused one
  bring-up  after the loader loaded DRBD: run the node's haify-drbd-up.service
  hold      keep the loader pod running until it is stopped
`

// runDRBDModule runs one drbd-module command and returns the process exit
// code. User-facing lines go to stdout, which is what `kubectl logs` shows;
// the zap traces go to the logger.
func runDRBDModule(log *zap.Logger, args []string, stdout, stderr io.Writer) int {
	log.Debug("runDRBDModule: start", zap.Strings("args", args))
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, drbdModuleUsage)
		return 2
	}
	cmd := args[0]
	fs := flag.NewFlagSet("drbd-module "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	minFlag := fs.String("min-version", "9.2.0", "oldest DRBD kernel module version Haify accepts")
	modprobeDir := fs.String("modprobe-dir", "", "the host's /etc/modprobe.d as mounted in this container; empty: write nothing there")
	stateDir := fs.String("state-dir", "/run/haify-drbd-loader", "directory shared by the loader pod's containers")
	interval := fs.Duration("interval", 5*time.Second, "wait: how often to look")
	timeout := fs.Duration("timeout", 0, "wait, bring-up: give up after this long; 0 waits for ever (wait) or 5m (bring-up)")
	if err := fs.Parse(args[1:]); err != nil {
		log.Debug("runDRBDModule: bad flags", zap.String("command", cmd), zap.Error(err))
		return 2
	}
	want, err := drbdmodule.ParseVersion(*minFlag)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "--min-version: %v\n", err)
		return 2
	}
	out := func(s string) { _, _ = fmt.Fprintln(stdout, s) }
	h := drbdmodule.New(log, *modprobeDir, *stateDir)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "check":
		msg, ok, err := h.Check(want)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
		out(msg)
		if !ok {
			return 1
		}
		return 0
	case "wait":
		if *timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, *timeout)
			defer cancel()
		}
		if err := h.Wait(ctx, want, *interval, out); err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	case "prepare":
		if _, err := h.Prepare(ctx, want, out); err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	case "bring-up":
		d := *timeout
		if d == 0 {
			d = 5 * time.Minute
		}
		bctx, cancel := context.WithTimeout(ctx, d)
		defer cancel()
		if err := h.BringUp(bctx, out); err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	case "hold":
		// The loader pod's only long-running container: it keeps the pod (and
		// so the DaemonSet) Ready while its readiness probe runs `check`.
		msg, _, err := h.Check(want)
		if err != nil {
			msg = err.Error()
		}
		out(msg)
		<-ctx.Done()
		log.Debug("runDRBDModule: hold stopped")
		return 0
	}
	log.Debug("runDRBDModule: unknown command", zap.String("command", cmd))
	_, _ = fmt.Fprintf(stderr, "unknown command %q\n\n%s", cmd, drbdModuleUsage)
	return 2
}
