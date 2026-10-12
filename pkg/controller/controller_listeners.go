package controller

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/rbac"
)

// startGRPCServer starts the gRPC server with gRPC-Gateway on separate ports
func (c *Controller) startGRPCServer() error {
	// Start gRPC server on the configured port
	grpcAddr := fmt.Sprintf("%s:%d", c.config.Server.ListenAddress, c.config.Server.Port)
	grpcLis, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		return fmt.Errorf("failed to listen for gRPC: %w", err)
	}

	// Create gRPC server. Interceptor order matters: metrics first so even
	// rejected requests are counted, then audit (so denied attempts are still
	// recorded), then authentication.
	unaryInterceptors := []grpc.UnaryServerInterceptor{traceUnaryInterceptor(c.logger)}
	streamInterceptors := []grpc.StreamServerInterceptor{traceStreamInterceptor(c.logger)}
	if c.metrics != nil {
		unaryInterceptors = append(unaryInterceptors, c.metrics.UnaryServerInterceptor())
	}

	// Build the RBAC engine first so the audit interceptor — which runs ahead
	// of the identity check — can still attribute each call to a user.
	var rbacEngine *rbac.Engine
	var auditUser userResolver
	if c.config.RBAC.Enabled {
		var err error
		rbacEngine, err = rbac.New(dbRBACStore{c.db}, toRBACUsers(c.config.RBAC.Users), toRBACPolicies(c.config.RBAC.Policies))
		if err != nil {
			return fmt.Errorf("failed to initialize RBAC: %w", err)
		}
		c.rbac = rbacEngine
		auditUser = func(ctx context.Context) string {
			name, _ := rbacEngine.ResolveUser(bearerToken(ctx))
			return name
		}
	}

	if c.config.Audit.Enabled {
		auditLog := c.logger.Named("audit")
		sink := c.auditSink()
		unaryInterceptors = append(unaryInterceptors,
			auditUnaryInterceptor(auditLog, c.config.Audit.IncludeReads, auditUser, sink))
		streamInterceptors = append(streamInterceptors,
			auditStreamInterceptor(auditLog, c.config.Audit.IncludeReads, auditUser, sink))
		c.logger.Info("API audit log enabled",
			zap.Bool("include_reads", c.config.Audit.IncludeReads),
			zap.Bool("persisted", sink != nil))
	}

	switch {
	case c.config.RBAC.Enabled:
		unaryInterceptors = append(unaryInterceptors,
			rbacIdentityUnaryInterceptor(rbacEngine), rbacAuthzUnaryInterceptor(rbacEngine))
		if c.approvals = newApprovalGate(c.config.RBAC.Approval, c.db, c.events, c.logger); c.approvals != nil {
			unaryInterceptors = append(unaryInterceptors, approvalUnaryInterceptor(c.approvals))
			c.logger.Info("Two-person approval enabled", zap.Strings("methods", c.config.RBAC.Approval.ApprovalMethods()))
		}
		streamInterceptors = append(streamInterceptors,
			rbacIdentityStreamInterceptor(rbacEngine), rbacAuthzStreamInterceptor(rbacEngine))
		c.logger.Info("API authorization enabled (RBAC)",
			zap.Int("users", len(c.config.RBAC.Users)))
	case c.config.Auth.Enabled:
		unaryInterceptors = append(unaryInterceptors, authUnaryInterceptor(c.config.Auth.Token))
		streamInterceptors = append(streamInterceptors, authStreamInterceptor(c.config.Auth.Token))
		c.logger.Info("API authentication enabled (bearer token)")
	default:
		c.logger.Warn("API authentication is DISABLED; enable [auth] or [rbac] in controller.toml for production")
	}
	// Transport security. Until this was wired up the whole [tls] section was
	// decoration: the listener stayed plaintext while the startup log reported
	// TLS as enabled, so the bearer token checked just above crossed the
	// network in the clear on a cluster whose operator believed otherwise.
	tlsSetup, err := newTLSSetup(c.config.TLS)
	if err != nil {
		return fmt.Errorf("failed to configure TLS: %w", err)
	}
	if tlsSetup != nil {
		c.restLoopbackTLS = tlsSetup.restLoopback
	}

	var opts []grpc.ServerOption
	if tlsSetup != nil {
		opts = append(opts, grpc.Creds(tlsSetup.serverCreds))
		c.logger.Info("API transport TLS enabled",
			zap.String("cert_file", c.config.TLS.CertFile),
			zap.Bool("mutual_tls", tlsSetup.mutual))
		if !tlsSetup.mutual {
			c.logger.Info("Client certificates are not required; set tls.client_ca_file for mutual TLS")
		}
	} else {
		c.logger.Warn("API transport is PLAINTEXT; bearer tokens cross the network unencrypted — enable [tls] in controller.toml for production")
	}
	if len(unaryInterceptors) > 0 {
		opts = append(opts, grpc.ChainUnaryInterceptor(unaryInterceptors...))
	}
	if len(streamInterceptors) > 0 {
		opts = append(opts, grpc.ChainStreamInterceptor(streamInterceptors...))
	}
	// The in-process REST gateway dials back with 10s keepalive pings; the
	// gRPC default enforcement (5 min) answers those with GOAWAY
	// "too_many_pings", and every REST request in the reconnect window
	// fails with a 500. Permit frequent pings explicitly.
	opts = append(opts, grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
		MinTime:             5 * time.Second,
		PermitWithoutStream: true,
	}))
	c.server = grpc.NewServer(opts...)

	// Register health service
	healthServer := health.NewServer()
	grpc_health_v1.RegisterHealthServer(c.server, healthServer)
	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)

	// Register Haify controller service
	haifyServer := NewServer(c)
	haifypb.RegisterHaifyControllerServer(c.server, haifyServer)
	// Reflection lets grpcurl and similar tools discover the API without a copy
	// of the .proto files. It describes the schema only; every call still goes
	// through the same auth interceptors.
	reflection.Register(c.server)

	c.logger.Info("Registered Haify controller service")

	// Start gRPC server
	go func() {
		c.logger.Info("gRPC server listening", zap.String("address", grpcAddr))
		if err := c.server.Serve(grpcLis); err != nil {
			c.logger.Error("gRPC server error", zap.Error(err))
		}
	}()

	// Start HTTP REST API gateway
	restPort := c.restPort()
	restAddr := fmt.Sprintf("%s:%d", c.config.Server.ListenAddress, restPort)
	restLis, err := net.Listen("tcp", restAddr)
	if err != nil {
		return fmt.Errorf("failed to listen for REST: %w", err)
	}
	restScheme := "http"
	if tlsSetup != nil && tlsSetup.restServer != nil {
		// [tls] rest: the bearer token on every REST call no longer crosses
		// the network in the clear. Without it, it still does — logged below.
		restLis = tls.NewListener(restLis, tlsSetup.restServer)
		restScheme = "https"
	} else if tlsSetup != nil {
		c.logger.Warn("The REST gateway is still PLAINTEXT although [tls] is enabled; set tls.rest = true to serve it over TLS",
			zap.String("address", restAddr))
	}

	// Create and register gRPC-Gateway. The default header matcher forwards
	// well-known headers (Authorization arrives as grpcgateway-authorization,
	// which the auth interceptor accepts). Forwarding ALL headers is not an
	// option: browsers send hop-by-hop headers like "Connection" that are
	// illegal in HTTP/2 and kill the loopback gRPC stream with
	// RST_STREAM PROTOCOL_ERROR.
	gatewayMux := runtime.NewServeMux()

	// Dial loopback explicitly — grpcAddr above is a LISTEN address and is
	// normally "0.0.0.0:3374", which is not a destination. See loopbackTarget.
	if err := haifypb.RegisterHaifyControllerHandlerFromEndpoint(
		context.Background(), gatewayMux, loopbackTarget(c.config), loopbackDialOptions(tlsSetup)); err != nil {
		return fmt.Errorf("failed to register gateway handler: %w", err)
	}

	// Read-only RBAC introspection endpoints for the UI/CLI (whoami / policies).
	c.registerRBACRoutes(gatewayMux, rbacEngine)

	// Browser-facing SSE event stream, alongside the generated
	// /v1/events/watch that serves newline-delimited JSON.
	c.registerEventRoutes(gatewayMux, rbacEngine)

	// Wrap with CORS handler
	corsHandler := corsMiddleware(gatewayMux)

	// Create HTTP server for gateway. Plain-text HTTP/1.1: h2 negotiation
	// only happens over TLS (TLSNextProto kept empty), and browsers never
	// speak h2c without an explicit upgrade, which we don't offer.
	gatewayServer := &http.Server{
		Handler:           corsHandler,
		ReadHeaderTimeout: 5 * time.Second,
		TLSNextProto:      make(map[string]func(*http.Server, *tls.Conn, http.Handler)),
	}
	c.restServer = gatewayServer

	go func() {
		c.logger.Info("HTTP REST API gateway listening", zap.String("address", restAddr), zap.String("scheme", restScheme))
		if err := gatewayServer.Serve(restLis); err != nil && err != http.ErrServerClosed {
			c.logger.Error("HTTP gateway server error", zap.Error(err))
		}
	}()

	c.logger.Info("Server listening",
		zap.String("grpc", grpcAddr),
		zap.String("rest", restAddr))

	return nil
}

// corsMiddleware adds CORS headers and answers preflight requests.
//
// Note: this deliberately does NOT force "Connection: close" or sniff for an
// HTTP/2 preface. A previous first-byte 'P' check meant to reject the h2c
// preface ("PRI ...") also killed every connection whose first request was a
// POST or PATCH, silently breaking all mutating REST calls from browsers.
func corsMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, PATCH, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With")
		w.Header().Set("Access-Control-Expose-Headers", "Content-Length, Content-Type")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		h.ServeHTTP(w, r)
	})
}

// startMetricsServer starts the Prometheus metrics HTTP server
func (c *Controller) startMetricsServer() error {
	addr := fmt.Sprintf("%s:%d", c.config.Metrics.ListenAddress, c.config.Metrics.Port)
	c.metricsServer = &http.Server{
		Addr:    addr,
		Handler: c.metrics.Handler(),
	}

	go func() {
		c.logger.Info("Metrics server listening", zap.String("address", addr))
		if err := c.metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			c.logger.Error("Metrics server error", zap.Error(err))
		}
	}()

	return nil
}
