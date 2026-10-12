// Package controller provides the Haify controller
package controller

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/config"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
)

// loopbackTarget is the address the REST gateway dials to reach this process's
// own gRPC server.
//
// It is deliberately NOT the listen address. Listening and dialling want
// opposite things: `listen_address` is usually "0.0.0.0", meaning "every
// interface", which is not a destination. Dialling "0.0.0.0:3374" happens to
// work on Linux (the kernel rewrites it to loopback) and that accident held
// until a node gained a VM-wide HTTP_PROXY: grpc-go saw a target that was not
// in NO_PROXY and sent the call to the proxy, which hung up. The gateway then
// answered every /v1 request with 503 "error reading server preface: EOF".
func loopbackTarget(cfg *config.Config) string {
	port := cfg.Server.Port
	if port == 0 {
		zap.L().Debug("loopbackTarget: no gRPC port configured, using the default", zap.Int("port", defaultGRPCPort))
		port = defaultGRPCPort
	}
	zap.L().Debug("loopbackTarget: dialling loopback", zap.Int("port", port))
	return fmt.Sprintf("127.0.0.1:%d", port)
}

// loopbackDialOptions are the dial options for that in-process hop.
//
// grpc.WithNoProxy is the load-bearing one: a call from the controller to
// itself must never traverse an HTTP proxy, no matter how the operator
// configured the machine. Relying on NO_PROXY listing every spelling of
// "local" is how this broke in the first place.
//
// tlsSetup must be the same one the gRPC server was built with, and nil only
// when the server is plaintext. Getting this wrong is silent in the direction
// that matters: the gRPC port keeps working perfectly while every REST call —
// and with it the whole web UI — dies at the handshake. See newTLSSetup for
// how the hop authenticates a server whose certificate was never issued for
// 127.0.0.1.
func loopbackDialOptions(setup *tlsSetup) []grpc.DialOption {
	creds := insecure.NewCredentials()
	if setup != nil {
		creds = setup.loopbackCreds
	}
	zap.L().Debug("loopbackDialOptions: building dial options", zap.Bool("tls", setup != nil))
	return []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
		grpc.WithNoProxy(),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(4 * 1024 * 1024)),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                10 * time.Second,
			Timeout:             time.Second,
			PermitWithoutStream: true,
		}),
	}
}

// GatewayServer and the two helpers below are legacy plumbing kept for tests
// only: the live REST gateway is built inline by Controller.startGRPCServer,
// which is the path that carries transport credentials. They dial plaintext
// and take a bare address with no config, so they must not be reintroduced on
// the serving path — under [tls] they would fail at the handshake.
//
// GatewayServer wraps the gRPC-Gateway HTTP server
type GatewayServer struct {
	grpcAddr   string
	port       int
	grpcServer *grpc.Server
	logger     *zap.Logger
}

// NewGatewayServer creates a new gRPC-Gateway HTTP server
func NewGatewayServer(grpcServer *grpc.Server, grpcAddr string, port int, logger *zap.Logger) *GatewayServer {
	logger.Debug("NewGatewayServer: start", zap.String("grpc_addr", grpcAddr), zap.Int("port", port))
	mux := runtime.NewServeMux(
		runtime.WithIncomingHeaderMatcher(func(key string) (string, bool) {
			// Allow all headers to pass through
			return key, true
		}),
	)

	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}

	// Register all services from the grpc server
	// Note: The gateway will connect to the gRPC server via the local address
	start := time.Now()
	if err := haifypb.RegisterHaifyControllerHandlerFromEndpoint(context.Background(), mux, grpcAddr, opts); err != nil {
		logger.Error("Failed to register gateway handler", zap.Error(err))
		return nil
	}
	logger.Debug("NewGatewayServer: handler registered", zap.String("grpc_addr", grpcAddr), zap.Duration("took", time.Since(start)))

	return &GatewayServer{
		grpcServer: grpcServer,
		grpcAddr:   grpcAddr,
		port:       port,
		logger:     logger,
	}
}

// Start starts the gateway HTTP server
func (g *GatewayServer) Start() error {
	// The gateway is started by the main controller using cmux
	return nil
}

// Handler returns the HTTP handler for the gateway
func (g *GatewayServer) Handler() http.Handler {
	g.logger.Debug("GatewayServer.Handler: building the REST mux", zap.String("grpc_addr", g.grpcAddr))
	mux := runtime.NewServeMux(
		runtime.WithIncomingHeaderMatcher(func(key string) (string, bool) {
			return key, true
		}),
	)

	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}

	start := time.Now()
	if err := haifypb.RegisterHaifyControllerHandlerFromEndpoint(context.Background(), mux, g.grpcAddr, opts); err != nil {
		g.logger.Error("Failed to register gateway handler", zap.Error(err))
		return nil
	}
	g.logger.Debug("GatewayServer.Handler: handler registered", zap.Duration("took", time.Since(start)))

	return mux
}

// RegisterGatewayHandler registers the gRPC-Gateway handler with the given HTTP serve mux
func RegisterGatewayHandler(ctx context.Context, mux *runtime.ServeMux, grpcAddr string, logger *zap.Logger) error {
	logger.Debug("RegisterGatewayHandler: start", zap.String("grpc_addr", grpcAddr))
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}

	start := time.Now()
	if err := haifypb.RegisterHaifyControllerHandlerFromEndpoint(ctx, mux, grpcAddr, opts); err != nil {
		logger.Debug("RegisterGatewayHandler: registration failed", zap.String("grpc_addr", grpcAddr),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		return fmt.Errorf("failed to register gateway handler: %w", err)
	}

	logger.Info("Registered gRPC-Gateway handler", zap.String("grpc_addr", grpcAddr))
	return nil
}
