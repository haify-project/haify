package main

import (
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/mcpauth"
	"github.com/haify-project/haify/pkg/mcpserver"
)

// defaultTokenStore lives on the Self-HA DRBD mount so the tokens follow the
// server when it moves to another node.
const defaultTokenStore = "/var/lib/haify/mcp/tokens.json"

func tokenStorePath(flag string) string {
	log := zap.L()
	if flag != "" {
		log.Debug("tokenStorePath: using --tokens", zap.String("path", flag))
		return flag
	}
	if env := os.Getenv("HAIFY_MCP_TOKENS"); env != "" {
		log.Debug("tokenStorePath: using HAIFY_MCP_TOKENS", zap.String("path", env))
		return env
	}
	log.Debug("tokenStorePath: using the default store", zap.String("path", defaultTokenStore))
	return defaultTokenStore
}

// serveCmd runs the remote server: MCP over HTTP, behind token authentication.
func serveCmd() *cobra.Command {
	var (
		conn        controllerConn
		listen      string
		adminListen string
		publicURL   string
		tokens      string
		maxRole     string
		tlsCert     string
		tlsKey      string
		trustProxy  bool
		debug       bool
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Serve MCP over HTTP with token authentication, for Claude Code, ChatGPT and other remote clients",
		Long: "Serves the Haify tools at /mcp. Every request needs a token made with `haify-mcp token create`;\n" +
			"the token's role decides which tools exist for that connection: read (inspect only), operate\n" +
			"(plus create/grow/snapshot/start/mount) or admin (everything, including delete and evict).\n\n" +
			"With --public-url the server also runs the OAuth flow that ChatGPT and claude.ai use to add a\n" +
			"server by URL: the operator approves the client by pasting a token on the authorization page.\n\n" +
			"Put it behind HTTPS. Either give --tls-cert/--tls-key, or terminate TLS in a reverse proxy and\n" +
			"bind this to a private address. Those two flags are this server's certificate; the connection to\n" +
			"a controller with [tls] enabled is configured with the --controller-tls* flags.",
		RunE: func(cmd *cobra.Command, args []string) error {
			role, err := mcpauth.ParseRole(maxRole)
			if err != nil {
				return fmt.Errorf("--max-role: %w", err)
			}
			if (tlsCert == "") != (tlsKey == "") {
				return fmt.Errorf("--tls-cert and --tls-key go together")
			}
			logger, err := newStderrLogger(debug)
			if err != nil {
				return fmt.Errorf("init logger: %w", err)
			}
			defer func() { _ = logger.Sync() }()
			logger.Debug("serveCmd: starting remote server", zap.String("listen", listen),
				zap.String("admin_listen", adminListen), zap.String("public_url", publicURL),
				zap.String("max_role", string(role)), zap.Bool("tls", tlsCert != ""), zap.Bool("trust_proxy", trustProxy),
				zap.String("controller", conn.addr))

			store, err := mcpauth.Open(tokenStorePath(tokens))
			if err != nil {
				logger.Debug("serveCmd: opening the token store failed", zap.Error(err))
				return err
			}
			if list, _ := store.List(); len(list) == 0 {
				logger.Warn("no tokens exist yet; every request will be refused until one is made with `haify-mcp token create`",
					zap.String("store", tokenStorePath(tokens)))
			} else {
				logger.Debug("serveCmd: token store has tokens", zap.Int("count", len(list)))
			}
			haifyClient, err := conn.dial()
			if err != nil {
				logger.Debug("serveCmd: dialing the controller failed", zap.Error(err))
				return err
			}
			defer func() { _ = haifyClient.Close() }()

			return mcpserver.ServeHTTP(cmd.Context(), haifyClient, logger, mcpserver.Options{Version: version}, mcpserver.HTTPOptions{
				Listen: listen, AdminListen: adminListen, PublicURL: publicURL, Tokens: store, MaxRole: role,
				TLSCert: tlsCert, TLSKey: tlsKey, TrustProxy: trustProxy,
			})
		},
	}
	f := cmd.Flags()
	conn.register(cmd, "controller-")
	f.StringVar(&listen, "listen", "127.0.0.1:43871", "address to listen on")
	f.StringVar(&adminListen, "admin-listen", "", "second address for the local network that is not capped by --max-role (only tokens from token create, no OAuth); never proxy it")
	f.StringVar(&publicURL, "public-url", "", "URL clients reach this server at, e.g. https://mcp.example.com; enables OAuth for ChatGPT and claude.ai")
	f.StringVar(&tokens, "tokens", "", "token store (default: HAIFY_MCP_TOKENS env, else "+defaultTokenStore+")")
	f.StringVar(&maxRole, "max-role", "admin", "highest role any token may use here: read, operate or admin")
	f.StringVar(&tlsCert, "tls-cert", "", "serve HTTPS with this certificate")
	f.StringVar(&tlsKey, "tls-key", "", "private key for --tls-cert")
	f.BoolVar(&trustProxy, "trust-proxy", false, "take the client address from X-Forwarded-For (only behind a proxy that sets it)")
	f.BoolVar(&debug, "debug", false, "enable debug logging on stderr")
	return cmd
}

// tokenCmd manages the tokens `serve` accepts. It works on the store file, so
// it can be run beside a serving process: a revoked token is refused on the
// next request.
func tokenCmd() *cobra.Command {
	var tokens string
	var debug bool
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Create, list and revoke access tokens for the remote server",
		// Traces go to stderr with --debug; stdout keeps only the token output.
		PersistentPreRunE: func(*cobra.Command, []string) error {
			if !debug {
				return nil
			}
			_, err := newStderrLogger(true)
			return err
		},
	}
	cmd.PersistentFlags().StringVar(&tokens, "tokens", "", "token store (default: HAIFY_MCP_TOKENS env, else "+defaultTokenStore+")")
	cmd.PersistentFlags().BoolVar(&debug, "debug", false, "enable debug logging on stderr")

	var (
		name    string
		role    string
		expires time.Duration
		url     string
	)
	create := &cobra.Command{
		Use:   "create",
		Short: "Make a token; it is shown once",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			log.Debug("tokenCmd create: creating token", zap.String("name", name), zap.String("role", role),
				zap.Duration("expires", expires))
			r, err := mcpauth.ParseRole(role)
			if err != nil {
				log.Debug("tokenCmd create: bad role", zap.Error(err))
				return err
			}
			store, err := mcpauth.Open(tokenStorePath(tokens))
			if err != nil {
				log.Debug("tokenCmd create: opening the token store failed", zap.Error(err))
				return err
			}
			secret, t, err := store.Create(name, r, expires)
			if err != nil {
				log.Debug("tokenCmd create: create failed", zap.Error(err))
				return err
			}
			until := "never expires"
			if !t.Expires.IsZero() {
				until = "expires " + t.Expires.Local().Format("2006-01-02 15:04")
			}
			log.Debug("tokenCmd create: token created", zap.String("token_id", t.ID), zap.String("role", string(t.Role)),
				zap.Time("expires", t.Expires))
			fmt.Printf("Token %q (%s), %s. It is not stored and cannot be shown again:\n\n  %s\n\n", t.Name, t.Role, until, secret)
			fmt.Printf("Claude Code:\n  claude mcp add --transport http haify %s --header \"Authorization: Bearer %s\"\n\n", url, secret)
			fmt.Printf("ChatGPT and claude.ai add the server by URL (%s) and ask for this token when they send you to the authorization page.\n", url)
			return nil
		},
	}
	create.Flags().StringVar(&name, "name", "", "what the token is for, e.g. laptop or chatgpt (required)")
	create.Flags().StringVar(&role, "role", "read", "read, operate or admin")
	create.Flags().DurationVar(&expires, "expires", 0, "lifetime, e.g. 720h; 0 never expires")
	create.Flags().StringVar(&url, "url", "https://<your-server>/mcp", "the server's URL, for the connection commands printed")
	_ = create.MarkFlagRequired("name")

	list := &cobra.Command{
		Use:   "list",
		Short: "List tokens",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			store, err := mcpauth.Open(tokenStorePath(tokens))
			if err != nil {
				log.Debug("tokenCmd list: opening the token store failed", zap.Error(err))
				return err
			}
			all, err := store.List()
			if err != nil {
				log.Debug("tokenCmd list: listing failed", zap.Error(err))
				return err
			}
			log.Debug("tokenCmd list: printing tokens", zap.Int("count", len(all)))
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "ID\tNAME\tROLE\tCREATED\tEXPIRES")
			for _, t := range all {
				exp := "never"
				if !t.Expires.IsZero() {
					exp = t.Expires.Local().Format("2006-01-02 15:04")
				}
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", t.ID, t.Name, t.Role, t.Created.Local().Format("2006-01-02 15:04"), exp)
			}
			return w.Flush()
		},
	}

	revoke := &cobra.Command{
		Use:   "revoke <name|id>",
		Short: "Revoke a token and everything OAuth issued under it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := zap.L()
			store, err := mcpauth.Open(tokenStorePath(tokens))
			if err != nil {
				log.Debug("tokenCmd revoke: opening the token store failed", zap.Error(err))
				return err
			}
			n, err := store.Revoke(args[0])
			if err != nil {
				log.Debug("tokenCmd revoke: revoke failed", zap.Error(err))
				return err
			}
			log.Debug("tokenCmd revoke: revoked", zap.Int("removed", n))
			fmt.Printf("revoked %q (%d record(s), including anything ChatGPT or claude.ai was given through it)\n", args[0], n)
			return nil
		},
	}
	cmd.AddCommand(create, list, revoke)
	return cmd
}
