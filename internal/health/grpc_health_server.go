package health

import (
	"context"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
)

// GRPCHealthServer implements the gRPC Health Checking Protocol (grpc.health.v1).
// It maintains a map of service-name -> serving status, updated via SetServingStatus.
// The empty string "" key represents the overall server health.
//
// UnimplementedHealthServer is embedded to satisfy any future method additions to
// the HealthServer interface without breaking compilation.
type GRPCHealthServer struct {
	grpc_health_v1.UnimplementedHealthServer
	mu       sync.RWMutex
	statuses map[string]grpc_health_v1.HealthCheckResponse_ServingStatus
}

// NewGRPCHealthServer creates a GRPCHealthServer with the overall status ("") set
// to SERVING.
func NewGRPCHealthServer() *GRPCHealthServer {
	s := &GRPCHealthServer{
		statuses: make(map[string]grpc_health_v1.HealthCheckResponse_ServingStatus),
	}
	// Default: overall server is SERVING.
	s.statuses[""] = grpc_health_v1.HealthCheckResponse_SERVING
	return s
}

// SetServingStatus sets the serving status for the named service.  Use the empty
// string "" to set the overall server status.
func (s *GRPCHealthServer) SetServingStatus(service string, st grpc_health_v1.HealthCheckResponse_ServingStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statuses[service] = st
}

// Check implements grpc_health_v1.HealthServer.  It returns the status recorded
// for req.Service, or SERVING when the service is not known (treating unknown
// services as alive so clients can query any service name without a pre-registration
// step).  The overall status ("") always reflects what SetServingStatus last set.
func (s *GRPCHealthServer) Check(
	_ context.Context,
	req *grpc_health_v1.HealthCheckRequest,
) (*grpc_health_v1.HealthCheckResponse, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	st, ok := s.statuses[req.GetService()]
	if !ok {
		// Unknown service: report SERVING so traffic keeps flowing by default.
		st = grpc_health_v1.HealthCheckResponse_SERVING
	}
	return &grpc_health_v1.HealthCheckResponse{Status: st}, nil
}

// Watch implements grpc_health_v1.HealthServer.  It sends a single status update
// immediately, then blocks until the client cancels the stream (context done).
// A full pub/sub implementation is not needed for the basic health-check use case.
func (s *GRPCHealthServer) Watch(
	req *grpc_health_v1.HealthCheckRequest,
	stream grpc_health_v1.Health_WatchServer,
) error {
	s.mu.RLock()
	st, ok := s.statuses[req.GetService()]
	if !ok {
		st = grpc_health_v1.HealthCheckResponse_SERVING
	}
	s.mu.RUnlock()

	if err := stream.Send(&grpc_health_v1.HealthCheckResponse{Status: st}); err != nil {
		return err
	}

	// Block until the client cancels.
	<-stream.Context().Done()

	if ctx := stream.Context(); ctx.Err() != nil {
		switch ctx.Err() {
		case context.Canceled:
			return status.Error(codes.Canceled, "client cancelled the watch")
		default:
			return status.Error(codes.DeadlineExceeded, "watch deadline exceeded")
		}
	}
	return nil
}

// Register registers this server's health-check handler on the provided gRPC
// server, making the grpc.health.v1.Health service available.
func (s *GRPCHealthServer) Register(srv *grpc.Server) {
	grpc_health_v1.RegisterHealthServer(srv, s)
}

// RegisterReflection enables gRPC Server Reflection on srv, which lets gRPC
// clients (e.g. grpc_cli, grpcurl) discover the services and methods available
// without a compiled proto descriptor.
func RegisterReflection(srv *grpc.Server) {
	reflection.Register(srv)
}
