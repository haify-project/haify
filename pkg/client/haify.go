// Package client provides Haify controller gRPC client
package client

import (
	"context"
	"fmt"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// HaifyClient wraps Haify controller gRPC client
type HaifyClient struct {
	conn   *grpc.ClientConn
	client haifypb.HaifyControllerClient
	addr   string
}

// NewHaifyClient creates a new Haify controller client
// Option customizes the Haify client connection.
type Option func(*clientOptions)

type clientOptions struct {
	token string
	tls   TLSOptions
}

// WithToken attaches a static bearer token to every RPC, matching the
// controller's [auth] configuration.
func WithToken(token string) Option {
	return func(o *clientOptions) { o.token = token }
}

// WithTLS dials the controller over TLS, matching its [tls] section. Without
// it the connection is plaintext, which is what every controller predating
// transport security still serves.
func WithTLS(tlsOpts TLSOptions) Option {
	return func(o *clientOptions) { o.tls = tlsOpts }
}

// tokenCredentials implements credentials.PerRPCCredentials for the static
// bearer-token scheme.
type tokenCredentials struct {
	token string
}

func (t tokenCredentials) GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + t.token}, nil
}

// RequireTransportSecurity stays false deliberately. Returning true would make
// grpc-go refuse to send the token over a plaintext connection, which would
// break every existing cluster that runs the API on a trusted management
// network — the deployment model this project shipped with. Transport security
// is opted into with WithTLS; the honest warning about the plaintext case is
// logged by the controller at startup, where the operator can act on it.
func (t tokenCredentials) RequireTransportSecurity() bool { return false }

func NewHaifyClient(addr string, opts ...Option) (*HaifyClient, error) {
	log := zap.L()
	var options clientOptions
	for _, opt := range opts {
		opt(&options)
	}
	log.Debug("NewHaifyClient: dialing controller", zap.String("addr", addr), zap.Int("options", len(opts)),
		zap.Bool("has_token", options.token != ""), zap.Bool("tls", options.tls.Active()))

	transport := insecure.NewCredentials()
	if options.tls.Active() {
		var err error
		if transport, err = options.tls.Credentials(); err != nil {
			log.Debug("NewHaifyClient: TLS credentials failed", zap.String("addr", addr), zap.Error(err))
			return nil, err
		}
		log.Debug("NewHaifyClient: using TLS transport", zap.String("addr", addr))
	} else {
		log.Debug("NewHaifyClient: using plaintext transport", zap.String("addr", addr))
	}

	dialOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(transport),
		grpc.WithChainUnaryInterceptor(TraceUnaryInterceptor()),
		grpc.WithChainStreamInterceptor(TraceStreamInterceptor()),
	}
	if options.token != "" {
		log.Debug("NewHaifyClient: attaching bearer token to every call")
		dialOpts = append(dialOpts, grpc.WithPerRPCCredentials(tokenCredentials{token: options.token}))
	} else {
		log.Debug("NewHaifyClient: no token, calls go unauthenticated")
	}

	conn, err := grpc.NewClient(addr, dialOpts...)
	if err != nil {
		log.Debug("NewHaifyClient: client creation failed", zap.String("addr", addr), zap.Error(err))
		return nil, fmt.Errorf("failed to connect to Haify controller at %s: %w", addr, err)
	}

	log.Debug("NewHaifyClient: client ready", zap.String("addr", addr))
	return &HaifyClient{
		conn:   conn,
		client: haifypb.NewHaifyControllerClient(conn),
		addr:   addr,
	}, nil
}

// Close closes the connection
func (c *HaifyClient) Close() error {
	err := c.conn.Close()
	zap.L().Debug("HaifyClient.Close: connection closed", zap.String("addr", c.addr), zap.Error(err))
	return err
}

// Address returns the controller address
func (c *HaifyClient) Address() string {
	return c.addr
}
