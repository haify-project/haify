// Command haify-csi is the Haify CSI driver for Kubernetes, in one binary:
//
//	haify-csi controller   the controller plugin (one Deployment per cluster)
//	haify-csi node         the node plugin (a DaemonSet, one per node)
//	haify-csi drbd-module  DRBD module checks for the loader and node plugin
//
// Each subcommand takes its own flags; `haify-csi <subcommand> -h` lists them.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/haify-project/haify/pkg/logging"
	"go.uber.org/zap"
)

const usage = `usage: haify-csi <command> [flags]

Commands:
  controller   run the CSI controller plugin
  node         run the CSI node plugin
  drbd-module  check, wait for or load the DRBD kernel module (see haify-csi drbd-module)
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches to a subcommand and returns the process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "controller":
		return runController(args[1:])
	case "node":
		return runNode(args[1:])
	case "drbd-module":
		log := logging.Production()
		code := runDRBDModule(log, args[1:], stdout, stderr)
		_ = log.Sync()
		return code
	case "-h", "--help", "help":
		_, _ = fmt.Fprint(stdout, usage)
		return 0
	default:
		zap.L().Debug("run: unknown command", zap.String("command", args[0]))
		_, _ = fmt.Fprintf(stderr, "unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}
