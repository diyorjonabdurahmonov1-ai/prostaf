package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"prostaff/internal/push"
)

// POST /me/push-test — send a real Expo push to the caller's own device.
//
// Push is the one part of the system that cannot be checked by reading logs:
// either the phone buzzes or it does not, and when it does not the reason is
// usually one of three boring things — no token stored, Expo rejected the
// token, or the app is running in Expo Go where remote push is disabled. This
// endpoint answers that question directly instead of leaving it to guesswork,
// so it reports the Expo ticket verbatim rather than a bare 200.
func (s *Server) handlePushTest(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)

	var body struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	// An empty body is fine — the defaults below make this a one-tap check, so
	// a missing or malformed payload is not worth a 400.
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Title == "" {
		body.Title = "Test bildirishnoma"
	}
	if body.Body == "" {
		body.Body = "Push ishlayapti — bu xabar serverdan keldi."
	}

	token := s.store.GetPushToken(u.ID)
	if token == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"sent":   false,
			"reason": "no_token",
			"hint": "Bu qurilma push tokenini ro'yxatdan o'tkazmagan. Expo Go'da remote push " +
				"ishlamaydi — development build kerak.",
		})
		return
	}

	tickets, err := push.SendSync(push.Urgent(token, body.Title, body.Body,
		map[string]any{"type": "test.push"}, 60))
	if err != nil {
		reason := "send_failed"
		if errors.Is(err, push.ErrNoRecipients) {
			reason = "no_token"
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"sent":   false,
			"reason": reason,
			"error":  err.Error(),
			"token":  token,
		})
		return
	}

	// Expo answers 200 even when it refuses an individual message, so the
	// ticket status is the only honest answer to "did it go out?".
	ok := len(tickets) > 0 && tickets[0].Status == "ok"
	out := map[string]any{"sent": ok, "token": token, "tickets": tickets}
	if !ok && len(tickets) > 0 {
		out["reason"] = tickets[0].Message
	}
	writeJSON(w, http.StatusOK, out)
}
