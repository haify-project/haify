package controller

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/haify-project/haify/pkg/database"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// API audit log. Every state-changing RPC — and, when configured, read RPCs —
// is recorded under a dedicated "audit" logger with the caller, the target
// resource, the outcome and the latency. The interceptor sits ahead of
// authentication in the chain so denied attempts (Unauthenticated /
// PermissionDenied) are captured too, which is what an audit trail needs.
//
// Because the REST surface is served by an in-process grpc-gateway, the direct
// gRPC peer for those calls is loopback; the real client address is recovered
// from the forwarded X-Forwarded-For header when present.

// readOnlyPrefixes are RPC verbs that do not change cluster state. Anything
// not matching is treated as mutating and always audited, so a newly added
// write RPC is never silently excluded from the trail.
var readOnlyPrefixes = []string{"List", "Get", "Describe", "Watch", "Stream", "Check"}

// readOnlyMethods are reads whose names carry no read verb: a status query,
// probes of the nodes, and a rebalance plan that only proposes moves. The web
// UI polls ResourceStatus every few seconds, so leaving it here would bury
// every real change in the trail.
var readOnlyMethods = map[string]bool{
	"HealthCheck":            true,
	"PlanRebalance":          true,
	"CollectNodeDiagnostics": true,
}

// shortMethod turns "/v1.HaifyController/CreatePool" into "CreatePool".
func shortMethod(fullMethod string) string {
	if i := strings.LastIndex(fullMethod, "/"); i >= 0 {
		return fullMethod[i+1:]
	}
	return fullMethod
}

func isReadOnly(method string) bool {
	// Any *Status method is a read, as RBAC classifies it.
	if readOnlyMethods[method] || strings.Contains(method, "Status") {
		zap.L().Debug("isReadOnly: read by name", zap.String("method", method))
		return true
	}
	for _, p := range readOnlyPrefixes {
		if strings.HasPrefix(method, p) {
			zap.L().Debug("isReadOnly: read by verb", zap.String("method", method), zap.String("prefix", p))
			return true
		}
	}
	zap.L().Debug("isReadOnly: mutating call", zap.String("method", method))
	return false
}

// clientAddr returns the best-known caller address: the forwarded client IP
// for REST/proxied calls, otherwise the direct gRPC peer.
func clientAddr(ctx context.Context) string {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		for _, key := range []string{"x-forwarded-for", "grpcgateway-x-forwarded-for"} {
			if vals := md.Get(key); len(vals) > 0 && vals[0] != "" {
				// X-Forwarded-For may be a list; the first hop is the client.
				addr := strings.TrimSpace(vals[0])
				if i := strings.IndexByte(vals[0], ','); i >= 0 {
					addr = strings.TrimSpace(vals[0][:i])
				}
				zap.L().Debug("clientAddr: from forwarded header", zap.String("header", key), zap.String("client", addr))
				return addr
			}
		}
	}
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		zap.L().Debug("clientAddr: from gRPC peer", zap.String("client", p.Addr.String()))
		return p.Addr.String()
	}
	zap.L().Debug("clientAddr: caller address unknown")
	return "unknown"
}

// auditTarget extracts the resource a request acts on, best-effort, from the
// common getters generated on the request messages. It keeps the trail
// answering "who deleted pool X" without per-RPC wiring.
func auditTarget(req interface{}) string {
	switch r := req.(type) {
	case interface{ GetName() string }:
		if v := r.GetName(); v != "" {
			return v
		}
	}
	switch r := req.(type) {
	case interface{ GetResource() string }:
		if v := r.GetResource(); v != "" {
			return v
		}
	}
	switch r := req.(type) {
	case interface{ GetPool() string }:
		if v := r.GetPool(); v != "" {
			return v
		}
	}
	switch r := req.(type) {
	case interface{ GetGateway() string }:
		if v := r.GetGateway(); v != "" {
			return v
		}
	}
	switch r := req.(type) {
	case interface{ GetSnapshot() string }:
		if v := r.GetSnapshot(); v != "" {
			return v
		}
	}
	return ""
}

// userResolver maps a request context to an authenticated user name for the
// audit trail. It is nil when RBAC is off (no per-user identity exists).
type userResolver func(context.Context) string

// auditSink persists an audit record. The process log alone is not enough: the
// controller relocates under Self-HA, so a journal-only trail is split across
// whichever nodes were active. A nil sink means log-only.
type auditSink func(ev *database.AuditEvent)

// writeAuditEntry emits a single structured audit record describing the
// outcome of an RPC, to the process log and — when configured — to the
// persistent trail.
func writeAuditEntry(log *zap.Logger, sink auditSink, method, addr, user, target string, start time.Time, err, softFailure error) {
	code := codes.OK
	if err != nil {
		code = status.Code(err)
	}
	result := code.String()
	if err == nil && softFailure != nil {
		// Reported through the response, not as an RPC error; see
		// softFailureOf.
		result = "FAILED"
		err = softFailure
	}
	fields := []zap.Field{
		zap.String("method", method),
		zap.String("client", addr),
		zap.String("result", result),
		zap.Bool("granted", code != codes.Unauthenticated && code != codes.PermissionDenied),
		zap.Duration("latency", time.Since(start)),
	}
	if user != "" {
		fields = append(fields, zap.String("user", user))
	}
	if target != "" {
		fields = append(fields, zap.String("target", target))
	}
	if err != nil {
		fields = append(fields, zap.String("error", status.Convert(err).Message()))
	}
	log.Info("api call", fields...)

	if sink == nil {
		log.Debug("writeAuditEntry: no persistent sink, log only", zap.String("method", method))
		return
	}
	ev := &database.AuditEvent{
		Timestamp: start,
		Method:    method,
		Client:    addr,
		User:      user,
		Target:    target,
		Result:    result,
		Granted:   code != codes.Unauthenticated && code != codes.PermissionDenied,
		Latency:   time.Since(start),
	}
	if err != nil {
		ev.Error = status.Convert(err).Message()
	}
	sinkStart := time.Now()
	sink(ev)
	log.Debug("writeAuditEntry: entry persisted", zap.String("method", method), zap.String("result", result),
		zap.Duration("took", time.Since(sinkStart)))
}

// softFailureOf reads a failure a handler reported in its response rather than
// as an RPC error. Most handlers here answer a failed operation with
// Success=false and a nil error, so the call completes as gRPC OK — and the
// audit trail recorded every failed add-volume, delete and failover as
// succeeded. The trail exists to answer what happened; a failure written down
// as a success is the one answer it must not give.
func softFailureOf(resp interface{}) error {
	r, ok := resp.(interface{ GetSuccess() bool })
	if !ok || r.GetSuccess() {
		return nil
	}
	msg := "operation reported failure"
	if m, ok := resp.(interface{ GetMessage() string }); ok && m.GetMessage() != "" {
		msg = m.GetMessage()
	}
	zap.L().Debug("softFailureOf: response reports failure", zap.String("message", msg))
	return errors.New(msg)
}

func resolveUser(resolve userResolver, ctx context.Context) string {
	if resolve == nil {
		return ""
	}
	return resolve(ctx)
}

// auditUnaryInterceptor records unary RPCs to the audit log.
func auditUnaryInterceptor(log *zap.Logger, includeReads bool, resolve userResolver, sink auditSink) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		if strings.HasPrefix(info.FullMethod, healthServicePrefix) {
			return handler(ctx, req)
		}
		method := shortMethod(info.FullMethod)
		if !includeReads && isReadOnly(method) {
			log.Debug("auditUnaryInterceptor: read not audited", zap.String("method", method))
			return handler(ctx, req)
		}
		log.Debug("auditUnaryInterceptor: auditing call", zap.String("method", method),
			zap.Bool("has_user_resolver", resolve != nil), zap.Bool("has_sink", sink != nil))
		start := time.Now()
		resp, err := handler(ctx, req)
		writeAuditEntry(log, sink, method, clientAddr(ctx), resolveUser(resolve, ctx), auditTarget(req), start, err, softFailureOf(resp))
		return resp, err
	}
}

// auditStreamInterceptor records streaming RPCs to the audit log.
func auditStreamInterceptor(log *zap.Logger, includeReads bool, resolve userResolver, sink auditSink) grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if strings.HasPrefix(info.FullMethod, healthServicePrefix) {
			return handler(srv, ss)
		}
		method := shortMethod(info.FullMethod)
		if !includeReads && isReadOnly(method) {
			log.Debug("auditStreamInterceptor: read stream not audited", zap.String("method", method))
			return handler(srv, ss)
		}
		log.Debug("auditStreamInterceptor: auditing stream", zap.String("method", method),
			zap.Bool("has_user_resolver", resolve != nil), zap.Bool("has_sink", sink != nil))
		start := time.Now()
		err := handler(srv, ss)
		writeAuditEntry(log, sink, method, clientAddr(ss.Context()), resolveUser(resolve, ss.Context()), "", start, err, nil)
		return err
	}
}
