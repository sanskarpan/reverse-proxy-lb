// coverage_gaps_test.go adds targeted coverage for the internal helpers that
// were at 0% after the initial run: defaultResource and Tracer.
// It uses package tracing (internal) so it can call the unexported defaultResource.
package tracing

import (
	"testing"
)

// TestDefaultResource verifies that defaultResource returns a non-nil resource
// with the expected service.name attribute.
func TestDefaultResource(t *testing.T) {
	r := defaultResource("my-service")
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
	r := defaultResource("")
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
