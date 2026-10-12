package controller

import (
	"context"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// traceUnaryInterceptor logs every RPC at Debug, outermost in the chain so
// calls the auth, RBAC or approval interceptors reject are traced too.
// Requests are not logged, since they can carry secrets.
func traceUnaryInterceptor(log *zap.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		from := peerAddr(ctx)
		log.Debug("rpc start", zap.String("method", info.FullMethod), zap.String("peer", from))
		start := time.Now()
		resp, err := handler(ctx, req)
		log.Debug("rpc end", zap.String("method", info.FullMethod), zap.String("peer", from),
			zap.Duration("took", time.Since(start)), zap.String("code", status.Code(err).String()), zap.Error(err))
		return resp, err
	}
}

// traceStreamInterceptor logs every stream at Debug, from open to close.
func traceStreamInterceptor(log *zap.Logger) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		from := peerAddr(ss.Context())
		log.Debug("stream start", zap.String("method", info.FullMethod), zap.String("peer", from))
		start := time.Now()
		err := handler(srv, ss)
		log.Debug("stream end", zap.String("method", info.FullMethod), zap.String("peer", from),
			zap.Duration("took", time.Since(start)), zap.String("code", status.Code(err).String()), zap.Error(err))
		return err
	}
}

func peerAddr(ctx context.Context) string {
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		return p.Addr.String()
	}
	return ""
}
