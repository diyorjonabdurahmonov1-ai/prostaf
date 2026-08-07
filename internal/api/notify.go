package api

import (
	"prostaff/internal/models"
	"prostaff/internal/push"
	"prostaff/internal/sse"
)

// The one place a user is told that something happened to them.
//
// Before this, an event could reach a phone three different ways — a socket
// frame, an Expo push, a row in `notifications` — and most events picked one.
// An order that expired published a socket event nobody was listening for; a
// role change published one too. Both looked, from the phone, like nothing had
// happened at all, and neither left a trace anybody could read afterwards.
//
// record stores the message and puts it on the socket; notify does that and
// rings the phone as well. Callers that already send their own push (a dispatch
// wave rings a hundred workers in one batched call) use record, so nobody is
// pushed twice for one event.

// record stores a notification and pushes it down the socket as
// `notification.new`, so an open app can drop it straight into the list without
// re-reading it.
func (s *Server) record(userID, kind, title, body string, data map[string]any) *models.Notification {
	if userID == "" {
		return nil
	}
	payload := map[string]any{"type": kind}
	for k, v := range data {
		payload[k] = v
	}
	n := s.store.AddNotification(&models.Notification{
		UserID: userID,
		Type:   kind,
		Title:  title,
		Body:   body,
		Data:   payload,
	})
	s.publish(userID, sse.Event{
		Type: "notification.new",
		Data: map[string]any{"notification": n},
	})
	return n
}

// notify is record plus the push that wakes a closed app.
func (s *Server) notify(userID, kind, title, body string, data map[string]any) *models.Notification {
	n := s.record(userID, kind, title, body, data)
	if n == nil {
		return nil
	}
	if tok := s.store.GetPushToken(userID); tok != "" {
		push.Send(push.Urgent(tok, title, body, n.Data, 0))
	}
	return n
}
