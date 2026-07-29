package log

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPRoundTripLogger(t *testing.T) {
	// Create a test server that returns a 500 error.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Custom-Header", "test-value")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error": "Internal server error", "code": 500}`))
	}))
	defer server.Close()

	client := NewHTTPClient()

	req, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		server.URL,
		strings.NewReader(`{"test": "data"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer secret-token")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("Expected status code 500, got %d", resp.StatusCode)
	}
}

func TestFormatHeaders(t *testing.T) {
	headers := http.Header{
		"Content-Type":  []string{"application/json"},
		"Authorization": []string{"Bearer secret-token"},
		"X-API-Key":     []string{"api-key-123"},
		"User-Agent":    []string{"test-agent"},
	}

	formatted := formatHeaders(headers)

	if formatted["Authorization"][0] != "[REDACTED]" {
		t.Error("Authorization header should be redacted")
	}
	if formatted["X-API-Key"][0] != "[REDACTED]" {
		t.Error("X-API-Key header should be redacted")
	}
	if formatted["Content-Type"][0] != "application/json" {
		t.Error("Content-Type header should be preserved")
	}
	if formatted["User-Agent"][0] != "test-agent" {
		t.Error("User-Agent header should be preserved")
	}
}

// chunkedServer serves each chunk, flushing it, sleeping interChunk between
// chunks. It returns promptly when the client disconnects (request context
// canceled) so the test stays fast.
func chunkedServer(t *testing.T, chunks []string, interChunk time.Duration) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		for _, c := range chunks {
			if _, err := w.Write([]byte(c)); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			select {
			case <-time.After(interChunk):
			case <-r.Context().Done():
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// TestHTTPRoundTripLogger_StreamsWhenDebugDisabled proves the core fix: with
// slog above Debug, the response body is the live stream. With the old
// unconditional drainBody, RoundTrip blocked until the WHOLE body was
// buffered and the first chunk only arrived after the full ~1.2s stream.
func TestHTTPRoundTripLogger_StreamsWhenDebugDisabled(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo})))

	const interChunk = 400 * time.Millisecond
	server := chunkedServer(t, []string{"chunk-0;", "chunk-1;", "chunk-2;"}, interChunk)

	client := NewHTTPClient()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	probe := make([]byte, 64)
	n, err := io.ReadAtLeast(resp.Body, probe, 1)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	elapsed := time.Since(start)

	if !strings.Contains(string(probe[:n]), "chunk-0") {
		t.Fatalf("first read missing chunk-0: %q", probe[:n])
	}
	// Full stream takes ~3*interChunk = 1.2s. The first chunk must arrive
	// far sooner, proving the body was not fully buffered before return.
	if elapsed > 300*time.Millisecond {
		t.Fatalf("first chunk took %v; response body was buffered, not streamed", elapsed)
	}
}

// TestHTTPRoundTripLogger_StreamsAndLogsWhenDebugEnabled proves that even
// when the debug wrapper IS installed (body wrapped in teeBody), the first
// chunk still streams in immediately, and the response preview is logged
// once when the body is closed.
func TestHTTPRoundTripLogger_StreamsAndLogsWhenDebugEnabled(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	var logBuf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	const interChunk = 400 * time.Millisecond
	server := chunkedServer(t, []string{"chunk-0;", "chunk-1;", "chunk-2;"}, interChunk)

	client := NewHTTPClient()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	// Even with the debug wrapper installed, the first chunk must stream in
	// immediately rather than after the whole body is buffered.
	probe := make([]byte, 64)
	n, err := io.ReadAtLeast(resp.Body, probe, 1)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	firstRead := time.Since(start)
	if !strings.Contains(string(probe[:n]), "chunk-0") {
		t.Fatalf("first read missing chunk-0: %q", probe[:n])
	}
	if firstRead > 300*time.Millisecond {
		t.Fatalf("first chunk took %v under debug; body not streamed", firstRead)
	}

	// Drain the live stream so the deferred Close-time response log fires.
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("drain: %v", err)
	}
	resp.Body.Close()

	logged := logBuf.String()
	if !strings.Contains(logged, "HTTP Response") {
		t.Errorf("expected 'HTTP Response' debug log; got:\n%s", logged)
	}
	if !strings.Contains(logged, "chunk-0") {
		t.Errorf("expected response preview to include chunk-0; got:\n%s", logged)
	}
}

// TestRetryTransport_NoRetryForPost locks the idempotency guard: a POST that
// gets a 5xx hits the server exactly once with no backoff delay.
func TestRetryTransport_NoRetryForPost(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	client := NewHTTPClient()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL, strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected POST to hit server exactly once (no retry), got %d", got)
	}
	if elapsed > time.Second {
		t.Fatalf("non-retryable POST took %v; should return without backoff", elapsed)
	}
}
