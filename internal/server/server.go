// Package server implements the HTTP + WebSocket server for crush's web mode.
// It serves the embedded React application and bridges the app's pubsub
// event system to connected browsers over WebSocket.
package server

import (
	"context"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"time"

	appPkg "github.com/charmbracelet/crush/internal/app"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  64 * 1024,
	WriteBufferSize: 64 * 1024,
	// Allow all origins for local use; tighten for production deployments.
	CheckOrigin: func(r *http.Request) bool { return true },
}

// Server wires together the HTTP mux, the WebSocket hub, and the app.
type Server struct {
	app    *appPkg.App
	hub    *Hub
	auth   *Auth
	addr   string
	static fs.FS
}

// New creates a Server. Pass a nil staticFS to proxy the dev server instead.
// Use addr "host:0" to let the OS pick a free port.
func New(a *appPkg.App, addr string, staticFS fs.FS) *Server {
	return &Server{
		app:    a,
		hub:    newHub(),
		auth:   newAuth(),
		addr:   addr,
		static: staticFS,
	}
}

// Token returns the auth token to be printed in the terminal.
func (s *Server) Token() string { return s.auth.Token() }

// Start runs the server until ctx is cancelled. onReady is called once the
// listener is bound (with the actual address, useful when port was 0).
func (s *Server) Start(ctx context.Context, onReady func(addr string)) error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}

	go s.hub.Run(ctx)
	go subscribeAndBroadcast(ctx, s.app, s.hub)

	mux := http.NewServeMux()

	// Auth endpoints ΓÇö no cookie required.
	mux.HandleFunc("/auth", s.auth.HandleAuth)
	mux.HandleFunc("/auth/check", s.auth.HandleAuthCheck)

	// WebSocket ΓÇö requires valid session cookie.
	mux.Handle("/ws", s.auth.Middleware(http.HandlerFunc(s.handleWS)))

	if s.static != nil {
		// Serve the embedded React build; fall back to index.html for SPA routing.
		mux.Handle("/", spaHandler(s.static))
	} else {
		// Dev mode: proxy to the rspack dev server.
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://localhost:3000"+r.RequestURI, http.StatusTemporaryRedirect)
		})
	}

	srv := &http.Server{
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // streaming responses have no timeout
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	slog.Info("crush web server listening", "addr", ln.Addr().String())

	// Notify caller synchronously ΓÇö address is known, server not yet serving.
	if onReady != nil {
		onReady(ln.Addr().String())
	}
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// handleWS upgrades the connection and runs the client read/write pumps.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Debug("ws: upgrade failed", "err", err)
		return
	}

	c := &Client{
		hub:  s.hub,
		conn: conn,
		send: make(chan []byte, sendBufSize),
	}
	s.hub.register <- c

	// Start write pump in background; read pump blocks this goroutine.
	go c.writePump()
	s.readPump(r.Context(), c)
}

// readPump reads messages from the WebSocket and dispatches them.
func (s *Server) readPump(ctx context.Context, c *Client) {
	defer func() {
		s.hub.unregister <- c
		c.conn.Close()
	}()

	c.conn.SetReadLimit(maxMessageSize)
	_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	// Fork merge note (origin/main 9c35ee01 "fix(server): recover from handler panics"):
	// upstream wraps the REST mux with recoverHandler; our WebSocket loop reads
	// frames directly. The equivalent in our architecture would be a recover()
	// inside handleIncoming or this for-loop — see CHANGELOG.fork.md section 4.A.
	for {
		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err,
				websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				slog.Debug("ws: read error", "err", err)
			}
			return
		}
		handleIncoming(ctx, s.app, c, raw)
	}
}

// spaHandler serves static files and falls back to index.html for any path
// that doesn't match a real file (needed for client-side routing).
func spaHandler(fsys fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(fsys))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/" {
			path = "index.html"
		}
		// Check if the file exists in the embedded FS.
		if _, err := fs.Stat(fsys, path[1:]); err == nil {
			fileServer.ServeHTTP(w, r)
			return
		}
		// Not found ΓÇö serve index.html so React Router can handle it.
		r.URL.Path = "/"
		fileServer.ServeHTTP(w, r)
	})
}
