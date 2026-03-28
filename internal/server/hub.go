package server

import (
	"context"
	"encoding/json"
	"log/slog"
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
)

// Client represents a single WebSocket connection.
type Client struct {
	hub  *Hub
	conn *websocket.Conn
	send chan []byte
}

// Hub maintains connected clients and an event replay buffer.
//
// All accesses to clients and buffer happen inside the single Run() goroutine,
// so no mutex is needed for those fields. The broadcast channel serialises
// messages from multiple producer goroutines.
type Hub struct {
	clients    map[*Client]struct{}
	buffer     [][]byte // circular replay buffer; only touched inside Run()
	broadcast  chan []byte
	register   chan *Client
	unregister chan *Client
}

func newHub() *Hub {
	return &Hub{
		clients:    make(map[*Client]struct{}),
		buffer:     make([][]byte, 0, maxBufferSize),
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
			for _, msg := range h.buffer {
				select {
				case c.send <- msg:
				default:
					// Client buffer full; skip older replayed events rather than block.
				}
			}

		case c := <-h.unregister:
			if _, ok := h.clients[c]; ok {
				delete(h.clients, c)
				close(c.send)
			}

		case msg := <-h.broadcast:
			// Store in replay buffer; drop oldest when full.
			if len(h.buffer) >= maxBufferSize {
				h.buffer = h.buffer[1:]
			}
			h.buffer = append(h.buffer, msg)

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
