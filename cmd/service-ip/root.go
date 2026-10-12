//go:build linux

package main

import (
	"log/slog"
	"os"

	"github.com/spf13/cobra"
)

var (
	targetIP       string
	targetDev      string
	targetInstance string
	debug          bool
)

var RootCmd = &cobra.Command{
	Use:   "service-ip",
	Short: "A lightweight HA IP manager",
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		level := slog.LevelInfo
		if debug {
			level = slog.LevelDebug
		}
		logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
		slog.SetDefault(logger)
		slog.Debug("RootCmd.PersistentPreRun: logger installed", "command", cmd.Name(), "debug", debug, "args", args)
	},
}

func Execute() {
	if err := RootCmd.Execute(); err != nil {
		slog.Debug("Execute: command failed, exiting", "error", err)
		os.Exit(1)
	}
}

func init() {
	RootCmd.PersistentFlags().StringVar(&targetIP, "ip", "", "Virtual IP (CIDR format)")
	RootCmd.PersistentFlags().StringVar(&targetDev, "dev", "", "Interface device (optional)")
	RootCmd.PersistentFlags().StringVarP(&targetInstance, "instance", "i", "", "Systemd instance name (e.g. 192.168.1.1-24_eth0)")
	RootCmd.PersistentFlags().BoolVar(&debug, "debug", false, "Enable debug logging")
}
