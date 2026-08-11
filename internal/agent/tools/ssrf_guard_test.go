package tools

// ssrf_guard_test.go covers the SSRF defense-in-depth gate shared by the
// download and fetch tools (see ssrf_guard.go). It checks three layers:
//
//  1. The pure range classifier (isForbiddenSSRFAddress) over IPv4 and
//     IPv6, including the cloud-metadata endpoints.
//  2. The wired client (NewSSRFGuardedClient): a guarded client must
//     refuse a dial to a 127.0.0.1 httptest.Server, while the same
//     client built with allowPrivate=true must succeed.
//  3. The tools themselves: download/fetch built with the default
//     (nil → guarded) client must refuse a 127.0.0.1 target rather
//     than exfiltrate its body.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

func TestIsForbiddenSSRFAddress(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		ip        string
		forbidden bool
	}{
		// Loopback — the most common local-dev target and the range
		// httptest.Server lives in.
		{"ipv4 loopback", "127.0.0.1", true},
		{"ipv4 loopback high", "127.255.255.254", true},
		{"ipv6 loopback", "::1", true},

		// RFC1918 private.
		{"10/8 private", "10.0.0.1", true},
		{"172.16/12 start", "172.16.0.1", true},
		{"172.16/12 end", "172.31.255.255", true},
		{"172.32 just outside private", "172.32.0.1", false},
		{"192.168/16 private", "192.168.1.1", true},

		// IPv6 unique-local (covers AWS IMDS IPv6 fd00:ec2::254).
		{"unique-local fd00 ec2 metadata", "fd00:ec2::254", true},
		{"unique-local fc01", "fc01::1", true},

		// Link-local — covers IPv4 cloud metadata (169.254.169.254).
		{"link-local metadata v4", "169.254.169.254", true},
		{"link-local v4 low", "169.254.0.1", true},
		{"link-local v6", "fe80::1", true},

		// Unspecified.
		{"unspecified v4", "0.0.0.0", true},
		{"unspecified v6", "::", true},

		// Public — must NOT be blocked (false-positive regression guard).
		{"public v4 a", "8.8.8.8", false},
		{"public v4 b", "1.1.1.1", false},
		{"public v6 cloudflare", "2606:4700:4700::1111", false},
		{"public v6 google", "2001:4860:4860::8888", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ip := net.ParseIP(tc.ip)
			require.NotNil(t, ip, "test fixture %q must parse as an IP", tc.ip)
			require.Equal(t, tc.forbidden, isForbiddenSSRFAddress(ip),
				"isForbiddenSSRFAddress(%s) want forbidden=%v", tc.ip, tc.forbidden)
		})
	}

	t.Run("nil ip is forbidden", func(t *testing.T) {
		t.Parallel()
		require.True(t, isForbiddenSSRFAddress(nil))
	})
}

// TestSSRFGuardedClient_DisablesProxy is the regression test for a bypass
// an independent @oh review found: http.DefaultTransport.Clone() inherits
// Proxy: ProxyFromEnvironment, and the Control hook only ever sees the
// address actually dialed. If HTTP_PROXY/HTTPS_PROXY is set in the
// environment, Control would see the PROXY's address, not the request's
// real target — so a guarded client would tunnel straight past the check
// to whatever host the proxied request line names, including a forbidden
// one. The guard must disable Transport-level proxying outright rather
// than trust an unvalidated proxy hop.
func TestSSRFGuardedClient_DisablesProxy(t *testing.T) {
	t.Parallel()

	guarded := NewSSRFGuardedClient(5*time.Second, false)
	guardedTransport, ok := guarded.Transport.(*http.Transport)
	require.True(t, ok, "guarded client must use *http.Transport")
	require.Nil(t, guardedTransport.Proxy,
		"a guarded (allowPrivate=false) transport must not honor any proxy — "+
			"otherwise Control only ever validates the proxy's address, not the real target")

	allowed := NewSSRFGuardedClient(5*time.Second, true)
	allowedTransport, ok := allowed.Transport.(*http.Transport)
	require.True(t, ok, "allow-private client must use *http.Transport")
	require.NotNil(t, allowedTransport.Proxy,
		"the allowPrivate=true escape hatch must keep normal proxy support "+
			"(it already trusts the operator with unrestricted network access)")
}

// TestSSRFGuardedClient_BlocksLoopback proves the dial-time guard fires
// against a real httptest.Server (which binds 127.0.0.1): a guarded
// client's request fails and the error chain contains errSSRFBlocked.
func TestSSRFGuardedClient_BlocksLoopback(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "you should never see this metadata")
	}))
	t.Cleanup(srv.Close)

	client := NewSSRFGuardedClient(5*time.Second, false)
	req, err := http.NewRequestWithContext(context.Background(), "GET", srv.URL, nil)
	require.NoError(t, err)

	resp, err := client.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.Error(t, err, "guarded client must refuse a 127.0.0.1 dial")
	require.True(t, errors.Is(err, errSSRFBlocked),
		"error must wrap errSSRFBlocked, got: %v", err)
}

// TestSSRFGuardedClient_AllowPrivateReachesLoopback is the companion:
// the same client built with allowPrivate=true must still reach the
// 127.0.0.1 server — proving the guard does not false-positive when an
// operator explicitly opts into loopback.
func TestSSRFGuardedClient_AllowPrivateReachesLoopback(t *testing.T) {
	t.Parallel()

	const body = "local content via allow-private client"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)

	client := NewSSRFGuardedClient(5*time.Second, true)
	req, err := http.NewRequestWithContext(context.Background(), "GET", srv.URL, nil)
	require.NoError(t, err)

	resp, err := client.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusOK, resp.StatusCode)

	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, body, string(got))
}

// TestDownloadTool_SSRFBlockedOnLoopback checks the tool end-to-end:
// with the default (nil → guarded) client, a download targeting a
// 127.0.0.1 httptest.Server must fail before any body is written to disk.
func TestDownloadTool_SSRFBlockedOnLoopback(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "exfiltrated metadata")
	}))
	t.Cleanup(srv.Close)

	workingDir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")
	// nil client → SSRF-guarded default, matching production wiring.
	tool := NewDownloadTool(&mockPermissionService{}, workingDir, nil)

	input, err := json.Marshal(DownloadParams{URL: srv.URL, FilePath: "leaked.bin"})
	require.NoError(t, err)

	_, runErr := tool.Run(ctx, fantasy.ToolCall{
		ID:    "test-call",
		Name:  DownloadToolName,
		Input: string(input),
	})
	require.Error(t, runErr, "guarded download must refuse a loopback URL")
	require.True(t, errors.Is(runErr, errSSRFBlocked),
		"download error must wrap errSSRFBlocked, got: %v", runErr)

	// Nothing reached disk.
	entries, err := os.ReadDir(workingDir)
	require.NoError(t, err)
	require.Empty(t, entries, "a blocked download must not write any file")
}

// TestFetchTool_SSRFBlockedOnLoopback checks the fetch tool: with the
// default (nil → guarded) client, fetching a 127.0.0.1 URL must fail and
// must NOT return the body in the tool response.
func TestFetchTool_SSRFBlockedOnLoopback(t *testing.T) {
	t.Parallel()

	const secret = "iam/security-credentials/role-name"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, secret)
	}))
	t.Cleanup(srv.Close)

	workingDir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")
	// nil client → SSRF-guarded default, matching production wiring.
	tool := NewFetchTool(&mockPermissionService{}, workingDir, nil)

	input, err := json.Marshal(FetchParams{URL: srv.URL, Format: "text"})
	require.NoError(t, err)

	resp, runErr := tool.Run(ctx, fantasy.ToolCall{
		ID:    "test-call",
		Name:  FetchToolName,
		Input: string(input),
	})
	require.Error(t, runErr, "guarded fetch must refuse a loopback URL")
	require.True(t, errors.Is(runErr, errSSRFBlocked),
		"fetch error must wrap errSSRFBlocked, got: %v", runErr)

	// Even defensively: no response content may contain the secret body.
	require.NotContains(t, resp.Content, secret, "blocked fetch must not leak body text")
}

// TestFetchTool_HappyPathWithAllowPrivate proves fetch still works for a
// legitimate local target once loopback is explicitly allowed — i.e. the
// guard integration did not break the normal fetch path.
func TestFetchTool_HappyPathWithAllowPrivate(t *testing.T) {
	t.Parallel()

	const body = "<html><body><p>hello local</p></body></html>"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)

	workingDir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")
	tool := NewFetchTool(&mockPermissionService{}, workingDir, NewSSRFGuardedClient(5*time.Second, true))

	input, err := json.Marshal(FetchParams{URL: srv.URL, Format: "text"})
	require.NoError(t, err)

	resp, runErr := tool.Run(ctx, fantasy.ToolCall{
		ID:    "test-call",
		Name:  FetchToolName,
		Input: string(input),
	})
	require.NoError(t, runErr)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "hello local")
}
