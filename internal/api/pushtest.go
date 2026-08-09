package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"prostaff/internal/fcm"
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

	// Firebase, when that is the path a real order would take. It is a single
	// synchronous call with a real answer, and no ticket/receipt dance: FCM
	// either accepted the message for that device or said why not.
	if s.fcm != nil {
		if device := s.store.GetDeviceToken(u.ID); device != "" {
			err := s.fcm.Send(fcm.Message{
				Token:     device,
				Title:     body.Title,
				Body:      body.Body,
				Data:      map[string]any{"type": "test.push"},
				ChannelID: "orders",
				TTL:       60 * time.Second,
			})
			out := map[string]any{"transport": "fcm", "token": device}
			switch {
			case err == nil:
				out["sent"], out["delivered"] = true, true
			case errors.Is(err, fcm.ErrUnregistered):
				s.store.ClearDeviceToken(device)
				out["sent"], out["code"] = false, "UNREGISTERED"
				out["hint"] = pushHint("DeviceNotRegistered")
			default:
				out["sent"], out["error"] = false, err.Error()
				out["hint"] = "Firebase xabarni qabul qilmadi. Server logidagi xatoni " +
					"tekshiring — odatda service account boshqa Firebase loyihasiga tegishli."
			}
			writeJSON(w, http.StatusOK, out)
			return
		}
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
		out["code"] = tickets[0].Code()
		out["hint"] = pushHint(tickets[0].Code())
		writeJSON(w, http.StatusOK, out)
		return
	}

	// An accepted ticket is not a delivered notification, and the difference is
	// the whole of "push works in the app but nothing arrives with it closed".
	// Everything that silently swallows a release build's notifications — an
	// FCM V1 service account that was never uploaded to the Expo project, or one
	// that belongs to a different Firebase project than google-services.json —
	// shows up only in the receipt, several seconds later. So this waits for it.
	if len(tickets) > 0 && tickets[0].ID != "" {
		if receipt, found := awaitReceipt(tickets[0].ID); found {
			out["receipt"] = receipt
			out["delivered"] = receipt.Status == "ok"
			if receipt.Status != "ok" {
				out["sent"] = false
				out["reason"] = receipt.Message
				out["code"] = receipt.Code()
				out["hint"] = pushHint(receipt.Code())
			}
		} else {
			// Expo has not decided yet. That is not a failure; saying so is
			// more useful than reporting a success nobody has confirmed.
			out["receipt_pending"] = true
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// awaitReceipt polls Expo for a verdict. Expo takes a few seconds to hear back
// from FCM/APNs and answers with an empty map until it has.
func awaitReceipt(ticketID string) (push.Receipt, bool) {
	for _, wait := range []time.Duration{3 * time.Second, 3 * time.Second, 4 * time.Second} {
		time.Sleep(wait)
		receipts, err := push.GetReceipts([]string{ticketID})
		if err != nil {
			return push.Receipt{}, false
		}
		if r, ok := receipts[ticketID]; ok {
			return r, true
		}
	}
	return push.Receipt{}, false
}

// pushHint turns Expo's error code into the thing that actually has to be
// changed. These are the only three that matter in practice, and each one has
// been mistaken for "the phone is broken" at least once.
func pushHint(code string) string {
	switch code {
	case "DeviceNotRegistered":
		return "Qurilma tokeni eskirgan (ilova o'chirilgan yoki qayta o'rnatilgan). " +
			"Ilovaga qayta kiring — yangi token ro'yxatdan o'tadi."
	case "MismatchSenderId":
		return "Expo loyihasidagi FCM kaliti google-services.json faylidagi Firebase " +
			"loyihasiga to'g'ri kelmayapti. `eas credentials` orqali FCM V1 service " +
			"account kalitini qayta yuklang va APK'ni qayta yig'ing."
	case "InvalidCredentials":
		return "Expo loyihasiga FCM V1 service account kaliti yuklanmagan. " +
			"Firebase Console → Project settings → Service accounts → Generate new " +
			"private key, so'ng `eas credentials` orqali yuklang."
	case "MessageTooBig":
		return "Xabar hajmi 4 KB dan oshib ketdi."
	case "MessageRateExceeded":
		return "Juda ko'p xabar yuborildi — biroz kutib qayta urinib ko'ring."
	default:
		return ""
	}
}
