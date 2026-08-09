package store

import (
	"prostaff/internal/id"
	"prostaff/internal/models"
)

// The tariffs a firm chooses between.
//
// A firm does not pay a monthly rate that gets multiplied out — it picks a
// term off a card and pays what the card says. Three months at 200 000 is not
// "100 000 × 3 minus a discount", it is a price the office set, and storing it
// as one number is what lets them price a year however they like without the
// server arguing.

const planCols = "id, months, price, old_price, title, active, sort, created_at"

func scanPlan(row interface{ Scan(dest ...any) error }) (*models.SubscriptionPlan, error) {
	var p models.SubscriptionPlan
	if err := row.Scan(&p.ID, &p.Months, &p.Price, &p.OldPrice, &p.Title, &p.Active, &p.Sort, &p.CreatedAt); err != nil {
		return nil, err
	}
	p.Derive()
	return &p, nil
}

// seedSubscriptionPlans puts a starting set of tariffs in front of the office
// the first time the server runs, derived from COMPANY_MONTHLY_PRICE so the one
// number in .env still means something. They are ordinary rows: the admin edits
// or deletes them, and this never runs again once any plan exists.
func (s *Store) seedSubscriptionPlans() {
	var n int
	if s.db.QueryRow("SELECT COUNT(*) FROM subscription_plans").Scan(&n) != nil || n > 0 {
		return
	}
	monthly := s.settings.CompanyMonthlyPrice
	if monthly <= 0 {
		return
	}
	// Longer terms cost less per month — that is the whole shape of the offer.
	// Rounded to the nearest thousand, because a tariff card reading 254 736
	// so'm is a tariff card nobody wrote on purpose.
	seeds := []struct {
		months  int
		percent int64
	}{
		{1, 100},
		{3, 85},
		{6, 75},
		{12, 60},
	}
	for i, sd := range seeds {
		full := monthly * int64(sd.months)
		price := roundToThousand(full * sd.percent / 100)
		old := int64(0)
		if price < full {
			old = full
		}
		s.db.Exec(`INSERT INTO subscription_plans (id, months, price, old_price, sort)
			VALUES ($1,$2,$3,$4,$5)`, id.New(), sd.months, price, old, i)
	}
}

func roundToThousand(v int64) int64 {
	if v <= 0 {
		return 0
	}
	return (v + 500) / 1000 * 1000
}

// ListSubscriptionPlans returns the tariffs, cheapest term first. With
// activeOnly the retired ones are left out — that is the firm's view; the admin
// sees everything.
func (s *Store) ListSubscriptionPlans(activeOnly bool) []models.SubscriptionPlan {
	q := "SELECT " + planCols + " FROM subscription_plans"
	if activeOnly {
		q += " WHERE active = TRUE"
	}
	q += " ORDER BY sort, months"
	rows, err := s.db.Query(q)
	if err != nil {
		return []models.SubscriptionPlan{}
	}
	defer rows.Close()
	out := []models.SubscriptionPlan{}
	for rows.Next() {
		if p, err := scanPlan(rows); err == nil {
			out = append(out, *p)
		}
	}
	return out
}

// GetSubscriptionPlan returns one tariff.
func (s *Store) GetSubscriptionPlan(planID string) (*models.SubscriptionPlan, bool) {
	p, err := scanPlan(s.db.QueryRow("SELECT "+planCols+" FROM subscription_plans WHERE id=$1", planID))
	if err != nil {
		return nil, false
	}
	return p, true
}

// PlanInput is a tariff as the office describes it.
type PlanInput struct {
	Months   int
	Price    int64
	OldPrice int64
	Title    string
	Active   *bool
	Sort     *int
}

// CreateSubscriptionPlan adds a tariff.
func (s *Store) CreateSubscriptionPlan(in PlanInput) (*models.SubscriptionPlan, error) {
	if in.Months <= 0 || in.Months > 60 || in.Price <= 0 {
		return nil, ErrBadRequest
	}
	active := true
	if in.Active != nil {
		active = *in.Active
	}
	sort := in.Months
	if in.Sort != nil {
		sort = *in.Sort
	}
	planID := id.New()
	if _, err := s.db.Exec(`INSERT INTO subscription_plans
		(id, months, price, old_price, title, active, sort) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		planID, in.Months, in.Price, in.OldPrice, in.Title, active, sort); err != nil {
		return nil, ErrConflict
	}
	p, _ := s.GetSubscriptionPlan(planID)
	return p, nil
}

// PlanPatch is a tariff correction. Everything is optional; only what was sent
// is written.
type PlanPatch struct {
	Months   *int
	Price    *int64
	OldPrice *int64
	Title    *string
	Active   *bool
	Sort     *int
}

// UpdateSubscriptionPlan applies an edit.
//
// A price change is not retroactive and does not need to be: what a firm paid
// is stored on its own payment row, so last month's sale keeps last month's
// number however this card is re-priced today.
func (s *Store) UpdateSubscriptionPlan(planID string, p PlanPatch) (*models.SubscriptionPlan, error) {
	plan, ok := s.GetSubscriptionPlan(planID)
	if !ok {
		return nil, ErrNotFound
	}
	if p.Months != nil {
		if *p.Months <= 0 || *p.Months > 60 {
			return nil, ErrBadRequest
		}
		plan.Months = *p.Months
	}
	if p.Price != nil {
		if *p.Price <= 0 {
			return nil, ErrBadRequest
		}
		plan.Price = *p.Price
	}
	if p.OldPrice != nil && *p.OldPrice >= 0 {
		plan.OldPrice = *p.OldPrice
	}
	if p.Title != nil {
		plan.Title = *p.Title
	}
	if p.Active != nil {
		plan.Active = *p.Active
	}
	if p.Sort != nil {
		plan.Sort = *p.Sort
	}
	s.db.Exec(`UPDATE subscription_plans
		SET months=$1, price=$2, old_price=$3, title=$4, active=$5, sort=$6 WHERE id=$7`,
		plan.Months, plan.Price, plan.OldPrice, plan.Title, plan.Active, plan.Sort, planID)
	plan.Derive()
	return plan, nil
}

// DeleteSubscriptionPlan removes a tariff.
//
// Safe to do at any time: a plan is only ever read to price a checkout, and the
// months a firm already bought live on its own record. Nothing points back here.
func (s *Store) DeleteSubscriptionPlan(planID string) error {
	res, err := s.db.Exec("DELETE FROM subscription_plans WHERE id=$1", planID)
	if err != nil {
		return ErrConflict
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CheapestMonthlyPrice is the best per-month rate on offer — the "from" figure
// the alerts quote. Falls back to the configured monthly price when no plan is
// on sale, so a firm is never told its subscription costs nothing.
func (s *Store) CheapestMonthlyPrice() int64 {
	best := int64(0)
	for _, p := range s.ListSubscriptionPlans(true) {
		if p.PerMonth > 0 && (best == 0 || p.PerMonth < best) {
			best = p.PerMonth
		}
	}
	if best == 0 {
		return s.settings.CompanyMonthlyPrice
	}
	return best
}
