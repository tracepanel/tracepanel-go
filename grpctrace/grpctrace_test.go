package grpctrace

import (
	"context"
	"fmt"
	"io"
	"net"
	"sort"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	tracepanel "github.com/tracepanel/tracepanel-go"
	"github.com/tracepanel/tracepanel-go/internal/tracetest"
)

// orders is a service of hand-written handlers (no generated code), on the health service's messages.
type orders struct{}

func unary(name string, handle func(context.Context) error) grpc.MethodDesc {
	return grpc.MethodDesc{MethodName: name, Handler: func(srv any, ctx context.Context, decode func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
		in := new(healthpb.HealthCheckRequest)
		if err := decode(in); err != nil {
			return nil, err
		}
		handler := func(ctx context.Context, _ any) (any, error) {
			if err := handle(ctx); err != nil {
				return nil, err
			}
			return &healthpb.HealthCheckResponse{Status: healthpb.HealthCheckResponse_SERVING}, nil
		}
		return interceptor(ctx, in, &grpc.UnaryServerInfo{Server: srv, FullMethod: "/shop.Orders/" + name}, handler)
	}}
}

var service = grpc.ServiceDesc{
	ServiceName: "shop.Orders",
	HandlerType: (*any)(nil),
	Methods: []grpc.MethodDesc{
		unary("Place", func(ctx context.Context) error {
			_, end := tracepanel.Span(ctx, "reserve stock")
			end()
			return nil
		}),
		unary("Find", func(context.Context) error { return status.Error(codes.NotFound, "no such order") }),
		unary("Break", func(context.Context) error { return status.Error(codes.Internal, "database down") }),
		unary("Boom", func(context.Context) error { panic("out of stock") }),
	},
	Streams: []grpc.StreamDesc{{
		StreamName: "Watch", ServerStreams: true,
		Handler: func(_ any, stream grpc.ServerStream) error {
			_, end := tracepanel.Span(stream.Context(), "watch")
			end()
			for range 2 {
				if err := stream.SendMsg(&healthpb.HealthCheckResponse{}); err != nil {
					return err
				}
			}
			return nil
		},
	}},
}

// recovering answers a panic with Internal, as a recovery interceptor would, before grpctrace's.
func recovering(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (response any, err error) {
	defer func() {
		if recover() != nil {
			err = status.Error(codes.Internal, "panic")
		}
	}()
	return handler(ctx, req)
}

func connect(t *testing.T) *grpc.ClientConn {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer(grpc.ChainUnaryInterceptor(recovering, UnaryServerInterceptor()), grpc.ChainStreamInterceptor(StreamServerInterceptor()))
	server.RegisterService(&service, orders{})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:///orders.internal:50051", append(DialOptions(),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func invoke(ctx context.Context, conn *grpc.ClientConn, method string) error {
	return conn.Invoke(ctx, "/shop.Orders/"+method, &healthpb.HealthCheckRequest{}, &healthpb.HealthCheckResponse{})
}

// byKind is the traces of each kind.
func byKind(traces []tracetest.Doc) map[string]tracetest.Doc {
	out := map[string]tracetest.Doc{}
	for _, trace := range traces {
		out[trace["kind"].(string)] = trace
	}
	return out
}

func TestACallIsASpanOfTheClientAndATraceOfTheServer(t *testing.T) {
	recorder := tracetest.Record(t)
	conn := connect(t)

	if err := tracepanel.Command(context.Background(), "checkout", func(ctx context.Context) error {
		return invoke(ctx, conn, "Place")
	}); err != nil {
		t.Fatal(err)
	}

	traces := byKind(recorder.Wait(t, 2))
	client, server := traces["command"], traces["rpc"]
	calls := tracetest.Spans(client, "grpc")
	if len(calls) != 1 {
		t.Fatalf("client spans: %v", client["spans"])
	}
	call := tracetest.Attrs(calls[0])
	if calls[0]["name"] != "shop.Orders/Place" || calls[0]["status"] != "ok" || call["rpc.system"] != "grpc" ||
		call["rpc.service"] != "shop.Orders" || call["rpc.method"] != "Place" || call["rpc.grpc.status_code"] != float64(0) ||
		call["server.address"] != "orders.internal" || call["server.port"] != float64(50051) || call["code.origin"] == nil {
		t.Fatalf("client span: %v %v", calls[0]["name"], call)
	}
	attrs := tracetest.Attrs(server)
	if server["name"] != "shop.Orders/Place" || server["status"] != "ok" || attrs["rpc.system"] != "grpc" ||
		attrs["rpc.service"] != "shop.Orders" || attrs["rpc.method"] != "Place" || attrs["rpc.grpc.status_code"] != float64(0) ||
		attrs["service.role"] != "web" {
		t.Fatalf("server trace: %v %v", server["name"], attrs)
	}
	// The server's trace is the client's call's, by the traceparent.
	if attrs["w3c.trace_id"] != tracetest.Attrs(client)["w3c.trace_id"] || attrs["w3c.parent_span_id"] != call["w3c.span_id"] {
		t.Fatalf("linked: server %v, client %v %v", attrs, tracetest.Attrs(client), call)
	}
	if work := tracetest.Spans(server, "function"); len(work) != 1 {
		t.Fatalf("server work: %v", server["spans"])
	}
}

func TestAClientErrorLeavesTheServerTraceOK(t *testing.T) {
	recorder := tracetest.Record(t)
	conn := connect(t)

	_ = tracepanel.Command(context.Background(), "lookup", func(ctx context.Context) error {
		_ = invoke(ctx, conn, "Find")
		return nil
	})

	traces := byKind(recorder.Wait(t, 2))
	server, client := traces["rpc"], traces["command"]
	if server["status"] != "ok" || tracetest.Attrs(server)["rpc.grpc.status_code"] != float64(codes.NotFound) {
		t.Fatalf("server: %v %v", server["status"], tracetest.Attrs(server))
	}
	call := tracetest.Spans(client, "grpc")[0]
	if call["status"] != "error" || tracetest.Attrs(call)["error.type"] != "NotFound" {
		t.Fatalf("client span: %v", call)
	}
}

func TestAServerFaultFailsTheTrace(t *testing.T) {
	recorder := tracetest.Record(t)
	conn := connect(t)

	_ = invoke(context.Background(), conn, "Break")

	server := recorder.Wait(t, 1)[0]
	errors := tracetest.List(server, "errors")
	if server["status"] != "error" || len(errors) != 1 || tracetest.Attrs(server)["rpc.grpc.status_code"] != float64(codes.Internal) {
		t.Fatalf("server: %v %v", server["status"], errors)
	}
}

func TestAPanicIsRecordedAndGoesOn(t *testing.T) {
	recorder := tracetest.Record(t)
	conn := connect(t)

	err := invoke(context.Background(), conn, "Boom")

	server := recorder.Wait(t, 1)[0]
	errors := tracetest.List(server, "errors")
	if status.Code(err) != codes.Internal || server["status"] != "error" || len(errors) != 1 || errors[0]["message"] != "out of stock" {
		t.Fatalf("err %v, server %v %v", err, server["status"], errors)
	}
}

func TestAStream(t *testing.T) {
	recorder := tracetest.Record(t)
	conn := connect(t)

	if err := tracepanel.Command(context.Background(), "watch", func(ctx context.Context) error {
		stream, err := conn.NewStream(ctx, &service.Streams[0], "/shop.Orders/Watch")
		if err != nil {
			return err
		}
		if err := stream.SendMsg(&healthpb.HealthCheckRequest{}); err != nil {
			return err
		}
		if err := stream.CloseSend(); err != nil {
			return err
		}
		for {
			if err := stream.RecvMsg(&healthpb.HealthCheckResponse{}); err == io.EOF {
				return nil
			} else if err != nil {
				return err
			}
		}
	}); err != nil {
		t.Fatal(err)
	}

	traces := byKind(recorder.Wait(t, 2))
	server, client := traces["rpc"], traces["command"]
	call := tracetest.Spans(client, "grpc")
	if len(call) != 1 || call[0]["name"] != "shop.Orders/Watch" || call[0]["status"] != "ok" {
		t.Fatalf("client: %v", client["spans"])
	}
	if server["name"] != "shop.Orders/Watch" || len(tracetest.Spans(server, "function")) != 1 ||
		tracetest.Attrs(server)["w3c.parent_span_id"] != tracetest.Attrs(call[0])["w3c.span_id"] {
		t.Fatalf("server: %v", server)
	}
}

func TestServer(t *testing.T) {
	for target, want := range map[string]map[string]any{
		"dns:///orders.internal:50051":       {"server.address": "orders.internal", "server.port": 50051},
		"orders.internal:443":                {"server.address": "orders.internal", "server.port": 443},
		"dns://8.8.8.8/orders.internal:9000": {"server.address": "orders.internal", "server.port": 9000},
		"unix:///run/orders.sock":            {"server.address": "/run/orders.sock"},
		"passthrough:///bufnet":              {"server.address": "bufnet"},
	} {
		if got := server(target); !tracetest.Equal(got, want) {
			t.Errorf("server(%q) = %v, want %v", target, got, want)
		}
	}
}

func TestACallAsksTheServerToProfileItsTrace(t *testing.T) {
	recorder := tracetest.RecordProfiles(t, false)
	conn := connect(t)

	// Asked by the call's metadata, as a request asks with X-Tracepanel-Profile; then by a profiled caller; then not.
	asking := metadata.AppendToOutgoingContext(context.Background(), "x-tracepanel-profile", "1")
	if err := invoke(asking, conn, "Place"); err != nil {
		t.Fatal(err)
	}
	if err := tracepanel.Command(context.Background(), "checkout", func(ctx context.Context) error {
		return invoke(tracepanel.Profile(ctx), conn, "Place")
	}); err != nil {
		t.Fatal(err)
	}
	if err := invoke(context.Background(), conn, "Place"); err != nil {
		t.Fatal(err)
	}

	// A profiled trace reaches the agent once its profile is cut, after the others: in no order.
	got := []string{}
	for _, trace := range recorder.Wait(t, 4) {
		got = append(got, fmt.Sprint(trace["kind"], " ", tracetest.Attrs(trace)["profile.reason"], " ", trace["profile"] != nil))
	}
	sort.Strings(got)
	want := []string{
		"command code true",
		"rpc <nil> false",
		"rpc header true",
		"rpc header true",
	}
	if !tracetest.Equal(got, want) {
		t.Fatalf("traces %v", got)
	}
}
