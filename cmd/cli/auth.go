package main

import (
	"io"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/client"
)

// newHaifyClient builds the controller client with the resolved API token and
// transport security. Every command must use this instead of
// client.NewHaifyClient directly — a command that dials on its own gets neither
// the token nor TLS, and against a TLS controller it fails at the handshake
// with an error that looks like the controller is down.
//
// Token resolution order is documented on client.ResolveToken:
// --token flag > HAIFY_TOKEN env > ~/.haify/token > /etc/haify/token.
// TLS follows the same flag-beats-environment rule, see client.ResolveTLS.
func newHaifyClient() (*client.HaifyClient, error) {
	log := zap.L()
	log.Debug("newHaifyClient: building controller client", zap.String("controller", controllerAddr), zap.Bool("has_token_flag", tokenFlag != ""))
	opts := []client.Option{}
	if token := client.ResolveToken(tokenFlag); token != "" {
		log.Debug("newHaifyClient: sending a bearer token")
		opts = append(opts, client.WithToken(token))
	} else {
		log.Debug("newHaifyClient: no token resolved")
	}
	if tlsOpts := client.ResolveTLS(tlsFlags); tlsOpts.Active() {
		log.Debug("newHaifyClient: TLS active", zap.String("ca_cert", tlsOpts.CACert), zap.String("server_name", tlsOpts.ServerName), zap.Bool("insecure", tlsOpts.Insecure))
		opts = append(opts, client.WithTLS(tlsOpts))
	} else {
		log.Debug("newHaifyClient: TLS not active, plaintext")
	}
	c, err := client.NewHaifyClient(controllerAddr, opts...)
	if err != nil {
		log.Debug("newHaifyClient: client creation failed", zap.String("controller", controllerAddr), zap.Error(err))
	}
	return c, err
}

// closeClient releases a controller connection at the end of a CLI command.
// The close error is deliberately dropped: it can only report that the
// transport teardown was untidy, which happens after the RPC has already
// returned and its result has been printed. Surfacing it would turn a command
// that did exactly what was asked into a non-zero exit, and there is nothing
// an operator could do about it anyway. Errors that mean the operation itself
// failed all come back from the RPC call, not from Close.
func closeClient(c io.Closer) {
	if err := c.Close(); err != nil {
		zap.L().Debug("closeClient: close error dropped", zap.Error(err))
	}
}
