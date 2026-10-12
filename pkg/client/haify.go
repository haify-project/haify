// Package client provides Haify controller gRPC client
package client

import (
	"context"
	"fmt"

	haifypb "github.com/haify-project/haify/api/proto/v1"
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
	var options clientOptions
	for _, opt := range opts {
		opt(&options)
	}

	transport := insecure.NewCredentials()
	if options.tls.Active() {
		var err error
		if transport, err = options.tls.Credentials(); err != nil {
			return nil, err
		}
	}

	dialOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(transport),
		grpc.WithChainUnaryInterceptor(TraceUnaryInterceptor()),
		grpc.WithChainStreamInterceptor(TraceStreamInterceptor()),
	}
	if options.token != "" {
		dialOpts = append(dialOpts, grpc.WithPerRPCCredentials(tokenCredentials{token: options.token}))
	}

	conn, err := grpc.NewClient(addr, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Haify controller at %s: %w", addr, err)
	}

	return &HaifyClient{
		conn:   conn,
		client: haifypb.NewHaifyControllerClient(conn),
		addr:   addr,
	}, nil
}

// Close closes the connection
func (c *HaifyClient) Close() error {
	return c.conn.Close()
}

// Address returns the controller address
func (c *HaifyClient) Address() string {
	return c.addr
}
