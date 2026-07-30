package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSecureCompare exercises the constant-time token comparison, including
// the equal-length requirement of subtle.ConstantTimeCompare — mismatched
// lengths must be treated as "not equal" rather than panicking or silently
// truncating.
func TestSecureCompare(t *testing.T) {
	t.Parallel()

	require.True(t, secureCompare("abc123", "abc123"))
	require.False(t, secureCompare("abc123", "abc124"))
	require.False(t, secureCompare("short", "muchlongertoken"))
	require.False(t, secureCompare("muchlongertoken", "short"))
	require.True(t, secureCompare("", ""))
	require.False(t, secureCompare("", "nonempty"))
}

// TestAuthIsValidToken covers the three accepted credential shapes: cookie,
// Authorization: Bearer header, and ?token= query param, now routed through
// secureCompare instead of ==.
func TestAuthIsValidToken(t *testing.T) {
	t.Parallel()

	a := newAuth()

	t.Run("cookie", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/ws", nil)
		r.AddCookie(&http.Cookie{Name: cookieName, Value: a.token})
		require.True(t, a.isValid(r))
	})

	t.Run("bearer header", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/ws", nil)
		r.Header.Set("Authorization", "Bearer "+a.token)
		require.True(t, a.isValid(r))
	})

	t.Run("query param", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/ws?token="+a.token, nil)
		require.True(t, a.isValid(r))
	})

	t.Run("wrong token rejected", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/ws?token=nope", nil)
		require.False(t, a.isValid(r))
	})

	t.Run("no credentials rejected", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/ws", nil)
		require.False(t, a.isValid(r))
	})
}
