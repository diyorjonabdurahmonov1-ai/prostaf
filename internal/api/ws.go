package api

import (
	"net/http"
	"time"

	"prostaff/internal/auth"
	"prostaff/internal/sse"
	"prostaff/internal/ws"
)

// GET /ws?token=<jwt> — the realtime connection.
//
// Authenticated by query parameter rather than a header: the WebSocket
// handshake is issued by the browser/RN networking stack, which does not let
// the caller set arbitrary headers on it. The token is the same short-lived
// access token every other route takes, so a leaked URL expires with it.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		token = bearerToken(r)
	}
	if token == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing token")
		return
	}
	sub, err := auth.Parse(s.cfg.JWTSecret, token)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "invalid token")
		return
	}
	u, ok := s.store.GetUser(sub)
	if !ok || !u.Active {
		writeError(w, http.StatusUnauthorized, "unauthorized", "unknown or inactive user")
		return
	}

	// A worker who is already on duty gets their presence restored, so a
	// reconnect (app resumed, network flapped) does not take them out of
	// dispatch until they next touch the toggle.
	//
	// Restored on a goroutine because Serve blocks for the life of the socket
	// and presence is only accepted once the connection is registered — which
	// happens inside Serve.
	if wp, ok := s.store.GetWorkerByUser(u.ID); ok && wp.Online && !wp.Banned {
		go func() {
			for i := 0; i < 20; i++ {
				if s.ws.IsConnected(u.ID) {
					s.ws.SetPresence(u.ID, presenceOf(wp.Lat, wp.Lng, wp.CategoryIDs, wp.Gender, true))
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		}()
	}

	// Blocks until the socket closes.
	s.ws.Serve(w, r, u.ID)
}

// presenceOf is the small slice of a worker's record that dispatch matches on.
func presenceOf(lat, lng float64, categories []string, gender string, online bool) ws.Presence {
	return ws.Presence{
		Lat:         lat,
		Lng:         lng,
		CategoryIDs: categories,
		Gender:      gender,
		Online:      online,
	}
}

// publish sends one event to a user over every transport they might be on.
//
// The two exist together on purpose: the WebSocket is the fast path and the
// only one React Native can actually consume (it has no EventSource), while SSE
// still serves the web build. Handlers call this and stay out of it.
func (s *Server) publish(userID string, e sse.Event) {
	s.hub.Publish(userID, e)
	s.ws.Publish(userID, ws.Event{Type: e.Type, Data: e.Data})
}

// publishMany is the same for a whole dispatch wave. The payload is encoded
// once for the socket half rather than per recipient.
func (s *Server) publishMany(userIDs []string, e sse.Event) {
	for _, id := range userIDs {
		s.hub.Publish(id, e)
	}
	s.ws.PublishMany(userIDs, ws.Event{Type: e.Type, Data: e.Data})
}
