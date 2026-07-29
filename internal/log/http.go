package log

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	maxRetries     = 3
	retryDelayBase = 2 * time.Second
	// maxBodyPreview bounds how many bytes of each request/response body
	// are captured for debug logging. It keeps memory predictable for
	// arbitrarily large payloads while still capturing the request echo,
	// the response status line/headers, and the first several SSE events
	// of an LLM stream — enough to diagnose auth, format, and transport
	// problems. 16 KiB is deliberately small; full bodies are never held.
	maxBodyPreview = 16 << 10 // 16 KiB
)

// NewHTTPClient creates an HTTP client with debug logging and retry on 5xx errors.
func NewHTTPClient() *http.Client {
	return &http.Client{
		Transport: &RetryTransport{
			Transport: &HTTPRoundTripLogger{
				Transport: http.DefaultTransport,
			},
		},
	}
}

// RetryTransport is an http.RoundTripper that retries idempotent requests
// (or any request carrying an Idempotency-Key) on 5xx errors.
type RetryTransport struct {
	Transport http.RoundTripper
}

// RoundTrip implements http.RoundTripper with retry logic for 5xx errors.
//
// Only idempotent methods (GET, HEAD, OPTIONS, PUT, DELETE) or requests
// carrying an Idempotency-Key/X-Idempotency-Key header are retried. Other
// methods — notably POST to LLM completion endpoints — are returned after
// the first response, since retrying a request the server may already be
// processing could double-charge or double-generate. This transport is only
// wired up in debug mode, but the guard is cheap and strictly safer.
func (r *RetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	retryable := isRetryable(req)

	// Only buffer the request body when we might actually retry, so that
	// non-retryable requests stream straight through to the inner transport.
	var bodyBytes []byte
	if retryable && req.Body != nil && req.Body != http.NoBody {
		var err error
		bodyBytes, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		if err = req.Body.Close(); err != nil {
			return nil, err
		}
	}

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if bodyBytes != nil {
			req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		}

		if attempt > 0 {
			delay := retryDelayBase * time.Duration(1<<(attempt-1)) // exponential backoff
			slog.Warn("Retrying HTTP request due to server error",
				"attempt", attempt,
				"delay", delay,
				"method", req.Method,
				"url", req.URL,
			)
			select {
			case <-time.After(delay):
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
		}

		resp, err := r.Transport.RoundTrip(req)
		if err != nil {
			return nil, err
		}

		// Non-retryable requests, non-5xx responses, and the final attempt
		// are returned immediately.
		if !retryable || resp.StatusCode < 500 || resp.StatusCode >= 600 || attempt >= maxRetries {
			return resp, nil
		}

		// Close the response body before retrying.
		resp.Body.Close()
	}

	return nil, http.ErrHandlerTimeout
}

// isRetryable reports whether req is safe to repeat.
func isRetryable(req *http.Request) bool {
	switch req.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut, http.MethodDelete:
		return true
	}
	return req.Header.Get("Idempotency-Key") != "" || req.Header.Get("X-Idempotency-Key") != ""
}

// HTTPRoundTripLogger is an http.RoundTripper that logs requests and responses.
type HTTPRoundTripLogger struct {
	Transport http.RoundTripper
}

// RoundTrip implements http.RoundTripper with streaming-safe debug logging.
//
// When debug logging is not enabled at the current slog level, request and
// response bodies are passed through untouched: streaming is fully preserved
// and there is zero buffering overhead. When debug logging is enabled, a
// bounded preview (maxBodyPreview bytes) of each body is captured — the
// request preview up front, the response preview lazily as the caller reads
// — and the response is logged once when its body is closed, never before
// RoundTrip returns. The response body returned to the caller is always the
// live stream, never a fully materialized buffer.
func (h *HTTPRoundTripLogger) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	debugEnabled := slog.Default().Enabled(ctx, slog.LevelDebug)

	if debugEnabled && req.Body != nil && req.Body != http.NoBody {
		preview, err := io.ReadAll(io.LimitReader(req.Body, maxBodyPreview))
		if err != nil {
			slog.Error("HTTP request body preview failed",
				"method", req.Method, "url", req.URL, "error", err)
		}
		// Rebuild the full request body so nothing is lost: the captured
		// preview followed by whatever the original stream still holds.
		req.Body = concatBody(preview, req.Body)
		truncated := ""
		if len(preview) >= maxBodyPreview {
			truncated = " …(truncated)"
		}
		slog.Debug("HTTP Request",
			"method", req.Method,
			"url", req.URL,
			"body", prettyBody(preview)+truncated,
		)
	} else if debugEnabled {
		slog.Debug("HTTP Request", "method", req.Method, "url", req.URL)
	}

	start := time.Now()
	resp, err := h.Transport.RoundTrip(req)
	duration := time.Since(start)
	if err != nil {
		slog.Error("HTTP request failed",
			"method", req.Method,
			"url", req.URL,
			"duration_ms", duration.Milliseconds(),
			"error", err,
		)
		return resp, err
	}

	if !debugEnabled || resp.Body == nil || resp.Body == http.NoBody {
		// Fast path: return the live streaming body untouched.
		return resp, nil
	}

	// Wrap the response body so the preview is captured as the caller
	// reads, and the debug log is emitted once on Close (after the stream
	// completes), not before RoundTrip returns.
	statusCode, status := resp.StatusCode, resp.Status
	headers := formatHeaders(resp.Header)
	contentLength := resp.ContentLength
	resp.Body = newTeeBody(resp.Body, maxBodyPreview, func(preview string) {
		slog.Debug("HTTP Response",
			"status_code", statusCode,
			"status", status,
			"headers", headers,
			"body", prettyBody([]byte(preview)),
			"content_length", contentLength,
			"duration_ms", duration.Milliseconds(),
		)
	})
	return resp, nil
}

// concatBody returns a ReadCloser that yields prefix then the remaining
// bytes of rest, closing rest when closed itself.
func concatBody(prefix []byte, rest io.ReadCloser) io.ReadCloser {
	return &multiReadCloser{Reader: io.MultiReader(bytes.NewReader(prefix), rest), Closer: rest}
}

type multiReadCloser struct {
	io.Reader
	io.Closer
}

// teeBody streams reads through to an underlying body while copying a
// bounded prefix into a preview buffer. It is safe for concurrent
// Read/Close (e.g. context cancellation racing a pending read) and invokes
// onClose at most once, with the captured preview, when the body is closed.
type teeBody struct {
	body    io.ReadCloser
	limit   int
	onClose func(preview string)

	mu     sync.Mutex
	closed bool
	once   sync.Once
	buf    bytes.Buffer
}

func newTeeBody(body io.ReadCloser, limit int, onClose func(string)) *teeBody {
	return &teeBody{body: body, limit: limit, onClose: onClose}
}

func (b *teeBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	if n > 0 {
		b.mu.Lock()
		if b.buf.Len() < b.limit {
			room := b.limit - b.buf.Len()
			if n < room {
				room = n
			}
			b.buf.Write(p[:room])
		}
		b.mu.Unlock()
	}
	return n, err
}

func (b *teeBody) Close() error {
	b.mu.Lock()
	already := b.closed
	b.closed = true
	preview := b.buf.String()
	b.mu.Unlock()
	if already {
		return nil
	}
	err := b.body.Close()
	b.once.Do(func() {
		if b.onClose != nil {
			b.onClose(preview)
		}
	})
	return err
}

// prettyBody returns a best-effort indented rendering of src for logging;
// non-JSON payloads (such as SSE streams) are returned verbatim.
func prettyBody(src []byte) string {
	trimmed := bytes.TrimSpace(src)
	var b bytes.Buffer
	if json.Indent(&b, trimmed, "", "  ") != nil {
		return string(src)
	}
	return b.String()
}

// formatHeaders formats HTTP headers for logging, redacting sensitive ones.
func formatHeaders(headers http.Header) map[string][]string {
	filtered := make(map[string][]string)
	for key, values := range headers {
		lowerKey := strings.ToLower(key)
		if strings.Contains(lowerKey, "authorization") ||
			strings.Contains(lowerKey, "api-key") ||
			strings.Contains(lowerKey, "token") ||
			strings.Contains(lowerKey, "secret") {
			filtered[key] = []string{"[REDACTED]"}
		} else {
			filtered[key] = values
		}
	}
	return filtered
}
