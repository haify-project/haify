//go:build linux

package main

import (
	"log/slog"
	"os"
	"time"

	"github.com/haify-project/haify/pkg/serviceip/ip"
	"github.com/spf13/cobra"
)

var downCmd = &cobra.Command{
	Use:   "down",
	Short: "Take down the virtual IP",
	Run: func(cmd *cobra.Command, args []string) {
		slog.Debug("down: start", "ip", targetIP, "dev", targetDev, "instance", targetInstance)
		if targetInstance != "" {
			parsedIP, parsedDev := ParseInstanceID(targetInstance)
			if targetIP == "" {
				targetIP = parsedIP
			}
			if targetDev == "" {
				targetDev = parsedDev
			}
			slog.Debug("Parsed instance", "instance", targetInstance, "ip", targetIP, "dev", targetDev)
		}

		if targetIP == "" {
			slog.Debug("down: no IP given, exiting", "instance", targetInstance)
			slog.Error("--ip or --instance is required")
			os.Exit(1)
		}

		slog.Info("Removing IP", "ip", targetIP, "dev", targetDev)
		start := time.Now()
		err := ip.RemoveIP(targetDev, targetIP)
		slog.Debug("down: RemoveIP returned", "ip", targetIP, "dev", targetDev, "error", err, "took", time.Since(start))
		if err != nil {
			slog.Error("Failed to remove IP", "error", err)
			os.Exit(1)
		}

		slog.Info("IP removed")
	},
}

func init() {
	RootCmd.AddCommand(downCmd)
}
