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
// when the debug wrapper IS installed (body wrapped in teeBody, which only
// happens when the LogHTTPBodies opt-in is also set — see P3.1), the first
// chunk still streams in immediately, and the response preview is logged
// once when the body is closed.
func TestHTTPRoundTripLogger_StreamsAndLogsWhenDebugEnabled(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	var logBuf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	prevFlag := LogHTTPBodies
	LogHTTPBodies = true
	t.Cleanup(func() { LogHTTPBodies = prevFlag })

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

// TestHTTPRoundTripLogger_BodyNotLoggedByDefault proves the P3.1 fix: even
// with slog at Debug level (the "someone passed --debug" case), request and
// response bodies are NOT captured or logged unless the separate
// LogHTTPBodies opt-in is also set. Only method/url/status/content_length/
// duration should appear.
func TestHTTPRoundTripLogger_BodyNotLoggedByDefault(t *testing.T) {
	prevLogger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prevLogger) })
	var logBuf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	prevFlag := LogHTTPBodies
	LogHTTPBodies = false
	t.Cleanup(func() { LogHTTPBodies = prevFlag })

	const secretReq = `{"api_key": "sk-super-secret-request", "prompt": "hello"}`
	const secretResp = `{"choices": [{"message": "hi"}], "api_key_echo": "sk-super-secret-response"}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(secretResp))
	}))
	t.Cleanup(server.Close)

	client := NewHTTPClient()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL, strings.NewReader(secretReq))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer request-secret-token")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("drain: %v", err)
	}
	resp.Body.Close()

	logged := logBuf.String()

	if !strings.Contains(logged, "HTTP Request") || !strings.Contains(logged, "HTTP Response") {
		t.Fatalf("expected both request/response debug lines; got:\n%s", logged)
	}
	if !strings.Contains(logged, "status_code=200") && !strings.Contains(logged, `status_code=200`) {
		// status_code is logged as a plain attr; just check the value is present somewhere.
		if !strings.Contains(logged, "200") {
			t.Errorf("expected status code 200 to be logged; got:\n%s", logged)
		}
	}
	if !strings.Contains(logged, "duration_ms") {
		t.Errorf("expected duration_ms to be logged; got:\n%s", logged)
	}
	if !strings.Contains(logged, "content_length") {
		t.Errorf("expected content_length to be logged; got:\n%s", logged)
	}

	if strings.Contains(logged, "sk-super-secret-request") {
		t.Errorf("request body leaked into debug log when LogHTTPBodies=false:\n%s", logged)
	}
	if strings.Contains(logged, "sk-super-secret-response") {
		t.Errorf("response body leaked into debug log when LogHTTPBodies=false:\n%s", logged)
	}
	if strings.Contains(logged, "hello") || strings.Contains(logged, "\"hi\"") {
		t.Errorf("body content leaked into debug log when LogHTTPBodies=false:\n%s", logged)
	}
	if strings.Contains(logged, `"body"`) {
		t.Errorf("a \"body\" field should not be present at all when LogHTTPBodies=false:\n%s", logged)
	}
}

// TestHTTPRoundTripLogger_BodyLoggedWithRedactionWhenOptedIn proves that
// when LogHTTPBodies=true, bodies ARE captured and logged, but known
// secret-shaped JSON fields (api_key, authorization, token, secret,
// password, etc. at any nesting depth) are redacted first.
func TestHTTPRoundTripLogger_BodyLoggedWithRedactionWhenOptedIn(t *testing.T) {
	prevLogger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prevLogger) })
	var logBuf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	prevFlag := LogHTTPBodies
	LogHTTPBodies = true
	t.Cleanup(func() { LogHTTPBodies = prevFlag })

	const secretReq = `{"api_key": "sk-super-secret-request", "prompt": "hello-world", "nested": {"password": "hunter2"}}`
	const secretResp = `{"choices": [{"message": "hi-there"}], "auth": {"token": "resp-secret-tok"}}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(secretResp))
	}))
	t.Cleanup(server.Close)

	client := NewHTTPClient()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL, strings.NewReader(secretReq))
	if err != nil {
		t.Fatal(err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("drain: %v", err)
	}
	resp.Body.Close()

	logged := logBuf.String()

	// Secrets must never appear verbatim.
	if strings.Contains(logged, "sk-super-secret-request") {
		t.Errorf("request api_key leaked unredacted:\n%s", logged)
	}
	if strings.Contains(logged, "hunter2") {
		t.Errorf("nested request password leaked unredacted:\n%s", logged)
	}
	if strings.Contains(logged, "resp-secret-tok") {
		t.Errorf("nested response token leaked unredacted:\n%s", logged)
	}

	// Non-sensitive content must still be present (proves body logging is
	// genuinely on, not just silently dropping everything).
	if !strings.Contains(logged, "hello-world") {
		t.Errorf("expected non-sensitive request field to be logged; got:\n%s", logged)
	}
	if !strings.Contains(logged, "hi-there") {
		t.Errorf("expected non-sensitive response field to be logged; got:\n%s", logged)
	}

	// The redaction marker must appear (proves redaction actually ran).
	if !strings.Contains(logged, "[REDACTED]") {
		t.Errorf("expected [REDACTED] marker in logged body; got:\n%s", logged)
	}
}

// TestRedactBodySecrets_SSE proves per-line SSE redaction: each "data: {...}"
// line's JSON payload is redacted independently.
func TestRedactBodySecrets_SSE(t *testing.T) {
	body := "event: message\n" +
		`data: {"delta": "hello", "api_key": "leak-me"}` + "\n" +
		"\n" +
		`data: {"delta": "world"}` + "\n"

	got := redactBodySecrets(body)

	if strings.Contains(got, "leak-me") {
		t.Errorf("SSE data line api_key leaked unredacted:\n%s", got)
	}
	if !strings.Contains(got, "hello") || !strings.Contains(got, "world") {
		t.Errorf("SSE redaction dropped non-sensitive content:\n%s", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Errorf("expected [REDACTED] marker:\n%s", got)
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
