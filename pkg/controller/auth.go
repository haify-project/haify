package controller

import (
	"context"
	"crypto/subtle"
	"strings"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// API authentication: a single static bearer token shared by all clients
// (single-admin model). Every gRPC request — and every REST request, which
// the grpc-gateway forwards with its headers intact — must carry
// "Authorization: Bearer <token>".
//
// Exemption: gRPC health checks stay open so systemd, load balancers, and
// failover probes keep working without credentials. Health output carries
// no cluster data.
const healthServicePrefix = "/grpc.health.v1.Health/"

// authTokenValid extracts the bearer token from the request metadata and
// compares it against the configured token in constant time. The
// grpc-gateway passes the HTTP Authorization header through under either
// "authorization" (custom header matcher) or "grpcgateway-authorization"
// (default matcher); both are accepted.
func authTokenValid(ctx context.Context, want string) bool {
	// Defense in depth: an empty configured token must never validate, even
	// though config validation already rejects it.
	log := zap.L()
	if want == "" {
		log.Debug("authTokenValid: no token configured, rejecting")
		return false
	}
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		log.Debug("authTokenValid: request carries no metadata, rejecting")
		return false
	}
	header := "authorization"
	values := md.Get("authorization")
	if len(values) == 0 {
		header = "grpcgateway-authorization"
		values = md.Get("grpcgateway-authorization")
	}
	log.Debug("authTokenValid: checking presented credentials", zap.String("header", header), zap.Int("count", len(values)))
	for _, v := range values {
		token := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v), "Bearer"))
		if subtle.ConstantTimeCompare([]byte(token), []byte(want)) == 1 {
			log.Debug("authTokenValid: token accepted", zap.String("header", header))
			return true
		}
	}
	log.Debug("authTokenValid: no presented token matched", zap.String("header", header), zap.Int("count", len(values)))
	return false
}

var errUnauthenticated = status.Error(codes.Unauthenticated,
	"missing or invalid API token; send 'Authorization: Bearer <token>'")

// authUnaryInterceptor enforces bearer-token authentication on unary RPCs.
func authUnaryInterceptor(token string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		if strings.HasPrefix(info.FullMethod, healthServicePrefix) {
			return handler(ctx, req)
		}
		if !authTokenValid(ctx, token) {
			zap.L().Debug("authUnaryInterceptor: unauthenticated call refused", zap.String("method", info.FullMethod))
			return nil, errUnauthenticated
		}
		zap.L().Debug("authUnaryInterceptor: call authenticated", zap.String("method", info.FullMethod))
		return handler(ctx, req)
	}
}

// authStreamInterceptor enforces bearer-token authentication on streaming RPCs.
func authStreamInterceptor(token string) grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if strings.HasPrefix(info.FullMethod, healthServicePrefix) {
			return handler(srv, ss)
		}
		if !authTokenValid(ss.Context(), token) {
			zap.L().Debug("authStreamInterceptor: unauthenticated stream refused", zap.String("method", info.FullMethod))
			return errUnauthenticated
		}
		zap.L().Debug("authStreamInterceptor: stream authenticated", zap.String("method", info.FullMethod))
		return handler(srv, ss)
	}
}
