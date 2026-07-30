package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// TestReadPumpUnregisterDoesNotBlockWhenHubStoppedReading is the regression
// test for L-7: readPump's cleanup used to be a single
//
//	defer func() {
//	    s.hub.unregister <- c   // unbuffered send, no escape hatch
//	    c.conn.Close()
//	}()
//
// Hub.Run stops servicing the unregister channel (cap 64) the moment its ctx
// is cancelled (see hub.go's `case <-ctx.Done(): return`). If 64 clients are
// already disconnecting around shutdown, or more simply if Run has already
// returned by the time a client's readPump exits, that channel send blocks
// forever — and because it ran before c.conn.Close(), the connection's own
// cleanup never happened either: goroutine leak + fd leak on every shutdown
// race.
//
// This test reproduces exactly that: a Hub whose Run has exited (context
// already cancelled, so nothing ever reads unregister again) and a readPump
// call using ctx already-Done. Before the fix this would hang forever; the
// fix's `select { case ...: case <-ctx.Done(): }` plus an unconditional
// separate `defer c.conn.Close()` must return promptly and must close the
// connection regardless.
func TestReadPumpUnregisterDoesNotBlockWhenHubStoppedReading(t *testing.T) {
	t.Parallel()

	hub := newHub()
	// Do NOT run hub.Run at all — this simulates the post-shutdown state
	// where nothing reads from hub.unregister ever again, the worst case
	// the fix must survive.
	//
	// unregister is buffered (cap 64, see newHub), so a single send would
	// succeed even on the pre-fix code and this test would pass for the
	// wrong reason. Fill the buffer completely first so the send this test
	// actually exercises has nowhere to go — reproducing the real
	// shutdown scenario where many clients are disconnecting at once and
	// nobody is left draining the channel.
	for i := 0; i < cap(hub.unregister); i++ {
		hub.unregister <- &Client{}
	}

	upgrader := websocket.Upgrader{}
	connReady := make(chan *websocket.Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		connReady <- conn
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	clientSide, handshakeResp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer clientSide.Close()
	defer handshakeResp.Body.Close()

	serverSideConn := <-connReady
	c := newClient(hub, serverSideConn)

	s := &Server{hub: hub}

	// ctx is already cancelled, mirroring the state readPump would observe
	// during/after server shutdown (Start's shutdown goroutine cancels the
	// request context tree, and this same ctx is what Hub.Run selects on).
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Make the client side close the connection so ReadMessage in readPump
	// returns promptly (its own read error path also runs the same
	// deferred cleanup) — this is what a disconnecting client looks like.
	require.NoError(t, clientSide.Close())

	done := make(chan struct{})
	go func() {
		s.readPump(ctx, c)
		close(done)
	}()

	select {
	case <-done:
		// Success: readPump returned instead of hanging on an unregister
		// send that nobody will ever receive.
	case <-time.After(3 * time.Second):
		t.Fatal("readPump blocked on hub.unregister send with a stopped Hub.Run — " +
			"the L-7 fix's select{} escape hatch did not fire")
	}

	// The connection must be closed unconditionally, independent of
	// whether the unregister send succeeded (it didn't — nothing ever read
	// it here).
	require.Error(t, serverSideConn.WriteMessage(websocket.TextMessage, []byte("x")),
		"server-side conn must be closed by readPump's cleanup even though the unregister send never completed")
}
