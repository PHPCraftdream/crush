package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"
)

const cookieName = "crush_session"

// Auth handles token-based authentication for the web UI.
// The token is generated at startup, printed to the terminal, and must be
// supplied by the browser user before a session cookie is issued.
type Auth struct {
	token string // hex-encoded random token
}

func newAuth() *Auth {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		panic("server: cannot generate auth token: " + err.Error())
	}
	return &Auth{token: hex.EncodeToString(b)}
}

// Token returns the human-visible token string to print in the terminal.
func (a *Auth) Token() string { return a.token }

// HandleAuth handles POST /auth — validates the submitted token and sets a
// session cookie on success.
func (a *Auth) HandleAuth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Token == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if body.Token != a.token {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    a.token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Expires:  time.Now().Add(30 * 24 * time.Hour),
	})
	w.WriteHeader(http.StatusOK)
}

// HandleAuthCheck handles GET /auth/check — returns 200 if authenticated,
// 401 otherwise. Used by the React app to skip the login page on reload.
func (a *Auth) HandleAuthCheck(w http.ResponseWriter, r *http.Request) {
	if a.isValid(r) {
		w.WriteHeader(http.StatusOK)
	} else {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}
}

// Middleware wraps a handler, requiring a valid session cookie OR a valid
// token in the Authorization header ("Bearer <token>") or query param "token".
// This covers WebSocket upgrades where browsers cannot set custom headers.
func (a *Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.isValid(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isValid accepts any of:
//   - session cookie set after successful /auth POST
//   - Authorization: Bearer <token> header
//   - ?token=<token> query parameter (for WebSocket clients)
func (a *Auth) isValid(r *http.Request) bool {
	// Cookie (primary method for browser clients).
	if c, err := r.Cookie(cookieName); err == nil && c.Value == a.token {
		return true
	}
	// Authorization header (Bearer token).
	if auth := r.Header.Get("Authorization"); auth == "Bearer "+a.token {
		return true
	}
	// Query parameter (fallback for WS where headers are hard to set).
	if r.URL.Query().Get("token") == a.token {
		return true
	}
	return false
}
