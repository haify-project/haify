//go:build linux

package main

import (
	"log/slog"
	"os"
	"time"

	"github.com/haify-project/haify/pkg/serviceip/ip"
	"github.com/spf13/cobra"
)

// OCF Exit Codes
const (
	OCF_SUCCESS     = 0
	OCF_ERR_GENERIC = 1
	OCF_NOT_RUNNING = 7
)

var monitorCmd = &cobra.Command{
	Use:   "monitor",
	Short: "Check if the virtual IP is up",
	Run: func(cmd *cobra.Command, args []string) {
		slog.Debug("monitor: start", "ip", targetIP, "dev", targetDev, "instance", targetInstance)
		if targetInstance != "" {
			parsedIP, parsedDev := ParseInstanceID(targetInstance)
			if targetIP == "" {
				targetIP = parsedIP
			}
			if targetDev == "" {
				targetDev = parsedDev
			}
			slog.Debug("monitor: parsed instance", "instance", targetInstance, "ip", targetIP, "dev", targetDev)
		}

		if targetIP == "" {
			slog.Debug("monitor: no IP given, exiting", "instance", targetInstance, "exit_code", OCF_ERR_GENERIC)
			slog.Error("--ip or --instance is required")
			os.Exit(OCF_ERR_GENERIC)
		}

		start := time.Now()
		exists, err := ip.CheckIP(targetDev, targetIP)
		slog.Debug("monitor: CheckIP returned", "ip", targetIP, "dev", targetDev, "exists", exists, "error", err, "took", time.Since(start))
		if err != nil {
			slog.Error("Error checking IP", "error", err)
			os.Exit(OCF_ERR_GENERIC)
		}

		if exists {
			slog.Debug("monitor: IP present, exiting", "ip", targetIP, "exit_code", OCF_SUCCESS)
			slog.Info("IP is present")
			os.Exit(OCF_SUCCESS)
		} else {
			slog.Debug("monitor: IP absent, exiting", "ip", targetIP, "exit_code", OCF_NOT_RUNNING)
			slog.Info("IP is NOT present")
			os.Exit(OCF_NOT_RUNNING)
		}
	},
}

func init() {
	RootCmd.AddCommand(monitorCmd)
}
