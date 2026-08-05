package api

import (
	"net/http"

	"prostaff/internal/models"
)

// GET /admin/realtime — who is actually connected right now.
//
// Without this, "the order did not reach the worker" has no answer: the order
// record proves a ring went out, but nothing shows whether the phone was
// listening. This reports the live socket set, so the question splits cleanly —
// dispatch found nobody, or dispatch found somebody who was not connected and
// had no push token either.
func (s *Server) handleAdminRealtime(w http.ResponseWriter, _ *http.Request) {
	conns, users, available := s.ws.Counts()

	// Connected users, resolved to something a human can recognise.
	type entry struct {
		UserID   string `json:"user_id"`
		Phone    string `json:"phone"`
		Name     string `json:"name,omitempty"`
		IsWorker bool   `json:"is_worker"`
		// Available means the worker is connected *and* on duty — the set
		// dispatch would choose from.
		Available bool `json:"available"`
		// PushToken reports only whether one exists; the token itself is a
		// credential and does not belong in a diagnostics response.
		HasPushToken bool `json:"has_push_token"`
	}

	online := s.ws.OnlineUserIDs()
	out := make([]entry, 0, len(online))
	for _, id := range online {
		e := entry{UserID: id, Available: s.ws.IsAvailable(id)}
		if u, ok := s.store.GetUser(id); ok {
			e.Phone = u.Phone
			e.Name = u.Name
			e.HasPushToken = u.PushToken != ""
			e.IsWorker = u.HasRole(models.RoleWorker)
		}
		out = append(out, e)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"connections":       conns,
		"users":             users,
		"available_workers": available,
		"connected":         out,
	})
}
