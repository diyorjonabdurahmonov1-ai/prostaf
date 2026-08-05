// Package ws is the realtime transport: one WebSocket per signed-in device,
// held open, with the set of who is connected kept in memory.
//
// It replaces polling. Before this, every worker's phone asked the server
// "anything for me?" every four seconds — 27 requests a minute each, every one
// of them a full read of the orders table. Now the server already knows who is
// listening and simply writes to them.
//
// # What lives in memory, and why
//
// A connection carries the few facts dispatch needs to choose recipients:
// where the worker is, what they do, whether they are taking work. That is the
// real win — not the socket itself. Picking who to ring used to be one SQL
// query per online worker per wave; against this set it is a walk over a map.
//
// # Scope
//
// One process. Two instances would each know only their own half of the users,
// so a second instance needs Redis (or NATS) behind `Publish` — the interface
// here is deliberately narrow enough that swapping the delivery underneath it
// changes nothing above.
//
// # What it is not
//
// It is not a replacement for push notifications. A phone in a pocket has no
// socket: iOS suspends the app on backgrounding and Android's Doze cuts it
// loose. For a dispatch app that is most of the day, so a ring still goes out
// as a push in parallel — this only makes the app-in-hand case instant.
package ws

import (
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Event is one message to a client. The shape matches the SSE envelope so both
// transports can carry the same payloads while they coexist.
type Event struct {
	Type string `json:"type"`
	Data any    `json:"data,omitempty"`
}

const (
	// How long a client has to answer a ping before the connection is dropped.
	// Mobile networks are slow and phones sleep; generous but not unbounded.
	pongWait = 70 * time.Second
	// Pings go out comfortably inside `pongWait`.
	pingPeriod = 25 * time.Second
	// A write that cannot complete in this long means the client is wedged.
	writeWait = 10 * time.Second
	// Largest message accepted from a client. Clients only ever send pings and
	// tiny acks, so this is a guard, not a budget.
	maxMessageSize = 4096
	// Messages buffered per connection before it is treated as dead. A phone on
	// a bad link must not hold up dispatch for everyone else.
	sendBuffer = 32
)

// Presence is what dispatch needs to know about a connected worker without
// asking the database. Zero values mean "unknown" — a connection that has not
// reported presence is still reachable, it just cannot be matched on location.
type Presence struct {
	Lat         float64
	Lng         float64
	CategoryIDs []string
	Gender      string
	// Online mirrors the worker's own availability toggle. Connected is not the
	// same as available: an app can be open with the toggle off.
	Online bool
}

type conn struct {
	ws     *websocket.Conn
	send   chan []byte
	userID string
	closed chan struct{}
	once   sync.Once
}

func (c *conn) close() {
	c.once.Do(func() {
		close(c.closed)
		c.ws.Close()
	})
}

// Hub owns every live connection, indexed by user.
//
// A user may have several: the same account signed in on a phone and a tablet,
// or a screen that has not finished tearing down while the next one connects.
// Every one of them gets the message.
type Hub struct {
	mu       sync.RWMutex
	conns    map[string]map[*conn]struct{}
	presence map[string]Presence

	upgrader websocket.Upgrader
}

func NewHub() *Hub {
	return &Hub{
		conns:    make(map[string]map[*conn]struct{}),
		presence: make(map[string]Presence),
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			// The API is token-authenticated and serves a mobile app, which sends
			// no meaningful Origin. Authentication happens before Serve is called.
			CheckOrigin: func(*http.Request) bool { return true },
		},
	}
}

// Serve upgrades an already-authenticated request and blocks until the socket
// closes. The caller is responsible for proving who `userID` is.
func (h *Hub) Serve(w http.ResponseWriter, r *http.Request, userID string) {
	socket, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote an error response.
		return
	}
	c := &conn{
		ws:     socket,
		send:   make(chan []byte, sendBuffer),
		userID: userID,
		closed: make(chan struct{}),
	}

	h.add(c)
	defer h.remove(c)

	go c.writeLoop()
	c.readLoop()
}

func (h *Hub) add(c *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.conns[c.userID]
	if set == nil {
		set = make(map[*conn]struct{}, 1)
		h.conns[c.userID] = set
	}
	set[c] = struct{}{}
}

func (h *Hub) remove(c *conn) {
	c.close()
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.conns[c.userID]
	if set == nil {
		return
	}
	delete(set, c)
	if len(set) == 0 {
		delete(h.conns, c.userID)
		// Presence belongs to a live connection. Leaving it behind would let
		// dispatch offer work to a phone that is no longer listening.
		delete(h.presence, c.userID)
	}
}

// readLoop drains the socket. Clients have nothing to say beyond keepalives, but
// the read must run: it is what detects a dead connection and services pongs.
func (c *conn) readLoop() {
	c.ws.SetReadLimit(maxMessageSize)
	_ = c.ws.SetReadDeadline(time.Now().Add(pongWait))
	c.ws.SetPongHandler(func(string) error {
		return c.ws.SetReadDeadline(time.Now().Add(pongWait))
	})
	for {
		if _, _, err := c.ws.ReadMessage(); err != nil {
			return
		}
		// Any traffic at all counts as proof of life.
		_ = c.ws.SetReadDeadline(time.Now().Add(pongWait))
	}
}

func (c *conn) writeLoop() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.close()
	}()
	for {
		select {
		case msg, ok := <-c.send:
			if !ok {
				return
			}
			_ = c.ws.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.ws.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.ws.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.ws.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		case <-c.closed:
			return
		}
	}
}

/* ------------------------------------------------------------------ *
 * Sending
 * ------------------------------------------------------------------ */

// Publish delivers to every connection a user has open. Silent when they have
// none — an offline user is reached by push, not here.
func (h *Hub) Publish(userID string, e Event) {
	payload, err := marshal(e)
	if err != nil {
		log.Printf("ws: marshal %s: %v", e.Type, err)
		return
	}
	h.mu.RLock()
	set := h.conns[userID]
	targets := make([]*conn, 0, len(set))
	for c := range set {
		targets = append(targets, c)
	}
	h.mu.RUnlock()

	for _, c := range targets {
		c.enqueue(payload)
	}
}

// PublishMany sends one payload to many users, marshalling it once. Dispatch
// rings every nearby worker at the same instant, and doing the JSON per
// recipient is the kind of waste that only shows up under load.
func (h *Hub) PublishMany(userIDs []string, e Event) {
	if len(userIDs) == 0 {
		return
	}
	payload, err := marshal(e)
	if err != nil {
		log.Printf("ws: marshal %s: %v", e.Type, err)
		return
	}
	h.mu.RLock()
	targets := make([]*conn, 0, len(userIDs))
	for _, id := range userIDs {
		for c := range h.conns[id] {
			targets = append(targets, c)
		}
	}
	h.mu.RUnlock()

	for _, c := range targets {
		c.enqueue(payload)
	}
}

// enqueue never blocks. A client that cannot keep up is disconnected rather
// than allowed to stall the sender — it will reconnect and re-read state.
func (c *conn) enqueue(payload []byte) {
	select {
	case c.send <- payload:
	default:
		c.close()
	}
}

/* ------------------------------------------------------------------ *
 * Presence
 * ------------------------------------------------------------------ */

// SetPresence records what a connected worker can be matched on. Called when
// they go online and whenever their location or categories change.
func (h *Hub) SetPresence(userID string, p Presence) {
	h.mu.Lock()
	defer h.mu.Unlock()
	// Only for users who are actually connected — otherwise the map grows
	// without anything to remove the entries.
	if _, ok := h.conns[userID]; !ok {
		return
	}
	h.presence[userID] = p
}

// ClearPresence marks a connected worker as not taking work, without dropping
// the socket — they still want order updates for the job they are on.
func (h *Hub) ClearPresence(userID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if p, ok := h.presence[userID]; ok {
		p.Online = false
		h.presence[userID] = p
	}
}

// IsConnected reports whether the user has a live socket. Dispatch uses it to
// decide whether a push is the only way to reach somebody.
func (h *Hub) IsConnected(userID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.conns[userID]
	return ok
}

// Snapshot copies the presence of everyone currently connected and available.
//
// A copy, not the live map: matching does distance maths over it and must not
// hold the lock while it does, or every connect and disconnect in the system
// queues up behind one dispatch.
func (h *Hub) Snapshot() map[string]Presence {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make(map[string]Presence, len(h.presence))
	for id, p := range h.presence {
		if p.Online {
			out[id] = p
		}
	}
	return out
}

// OnlineUserIDs lists everyone with at least one live connection. A copy, so
// the caller can resolve names against the database without holding the lock.
func (h *Hub) OnlineUserIDs() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]string, 0, len(h.conns))
	for id := range h.conns {
		out = append(out, id)
	}
	return out
}

// IsAvailable reports whether a connected worker is also on duty — connected is
// not the same as taking work.
func (h *Hub) IsAvailable(userID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.presence[userID].Online
}

// Counts reports live connections and available workers, for /admin/stats and
// for knowing when this process is near its ceiling.
func (h *Hub) Counts() (conns int, users int, available int) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, set := range h.conns {
		conns += len(set)
	}
	users = len(h.conns)
	for _, p := range h.presence {
		if p.Online {
			available++
		}
	}
	return
}
