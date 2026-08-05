// Package sse implements an in-process Server-Sent Events hub that fans events
// out to per-user subscriber channels.
package sse

import "sync"

// Event is a single message delivered to a subscriber. It is serialised as
// {"type": ..., "data": ...} on the wire.
type Event struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}

// Hub routes events to all live connections for a given user ID.
type Hub struct {
	mu   sync.RWMutex
	subs map[string]map[chan Event]struct{}
}

// New creates an empty hub.
func New() *Hub {
	return &Hub{subs: make(map[string]map[chan Event]struct{})}
}

// Subscribe registers a new connection for userID. It returns a receive-only
// channel and an unsubscribe function that must be called on disconnect.
func (h *Hub) Subscribe(userID string) (<-chan Event, func()) {
	ch := make(chan Event, 16)
	h.mu.Lock()
	set, ok := h.subs[userID]
	if !ok {
		set = make(map[chan Event]struct{})
		h.subs[userID] = set
	}
	set[ch] = struct{}{}
	h.mu.Unlock()

	unsubscribe := func() {
		h.mu.Lock()
		if set, ok := h.subs[userID]; ok {
			if _, ok := set[ch]; ok {
				delete(set, ch)
				close(ch)
			}
			if len(set) == 0 {
				delete(h.subs, userID)
			}
		}
		h.mu.Unlock()
	}
	return ch, unsubscribe
}

// Publish delivers an event to every live connection for userID. Delivery is
// non-blocking: if a subscriber's buffer is full the event is dropped for that
// connection rather than stalling the publisher.
func (h *Hub) Publish(userID string, e Event) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for ch := range h.subs[userID] {
		select {
		case ch <- e:
		default:
		}
	}
}
