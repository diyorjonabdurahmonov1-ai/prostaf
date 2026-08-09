package api

import (
	"fmt"
	"net/http"
	"time"

	"prostaff/internal/models"
	"prostaff/internal/sse"
	"prostaff/internal/store"
)

// GET /alerts — the standing warnings for the caller.
//
// Separate from /notifications on purpose. A notification is a thing that
// happened and is read once; an alert is the state the account is in, and it has
// to still be on screen tomorrow if nothing was done about it. The app draws
// these as a banner on the worker and firm panels.
func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	alerts := s.store.Alerts(u.ID)
	if alerts == nil {
		alerts = []models.Alert{}
	}
	writeJSON(w, http.StatusOK, alerts)
}

// notifyTrialSpent tells a provider when the free jobs run out, at the moment
// they run out.
//
// The banner says the same thing, but only to somebody who opens the app. The
// job that spent the last free one is the moment the rules change for them, and
// finding that out by having the phone stop ringing is how a worker concludes
// the platform is broken.
func (s *Server) notifyTrialSpent(userID string, left int, role string) {
	if left > 0 {
		return
	}
	switch role {
	case models.RoleCompany:
		s.notify(userID, "trial.exhausted",
			"Bepul buyurtmalar tugadi",
			fmt.Sprintf("Siz %d ta bepul buyurtmani ishlatdingiz. Yangi buyurtma olish uchun oylik obunani rasmiylashtiring.",
				s.store.FreeOrderLimit()),
			map[string]any{"role": role})
	default:
		s.notify(userID, "trial.exhausted",
			"Bepul buyurtmalar tugadi",
			fmt.Sprintf("Siz %d ta bepul buyurtmani ishlatdingiz. Yangi buyurtma olish uchun hisobingizni to'ldiring.",
				s.store.FreeOrderLimit()),
			map[string]any{"role": role})
	}
}

// subscriptionCheckInterval is how often the firms are swept for subscriptions
// about to run out. Hourly rather than daily: the process restarts often enough
// that a once-a-day tick could be missed entirely, and the marker on each firm
// means the extra sweeps cost a query and send nothing.
const subscriptionCheckInterval = time.Hour

// WatchSubscriptions warns firms whose subscription is inside the notice period
// and tells the ones whose period has just ended. Runs until the process exits.
//
// Each firm is marked once per message, so a firm a week out from expiry hears
// about it on the day it enters the window and then not again until the day it
// actually expires — a warning repeated every hour is a warning nobody reads.
func (s *Server) WatchSubscriptions() {
	s.sweepSubscriptions()
	for range time.Tick(subscriptionCheckInterval) {
		s.sweepSubscriptions()
	}
}

func (s *Server) sweepSubscriptions() {
	for _, ev := range s.store.DueSubscriptionWarnings() {
		title, body := subscriptionMessage(ev)
		s.notify(ev.UserID, "subscription.expiring", title, body, map[string]any{
			"company_id": ev.CompanyID,
			"ends_at":    ev.EndsAt,
			"days_left":  ev.DaysLeft,
			"expired":    ev.Expired,
		})
		// The banner is computed from the same date, so nudging the app is enough
		// to make it appear without waiting for the next screen open.
		s.publish(ev.UserID, sse.Event{
			Type: "subscription.changed",
			Data: map[string]any{"ends_at": ev.EndsAt, "days_left": ev.DaysLeft, "expired": ev.Expired},
		})
		s.store.MarkSubscriptionWarned(ev.CompanyID)
	}
}

func subscriptionMessage(ev store.SubscriptionEvent) (string, string) {
	if ev.Expired {
		return "Obuna muddati tugadi",
			fmt.Sprintf("Hisobingiz %s kuni yopildi. Yangi buyurtmalar kelmaydi — obunani yangilash uchun operatorga murojaat qiling.",
				ev.EndsAt.Format("02.01.2006"))
	}
	return fmt.Sprintf("Obunangiz %d kundan keyin tugaydi", ev.DaysLeft),
		fmt.Sprintf("Hisobingiz %s kuni yopiladi. Uzluksiz ishlash uchun obunani shu muddatgacha yangilang.",
			ev.EndsAt.Format("02.01.2006"))
}
