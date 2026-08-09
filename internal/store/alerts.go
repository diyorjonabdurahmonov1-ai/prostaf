package store

import (
	"fmt"
	"strconv"
	"strings"

	"prostaff/internal/models"
)

// Alerts are the standing conditions a provider is in, computed fresh on every
// read.
//
// A notification is an event — "somebody accepted your order" — and it is true
// once. An alert is a state: the three free jobs are gone and the wallet is
// under the minimum, and that stays true, and stays worth saying, until money
// arrives. A worker who swiped the push away still has to be told why no jobs
// are reaching them, and telling them is the difference between "the app is
// broken" and "top up your balance".
//
// The wording is the office's own: it names the number that has to be reached,
// because "insufficient balance" answers nothing anybody actually asked.

// Alerts returns everything the given account should currently be warned about,
// across both hats it may wear.
func (s *Store) Alerts(userID string) []models.Alert {
	out := []models.Alert{}
	if wp, ok := s.GetWorkerByUser(userID); ok {
		if a, has := s.workerAlert(wp); has {
			out = append(out, a)
		}
	}
	if cp, ok := s.getCompanyByUser(userID); ok {
		if a, has := s.companyAlert(cp); has {
			out = append(out, a)
		}
	}
	return out
}

// workerAlert is the worker's side: trial jobs while they last, then the balance
// rule the moment they run out.
func (s *Store) workerAlert(wp *models.WorkerProfile) (models.Alert, bool) {
	if wp.Banned {
		return models.Alert{
			Type:  models.AlertTrialExhausted,
			Level: models.AlertDanger,
			Role:  models.RoleWorker,
			Title: "Hisobingiz bloklangan",
			Body:  "Siz vaqtincha buyurtma qabul qila olmaysiz. Batafsil ma'lumot uchun operatorga murojaat qiling.",
		}, true
	}
	if left := wp.FreeOrdersLeft(); left > 0 {
		return models.Alert{
			Type:  models.AlertTrialLeft,
			Level: models.AlertInfo,
			Role:  models.RoleWorker,
			Title: fmt.Sprintf("%d ta bepul ish qoldi", left),
			Body: fmt.Sprintf(
				"Yangi hisob uchun %d ta buyurtmani balanssiz qabul qila olasiz. Keyin hisobingizda kamida %s so'm bo'lishi kerak.",
				s.FreeOrderLimit(), formatSom(s.settings.MinBalance)),
			Action: "topup",
		}, true
	}
	w := s.getWallet(wp.UserID)
	if w == nil {
		return models.Alert{}, false
	}
	if w.Balance >= w.MinBalance || w.CreditLimit > 0 {
		return models.Alert{}, false
	}
	// The one the whole feature exists for. It names both facts a worker needs:
	// the free jobs are spent, and here is the number to get back to.
	return models.Alert{
		Type:  models.AlertLowBalance,
		Level: models.AlertDanger,
		Role:  models.RoleWorker,
		Title: "Hisobingizda mablag' yo'q",
		Body: fmt.Sprintf(
			"Bepul %d ta ish qabul qilish imkoniyati tugadi. Buyurtma olish uchun hisobingizdagi mablag' %s so'mdan ko'p bo'lishi kerak.",
			s.FreeOrderLimit(), formatSom(w.MinBalance)),
		Action: "topup",
	}, true
}

// companyAlert is the firm's side: the same trial, then a date instead of a
// balance — a week before it runs out, and again once it has.
func (s *Store) companyAlert(cp *models.CompanyProfile) (models.Alert, bool) {
	if cp.Banned {
		return models.Alert{
			Type:  models.AlertSubscriptionMissing,
			Level: models.AlertDanger,
			Role:  models.RoleCompany,
			Title: "Firma bloklangan",
			Body:  "Firmangiz vaqtincha buyurtma qabul qila olmaydi. Batafsil ma'lumot uchun operatorga murojaat qiling.",
		}, true
	}
	if cp.SubscriptionActive {
		if warn := s.SubscriptionWarnDays(); warn > 0 && cp.SubscriptionDaysLeft <= warn {
			return models.Alert{
				Type:  models.AlertSubscriptionExpiring,
				Level: models.AlertWarning,
				Role:  models.RoleCompany,
				Title: fmt.Sprintf("Obunangiz %d kundan keyin tugaydi", cp.SubscriptionDaysLeft),
				Body: fmt.Sprintf(
					"Hisobingiz %s kuni yopiladi. Uzluksiz ishlash uchun obunani shu muddatgacha yangilang.",
					cp.SubscriptionUntil.Format("02.01.2006")),
				Action: "subscription",
			}, true
		}
		return models.Alert{}, false
	}
	if left := cp.FreeOrdersLeft(); left > 0 {
		return models.Alert{
			Type:  models.AlertTrialLeft,
			Level: models.AlertInfo,
			Role:  models.RoleCompany,
			Title: fmt.Sprintf("%d ta bepul ish qoldi", left),
			Body: fmt.Sprintf(
				"Yangi firma uchun %d ta buyurtmani obunasiz qabul qila olasiz. Keyin oylik obuna kerak bo'ladi — bir oy %s so'm.",
				s.FreeOrderLimit(), formatSom(s.CheapestMonthlyPrice())),
			Action: "subscription",
		}, true
	}
	// Never subscribed and out of free jobs reads differently from a firm whose
	// paid period ran out, even though both are refused work.
	if cp.SubscriptionUntil == nil {
		return models.Alert{
			Type:  models.AlertSubscriptionMissing,
			Level: models.AlertDanger,
			Role:  models.RoleCompany,
			Title: "Obuna kerak",
			Body: fmt.Sprintf(
				"Bepul %d ta ish qabul qilish imkoniyati tugadi. Buyurtma olish uchun oylik obunani rasmiylashtiring.",
				s.FreeOrderLimit()),
			Action: "subscription",
		}, true
	}
	return models.Alert{
		Type:  models.AlertSubscriptionExpired,
		Level: models.AlertDanger,
		Role:  models.RoleCompany,
		Title: "Obuna muddati tugadi",
		Body: fmt.Sprintf(
			"Hisobingiz %s kuni yopildi va yangi buyurtmalar kelmaydi. Obunani yangilang — bir oy %s so'm.",
			cp.SubscriptionUntil.Format("02.01.2006"), formatSom(s.CheapestMonthlyPrice())),
		Action: "subscription",
	}, true
}

// FormatSom is formatSom for callers outside this package — the API layer
// quotes money in the messages it sends, and it must read the same way there.
func FormatSom(v int64) string { return formatSom(v) }

// formatSom groups a so'm figure in threes the way the apps print it
// (10000 → "10 000"), so a number quoted in a warning matches the number on the
// wallet screen it is telling somebody to go and look at.
func formatSom(v int64) string {
	digits := strconv.FormatInt(v, 10)
	neg := strings.HasPrefix(digits, "-")
	digits = strings.TrimPrefix(digits, "-")

	var b strings.Builder
	for i, r := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}
