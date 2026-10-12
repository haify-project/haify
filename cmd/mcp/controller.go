package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/client"
)

// controllerConn is how haify-mcp reaches the Haify controller: the bearer token
// and the transport security matching the controller's [auth] and [tls].
// Without the TLS half haify-mcp could not talk to a controller with
// `[tls] enabled = true` at all.
type controllerConn struct {
	addr  string
	token string
	tls   client.TLSOptions
}

// register adds the controller flags to cmd. prefix is "" for the stdio
// server, where the names match the haify CLI (--token, --tls-ca, ...), and
// "controller-" for `serve`, whose own --tls-cert/--tls-key are the HTTPS
// listener's certificate.
func (c *controllerConn) register(cmd *cobra.Command, prefix string) {
	f := cmd.Flags()
	f.StringVarP(&c.addr, "controller", "c", "127.0.0.1:3374", "Haify controller address")
	f.StringVar(&c.token, prefix+"token", "",
		"API token for the controller (default: HAIFY_TOKEN env, ~/.haify/token, /etc/haify/token)")
	f.BoolVar(&c.tls.Enabled, prefix+"tls", false,
		"connect to the controller over TLS (implied by the other --"+prefix+"tls-* flags; env HAIFY_TLS)")
	f.StringVar(&c.tls.CACert, prefix+"tls-ca", "",
		"CA bundle that signed the controller certificate (env HAIFY_TLS_CA; default: system trust store)")
	f.StringVar(&c.tls.ClientCert, prefix+"tls-cert", "",
		"client certificate, for a controller requiring mutual TLS (env HAIFY_TLS_CERT)")
	f.StringVar(&c.tls.ClientKey, prefix+"tls-key", "", "client private key (env HAIFY_TLS_KEY)")
	f.StringVar(&c.tls.ServerName, prefix+"tls-server-name", "",
		"name to verify against the controller certificate (env HAIFY_TLS_SERVER_NAME)")
	f.BoolVar(&c.tls.Insecure, prefix+"tls-insecure", false,
		"encrypt but do NOT verify the controller certificate (env HAIFY_TLS_INSECURE)")
}

// options resolves flags against the environment, flag first, the same way
// the haify CLI does.
func (c *controllerConn) options() []client.Option {
	log := zap.L()
	var opts []client.Option
	if token := client.ResolveToken(c.token); token != "" {
		log.Debug("controllerConn.options: using a controller token", zap.Bool("from_flag", c.token != ""))
		opts = append(opts, client.WithToken(token))
	} else {
		log.Debug("controllerConn.options: no controller token found")
	}
	if t := client.ResolveTLS(c.tls); t.Active() {
		log.Debug("controllerConn.options: connecting over TLS", zap.Bool("has_ca", t.CACert != ""),
			zap.Bool("has_client_cert", t.ClientCert != ""), zap.String("server_name", t.ServerName),
			zap.Bool("insecure", t.Insecure))
		opts = append(opts, client.WithTLS(t))
	} else {
		log.Debug("controllerConn.options: connecting without TLS")
	}
	return opts
}

// dial builds the controller client. The connection itself is lazy; a bad
// CA or key pair fails here, before the MCP server starts.
func (c *controllerConn) dial() (*client.HaifyClient, error) {
	log := zap.L()
	log.Debug("controllerConn.dial: building controller client", zap.String("controller", c.addr))
	start := time.Now()
	haifyClient, err := client.NewHaifyClient(c.addr, c.options()...)
	if err != nil {
		log.Debug("controllerConn.dial: building the client failed", zap.String("controller", c.addr), zap.Error(err),
			zap.Duration("took", time.Since(start)))
		return nil, fmt.Errorf("connect to controller %s: %w", c.addr, err)
	}
	log.Debug("controllerConn.dial: controller client ready", zap.String("controller", c.addr),
		zap.Duration("took", time.Since(start)))
	return haifyClient, nil
}
