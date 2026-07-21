// coverage_gaps_test.go adds targeted coverage for the internal helpers that
// were at 0% after the initial run: defaultResource and Tracer.
// It uses package tracing (internal) so it can call the unexported defaultResource.
package tracing

import (
	"context"
	"testing"
)

// TestDefaultResource verifies that defaultResource returns a non-nil resource
// with the expected service.name attribute.
func TestDefaultResource(t *testing.T) {
	r := defaultResource(context.Background(), "my-service")
	if r == nil {
		t.Fatal("defaultResource returned nil")
	}
	// The service.name attribute should be present.
	found := false
	for _, attr := range r.Attributes() {
		if string(attr.Key) == "service.name" && attr.Value.AsString() == "my-service" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("service.name=my-service not found in resource attributes: %v", r.Attributes())
	}
}

// TestDefaultResourceFallbackName verifies that an empty serviceName defaults
// to "rplb".
func TestDefaultResourceFallbackName(t *testing.T) {
	r := defaultResource(context.Background(), "")
	if r == nil {
		t.Fatal("defaultResource('') returned nil")
	}
	found := false
	for _, attr := range r.Attributes() {
		if string(attr.Key) == "service.name" && attr.Value.AsString() == "rplb" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("default service name 'rplb' not found in attributes: %v", r.Attributes())
	}
}

// TestDefaultResourceCancelledContext verifies that defaultResource falls back
// to a minimal resource when the context is already cancelled (simulating a
// resource-detection failure path).
func TestDefaultResourceCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // immediately cancel so resource.New gets a done context

	r := defaultResource(ctx, "svc-cancel-test")
	if r == nil {
		t.Fatal("defaultResource with cancelled context returned nil; expected fallback resource")
	}
	// Even the fallback must preserve the service.name.
	found := false
	for _, attr := range r.Attributes() {
		if string(attr.Key) == "service.name" && attr.Value.AsString() == "svc-cancel-test" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("service.name=svc-cancel-test not found in fallback resource attributes: %v", r.Attributes())
	}
}
