package client

import (
	"context"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

// TraceUnaryInterceptor logs every call at Debug on the process-wide logger:
// method, duration and status code. Requests and replies are not logged,
// since they can carry secrets.
func TraceUnaryInterceptor() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		log := zap.L()
		log.Debug("rpc call", zap.String("method", method), zap.String("target", cc.Target()))
		start := time.Now()
		err := invoker(ctx, method, req, reply, cc, opts...)
		log.Debug("rpc done", zap.String("method", method), zap.Duration("took", time.Since(start)),
			zap.String("code", status.Code(err).String()), zap.Error(err))
		return err
	}
}

// TraceStreamInterceptor logs the opening of every stream at Debug.
func TraceStreamInterceptor() grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		start := time.Now()
		s, err := streamer(ctx, desc, cc, method, opts...)
		zap.L().Debug("rpc stream opened", zap.String("method", method), zap.String("target", cc.Target()),
			zap.Duration("took", time.Since(start)), zap.String("code", status.Code(err).String()), zap.Error(err))
		return s, err
	}
}
