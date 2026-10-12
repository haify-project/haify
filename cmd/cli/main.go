package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/haify-project/haify/pkg/client"
	"github.com/haify-project/haify/pkg/logging"
)

var (
	controllerAddr string
	tokenFlag      string
	debugFlag      bool
	tlsFlags       client.TLSOptions
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "haify",
		Short: "Haify CLI - Software Defined Storage Management",
		// main prints the error, once.
		SilenceErrors: true,
		// Usage is for a command line that did not parse. Once a command runs,
		// its error comes from the cluster, and forty lines of flags printed
		// after it bury the one line that matters. Cobra validates arguments
		// before this hook and required flags after it; main covers those.
		PersistentPreRun: func(cmd *cobra.Command, _ []string) {
			cmd.SilenceUsage = true
			setupLogging()
		},
	}

	rootCmd.PersistentFlags().StringVarP(&controllerAddr, "controller", "c", "127.0.0.1:3374", "Controller address")
	rootCmd.PersistentFlags().BoolVar(&debugFlag, "debug", false, "Trace every step and controller call on stderr (env HAIFY_LOG_LEVEL=debug)")
	rootCmd.PersistentFlags().StringVar(&tokenFlag, "token", "", "API token (default: HAIFY_TOKEN env, ~/.haify/token, /etc/haify/token)")

	// Transport security, matching the controller's [tls] section. Any of the
	// material flags implies --tls, so an operator who points at a CA cannot
	// end up talking plaintext to a TLS controller and reading the handshake
	// error as an outage.
	rootCmd.PersistentFlags().BoolVar(&tlsFlags.Enabled, "tls", false, "Connect over TLS (implied by --tls-ca/--tls-cert; env HAIFY_TLS)")
	rootCmd.PersistentFlags().StringVar(&tlsFlags.CACert, "tls-ca", "", "CA bundle that signed the controller certificate (env HAIFY_TLS_CA; default: system trust store)")
	rootCmd.PersistentFlags().StringVar(&tlsFlags.ClientCert, "tls-cert", "", "Client certificate, for a controller requiring mutual TLS (env HAIFY_TLS_CERT)")
	rootCmd.PersistentFlags().StringVar(&tlsFlags.ClientKey, "tls-key", "", "Client private key (env HAIFY_TLS_KEY)")
	rootCmd.PersistentFlags().StringVar(&tlsFlags.ServerName, "tls-server-name", "", "Name to verify against the controller certificate (env HAIFY_TLS_SERVER_NAME)")
	rootCmd.PersistentFlags().BoolVar(&tlsFlags.Insecure, "tls-insecure", false, "Encrypt but do NOT verify the controller — accepts any certificate (env HAIFY_TLS_INSECURE)")

	rootCmd.AddCommand(poolCommand())
	rootCmd.AddCommand(nodeCommand())
	rootCmd.AddCommand(resourceCommand())
	rootCmd.AddCommand(haCommand())
	rootCmd.AddCommand(gatewayCommand())
	rootCmd.AddCommand(shipCommand())
	rootCmd.AddCommand(appCommand())
	rootCmd.AddCommand(healthCommand())
	rootCmd.AddCommand(rbacCommand())
	rootCmd.AddCommand(approvalCommand())
	rootCmd.AddCommand(rebalanceCommand())
	rootCmd.AddCommand(eventCommand())
	rootCmd.AddCommand(inspectCommand())
	rootCmd.AddCommand(notifyCommand())
	rootCmd.AddCommand(wanCommand())
	rootCmd.AddCommand(backupCommand())
	rootCmd.AddCommand(replicationTLSCommand())

	if cmd, err := rootCmd.ExecuteC(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		if strings.HasPrefix(err.Error(), "required flag") {
			fmt.Fprint(os.Stderr, "\n"+cmd.UsageString())
		}
		os.Exit(1)
	}
}

// setupLogging installs the process-wide logger the CLI's traces go to:
// stderr at debug with --debug, at HAIFY_LOG_LEVEL when that is set, and
// nowhere otherwise, so normal output stays clean.
func setupLogging() {
	level := logging.ParseLevel(os.Getenv(logging.EnvLevel), zapcore.InvalidLevel)
	if debugFlag {
		level = zapcore.DebugLevel
	}
	if level == zapcore.InvalidLevel {
		logging.Install(zap.NewNop())
		return
	}
	logging.Console(level).Debug("haify cli start", zap.Strings("args", redactArgs(os.Args[1:])), zap.String("controller", controllerAddr))
}

// redactArgs hides the value of every flag that can carry a secret.
func redactArgs(args []string) []string {
	secret := func(flag string) bool {
		f := strings.ToLower(strings.TrimLeft(flag, "-"))
		for _, word := range []string{"token", "password", "secret", "key", "passphrase", "credential"} {
			if strings.Contains(f, word) {
				return true
			}
		}
		return false
	}
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = a
		if name, _, ok := strings.Cut(a, "="); ok && strings.HasPrefix(a, "-") && secret(name) {
			out[i] = name + "=<redacted>"
		} else if i > 0 && strings.HasPrefix(args[i-1], "-") && !strings.Contains(args[i-1], "=") && secret(args[i-1]) {
			out[i] = "<redacted>"
		}
	}
	return out
}
