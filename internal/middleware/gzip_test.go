package middleware

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGzipCompressesWhenAccepted(t *testing.T) {
	body := strings.Repeat("hello world ", 100)
	h := Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, body)
	}))

	req := httptest.NewRequest(http.MethodGet, "http://x/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("expected Content-Encoding gzip, got %q", rec.Header().Get("Content-Encoding"))
	}
	gz, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("response was not valid gzip: %v", err)
	}
	got, _ := io.ReadAll(gz)
	if string(got) != body {
		t.Errorf("decompressed body mismatch")
	}
	if rec.Body.Len() >= len(body) {
		t.Errorf("expected compressed body smaller than original (%d) got %d", len(body), rec.Body.Len())
	}
}

func TestGzipSkippedWhenNotAccepted(t *testing.T) {
	h := Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "plain")
	}))
	req := httptest.NewRequest(http.MethodGet, "http://x/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Header().Get("Content-Encoding") == "gzip" {
		t.Error("should not gzip when client does not accept it")
	}
	if rec.Body.String() != "plain" {
		t.Errorf("unexpected body %q", rec.Body.String())
	}
}

func TestGzipDoesNotDoubleEncode(t *testing.T) {
	h := Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "br") // pretend already compressed
		io.WriteString(w, "already-encoded")
	}))
	req := httptest.NewRequest(http.MethodGet, "http://x/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Header().Get("Content-Encoding") != "br" {
		t.Errorf("expected original Content-Encoding preserved, got %q", rec.Header().Get("Content-Encoding"))
	}
	if rec.Body.String() != "already-encoded" {
		t.Errorf("body should be untouched, got %q", rec.Body.String())
	}
}

func TestGzipSkipsWebSocketUpgrade(t *testing.T) {
	h := Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := w.(*gzipResponseWriter); ok {
			t.Error("gzip wrapper must not wrap a websocket upgrade request")
		}
	}))
	req := httptest.NewRequest(http.MethodGet, "http://x/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	h.ServeHTTP(httptest.NewRecorder(), req)
}
