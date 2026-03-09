package log

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const (
	maxRetries     = 3
	retryDelayBase = 2 * time.Second
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

// RetryTransport is an http.RoundTripper that retries requests on 5xx errors.
type RetryTransport struct {
	Transport http.RoundTripper
}

// RoundTrip implements http.RoundTripper with retry logic for 5xx errors.
func (r *RetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Save request body for potential retries
	var bodyBytes []byte
	if req.Body != nil && req.Body != http.NoBody {
		var err error
		bodyBytes, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body.Close()
	}

	for attempt := 0; attempt <= maxRetries; attempt++ {
		// Restore request body for each attempt
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

		// Return response if not a 5xx error or max retries reached
		if resp.StatusCode < 500 || resp.StatusCode >= 600 || attempt >= maxRetries {
			return resp, nil
		}

		// Close response body before retry
		resp.Body.Close()
	}

	// This should never be reached, but return an error just in case
	return nil, http.ErrHandlerTimeout
}

// HTTPRoundTripLogger is an http.RoundTripper that logs requests and responses.
type HTTPRoundTripLogger struct {
	Transport http.RoundTripper
}

// RoundTrip implements http.RoundTripper interface with logging.
func (h *HTTPRoundTripLogger) RoundTrip(req *http.Request) (*http.Response, error) {
	var err error
	var save io.ReadCloser
	save, req.Body, err = drainBody(req.Body)
	if err != nil {
		slog.Error(
			"HTTP request failed",
			"method", req.Method,
			"url", req.URL,
			"error", err,
		)
		return nil, err
	}

	if slog.Default().Enabled(req.Context(), slog.LevelDebug) {
		slog.Debug(
			"HTTP Request",
			"method", req.Method,
			"url", req.URL,
			"body", bodyToString(save),
		)
	}

	start := time.Now()
	resp, err := h.Transport.RoundTrip(req)
	duration := time.Since(start)
	if err != nil {
		slog.Error(
			"HTTP request failed",
			"method", req.Method,
			"url", req.URL,
			"duration_ms", duration.Milliseconds(),
			"error", err,
		)
		return resp, err
	}

	save, resp.Body, err = drainBody(resp.Body)
	if slog.Default().Enabled(req.Context(), slog.LevelDebug) {
		slog.Debug(
			"HTTP Response",
			"status_code", resp.StatusCode,
			"status", resp.Status,
			"headers", formatHeaders(resp.Header),
			"body", bodyToString(save),
			"content_length", resp.ContentLength,
			"duration_ms", duration.Milliseconds(),
			"error", err,
		)
	}
	return resp, err
}

func bodyToString(body io.ReadCloser) string {
	if body == nil {
		return ""
	}
	src, err := io.ReadAll(body)
	if err != nil {
		slog.Error("Failed to read body", "error", err)
		return ""
	}
	var b bytes.Buffer
	if json.Indent(&b, bytes.TrimSpace(src), "", "  ") != nil {
		// not json probably
		return string(src)
	}
	return b.String()
}

// formatHeaders formats HTTP headers for logging, filtering out sensitive information.
func formatHeaders(headers http.Header) map[string][]string {
	filtered := make(map[string][]string)
	for key, values := range headers {
		lowerKey := strings.ToLower(key)
		// Filter out sensitive headers
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

func drainBody(b io.ReadCloser) (r1, r2 io.ReadCloser, err error) {
	if b == nil || b == http.NoBody {
		return http.NoBody, http.NoBody, nil
	}
	var buf bytes.Buffer
	if _, err = buf.ReadFrom(b); err != nil {
		return nil, b, err
	}
	if err = b.Close(); err != nil {
		return nil, b, err
	}
	return io.NopCloser(&buf), io.NopCloser(bytes.NewReader(buf.Bytes())), nil
}
