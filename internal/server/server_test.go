package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// newOriginRequest builds a fake WS upgrade request carrying the given
// Origin header (omit by passing "").
func newOriginRequest(origin string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/ws", nil)
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	return r
}

// TestCheckOriginEmptyAccepted verifies non-browser clients (CLI, scripts,
// curl, gorilla/websocket dialers) that never send an Origin header are
// allowed through. This is the fork's primary non-interactive use case and
// must never regress.
func TestCheckOriginEmptyAccepted(t *testing.T) {
	t.Parallel()

	s := &Server{port: "8080"}
	require.True(t, s.checkOrigin(newOriginRequest("")), "empty Origin (non-browser client) must be accepted")
}

// TestCheckOriginLocalhostAccepted verifies browser requests from
// http://localhost:<port> and http://127.0.0.1:<port> — where <port> is the
// port this server actually bound to — are accepted.
func TestCheckOriginLocalhostAccepted(t *testing.T) {
	t.Parallel()

	s := &Server{port: "8080"}

	cases := []string{
		"http://localhost:8080",
		"http://127.0.0.1:8080",
		"http://[::1]:8080",
	}
	for _, origin := range cases {
		require.True(t, s.checkOrigin(newOriginRequest(origin)), "origin %q should be accepted", origin)
	}
}

// TestCheckOriginWrongPortRejected verifies that even a same-host origin is
// rejected if the port doesn't match the one this server actually bound to —
// otherwise a malicious page on the same machine but a different crush
// instance's port (or a stale cached value) could sneak through.
func TestCheckOriginWrongPortRejected(t *testing.T) {
	t.Parallel()

	s := &Server{port: "8080"}
	require.False(t, s.checkOrigin(newOriginRequest("http://localhost:9999")))
}

// TestCheckOriginForeignHostRejected is the core CSWSH regression test: a
// page hosted on an attacker-controlled origin must never be allowed to
// complete the WebSocket handshake, regardless of scheme or port.
func TestCheckOriginForeignHostRejected(t *testing.T) {
	t.Parallel()

	s := &Server{port: "8080"}

	cases := []string{
		"http://evil.example.com",
		"https://evil.example.com:8080",
		"http://evil.example.com:8080",
		"null",
		"http://localhost.evil.com:8080",
		"http://127.0.0.1.evil.com:8080",
	}
	for _, origin := range cases {
		require.False(t, s.checkOrigin(newOriginRequest(origin)), "origin %q must be rejected", origin)
	}
}

// TestCheckOriginHttpsRejected verifies the fork doesn't accidentally widen
// the allow-list to https origins: the web UI is served over plain HTTP on
// localhost, so an https:// origin has no legitimate reason to appear here.
func TestCheckOriginHttpsRejected(t *testing.T) {
	t.Parallel()

	s := &Server{port: "8080"}
	require.False(t, s.checkOrigin(newOriginRequest("https://localhost:8080")))
}
