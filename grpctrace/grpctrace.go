// Package grpctrace traces gRPC with Tracepanel: each call a server serves is an `rpc` trace of its own, named
// Service/Method, and each call a client makes is a `grpc` span of the trace in its context. A call carries a W3C
// traceparent in its metadata, so the server's trace links to the client's call.
//
//	server := grpc.NewServer(grpctrace.ServerOptions()...)
//	conn, err := grpc.NewClient(target, append(grpctrace.DialOptions(), grpc.WithTransportCredentials(creds))...)
//
// The interceptors go first in a chain: a recovery interceptor before them gets the panics they record.
package grpctrace

import (
	"context"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	tracepanel "github.com/tracepanel/tracepanel-go"
	"github.com/tracepanel/tracepanel-go/internal/hook"
)

const traceparent = "traceparent"

// ServerOptions are the server interceptors, unary and stream, as grpc.NewServer takes them. They turn Tracepanel
// on (tracepanel.Setup, which takes a Config when called before them).
func ServerOptions() []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(UnaryServerInterceptor()),
		grpc.ChainStreamInterceptor(StreamServerInterceptor()),
	}
}

// DialOptions are the client interceptors, unary and stream, as grpc.NewClient takes them.
func DialOptions() []grpc.DialOption {
	return []grpc.DialOption{
		grpc.WithChainUnaryInterceptor(UnaryClientInterceptor()),
		grpc.WithChainStreamInterceptor(StreamClientInterceptor()),
	}
}

// ---------- servers ----------

// UnaryServerInterceptor traces each unary call the server serves.
func UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	tracepanel.Setup()
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (response any, err error) {
		ctx, trace := serve(ctx, info.FullMethod)
		if trace == nil {
			return handler(ctx, req)
		}
		defer func() { end(trace, err, recover()) }()
		return handler(ctx, req)
	}
}

// StreamServerInterceptor traces each streaming call the server serves, until its handler returns.
func StreamServerInterceptor() grpc.StreamServerInterceptor {
	tracepanel.Setup()
	return func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		ctx, trace := serve(stream.Context(), info.FullMethod)
		if trace == nil {
			return handler(srv, stream)
		}
		defer func() { end(trace, err, recover()) }()
		return handler(srv, &serverStream{ServerStream: stream, ctx: ctx})
	}
}

type serverStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *serverStream) Context() context.Context { return s.ctx }

// serve starts a call's trace, linked to the caller's by the traceparent its metadata carries.
func serve(ctx context.Context, fullMethod string) (context.Context, hook.Trace) {
	if !hook.Enabled() {
		return ctx, nil
	}
	service, method := split(fullMethod)
	attrs := map[string]any{
		"rpc.system": "grpc", "rpc.service": service, "rpc.method": method,
		"service.role": "web", "tracepanel.framework": "grpc",
	}
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if values := md.Get(traceparent); len(values) > 0 {
			for key, value := range hook.Incoming(values[0]) {
				attrs[key] = value
			}
		}
	}
	trace := hook.Start("rpc", "rpc", service+"/"+method, attrs)
	if trace == nil {
		return ctx, nil
	}
	return trace.Context(ctx), trace
}

// end ends a call's trace with its status: a server's fault (Internal, Unknown, Unavailable...) or a panic fails it,
// a client's (NotFound, InvalidArgument...) doesn't. The panic goes on.
func end(trace hook.Trace, err error, recovered any) {
	if recovered != nil {
		trace.Panic(recovered)
		trace.Set("rpc.grpc.status_code", int(codes.Unknown))
		trace.Fail()
		trace.Finish()
		panic(recovered)
	}
	code := status.Code(err)
	trace.Set("rpc.grpc.status_code", int(code))
	if err != nil && serverFault(code) {
		trace.Error(err)
		trace.Fail()
	}
	trace.Finish()
}

// serverFault: the codes OpenTelemetry counts as a server's error.
func serverFault(code codes.Code) bool {
	switch code {
	case codes.Unknown, codes.DeadlineExceeded, codes.Unimplemented, codes.Internal, codes.Unavailable, codes.DataLoss:
		return true
	}
	return false
}

// ---------- clients ----------

// UnaryClientInterceptor records each unary call as a `grpc` span of the trace in its context.
func UnaryClientInterceptor() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, fullMethod string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		ctx, span := call(ctx, fullMethod, cc)
		if span == nil {
			return invoker(ctx, fullMethod, req, reply, cc, opts...)
		}
		err := invoker(ctx, fullMethod, req, reply, cc, opts...)
		finish(span, err)
		return err
	}
}

// StreamClientInterceptor records each streaming call as a `grpc` span, until the stream ends.
func StreamClientInterceptor() grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, fullMethod string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		ctx, span := call(ctx, fullMethod, cc)
		if span == nil {
			return streamer(ctx, desc, cc, fullMethod, opts...)
		}
		stream, err := streamer(ctx, desc, cc, fullMethod, opts...)
		if err != nil {
			finish(span, err)
			return stream, err
		}
		return &clientStream{ClientStream: stream, span: span, single: !desc.ServerStreams}, nil
	}
}

// clientStream ends its call's span when the stream ends: an error (io.EOF is the end of a stream that went well), or
// the one answer of a stream the server answers once.
type clientStream struct {
	grpc.ClientStream
	span   hook.Span
	single bool
	once   sync.Once
}

func (s *clientStream) RecvMsg(message any) error {
	err := s.ClientStream.RecvMsg(message)
	if err == io.EOF {
		s.end(nil)
	} else if err != nil || s.single {
		s.end(err)
	}
	return err
}

func (s *clientStream) SendMsg(message any) error {
	err := s.ClientStream.SendMsg(message)
	if err != nil && err != io.EOF {
		s.end(err)
	}
	return err
}

func (s *clientStream) end(err error) { s.once.Do(func() { finish(s.span, err) }) }

// call opens a call's span, and is ctx with its traceparent in the outgoing metadata. None for a call outside a
// trace, or one that carries a traceparent already.
func call(ctx context.Context, fullMethod string, cc *grpc.ClientConn) (context.Context, hook.Span) {
	if !hook.Enabled() || !hook.Traced(ctx) {
		return ctx, nil
	}
	if md, ok := metadata.FromOutgoingContext(ctx); ok && len(md.Get(traceparent)) > 0 {
		return ctx, nil
	}
	service, method := split(fullMethod)
	attrs := map[string]any{"rpc.system": "grpc", "rpc.service": service, "rpc.method": method, "code.origin": nil}
	if origin := hook.Origin(); origin != "" {
		attrs["code.origin"] = origin
	}
	for key, value := range server(cc.Target()) {
		attrs[key] = value
	}
	span := hook.Begin(ctx, "grpc", service+"/"+method, attrs)
	if span == nil {
		return ctx, nil
	}
	span.Set("w3c.span_id", span.W3CID())
	return metadata.AppendToOutgoingContext(ctx, traceparent, span.Traceparent()), span
}

// finish ends a call's span with its status: any code but OK fails it.
func finish(span hook.Span, err error) {
	code := status.Code(err)
	span.Set("rpc.grpc.status_code", int(code))
	if code != codes.OK {
		span.Set("error.type", code.String())
	}
	span.End(code != codes.OK)
}

// split is "/helloworld.Greeter/SayHello" as its service and its method.
func split(fullMethod string) (string, string) {
	service, method, found := strings.Cut(strings.TrimPrefix(fullMethod, "/"), "/")
	if !found {
		return service, ""
	}
	return service, method
}

// server is the address and the port a client's target names: "dns:///orders.internal:50051", "orders:443".
func server(target string) map[string]any {
	scheme, rest, found := strings.Cut(target, "://")
	if !found {
		scheme, rest = "", target
	}
	if scheme == "unix" || scheme == "unix-abstract" {
		return map[string]any{"server.address": rest}
	}
	// The authority, if any, comes first: dns://8.8.8.8/orders.internal:50051.
	if slash := strings.LastIndex(rest, "/"); slash >= 0 {
		rest = rest[slash+1:]
	}
	if rest == "" {
		return nil
	}
	host, port, err := net.SplitHostPort(rest)
	if err != nil {
		return map[string]any{"server.address": rest}
	}
	out := map[string]any{"server.address": host}
	if number, err := strconv.Atoi(port); err == nil {
		out["server.port"] = number
	}
	return out
}
