package api

import (
	"net/http"
	"time"

	"prostaff/internal/models"
)

// GET /me
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	view, ok := s.store.UserView(u.ID)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "user not found")
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// POST /me/push-token
func (s *Server) handleSetPushToken(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	var body struct {
		Token string `json:"token"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Token == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "token is required")
		return
	}
	if err := s.store.SetPushToken(u.ID, body.Token); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// POST /me/avatar
func (s *Server) handleSetAvatar(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	var body struct {
		AvatarURL string `json:"avatar_url"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	updated, err := s.store.SetAvatarURL(u.ID, body.AvatarURL)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// GET /categories
func (s *Server) handleCategories(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.store.Categories())
}

// GET /notifications?since=<rfc3339>
func (s *Server) handleNotifications(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	since := parseSince(r.URL.Query().Get("since"))
	list := s.store.ListNotifications(u.ID, since)
	if list == nil {
		list = []models.Notification{}
	}
	writeJSON(w, http.StatusOK, list)
}

// parseSince parses an RFC3339 timestamp, returning the zero time when empty or
// invalid.
func parseSince(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
