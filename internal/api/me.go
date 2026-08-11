package api

import (
	"encoding/json"
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
//
// Two tokens, because they are two different delivery paths and only one of
// them reaches a closed Android app. `token` is the Expo push token, which
// routes through Expo's service and arrives as a data-only FCM message that
// nothing renders unless the app's process is alive. `device_token` is the
// phone's own FCM registration token, which lets the server send a real
// notification payload that Android draws by itself.
//
// Either may be absent: iOS and older builds send only the Expo one, and a
// device that has Firebase but could not reach Expo sends only the other.
func (s *Server) handleSetPushToken(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	var body struct {
		Token       string `json:"token"`
		DeviceToken string `json:"device_token"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Token == "" && body.DeviceToken == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "token or device_token is required")
		return
	}
	if body.Token != "" {
		if err := s.store.SetPushToken(u.ID, body.Token); err != nil {
			writeStoreError(w, err)
			return
		}
	}
	if body.DeviceToken != "" {
		if err := s.store.SetDeviceToken(u.ID, body.DeviceToken); err != nil {
			writeStoreError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		// So the diagnostics screen can say which transport this device will
		// actually be reached on, rather than assuming.
		"transport": s.transportFor(u.ID),
	})
}

// transportFor names the path a push to this user would take right now.
func (s *Server) transportFor(userID string) string {
	if s.fcm != nil && s.store.GetDeviceToken(userID) != "" {
		return "fcm"
	}
	if s.store.GetPushToken(userID) != "" {
		return "expo"
	}
	return "none"
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
//
// Retired categories are included, carrying active=false. The apps need this
// list for two things — filling a picker and naming the category on an order
// that already exists — and hiding the retired rows would answer the first by
// blanking the service on every old order. Pickers filter on `active`.
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

// POST /notifications/read — body { ids } (empty marks the whole inbox).
func (s *Server) handleReadNotifications(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	var body struct {
		IDs []string `json:"ids"`
	}
	// A bodyless call is the common one ("I opened the list"), so a missing or
	// unreadable body is not an error — it means "all of them".
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.store.MarkNotificationsRead(u.ID, body.IDs)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
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
