package store

import (
	"database/sql"
	"time"

	"prostaff/internal/models"
)

// AdminUserPatch holds admin-editable user fields, including the worker
// personal fields that are read-only to the worker.
type AdminUserPatch struct {
	Name     *string
	Gmail    *string
	Active   *bool
	FullName *string // worker profile
	Phone    *string // worker profile
	Age      *int    // worker profile
}

// AdminUpdateUser applies an admin patch to a user (and their worker profile).
func (s *Store) AdminUpdateUser(userID string, p AdminUserPatch) (*models.User, error) {
	u, ok := s.GetUser(userID)
	if !ok {
		return nil, ErrNotFound
	}
	if p.Name != nil {
		u.Name = *p.Name
	}
	if p.Gmail != nil {
		u.Gmail = *p.Gmail
	}
	if p.Active != nil {
		u.Active = *p.Active
	}
	s.db.Exec("UPDATE users SET name=$1, gmail=$2, active=$3 WHERE id=$4",
		u.Name, u.Gmail, u.Active, userID)

	// Update worker profile locked fields.
	if wp, ok := s.GetWorkerByUser(userID); ok {
		if p.FullName != nil {
			wp.FullName = *p.FullName
		}
		if p.Phone != nil {
			wp.Phone = *p.Phone
		}
		if p.Age != nil {
			wp.Age = *p.Age
		}
		s.db.Exec("UPDATE workers SET full_name=$1, phone=$2, age=$3 WHERE user_id=$4",
			wp.FullName, wp.Phone, wp.Age, userID)
	}

	v, _ := s.UserView(userID)
	return v, nil
}

// ChangeRole adds or removes a role from a user.
func (s *Store) ChangeRole(userID, role, action string) (*models.User, error) {
	switch role {
	case models.RoleCustomer, models.RoleWorker, models.RoleCompany, models.RoleAdmin:
	default:
		return nil, ErrBadRequest
	}
	u, ok := s.GetUser(userID)
	if !ok {
		return nil, ErrNotFound
	}
	switch action {
	case "add":
		if !u.HasRole(role) {
			u.Roles = append(u.Roles, role)
		}
	case "remove":
		var nr []string
		for _, r := range u.Roles {
			if r != role {
				nr = append(nr, r)
			}
		}
		u.Roles = nr
	default:
		return nil, ErrBadRequest
	}
	s.db.Exec("UPDATE users SET roles=$1 WHERE id=$2", marshalJSON(u.Roles), userID)
	v, _ := s.UserView(userID)
	return v, nil
}

// SetWorkerBanned bans/unbans a worker.
func (s *Store) SetWorkerBanned(idOrUser string, banned bool) (*models.WorkerProfile, error) {
	// Try as profile ID first, then user ID.
	uid, ok := s.WorkerUserID(idOrUser)
	if !ok {
		// Maybe it's already a user ID.
		if _, ok := s.GetWorkerByUser(idOrUser); ok {
			uid = idOrUser
		} else {
			return nil, ErrNotFound
		}
	}
	if banned {
		s.BanWorker(uid)
	} else {
		s.UnbanWorker(uid)
	}
	wp, _ := s.GetWorkerByUser(uid)
	return wp, nil
}

// SetWorkerCreditLimit grants a worker a credit limit.
func (s *Store) SetWorkerCreditLimit(idOrUser string, amount int64) (*models.WorkerProfile, error) {
	uid, ok := s.WorkerUserID(idOrUser)
	if !ok {
		if _, ok := s.GetWorkerByUser(idOrUser); ok {
			uid = idOrUser
		} else {
			return nil, ErrNotFound
		}
	}
	s.SetCreditLimit(uid, amount)
	s.SetWalletCreditLimit(uid, amount)
	wp, _ := s.GetWorkerByUser(uid)
	return wp, nil
}

// SetWorkerOnline lets an admin force a worker online/offline. Accepts either a
// worker profile ID or the owning user ID.
func (s *Store) SetWorkerOnline(idOrUser string, online bool) (*models.WorkerProfile, error) {
	uid, ok := s.WorkerUserID(idOrUser)
	if !ok {
		if _, ok := s.GetWorkerByUser(idOrUser); !ok {
			return nil, ErrNotFound
		}
		uid = idOrUser
	}
	return s.SetWorkerStatus(uid, online, nil, nil)
}

// SetCompanyOnline lets an admin force a company online/offline. Accepts either
// a company profile ID or the owning user ID.
func (s *Store) SetCompanyOnline(idOrUser string, online bool) (*models.CompanyProfile, error) {
	uid := idOrUser
	if cp, ok := s.GetCompanyByID(idOrUser); ok {
		uid = cp.UserID
	} else if _, ok := s.getCompanyByUser(idOrUser); !ok {
		return nil, ErrNotFound
	}
	return s.SetCompanyStatus(uid, online)
}

// SetUserCreditLimit grants any provider — worker or company — the right to
// accept jobs with an empty wallet. Workers also carry the limit on their
// profile so the app can show it.
func (s *Store) SetUserCreditLimit(userID string, amount int64) (*models.Wallet, error) {
	if _, ok := s.GetUser(userID); !ok {
		return nil, ErrNotFound
	}
	s.createWalletIfNotExists(userID)
	s.SetWalletCreditLimit(userID, amount)
	if _, ok := s.GetWorkerByUser(userID); ok {
		s.SetCreditLimit(userID, amount)
	}
	w, _ := s.GetWallet(userID)
	return w, nil
}

// AdminAdjustBalance credits (or debits, when negative) a user's wallet and
// records a payment row so the movement shows up in the finance overview.
func (s *Store) AdminAdjustBalance(userID string, amount int64) (*models.Wallet, error) {
	if _, ok := s.GetUser(userID); !ok {
		return nil, ErrNotFound
	}
	if amount == 0 {
		return nil, ErrBadRequest
	}
	s.createWalletIfNotExists(userID)
	s.TopupWallet(userID, amount)
	if amount > 0 {
		s.CreatePayment(userID, "admin", amount, models.PurposeWallet)
	}
	w, _ := s.GetWallet(userID)
	return w, nil
}

// ListCompanies returns all company profiles (admin view).
func (s *Store) ListCompanies() []models.CompanyProfile {
	return s.ListAllCompanies()
}

// SetCompanyBanned bans/unbans a company.
func (s *Store) SetCompanyBanned(idOrUser string, banned bool) (*models.CompanyProfile, error) {
	// Try as profile ID.
	if banned {
		if err := s.BanCompany(idOrUser); err == nil {
			cp, _ := s.GetCompanyByID(idOrUser)
			return cp, nil
		}
	} else {
		if err := s.UnbanCompany(idOrUser); err == nil {
			cp, _ := s.GetCompanyByID(idOrUser)
			return cp, nil
		}
	}
	return nil, ErrNotFound
}

// UpdateCompanyAdmin applies an admin patch.
func (s *Store) UpdateCompanyAdmin(idOrUser string, p CompanyPatch) (*models.CompanyProfile, error) {
	return s.AdminUpdateCompany(idOrUser, p)
}

// ---- Provider onboarding (admin only) --------------------------------------
//
// Nobody turns themselves into a worker or a company from the app. The office
// takes the details over the phone and creates the account here; the person
// then simply signs in with that phone number and finds the panel waiting.

// NormalizePhone exposes normalizePhone to the API layer, so an OTP row, a user
// row and a sign-in request all key off the exact same string.
func NormalizePhone(raw string) string { return normalizePhone(raw) }

// normalizePhone brings an operator-typed number to the shape the app signs in
// with (+998XXXXXXXXX), so the account created here is the account the worker
// logs into. Nine bare digits are read as a local Uzbek number.
func normalizePhone(raw string) string {
	digits := make([]rune, 0, len(raw))
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			digits = append(digits, r)
		}
	}
	switch {
	case len(digits) == 0:
		return ""
	case len(digits) == 9:
		return "+998" + string(digits)
	default:
		return "+" + string(digits)
	}
}

// AdminWorkerInput is everything an operator fills in to put a worker on the
// platform.
type AdminWorkerInput struct {
	Phone       string // account + contact number
	Gmail       string
	Name        string // account name; defaults to FullName
	FullName    string
	Gender      string
	Age         int
	PriceMin    int64
	PriceMax    int64
	Lat         float64
	Lng         float64
	CategoryIDs []string
	CreditLimit int64
	Online      bool
	// PassportSeries is required — see WorkerInput.
	PassportSeries string

	// The public profile. Optional here: the office can open the account first
	// and the worker fills in their photo and bio from their own panel.
	AvatarURL       string
	About           string
	ExperienceYears int
	Address         string
	Images          []string
}

// AdminCreateWorker creates (or reuses) the account for a phone number and
// turns it into a worker with everything filled in.
func (s *Store) AdminCreateWorker(in AdminWorkerInput) (*models.User, error) {
	phone := normalizePhone(in.Phone)
	if phone == "" || in.FullName == "" {
		return nil, ErrBadRequest
	}
	// The paperwork is judged before the account is opened. GetOrCreateUserByPhone
	// below *creates* a customer, so a form rejected further down would leave a
	// bare account behind for a worker who was never registered.
	passport := NormalizePassport(in.PassportSeries)
	if !ValidPassport(passport) {
		return nil, ErrInvalidPassport
	}
	if s.passportTaken(passport, "") {
		return nil, ErrDuplicatePassport
	}
	u, _ := s.GetOrCreateUserByPhone(phone)
	if u == nil {
		return nil, ErrBadRequest
	}
	if _, exists := s.GetWorkerByUser(u.ID); exists {
		return nil, ErrConflict
	}
	name := in.Name
	if name == "" {
		name = in.FullName
	}
	s.db.Exec("UPDATE users SET name=$1 WHERE id=$2", name, u.ID)
	if in.Gmail != "" {
		s.db.Exec("UPDATE users SET gmail=$1 WHERE id=$2", in.Gmail, u.ID)
	}
	s.createWalletIfNotExists(u.ID)

	if _, err := s.CreateWorkerProfile(u.ID, WorkerInput{
		PassportSeries:  in.PassportSeries,
		FullName:        in.FullName,
		Phone:           phone,
		Gender:          in.Gender,
		Age:             in.Age,
		PriceMin:        in.PriceMin,
		PriceMax:        in.PriceMax,
		Lat:             in.Lat,
		Lng:             in.Lng,
		CategoryIDs:     in.CategoryIDs,
		AvatarURL:       in.AvatarURL,
		About:           in.About,
		ExperienceYears: in.ExperienceYears,
		Address:         in.Address,
		Images:          in.Images,
	}); err != nil {
		return nil, err
	}
	// The profile alone does not make the account a worker: the admin screens
	// sort people by role, and the role.changed event below carries the roles.
	s.ChangeRole(u.ID, models.RoleWorker, "add")
	// A credit limit lets the worker take jobs before topping up — the usual
	// arrangement for someone the office just signed up.
	if in.CreditLimit > 0 {
		s.SetCreditLimit(u.ID, in.CreditLimit)
		s.SetWalletCreditLimit(u.ID, in.CreditLimit)
	}
	if in.Online {
		s.SetWorkerStatus(u.ID, true, nil, nil)
	}
	v, _ := s.UserView(u.ID)
	return v, nil
}

// AdminWorkerPatch is the full worker record as the office may correct it —
// including the fields the worker cannot touch themselves.
type AdminWorkerPatch struct {
	// PassportSeries can be corrected, which is why this patch exists at all for
	// it: a mistyped series is a worker filed under somebody else's document.
	PassportSeries *string

	FullName *string
	Phone    *string
	Gender      *string
	Age         *int
	PriceMin    *int64
	PriceMax    *int64
	Lat         *float64
	Lng         *float64
	CategoryIDs *[]string

	AvatarURL       *string
	About           *string
	ExperienceYears *int
	Address         *string
	Images          *[]string
}

// AdminUpdateWorker applies an operator's edit. Accepts a worker profile ID or
// the owning user ID.
func (s *Store) AdminUpdateWorker(idOrUser string, p AdminWorkerPatch) (*models.WorkerProfile, error) {
	uid, ok := s.WorkerUserID(idOrUser)
	if !ok {
		if _, ok := s.GetWorkerByUser(idOrUser); !ok {
			return nil, ErrNotFound
		}
		uid = idOrUser
	}
	wp, ok := s.GetWorkerByUser(uid)
	if !ok {
		return nil, ErrNotFound
	}
	if p.PassportSeries != nil {
		series := NormalizePassport(*p.PassportSeries)
		if !ValidPassport(series) {
			return nil, ErrInvalidPassport
		}
		if s.passportTaken(series, uid) {
			return nil, ErrDuplicatePassport
		}
		// Written on its own so the trial counter follows the correction: the
		// ledger is keyed on the series, so the worker's used-count is now read
		// from the document they actually hold.
		if _, err := s.db.Exec("UPDATE workers SET passport_series=$1 WHERE user_id=$2", series, uid); err != nil {
			return nil, ErrDuplicatePassport
		}
		wp.PassportSeries = series
	}
	if p.FullName != nil {
		wp.FullName = *p.FullName
	}
	if p.Phone != nil {
		wp.Phone = normalizePhone(*p.Phone)
	}
	if p.Gender != nil && (*p.Gender == "male" || *p.Gender == "female") {
		wp.Gender = *p.Gender
	}
	if p.Age != nil {
		wp.Age = *p.Age
	}
	if p.PriceMin != nil {
		wp.PriceMin = *p.PriceMin
	}
	if p.PriceMax != nil {
		wp.PriceMax = *p.PriceMax
	}
	if p.Lat != nil {
		wp.Lat = *p.Lat
	}
	if p.Lng != nil {
		wp.Lng = *p.Lng
	}
	if p.CategoryIDs != nil {
		for _, c := range *p.CategoryIDs {
			if !s.categoryExists(c) {
				return nil, ErrBadRequest
			}
		}
		wp.CategoryIDs = *p.CategoryIDs
	}
	applyWorkerPresentation(wp, p.AvatarURL, p.About, p.ExperienceYears, p.Address, p.Images)
	s.db.Exec(`UPDATE workers SET full_name=$1, phone=$2, gender=$3, age=$4, price_min=$5,
		price_max=$6, lat=$7, lng=$8, category_ids=$9, avatar_url=$10, about=$11,
		experience_years=$12, address=$13, images=$14 WHERE user_id=$15`,
		wp.FullName, wp.Phone, wp.Gender, wp.Age, wp.PriceMin, wp.PriceMax, wp.Lat, wp.Lng,
		marshalJSON(wp.CategoryIDs), wp.AvatarURL, wp.About, wp.ExperienceYears, wp.Address,
		marshalJSON(wp.Images), uid)
	return wp, nil
}

// AdminCompanyInput is everything an operator fills in to put a company on the
// platform.
type AdminCompanyInput struct {
	Phone       string
	Gmail       string
	Name        string
	Description string
	Images      []string
	Price       int64
	CategoryIDs []string
	Lat         float64
	Lng         float64
	CreditLimit int64
	Online      bool
	// INN is required — see CompanyInput.
	INN string
	// SubscriptionMonths, when set, sells the firm its first months in the same
	// call that opens the account. Zero leaves it on its free trial jobs.
	SubscriptionMonths int

	AvatarURL       string
	Address         string
	WorkingHours    string
	EmployeesCount  int
	ExperienceYears int
	Email           string
	Website         string
}

// AdminCreateCompany creates (or reuses) the account for a phone number and
// turns it into a service company.
func (s *Store) AdminCreateCompany(in AdminCompanyInput) (*models.User, error) {
	phone := normalizePhone(in.Phone)
	if phone == "" || in.Name == "" || in.Price <= 0 {
		return nil, ErrBadRequest
	}
	// Same order as for a worker: the INN is judged before an account exists.
	inn := NormalizeINN(in.INN)
	if !ValidINN(inn) {
		return nil, ErrInvalidINN
	}
	if s.innTaken(inn, "") {
		return nil, ErrDuplicateINN
	}
	u, _ := s.GetOrCreateUserByPhone(phone)
	if u == nil {
		return nil, ErrBadRequest
	}
	if _, exists := s.getCompanyByUser(u.ID); exists {
		return nil, ErrConflict
	}
	s.db.Exec("UPDATE users SET name=$1 WHERE id=$2", in.Name, u.ID)
	if in.Gmail != "" {
		s.db.Exec("UPDATE users SET gmail=$1 WHERE id=$2", in.Gmail, u.ID)
	}
	s.createWalletIfNotExists(u.ID)

	if _, err := s.CreateCompanyProfile(u.ID, CompanyInput{
		INN:             in.INN,
		Name:            in.Name,
		Phone:           phone,
		Description:     in.Description,
		Images:          in.Images,
		Price:           in.Price,
		CategoryIDs:     in.CategoryIDs,
		Lat:             in.Lat,
		Lng:             in.Lng,
		AvatarURL:       in.AvatarURL,
		Address:         in.Address,
		WorkingHours:    in.WorkingHours,
		EmployeesCount:  in.EmployeesCount,
		ExperienceYears: in.ExperienceYears,
		Email:           in.Email,
		Website:         in.Website,
	}); err != nil {
		return nil, err
	}
	// Same as for a worker: without the role the firm shows up as a plain
	// customer in the admin list.
	s.ChangeRole(u.ID, models.RoleCompany, "add")
	if in.CreditLimit > 0 {
		s.SetWalletCreditLimit(u.ID, in.CreditLimit)
	}
	// A firm that paid at the counter starts subscribed. One that did not starts
	// on its free jobs, which is exactly what the office wants for a trial.
	if in.SubscriptionMonths > 0 {
		if cp, ok := s.getCompanyByUser(u.ID); ok {
			s.ExtendCompanySubscription(cp.ID, SubscriptionExtension{Months: in.SubscriptionMonths})
		}
	}
	if in.Online {
		s.SetCompanyStatus(u.ID, true)
	}
	v, _ := s.UserView(u.ID)
	return v, nil
}

// ---- Analytics / finance / settings ----------------------------------------

type Bucket struct {
	Label   string `json:"label"`
	Count   int    `json:"count"`
	Revenue int64  `json:"revenue"`
}

type Analytics struct {
	Buckets []Bucket `json:"buckets"`
	Totals  struct {
		Count   int   `json:"count"`
		Revenue int64 `json:"revenue"`
	} `json:"totals"`
}

func (s *Store) OrderAnalytics(period string) Analytics {
	var format string
	switch period {
	case "month":
		format = "YYYY-MM"
	case "year":
		format = "YYYY"
	default:
		format = "YYYY-MM-DD"
	}
	rows, err := s.db.Query(
		"SELECT to_char(created_at, '" + format + "') as p, COUNT(*), COALESCE(SUM(commission_amount),0) FROM orders GROUP BY p ORDER BY p")
	if err != nil {
		return Analytics{Buckets: []Bucket{}}
	}
	defer rows.Close()
	var a Analytics
	for rows.Next() {
		var b Bucket
		rows.Scan(&b.Label, &b.Count, &b.Revenue)
		a.Totals.Count += b.Count
		a.Totals.Revenue += b.Revenue
		a.Buckets = append(a.Buckets, b)
	}
	if a.Buckets == nil {
		a.Buckets = []Bucket{}
	}
	return a
}

// AdminStats is the admin dashboard summary — every number is real.
type AdminStats struct {
	TotalUsers      int      `json:"total_users"`
	TotalCustomers  int      `json:"total_customers"`
	TotalWorkers    int      `json:"total_workers"`
	OnlineWorkers   int      `json:"online_workers"`
	BannedWorkers   int      `json:"banned_workers"`
	TotalCompanies  int      `json:"total_companies"`
	OnlineCompanies int      `json:"online_companies"`
	TotalOrders     int      `json:"total_orders"`
	ActiveOrders    int      `json:"active_orders"`
	CompletedOrders int      `json:"completed_orders"`
	TotalRevenue    int64    `json:"total_revenue"`
	RevenueGrowth   int      `json:"revenue_growth"`
	Series          []Bucket `json:"series"`
}

// AdminStats gathers the dashboard counters plus a 7-day commission series.
func (s *Store) AdminStats() AdminStats {
	var a AdminStats
	count := func(q string, dest *int) {
		_ = s.db.QueryRow(q).Scan(dest)
	}
	count("SELECT COUNT(*) FROM users", &a.TotalUsers)
	count("SELECT COUNT(*) FROM workers", &a.TotalWorkers)
	count("SELECT COUNT(*) FROM workers WHERE online", &a.OnlineWorkers)
	count("SELECT COUNT(*) FROM workers WHERE banned", &a.BannedWorkers)
	count("SELECT COUNT(*) FROM companies", &a.TotalCompanies)
	count("SELECT COUNT(*) FROM companies WHERE online", &a.OnlineCompanies)
	count("SELECT COUNT(*) FROM orders", &a.TotalOrders)
	count("SELECT COUNT(*) FROM orders WHERE status IN ('pending','matching','invited','accepted','in_progress')", &a.ActiveOrders)
	count("SELECT COUNT(*) FROM orders WHERE status='completed'", &a.CompletedOrders)
	a.TotalCustomers = max(a.TotalUsers-a.TotalWorkers-a.TotalCompanies, 0)
	_ = s.db.QueryRow("SELECT COALESCE(SUM(commission_amount),0) FROM orders").Scan(&a.TotalRevenue)

	// Last 7 days, oldest first, with zero-filled gaps so the chart never lies.
	revByDay := map[string]int64{}
	cntByDay := map[string]int{}
	rows, err := s.db.Query(`SELECT to_char(created_at,'YYYY-MM-DD'), COUNT(*), COALESCE(SUM(commission_amount),0)
		FROM orders WHERE created_at >= NOW() - INTERVAL '7 days' GROUP BY 1`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var day string
			var c int
			var rev int64
			if rows.Scan(&day, &c, &rev) == nil {
				revByDay[day] = rev
				cntByDay[day] = c
			}
		}
	}
	today := time.Now()
	for i := 6; i >= 0; i-- {
		label := today.AddDate(0, 0, -i).Format("2006-01-02")
		a.Series = append(a.Series, Bucket{Label: label, Count: cntByDay[label], Revenue: revByDay[label]})
	}

	// Growth = last 3 days vs the 3 before them.
	var recent, previous int64
	for i, b := range a.Series {
		if i >= 4 {
			recent += b.Revenue
		} else if i >= 1 {
			previous += b.Revenue
		}
	}
	if previous > 0 {
		a.RevenueGrowth = int((recent - previous) * 100 / previous)
	} else if recent > 0 {
		a.RevenueGrowth = 100
	}
	return a
}

// AdminOrder is an order enriched with the human-readable names an admin needs
// (the raw order only carries IDs).
type AdminOrder struct {
	models.Order
	CustomerEmail string `json:"customer_email"`
	WorkerName    string `json:"worker_name,omitempty"`
	CompanyName   string `json:"company_name,omitempty"`
	CategoryName  string `json:"category_name,omitempty"`
	Commission    int64  `json:"commission"`
	// Bucket is the day / month / year label the order falls in, so the client
	// can group without re-parsing dates.
	Bucket string `json:"bucket"`
}

// ListAllOrders returns every order, newest first, enriched with names and a
// period bucket label. period is "day" (default), "month" or "year".
func (s *Store) ListAllOrders(period, status string) []AdminOrder {
	format := "YYYY-MM-DD"
	switch period {
	case "month":
		format = "YYYY-MM"
	case "year":
		format = "YYYY"
	}
	q := `SELECT o.id, o.customer_id, o.worker_id, o.company_id, o.category_id, o.gender,
		o.description, o.image_url, o.customer_phone, o.worker_phone, o.lat, o.lng, o.status,
		o.invited_worker_ids, o.invite_expires_at, o.commission_amount, o.created_at,
		COALESCE(o.customer_name,''), o.price,
		to_char(o.created_at, '` + format + `'),
		COALESCE(NULLIF(cu.name,''), o.customer_name, ''), COALESCE(cu.gmail,''),
		COALESCE(w.full_name,''), COALESCE(co.name,''), COALESCE(cat.name_uz,'')
		FROM orders o
		LEFT JOIN users cu ON cu.id = o.customer_id
		LEFT JOIN workers w ON w.user_id = o.worker_id
		LEFT JOIN companies co ON co.id = o.company_id
		LEFT JOIN categories cat ON cat.id = o.category_id`
	args := []any{}
	if status != "" && status != "all" {
		q += " WHERE o.status=$1"
		args = append(args, status)
	}
	q += " ORDER BY o.created_at DESC"

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return []AdminOrder{}
	}
	defer rows.Close()

	out := []AdminOrder{}
	for rows.Next() {
		var ao AdminOrder
		var invitedIDs, joinedName string
		var inviteExpires sql.NullTime
		err := rows.Scan(&ao.ID, &ao.CustomerID, &ao.WorkerID, &ao.CompanyID, &ao.CategoryID,
			&ao.Gender, &ao.Description, &ao.ImageURL, &ao.CustomerPhone, &ao.WorkerPhone,
			&ao.Lat, &ao.Lng, &ao.Status, &invitedIDs, &inviteExpires, &ao.CommissionAmount,
			&ao.CreatedAt, &ao.CustomerName, &ao.Price, &ao.Bucket, &joinedName, &ao.CustomerEmail,
			&ao.WorkerName, &ao.CompanyName, &ao.CategoryName)
		if err != nil {
			continue
		}
		ao.InvitedWorkerIDs = unmarshalStrings(invitedIDs)
		if inviteExpires.Valid {
			ao.InviteExpiresAt = &inviteExpires.Time
		}
		if joinedName != "" {
			ao.CustomerName = joinedName
		}
		ao.Commission = ao.CommissionAmount
		out = append(out, ao)
	}
	return out
}

type Finance struct {
	Payments []models.Payment `json:"payments"`
	Totals   struct {
		Topups int64 `json:"topups"`
		// Subscriptions is what firms have paid for months. Kept apart from
		// top-ups because it is a different business: a top-up is a worker's own
		// money sitting in a wallet waiting to be spent on commission, while a
		// subscription is revenue the moment it is taken.
		Subscriptions int64 `json:"subscriptions"`
		Commission    int64 `json:"commission"`
	} `json:"totals"`
	CommissionPercent int `json:"commission_percent"`
}

func (s *Store) FinanceOverview() Finance {
	var f Finance
	f.Payments = s.ListPayments()
	if f.Payments == nil {
		f.Payments = []models.Payment{}
	}
	// Only settled money counts. A payment that is still pending is somebody
	// looking at an invoice page, and a cancelled one never happened.
	for _, p := range f.Payments {
		if p.Status != models.PaymentSuccess && p.Status != "completed" {
			continue
		}
		if p.Purpose == models.PurposeSubscription {
			f.Totals.Subscriptions += p.Amount
			continue
		}
		f.Totals.Topups += p.Amount
	}
	// Sum commission from orders.
	s.db.QueryRow("SELECT COALESCE(SUM(commission_amount),0) FROM orders").Scan(&f.Totals.Commission)
	f.CommissionPercent = s.settings.CommissionPercent
	return f
}

type SettingsPatch struct {
	CommissionPercent *int
	MinBalance        *int64

	FreeOrderLimit         *int
	CompanyMonthlyPrice    *int64
	CompanyDiscountPercent *int
	SubscriptionWarnDays   *int
}
