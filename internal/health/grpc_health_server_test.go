package health_test

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"reverse-proxy-lb/internal/health"
)

const bufSize = 1 << 20 // 1 MiB

// newInProcessServer creates a gRPC server with the given GRPCHealthServer
// registered, listening on an in-memory bufconn listener.  The returned cleanup
// function stops the server and closes the listener.
func newInProcessServer(t *testing.T, hs *health.GRPCHealthServer) (grpc_health_v1.HealthClient, func()) {
	t.Helper()

	lis := bufconn.Listen(bufSize)
	srv := grpc.NewServer()
	hs.Register(srv)

	go func() {
		if err := srv.Serve(lis); err != nil {
			// Serve returns a non-nil error when the listener is closed; that is
			// expected during teardown, so we swallow it here.
			_ = err
		}
	}()

	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(_ context.Context, _ string) (net.Conn, error) {
			return lis.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("failed to dial in-process gRPC server: %v", err)
	}

	client := grpc_health_v1.NewHealthClient(conn)

	cleanup := func() {
		_ = conn.Close()
		srv.Stop()
		_ = lis.Close()
	}
	return client, cleanup
}

// TestGRPCHealthServerCheck verifies that Check returns the correct status for
// the overall key (""), a known service, and an unknown service.
func TestGRPCHealthServerCheck(t *testing.T) {
	hs := health.NewGRPCHealthServer()
	hs.SetServingStatus("proxy", grpc_health_v1.HealthCheckResponse_SERVING)

	client, cleanup := newInProcessServer(t, hs)
	defer cleanup()

	ctx := context.Background()

	// Overall health ("") should be SERVING (set by NewGRPCHealthServer default).
	resp, err := client.Check(ctx, &grpc_health_v1.HealthCheckRequest{Service: ""})
	if err != nil {
		t.Fatalf("Check(\"\") error: %v", err)
	}
	if resp.Status != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Errorf("Check(\"\") status = %v, want SERVING", resp.Status)
	}

	// Known service "proxy" should be SERVING.
	resp, err = client.Check(ctx, &grpc_health_v1.HealthCheckRequest{Service: "proxy"})
	if err != nil {
		t.Fatalf("Check(\"proxy\") error: %v", err)
	}
	if resp.Status != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Errorf("Check(\"proxy\") status = %v, want SERVING", resp.Status)
	}

	// Unknown service should also return SERVING (unknown -> assume alive).
	resp, err = client.Check(ctx, &grpc_health_v1.HealthCheckRequest{Service: "unknown-service"})
	if err != nil {
		t.Fatalf("Check(\"unknown-service\") error: %v", err)
	}
	if resp.Status != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Errorf("Check(\"unknown-service\") status = %v, want SERVING", resp.Status)
	}
}

// TestGRPCHealthServerUpdate verifies that SetServingStatus changes what
// subsequent Check calls return.
func TestGRPCHealthServerUpdate(t *testing.T) {
	hs := health.NewGRPCHealthServer()
	hs.SetServingStatus("proxy", grpc_health_v1.HealthCheckResponse_SERVING)

	client, cleanup := newInProcessServer(t, hs)
	defer cleanup()

	ctx := context.Background()

	// Initially SERVING.
	resp, err := client.Check(ctx, &grpc_health_v1.HealthCheckRequest{Service: "proxy"})
	if err != nil {
		t.Fatalf("initial Check error: %v", err)
	}
	if resp.Status != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Fatalf("initial status = %v, want SERVING", resp.Status)
	}

	// Flip to NOT_SERVING.
	hs.SetServingStatus("proxy", grpc_health_v1.HealthCheckResponse_NOT_SERVING)

	resp, err = client.Check(ctx, &grpc_health_v1.HealthCheckRequest{Service: "proxy"})
	if err != nil {
		t.Fatalf("Check after update error: %v", err)
	}
	if resp.Status != grpc_health_v1.HealthCheckResponse_NOT_SERVING {
		t.Errorf("status after update = %v, want NOT_SERVING", resp.Status)
	}

	// Flip back to SERVING.
	hs.SetServingStatus("proxy", grpc_health_v1.HealthCheckResponse_SERVING)

	resp, err = client.Check(ctx, &grpc_health_v1.HealthCheckRequest{Service: "proxy"})
	if err != nil {
		t.Fatalf("Check after second update error: %v", err)
	}
	if resp.Status != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Errorf("status after second update = %v, want SERVING", resp.Status)
	}
}

// TestGRPCHealthServerWatch verifies that Watch sends an initial status
// message and then unblocks when the context is cancelled.
func TestGRPCHealthServerWatch(t *testing.T) {
	hs := health.NewGRPCHealthServer()
	hs.SetServingStatus("proxy", grpc_health_v1.HealthCheckResponse_SERVING)

	client, cleanup := newInProcessServer(t, hs)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())

	stream, err := client.Watch(ctx, &grpc_health_v1.HealthCheckRequest{Service: "proxy"})
	if err != nil {
		t.Fatalf("Watch error: %v", err)
	}

	// The first message should arrive immediately with the current status.
	msg, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv first message error: %v", err)
	}
	if msg.Status != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Errorf("Watch first message status = %v, want SERVING", msg.Status)
	}

	// Cancelling the context should cause the server to close the stream.
	cancel()

	// Subsequent Recv should return a cancellation-related error.
	_, err = stream.Recv()
	if err == nil {
		t.Fatal("expected error after context cancellation, got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("error is not a gRPC status: %v", err)
	}
	// The server returns Canceled; the client transport may surface it as either
	// Canceled or Unavailable depending on timing, so accept both.
	if st.Code() != codes.Canceled && st.Code() != codes.Unavailable {
		t.Errorf("unexpected error code %v (error: %v)", st.Code(), err)
	}
}
