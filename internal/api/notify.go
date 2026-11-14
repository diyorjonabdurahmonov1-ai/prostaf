package api

import (
	"errors"
	"log"
	"time"

	"prostaff/internal/fcm"
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
	payload, _ := n.Data.(map[string]any)
	s.ring(userID, title, body, payload, 0)
	return n
}

// ring sends the push itself, choosing the transport that can actually reach a
// closed app.
//
// Firebase first, when a service account is configured and the device has
// handed us its own registration token. That path sends a `notification`
// payload, which Android draws with the app entirely uninvolved — the whole
// reason this exists. Expo is the fallback: it is all iOS has here, and all any
// build predating the device-token upload has, and it works fine right up until
// the app's process goes away.
//
// Best-effort and asynchronous, like the Expo path always was: a push must
// never hold up an HTTP handler, and a failure must never break the flow that
// caused it.
func (s *Server) ring(userID, title, body string, data map[string]any, ttl time.Duration) {
	if s.fcm != nil {
		if token := s.store.GetDeviceToken(userID); token != "" {
			go s.sendFCM(token, s.store.GetPushChannel(userID), title, body, data, ttl)
			return
		}
	}
	if token := s.store.GetPushToken(userID); token != "" {
		m := push.Urgent(token, title, body, data, int(ttl.Seconds()))
		m.ChannelID = s.store.GetPushChannel(userID)
		push.Send(m)
	}
}

// sendFCM delivers one Firebase message and retires a token Firebase says is
// gone. Falling back to Expo on any other error would double-send to a device
// that is simply unreachable this second, so it does not.
func (s *Server) sendFCM(token, channel, title, body string, data map[string]any, ttl time.Duration) {
	err := s.fcm.Send(fcm.Message{
		Token:     token,
		Title:     title,
		Body:      body,
		Data:      data,
		ChannelID: channel,
		TTL:       ttl,
	})
	switch {
	case err == nil:
	case errors.Is(err, fcm.ErrUnregistered):
		s.store.ClearDeviceToken(token)
	default:
		log.Printf("push: %v", err)
	}
}
