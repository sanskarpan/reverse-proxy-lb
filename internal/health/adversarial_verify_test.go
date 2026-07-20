package health_test

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health/grpc_health_v1"

	"reverse-proxy-lb/internal/health"
)

// TestAdversarial_CheckEmptyServiceServing verifies Check("") returns SERVING.
func TestAdversarial_CheckEmptyServiceServing(t *testing.T) {
	hs := health.NewGRPCHealthServer()
	client, cleanup := newInProcessServer(t, hs)
	defer cleanup()

	resp, err := client.Check(context.Background(), &grpc_health_v1.HealthCheckRequest{Service: ""})
	if err != nil {
		t.Fatalf("Check(\"\") error: %v", err)
	}
	if resp.Status != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Errorf("Check(\"\") = %v, want SERVING", resp.Status)
	}
}

// TestAdversarial_WatchExactlyOneInitialResponse verifies that Watch sends exactly
// one initial status message and then blocks until the context is cancelled.
func TestAdversarial_WatchExactlyOneInitialResponse(t *testing.T) {
	hs := health.NewGRPCHealthServer()
	hs.SetServingStatus("proxy", grpc_health_v1.HealthCheckResponse_SERVING)
	client, cleanup := newInProcessServer(t, hs)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := client.Watch(ctx, &grpc_health_v1.HealthCheckRequest{Service: "proxy"})
	if err != nil {
		t.Fatalf("Watch() error: %v", err)
	}

	// First message must arrive promptly.
	msg, err := stream.Recv()
	if err != nil {
		t.Fatalf("first Recv() error: %v", err)
	}
	if msg.Status != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Errorf("first Watch message status = %v, want SERVING", msg.Status)
	}

	// No second message should arrive within 200 ms (the server blocks after the
	// first send until the context is cancelled).
	secondCh := make(chan error, 1)
	go func() {
		_, err := stream.Recv()
		secondCh <- err
	}()

	select {
	case err := <-secondCh:
		// A receive here means either a second real message or a stream close;
		// both are unexpected before we cancel.
		t.Errorf("got premature second Recv() result: %v", err)
	case <-time.After(200 * time.Millisecond):
		// Good: server is blocking.
	}

	// Cancel should unblock the server's Watch goroutine and close the stream.
	cancel()
	select {
	case <-secondCh:
		// Expected: stream closed/errored after cancel.
	case <-time.After(2 * time.Second):
		t.Fatal("Watch did not unblock within 2s after context cancel")
	}
}

// TestAdversarial_GRPCServerGracefulStop verifies that GracefulStop on the gRPC
// server exits cleanly (no goroutine leak).
func TestAdversarial_GRPCServerGracefulStop(t *testing.T) {
	hs := health.NewGRPCHealthServer()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := grpc.NewServer()
	hs.Register(srv)

	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = srv.Serve(lis)
	}()

	// Give the server a moment to accept connections.
	time.Sleep(10 * time.Millisecond)

	srv.GracefulStop()

	select {
	case <-serveDone:
		// Good: Serve() returned after GracefulStop.
	case <-time.After(2 * time.Second):
		t.Fatal("GracefulStop: Serve() did not exit within 2s (goroutine leak)")
	}
}

// TestAdversarial_RaceSafety verifies no data race between concurrent
// SetServingStatus and Check calls (the -race detector will catch any issues).
func TestAdversarial_RaceSafety(t *testing.T) {
	hs := health.NewGRPCHealthServer()
	client, cleanup := newInProcessServer(t, hs)
	defer cleanup()

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				hs.SetServingStatus("svc", grpc_health_v1.HealthCheckResponse_SERVING)
				hs.SetServingStatus("svc", grpc_health_v1.HealthCheckResponse_NOT_SERVING)
			}
		}()
		go func() {
			defer wg.Done()
			ctx := context.Background()
			for j := 0; j < 500; j++ {
				_, _ = client.Check(ctx, &grpc_health_v1.HealthCheckRequest{Service: "svc"})
			}
		}()
	}
	wg.Wait()
}
