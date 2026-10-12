// Command haify-mcp exposes the Haify controller as a Model Context Protocol
// (MCP) server over stdio, so AI assistants (Claude Code, Claude Desktop,
// and other MCP clients) can inspect and manage storage.
//
// Register with an MCP client, e.g.:
//
//	claude mcp add haify -- haify-mcp --controller node1:3374
//
// All logs go to stderr; stdout carries the MCP protocol.
package main

import (
	"fmt"
	"os"

	"github.com/haify-project/haify/pkg/logging"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/haify-project/haify/pkg/k8sapp"
	"github.com/haify-project/haify/pkg/mcpserver"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	var (
		conn       controllerConn
		readOnly   bool
		allowWrite []string
		debug      bool
	)

	rootCmd := &cobra.Command{
		Use:           "haify-mcp",
		Short:         "MCP server for the Haify storage controller",
		Long:          "Serves Haify storage management tools over the Model Context Protocol (stdio transport).",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			logger, err := newStderrLogger(debug)
			if err != nil {
				return fmt.Errorf("init logger: %w", err)
			}
			defer func() { _ = logger.Sync() }()

			haifyClient, err := conn.dial()
			if err != nil {
				return err
			}
			defer func() { _ = haifyClient.Close() }()

			srv := mcpserver.New(haifyClient, logger, mcpserver.Options{
				ReadOnly:   readOnly,
				AllowWrite: allowWrite,
				Version:    version,
			})
			return srv.Run(cmd.Context())
		},
	}

	conn.register(rootCmd, "")
	rootCmd.Flags().BoolVar(&readOnly, "read-only", false, "register only read-only tools (list/status/health)")
	rootCmd.Flags().StringSliceVar(&allowWrite, "allow", nil,
		"mutating tools to register by name despite --read-only, e.g. --allow haify_ha_evict. "+
			"Implies --read-only. Refuses to start on a name no tool answers to")
	rootCmd.Flags().BoolVar(&debug, "debug", false, "enable debug logging on stderr")

	rootCmd.AddCommand(k8sCmd())
	rootCmd.AddCommand(serveCmd(), tokenCmd())

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// newStderrLogger builds a console logger writing to stderr only.
// stdout must stay clean: it carries the MCP stdio protocol.
func newStderrLogger(debug bool) (*zap.Logger, error) {
	cfg := zap.NewProductionConfig()
	cfg.OutputPaths = []string{"stderr"}
	cfg.ErrorOutputPaths = []string{"stderr"}
	cfg.Encoding = "console"
	cfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	if debug {
		cfg.Level = zap.NewAtomicLevelAt(zap.DebugLevel)
	}
	logger, err := cfg.Build()
	if err != nil {
		return nil, err
	}
	return logging.Install(logger), nil
}

// k8sCmd serves the Kubernetes (CSI) tools as their own MCP server, haify-k8s.
// It talks to a Kubernetes API server, not the Haify controller.
func k8sCmd() *cobra.Command {
	var (
		kubeconfig string
		readOnly   bool
		allowWrite []string
		debug      bool
	)
	cmd := &cobra.Command{
		Use:   "k8s",
		Short: "MCP server for Haify on Kubernetes (haify_k8s_* tools)",
		RunE: func(cmd *cobra.Command, args []string) error {
			logger, err := newStderrLogger(debug)
			if err != nil {
				return fmt.Errorf("init logger: %w", err)
			}
			defer func() { _ = logger.Sync() }()
			apps, err := k8sapp.NewManager(kubeconfig)
			if err != nil {
				return err
			}
			if apps == nil {
				return fmt.Errorf("no Kubernetes cluster: pass --kubeconfig (or HAIFY_KUBECONFIG), or run inside a pod")
			}
			return mcpserver.NewK8s(apps, logger, mcpserver.Options{
				ReadOnly:   readOnly,
				AllowWrite: allowWrite,
				Version:    version,
			}).Run(cmd.Context())
		},
	}
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", os.Getenv("HAIFY_KUBECONFIG"),
		"kubeconfig (env HAIFY_KUBECONFIG; in-cluster config inside a pod when empty)")
	cmd.Flags().BoolVar(&readOnly, "read-only", false, "register only read-only tools")
	cmd.Flags().StringSliceVar(&allowWrite, "allow", nil, "mutating tools to register despite --read-only")
	cmd.Flags().BoolVar(&debug, "debug", false, "enable debug logging on stderr")
	return cmd
}
