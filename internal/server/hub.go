package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/gorilla/websocket"
)

const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 20 * 1024 * 1024 // 20 MB — supports image attachments
	sendBufSize    = 512
	maxBufferSize  = 2000 // max events to replay to new clients

	// maxConcurrentHandlersPerConn bounds how many handleX goroutines a single
	// WebSocket connection may have in flight at once. Each handler can hold a
	// SQLite connection and/or call into the agent coordinator, so an unbounded
	// client (malicious or buggy UI firing thousands of messages) would
	// otherwise be able to exhaust the process's goroutine pool and DB
	// connections. 12 is generous headroom above realistic UI concurrency (a
	// handful of tabs each issuing a couple of in-flight requests) while still
	// capping worst-case blast radius per connection; acquiring the semaphore
	// applies natural backpressure to readPump once the cap is hit, so excess
	// frames simply wait to be dispatched instead of spawning more goroutines.
	maxConcurrentHandlersPerConn = 12

	// replayByteBudget bounds the total bytes held in the replay buffer across all
	// events. Normal traffic (small JSON deltas) fits ~2000 events well under 8 MiB,
	// so the per-event count limit stays the binding constraint and full history is
	// preserved; this only kicks in for pathological streams (e.g. hundreds of large
	// growing-message snapshots), hard-capping a single hub's replay memory.
	replayByteBudget = 16 * 1024 * 1024 // 16 MiB

	// replayMaxEventSize is the per-event ceiling for STORAGE in the replay buffer.
	// Events larger than this are almost always image attachments or huge tool
	// outputs; a newly-connecting client doesn't need them re-transmitted from the
	// buffer (they're already persisted in the DB/message layer the UI loads on
	// initial fetch). Skipping storage here (1) stops one blob evicting dozens of
	// normal events, (2) bounds the byte-budget eviction loop. Live fan-out to
	// already-connected clients is unaffected.
	replayMaxEventSize = 1024 * 1024 // 1 MiB per event
)

// Client represents a single WebSocket connection.
type Client struct {
	hub  *Hub
	conn *websocket.Conn
	send chan []byte

	// sem bounds the number of concurrently running handleX goroutines
	// spawned for THIS connection (see maxConcurrentHandlersPerConn). It is
	// a counting semaphore implemented as a buffered channel: acquire sends
	// a token, release receives one.
	sem chan struct{}
}

// newClient constructs a Client with its per-connection handler semaphore
// initialised. Always use this instead of a bare &Client{} literal so the
// semaphore is never nil.
func newClient(hub *Hub, conn *websocket.Conn) *Client {
	return &Client{
		hub:  hub,
		conn: conn,
		send: make(chan []byte, sendBufSize),
		sem:  make(chan struct{}, maxConcurrentHandlersPerConn),
	}
}

// dispatch runs fn in a new goroutine, subject to two protections:
//
//   - Concurrency limit: acquires a slot from the connection's semaphore
//     before spawning, blocking the CALLER (readPump, via handleIncoming)
//     once maxConcurrentHandlersPerConn handlers are already in flight for
//     this connection. This is deliberate backpressure — once the cap is
//     hit, further inbound frames simply wait to be dispatched rather than
//     spawning unbounded goroutines.
//   - Panic isolation: recovers any panic inside fn, logs it (with a stack
//     trace) instead of propagating it. Without this, a nil-deref or
//     out-of-range index triggered by an unexpected payload in ANY handler
//     would crash the entire crush web process, taking down every other
//     connection and in-flight agent session with it.
//
// name is a short identifier (typically the WS command type or handler
// function name) used only for logging.
func (c *Client) dispatch(name string, fn func()) {
	c.sem <- struct{}{}
	go func() {
		defer func() { <-c.sem }()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("ws: handler panic",
					"handler", name,
					"panic", r,
					"stack", string(debug.Stack()))
			}
		}()
		fn()
	}()
}

// replayBuffer is a fixed-capacity ring buffer of broadcast events.
//
// It enforces three independent bounds:
//   - count:     at most cap entries (legacy maxBufferSize semantic),
//   - bytes:     total stored bytes <= replayByteBudget,
//   - per-event: events larger than replayMaxEventSize are not stored at all.
//
// All ops are O(1): the backing slice is pre-allocated once and never re-sliced
// or appended to, so eviction never copies the underlying array.
//
// NOTE: a cleaner long-term design would store only the latest snapshot per
// entity (session/message/tool) and serve it to new clients via REST/DB instead
// of replaying intermediate events. That is a larger architectural change and is
// intentionally out of scope here; see task #130.
type replayBuffer struct {
	buf   [][]byte // len == cap, pre-allocated; entries recycled in place
	head  int      // index of the oldest stored entry
	tail  int      // index where the next push writes
	count int      // number of valid entries, count <= len(buf)
	bytes int      // sum of len() of valid entries
}

// newReplayBuffer returns an empty ring buffer with capacity maxBufferSize.
func newReplayBuffer() replayBuffer {
	return replayBuffer{buf: make([][]byte, maxBufferSize)}
}

// push appends msg, evicting oldest entries as needed to satisfy the count and
// byte-budget bounds. Events exceeding replayMaxEventSize are rejected (not
// stored) and reported via slog at debug level. Returns true if stored.
func (r *replayBuffer) push(msg []byte) bool {
	if len(msg) > replayMaxEventSize {
		slog.Debug("ws: replay event exceeds per-event limit, not buffering",
			"len", len(msg), "limit", replayMaxEventSize)
		return false
	}
	capN := len(r.buf)
	if r.count == capN { // count bound: full -> evict oldest
		r.evictHead()
	}
	r.buf[r.tail] = msg
	r.tail = (r.tail + 1) % capN
	r.count++
	r.bytes += len(msg)
	// Byte-budget bound: evict oldest until within budget. Guard count > 1 so
	// the just-pushed event (<= replayMaxEventSize <= replayByteBudget) is kept.
	for r.bytes > replayByteBudget && r.count > 1 {
		r.evictHead()
	}
	return true
}

// evictHead removes the oldest entry. Caller guarantees count >= 1.
func (r *replayBuffer) evictHead() {
	capN := len(r.buf)
	r.bytes -= len(r.buf[r.head])
	r.buf[r.head] = nil // drop reference so the slice can be GC'd
	r.head = (r.head + 1) % capN
	r.count--
}

// forEach applies fn to each stored entry in oldest-to-newest order.
func (r *replayBuffer) forEach(fn func([]byte)) {
	capN := len(r.buf)
	for i := 0; i < r.count; i++ {
		fn(r.buf[(r.head+i)%capN])
	}
}

// Hub maintains connected clients and an event replay buffer.
//
// All accesses to clients and buffer happen inside the single Run() goroutine,
// so no mutex is needed for those fields. The broadcast channel serialises
// messages from multiple producer goroutines.
type Hub struct {
	clients    map[*Client]struct{}
	buffer     replayBuffer // ring replay buffer; only touched inside Run()
	broadcast  chan []byte
	register   chan *Client
	unregister chan *Client
}

func newHub() *Hub {
	return &Hub{
		clients:    make(map[*Client]struct{}),
		buffer:     newReplayBuffer(),
		broadcast:  make(chan []byte, 1024),
		register:   make(chan *Client, 64),
		unregister: make(chan *Client, 64),
	}
}

// Run is the hub's single event loop. It must be called in its own goroutine.
func (h *Hub) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			for c := range h.clients {
				close(c.send)
			}
			return

		case c := <-h.register:
			// Add to active set first so no broadcasts are lost after this point.
			h.clients[c] = struct{}{}
			// Replay all buffered events to the new client (non-blocking per-event).
			h.buffer.forEach(func(msg []byte) {
				select {
				case c.send <- msg:
				default:
					// Client buffer full; skip older replayed events rather than block.
				}
			})

		case c := <-h.unregister:
			if _, ok := h.clients[c]; ok {
				delete(h.clients, c)
				close(c.send)
			}

		case msg := <-h.broadcast:
			h.buffer.push(msg) // store (skips oversized events; eviction handled inside)

			// Fan-out to all active clients (non-blocking; slow clients drop messages).
			for c := range h.clients {
				select {
				case c.send <- msg:
				default:
					slog.Debug("ws: slow client, dropping message")
				}
			}
		}
	}
}

// Broadcast encodes a typed event and queues it for all clients + the replay buffer.
// Safe to call from any goroutine; never blocks (drops on full channel).
func (h *Hub) Broadcast(msgType string, payload any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		slog.Error("ws: marshal broadcast payload", "type", msgType, "err", err)
		return
	}
	env, err := json.Marshal(WSMessage{Type: msgType, Payload: raw})
	if err != nil {
		slog.Error("ws: marshal broadcast envelope", "err", err)
		return
	}
	select {
	case h.broadcast <- env:
	default:
		slog.Warn("ws: broadcast channel full, dropping", "type", msgType)
	}
}

// reply sends a response directly to one client (request/response pattern).
// Safe to call from any goroutine; recovers from send-to-closed-channel.
func (c *Client) reply(id, msgType string, payload any, errMsg string) {
	raw, _ := json.Marshal(payload)
	env, err := json.Marshal(WSMessage{ID: id, Type: msgType, Payload: raw, Error: errMsg})
	if err != nil {
		return
	}
	// Recover from panic if the client's send channel was closed concurrently.
	defer func() { recover() }() //nolint:errcheck
	select {
	case c.send <- env:
	default:
		slog.Warn("ws: client send buffer full, dropping reply")
	}
}

// writePump pumps messages from the send channel to the WebSocket connection.
// Exactly one writePump goroutine runs per client.
func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {
		case msg, ok := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				slog.Debug("ws: write error", "err", err)
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
