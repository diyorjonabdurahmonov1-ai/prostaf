package store

import (
	"database/sql"
	"time"

	"prostaff/internal/id"
	"prostaff/internal/models"
)

// A firm does not pay per job — it pays for months.
//
// Commission is the worker's arrangement: a percentage off each job, taken from
// a wallet that has to stay above a minimum. A firm is a different customer of
// the platform: it wants to know what the year costs, and the office wants to be
// able to discount that and to sell three months at a time. So a company's right
// to accept work is a date, not a balance, and this file is where that date is
// bought and read.
//
// Once a firm holds a subscription its orders are commission-free — it has
// already paid for the month the job arrived in.

// SubscriptionExtension is one purchase as the caller describes it. Price and
// discount are pointers so "not sent" means "use the current settings", which is
// the normal case: the admin sets the monthly price once and then just types how
// many months.
type SubscriptionExtension struct {
	Months          int
	PricePerMonth   *int64
	DiscountPercent *int
	// AlreadyPaid says the money has been booked elsewhere — the firm paid
	// through the gateway and the callback that settled it wrote the payment
	// row. Without this the same months would appear twice in the finance
	// overview: once as the gateway payment, once as a counter sale.
	AlreadyPaid bool
}

// ExtendCompanySubscription sells a firm `Months` more months and returns the
// record plus the updated profile.
//
// Months are added to whatever the firm already has left, not to today: a firm
// that renews a week early keeps that week. The price and discount of the day
// are copied onto the row — today's settings must not be able to rewrite what
// last year's payment cost.
func (s *Store) ExtendCompanySubscription(idOrUser string, in SubscriptionExtension) (*models.CompanySubscription, *models.CompanyProfile, error) {
	cp, ok := s.companyByIDOrUser(idOrUser)
	if !ok {
		return nil, nil, ErrNotFound
	}
	if in.Months <= 0 || in.Months > 60 {
		return nil, nil, ErrBadRequest
	}
	// What a term costs is the tariff card's business.
	//
	// The office selling three months at the counter must charge what the app
	// charges for three months, so the price comes off the card for that term.
	// Deriving it from a monthly rate instead would quietly undo every discount
	// on the card — a firm would pay one price in the app and another over the
	// phone for exactly the same thing.
	//
	// A term with no card is not priced by guesswork either: the caller has to
	// name the price, which is also how the office gives one firm a deal.
	price := int64(0)
	discount := 0
	amount := int64(0)
	if plan, ok := s.planForMonths(in.Months); ok {
		amount = plan.Price
		price = plan.PerMonth
		discount = plan.DiscountPercent
	}
	if in.PricePerMonth != nil {
		price = *in.PricePerMonth
		amount = 0
	}
	if in.DiscountPercent != nil {
		discount = *in.DiscountPercent
		// An explicit discount is an explicit instruction: recompute rather than
		// keep the card's total.
		amount = 0
	}
	if price < 0 || discount < 0 || discount > 100 {
		return nil, nil, ErrBadRequest
	}
	if amount == 0 {
		amount = price * int64(in.Months) * int64(100-discount) / 100
	}
	if amount <= 0 && !in.AlreadyPaid {
		// No card for this term and no price given. Refusing is the honest
		// answer: the alternative is a subscription sold for nothing, recorded
		// as if it had been paid for.
		return nil, nil, ErrPriceNotSet
	}

	now := time.Now()
	starts := now
	if cp.SubscriptionUntil != nil && cp.SubscriptionUntil.After(now) {
		starts = *cp.SubscriptionUntil
	}
	ends := starts.AddDate(0, in.Months, 0)

	sub := &models.CompanySubscription{
		ID:              id.New(),
		CompanyID:       cp.ID,
		Months:          in.Months,
		PricePerMonth:   price,
		DiscountPercent: discount,
		Amount:          amount,
		StartsAt:        starts,
		EndsAt:          ends,
		CreatedAt:       now,
	}
	if _, err := s.db.Exec(`INSERT INTO company_subscriptions
		(id, company_id, months, price_per_month, discount_percent, amount, starts_at, ends_at, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		sub.ID, sub.CompanyID, sub.Months, sub.PricePerMonth, sub.DiscountPercent,
		sub.Amount, sub.StartsAt, sub.EndsAt, sub.CreatedAt); err != nil {
		return nil, nil, ErrConflict
	}
	// The warning marker is cleared with the same statement that moves the date,
	// so the next "7 days left" fires for the new period instead of being
	// suppressed by the one sent for the old one.
	s.db.Exec("UPDATE companies SET subscription_until=$1, subscription_warned_at=NULL WHERE id=$2",
		ends, cp.ID)
	// Booked as a payment as well, so the money shows up in the finance overview
	// next to the top-ups instead of living only in this table.
	//
	// `paidBy` distinguishes the two ways months are bought: the office taking
	// cash at the counter, or the firm paying through the gateway — where the
	// payment row already exists and was settled by the callback, so booking a
	// second one here would double-count the same money.
	if amount > 0 && !in.AlreadyPaid {
		s.CreatePayment(cp.UserID, "admin", amount, models.PurposeSubscription)
	}

	fresh, _ := s.GetCompanyByID(cp.ID)
	return sub, fresh, nil
}

// ListCompanySubscriptions returns a firm's purchase history, newest first.
func (s *Store) ListCompanySubscriptions(idOrUser string) []models.CompanySubscription {
	cp, ok := s.companyByIDOrUser(idOrUser)
	if !ok {
		return []models.CompanySubscription{}
	}
	rows, err := s.db.Query(`SELECT id, company_id, months, price_per_month, discount_percent,
		amount, starts_at, ends_at, created_at
		FROM company_subscriptions WHERE company_id=$1 ORDER BY created_at DESC`, cp.ID)
	if err != nil {
		return []models.CompanySubscription{}
	}
	defer rows.Close()
	out := []models.CompanySubscription{}
	for rows.Next() {
		var sub models.CompanySubscription
		if rows.Scan(&sub.ID, &sub.CompanyID, &sub.Months, &sub.PricePerMonth,
			&sub.DiscountPercent, &sub.Amount, &sub.StartsAt, &sub.EndsAt, &sub.CreatedAt) == nil {
			out = append(out, sub)
		}
	}
	return out
}

// AdminSubscription is a purchase with the firm behind it — the admin history
// needs a name, not a company id.
type AdminSubscription struct {
	models.CompanySubscription
	CompanyName string `json:"company_name"`
	CompanyPhone string `json:"company_phone"`
	UserID      string `json:"user_id"`
	// INN is on the row because it is how the office identifies a firm on paper,
	// and two firms can trade under similar names.
	INN string `json:"inn,omitempty"`
}

// ListAllSubscriptions is every tariff ever sold, newest first — who bought
// what, for how long, and what they paid.
//
// The figures come off the purchase rows rather than today's tariff card: a
// price the office changed last week must not rewrite what somebody paid last
// month, which is the whole reason those numbers are copied onto the row when
// the sale is made.
func (s *Store) ListAllSubscriptions(limit int) []AdminSubscription {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.db.Query(`SELECT sub.id, sub.company_id, sub.months, sub.price_per_month,
		sub.discount_percent, sub.amount, sub.starts_at, sub.ends_at, sub.created_at,
		COALESCE(c.name,''), COALESCE(c.phone,''), COALESCE(c.user_id,''), COALESCE(c.inn,'')
		FROM company_subscriptions sub
		LEFT JOIN companies c ON c.id = sub.company_id
		ORDER BY sub.created_at DESC
		LIMIT $1`, limit)
	if err != nil {
		return []AdminSubscription{}
	}
	defer rows.Close()

	out := []AdminSubscription{}
	for rows.Next() {
		var a AdminSubscription
		if rows.Scan(&a.ID, &a.CompanyID, &a.Months, &a.PricePerMonth, &a.DiscountPercent,
			&a.Amount, &a.StartsAt, &a.EndsAt, &a.CreatedAt,
			&a.CompanyName, &a.CompanyPhone, &a.UserID, &a.INN) == nil {
			out = append(out, a)
		}
	}
	return out
}

// SubscriptionTotals is the money side of the history: how much has been sold,
// and how many firms are paid up right now.
type SubscriptionTotals struct {
	Sold          int64 `json:"sold"`
	Count         int   `json:"count"`
	ActiveFirms   int   `json:"active_firms"`
	ExpiringFirms int   `json:"expiring_firms"`
}

// SubscriptionOverview counts what the history is worth.
func (s *Store) SubscriptionOverview() SubscriptionTotals {
	var t SubscriptionTotals
	s.db.QueryRow("SELECT COALESCE(SUM(amount),0), COUNT(*) FROM company_subscriptions").
		Scan(&t.Sold, &t.Count)
	s.db.QueryRow("SELECT COUNT(*) FROM companies WHERE subscription_until > NOW()").
		Scan(&t.ActiveFirms)
	// Inside the notice window — the firms somebody should be ringing this week.
	s.db.QueryRow(`SELECT COUNT(*) FROM companies
		WHERE subscription_until > NOW()
		  AND subscription_until <= NOW() + make_interval(days => $1)`,
		s.SubscriptionWarnDays()).Scan(&t.ExpiringFirms)
	return t
}

// companyByIDOrUser accepts either a company profile id or the owning user id,
// the same courtesy every other admin route extends.
func (s *Store) companyByIDOrUser(idOrUser string) (*models.CompanyProfile, bool) {
	if cp, ok := s.GetCompanyByID(idOrUser); ok {
		return cp, true
	}
	return s.getCompanyByUser(idOrUser)
}

// planForMonths finds the tariff card for a term, so a counter sale is priced
// exactly like the same term bought in the app.
func (s *Store) planForMonths(months int) (models.SubscriptionPlan, bool) {
	for _, plan := range s.ListSubscriptionPlans(true) {
		if plan.Months == months {
			return plan, true
		}
	}
	return models.SubscriptionPlan{}, false
}

// SubscriptionWarnDays is the notice period the admin has set.
func (s *Store) SubscriptionWarnDays() int {
	if s.settings.SubscriptionWarnDays < 0 {
		return 0
	}
	return s.settings.SubscriptionWarnDays
}

// SubscriptionEvent is one firm that has to be told something about its
// subscription today.
type SubscriptionEvent struct {
	CompanyID string
	UserID    string
	Name      string
	EndsAt    time.Time
	DaysLeft  int
	// Expired distinguishes "your week starts now" from "it is over".
	Expired bool
}

// DueSubscriptionWarnings returns the firms whose subscription is inside the
// notice period, or has just run out, and which have not been told yet.
//
// `subscription_warned_at` is the marker for both messages and it is compared
// against the period rather than against the clock: a warning sent inside the
// notice window sits between (ends_at - N days) and ends_at, so the expiry
// notice — stamped after ends_at — still goes out, and a renewal that pushes
// ends_at forward leaves the old marker behind the new window and the next
// period warns again from scratch.
func (s *Store) DueSubscriptionWarnings() []SubscriptionEvent {
	days := s.SubscriptionWarnDays()
	rows, err := s.db.Query(`SELECT id, user_id, name, subscription_until, subscription_warned_at
		FROM companies
		WHERE subscription_until IS NOT NULL AND banned = FALSE
		  AND (
		    (subscription_until >  NOW()
		     AND subscription_until <= NOW() + make_interval(days => $1)
		     AND (subscription_warned_at IS NULL
		          OR subscription_warned_at < subscription_until - make_interval(days => $1)))
		    OR
		    (subscription_until <= NOW()
		     AND (subscription_warned_at IS NULL OR subscription_warned_at < subscription_until))
		  )`, days)
	if err != nil {
		return nil
	}
	defer rows.Close()

	now := time.Now()
	var out []SubscriptionEvent
	for rows.Next() {
		var ev SubscriptionEvent
		var until, warned sql.NullTime
		if rows.Scan(&ev.CompanyID, &ev.UserID, &ev.Name, &until, &warned) != nil {
			continue
		}
		if !until.Valid {
			continue
		}
		ev.EndsAt = until.Time
		ev.Expired = !until.Time.After(now)
		if !ev.Expired {
			// Rounded up: half a day left is still "1 kun", never "0 kun qoldi"
			// on a subscription that is still working.
			ev.DaysLeft = int(until.Time.Sub(now).Hours()/24) + 1
		}
		out = append(out, ev)
	}
	return out
}

// MarkSubscriptionWarned records that the firm has been told, so the daily check
// does not repeat itself every morning for a week.
func (s *Store) MarkSubscriptionWarned(companyID string) {
	s.db.Exec("UPDATE companies SET subscription_warned_at=NOW() WHERE id=$1", companyID)
}
