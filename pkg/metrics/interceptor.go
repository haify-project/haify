// Package metrics provides Prometheus metrics support for the Haify controller
package metrics

import (
	"context"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

// UnaryServerInterceptor returns a gRPC unary server interceptor that records metrics
func (m *Metrics) UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	m.log().Debug("Metrics.UnaryServerInterceptor: building unary metrics interceptor")
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		start := time.Now()

		// Call the handler
		resp, err := handler(ctx, req)

		// Calculate duration
		duration := time.Since(start).Seconds()

		// Determine status from error
		statusCode := "OK"
		if err != nil {
			statusCode = status.Code(err).String()
		}

		// Record metrics
		m.RecordGRPCRequest(info.FullMethod, statusCode, duration)

		return resp, err
	}
}

// StreamServerInterceptor returns a gRPC stream server interceptor that records metrics
// Note: For streaming, we record on stream creation since duration tracking for the
// entire stream would require wrapping the stream
func (m *Metrics) StreamServerInterceptor() grpc.StreamServerInterceptor {
	m.log().Debug("Metrics.StreamServerInterceptor: building stream metrics interceptor")
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		start := time.Now()

		err := handler(srv, ss)

		duration := time.Since(start).Seconds()

		statusCode := "OK"
		if err != nil {
			statusCode = status.Code(err).String()
		}

		m.RecordGRPCRequest(info.FullMethod, statusCode, duration)

		return err
	}
}

// ChainUnaryServer creates a single interceptor from multiple interceptors
// This can be used to combine metrics interceptors with other interceptors
func ChainUnaryServer(interceptors ...grpc.UnaryServerInterceptor) grpc.UnaryServerInterceptor {
	zap.L().Debug("ChainUnaryServer: chaining unary interceptors", zap.Int("count", len(interceptors)))
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		// Build the chain in reverse order so the first interceptor is outermost
		chainedHandler := handler
		for i := len(interceptors) - 1; i >= 0; i-- {
			chainedHandler = wrapInterceptor(interceptors[i], chainedHandler, info)
		}
		return chainedHandler(ctx, req)
	}
}

// wrapInterceptor wraps a handler with an interceptor
func wrapInterceptor(interceptor grpc.UnaryServerInterceptor, handler grpc.UnaryHandler, info *grpc.UnaryServerInfo) grpc.UnaryHandler {
	return func(ctx context.Context, req interface{}) (interface{}, error) {
		return interceptor(ctx, req, info, handler)
	}
}
