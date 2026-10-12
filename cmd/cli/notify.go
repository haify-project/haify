package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/client"
)

// notifySecretEnvVar carries a DingTalk signing secret, for the same reason
// HAIFY_BACKUP_SECRET exists: a flag would put it in shell history and in argv.
const notifySecretEnvVar = "HAIFY_NOTIFY_SECRET"

func notifyCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "channel",
		Aliases: []string{"channels"},
		Short:   "Manage where alerts are delivered (Feishu, Slack, WeCom, DingTalk, or your own receiver)",
		Long: `Manage notification channels.

A channel has a KIND, because a chat service will not accept an arbitrary JSON
document: Feishu, Slack, WeCom and DingTalk each define their own message
envelope. Worse, Feishu, WeCom and DingTalk report a refusal inside an HTTP 200,
so a channel with the wrong kind looks like it is delivering and is not. Use
"test" after adding one — it reports what the far end actually said.

Channels live in the controller database, not in controller.toml, so adding or
muting one takes effect immediately and needs no restart. The [alert] receivers
in the config file still work and are independent of these.`,
	}
	cmd.AddCommand(notifyListCommand(), notifyAddCommand(), notifyDeleteCommand(), notifyTestCommand())
	return cmd
}

func notifyListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List notification channels",
		RunE: func(cmd *cobra.Command, _ []string) error {
			log := zap.L()
			log.Debug("notifyListCommand: running command")
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			c, err := newHaifyClient()
			if err != nil {
				log.Debug("notifyListCommand: newHaifyClient failed", zap.Error(err))
				return err
			}
			defer func() { _ = c.Close() }()

			channels, kinds, err := c.ListNotifyChannels(ctx)
			if err != nil {
				log.Debug("notifyListCommand: ListNotifyChannels failed", zap.Error(err))
				return err
			}
			if len(channels) == 0 {
				log.Debug("notifyListCommand: no channels", zap.Strings("kinds", kinds))
				fmt.Println("No notification channels configured.")
				fmt.Printf("Supported kinds: %s\n", strings.Join(kinds, ", "))
				return nil
			}
			for _, ch := range channels {
				log.Debug("notifyListCommand: channel", zap.String("name", ch.Name), zap.String("kind", ch.Kind),
					zap.Bool("enabled", ch.Enabled), zap.String("min_severity", ch.MinSeverity), zap.Bool("has_secret", ch.HasSecret))
				state := "enabled"
				if !ch.Enabled {
					state = "MUTED"
				}
				fmt.Printf("%s\n", ch.Name)
				fmt.Printf("  %s %s [%s]\n", ch.Kind, ch.Url, state)
				min := ch.MinSeverity
				if min == "" {
					min = "info"
				}
				fmt.Printf("  min severity: %s\n", min)
				if len(ch.Types) > 0 {
					fmt.Printf("  types: %s\n", strings.Join(ch.Types, ", "))
				}
				if ch.HasSecret {
					fmt.Printf("  signing secret: configured\n")
				}
			}
			log.Debug("notifyListCommand: done", zap.Int("count", len(channels)))
			return nil
		},
	}
}

func notifyAddCommand() *cobra.Command {
	var (
		name, kind, url, minSeverity, secretFile string
		types                                    []string
		headers                                  []string
		disabled, clearSecret                    bool
	)
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Add or replace a notification channel",
		Long: `Add a notification channel, or replace one with the same name.

The DingTalk signing secret is never a flag: it is read from ` + notifySecretEnvVar + `
or from --secret-file ("-" reads stdin). Re-running add without supplying one
LEAVES THE STORED SECRET ALONE, so changing a threshold does not silently
unsign the channel; --clear-secret removes it.

Examples:
  haify channel add --name oncall --kind feishu \
      --url https://open.feishu.cn/open-apis/bot/v2/hook/xxxx --min-severity warning

  haify channel add --name pager --kind slack \
      --url https://hooks.slack.com/services/T00/B00/xxxx --min-severity critical

  export ` + notifySecretEnvVar + `=SECxxxx
  haify channel add --name ops --kind dingtalk \
      --url 'https://oapi.dingtalk.com/robot/send?access_token=xxxx'`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			log := zap.L()
			log.Debug("notifyAddCommand: running command", zap.String("name", name), zap.String("kind", kind),
				zap.Bool("has_url", url != ""), zap.String("min_severity", minSeverity), zap.Strings("types", types),
				zap.Int("headers", len(headers)), zap.Bool("muted", disabled), zap.Bool("has_secret_file", secretFile != ""),
				zap.Bool("clear_secret", clearSecret))
			if name == "" || url == "" {
				log.Debug("notifyAddCommand: --name or --url missing", zap.Bool("has_name", name != ""), zap.Bool("has_url", url != ""))
				return fmt.Errorf("--name and --url are required")
			}
			hdrs, err := parseHeaderFlags(headers)
			if err != nil {
				log.Debug("notifyAddCommand: parseHeaderFlags failed", zap.Error(err))
				return err
			}
			// Absent means "keep what is stored"; the empty string would be
			// indistinguishable from an explicit clear, which is why
			// --clear-secret exists as its own flag.
			secret, err := readOptionalNotifySecret(cmd.InOrStdin(), secretFile)
			if err != nil {
				log.Debug("notifyAddCommand: readOptionalNotifySecret failed", zap.Error(err))
				return err
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			c, err := newHaifyClient()
			if err != nil {
				log.Debug("notifyAddCommand: newHaifyClient failed", zap.Error(err))
				return err
			}
			defer func() { _ = c.Close() }()

			ch, err := c.SaveNotifyChannel(ctx, clientNotifySpec(name, kind, url, minSeverity,
				types, hdrs, !disabled, secret, clearSecret))
			if err != nil {
				log.Debug("notifyAddCommand: SaveNotifyChannel failed", zap.Error(err))
				return err
			}
			fmt.Printf("Notification channel %q saved (%s)\n", ch.Name, ch.Kind)
			fmt.Printf("Send a test message with: haify channel test %s\n", ch.Name)
			log.Debug("notifyAddCommand: done", zap.String("name", ch.Name), zap.String("kind", ch.Kind), zap.Bool("has_secret", secret != ""))
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Channel name (required)")
	cmd.Flags().StringVar(&kind, "kind", "generic",
		"Message format: generic, feishu, slack, wecom or dingtalk")
	cmd.Flags().StringVar(&url, "url", "", "Bot or receiver URL (required)")
	cmd.Flags().StringVar(&minSeverity, "min-severity", "info",
		"Drop anything less severe: info, warning or critical")
	cmd.Flags().StringSliceVar(&types, "type", nil,
		"Only these event types, e.g. resource.failover,node.unreachable (default: all)")
	cmd.Flags().StringSliceVar(&headers, "header", nil,
		"Extra request header as Name=Value (repeatable)")
	cmd.Flags().BoolVar(&disabled, "muted", false, "Store the channel but do not deliver to it")
	cmd.Flags().StringVar(&secretFile, "secret-file", "",
		"Read the DingTalk signing secret from this file (\"-\" for stdin); default: the "+
			notifySecretEnvVar+" environment variable")
	cmd.Flags().BoolVar(&clearSecret, "clear-secret", false, "Remove the stored signing secret")
	return cmd
}

func notifyDeleteCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a notification channel",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("notifyDeleteCommand: running command", zap.Strings("args", args))
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			c, err := newHaifyClient()
			if err != nil {
				log.Debug("notifyDeleteCommand: newHaifyClient failed", zap.Error(err))
				return err
			}
			defer func() { _ = c.Close() }()

			if err := c.DeleteNotifyChannel(ctx, args[0]); err != nil {
				log.Debug("notifyDeleteCommand: DeleteNotifyChannel failed", zap.Error(err))
				return err
			}
			fmt.Printf("Notification channel %q deleted\n", args[0])
			log.Debug("notifyDeleteCommand: done")
			return nil
		},
	}
}

func notifyTestCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "test <name>",
		Short: "Send one test message and report what the service said",
		Long: `Deliver a single synthetic message to one channel.

This is the only way to know a channel works. Feishu, WeCom and DingTalk answer
HTTP 200 for a message they refused and put the reason in the body, so a wrong
bot URL, an expired token or a missing signature all look like success from the
outside. This reports their answer.

The message does not go through the event bus: it neither reaches the other
channels nor appears in the event history.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("notifyTestCommand: running command", zap.Strings("args", args))
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			c, err := newHaifyClient()
			if err != nil {
				log.Debug("notifyTestCommand: newHaifyClient failed", zap.Error(err))
				return err
			}
			defer func() { _ = c.Close() }()

			msg, err := c.TestNotifyChannel(ctx, args[0])
			if err != nil {
				log.Debug("notifyTestCommand: TestNotifyChannel failed", zap.Error(err))
				return err
			}
			fmt.Println(msg)
			log.Debug("notifyTestCommand: done")
			return nil
		},
	}
}

// parseHeaderFlags turns repeated Name=Value flags into a map.
func parseHeaderFlags(in []string) (map[string]string, error) {
	// Header values can be credentials (Authorization, API keys): only the
	// names are traced.
	log := zap.L()
	log.Debug("parseHeaderFlags: parsing headers", zap.Int("count", len(in)))
	if len(in) == 0 {
		log.Debug("parseHeaderFlags: no headers")
		return nil, nil
	}
	out := make(map[string]string, len(in))
	for _, h := range in {
		name, value, ok := strings.Cut(h, "=")
		if !ok || strings.TrimSpace(name) == "" {
			log.Debug("parseHeaderFlags: header not in Name=Value form", zap.String("name", name), zap.Bool("has_separator", ok))
			return nil, fmt.Errorf("header %q is not in Name=Value form", h)
		}
		log.Debug("parseHeaderFlags: header", zap.String("name", strings.TrimSpace(name)), zap.Bool("has_value", value != ""))
		out[strings.TrimSpace(name)] = value
	}
	log.Debug("parseHeaderFlags: done", zap.Int("count", len(out)))
	return out, nil
}

// readOptionalNotifySecret resolves a signing secret if one was supplied.
//
// Unlike readBackupSecret, a missing secret is NOT an error: most kinds have no
// secret at all, and DingTalk robots can be secured by a keyword or an IP
// allowlist instead of by signing.
func readOptionalNotifySecret(stdin io.Reader, secretFile string) (string, error) {
	log := zap.L()
	log.Debug("readOptionalNotifySecret: resolving secret source", zap.Bool("has_secret_file", secretFile != ""))
	switch {
	case secretFile == "-":
		log.Debug("readOptionalNotifySecret: reading secret from stdin")
		data, err := io.ReadAll(stdin)
		if err != nil {
			log.Debug("readOptionalNotifySecret: ReadAll failed", zap.Error(err))
			return "", fmt.Errorf("read secret from stdin: %w", err)
		}
		return strings.TrimRight(string(data), "\r\n"), nil
	case secretFile != "":
		log.Debug("readOptionalNotifySecret: reading secret file", zap.String("path", secretFile))
		data, err := os.ReadFile(secretFile)
		if err != nil {
			log.Debug("readOptionalNotifySecret: ReadFile failed", zap.String("path", secretFile), zap.Error(err))
			return "", fmt.Errorf("read secret file: %w", err)
		}
		return strings.TrimRight(string(data), "\r\n"), nil
	default:
		secret := os.Getenv(notifySecretEnvVar)
		log.Debug("readOptionalNotifySecret: secret from environment", zap.String("env", notifySecretEnvVar), zap.Bool("has_secret", secret != ""))
		return secret, nil
	}
}

// clientNotifySpec assembles the client-side spec. Split out so the flag
// handling above reads as flag handling.
func clientNotifySpec(name, kind, url, minSeverity string, types []string,
	headers map[string]string, enabled bool, secret string, clearSecret bool) client.NotifyChannelSpec {
	return client.NotifyChannelSpec{
		Name: name, Kind: kind, URL: url, MinSeverity: minSeverity,
		Types: types, Headers: headers, Enabled: enabled,
		Secret: secret, ClearSecret: clearSecret,
	}
}
