// Package store is the data layer for PROSTAFF. Backed by PostgreSQL.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"prostaff/internal/blob"
	"prostaff/internal/config"
	"prostaff/internal/id"
	"prostaff/internal/models"
)

// Sentinel errors mapped to HTTP statuses by the API layer.
var (
	ErrNotFound            = errors.New("not_found")
	ErrConflict            = errors.New("conflict")
	ErrForbidden           = errors.New("forbidden")
	ErrBadRequest          = errors.New("bad_request")
	ErrInsufficientBalance = errors.New("insufficient_balance")

	// The reasons an order to a firm is refused. They were all plain
	// ErrConflict / ErrForbidden, which reached the customer as "request
	// conflicts with current state" — a sentence that tells somebody staring at
	// an order form precisely nothing, and reads exactly like the button did not
	// work. Each of these becomes its own message.
	ErrCompanyBusy   = errors.New("company_busy")
	ErrCompanyBanned = errors.New("company_banned")
	ErrOwnCompany    = errors.New("own_company")
	ErrOrderNotSaved = errors.New("order_not_saved")

	// The paperwork. A worker is opened against a passport series and a firm
	// against an INN, both unique across the platform, and the operator typing
	// them in needs to be told which of the two things went wrong: a number that
	// cannot be a passport at all, or one that already belongs to somebody.
	ErrInvalidPassport   = errors.New("invalid_passport")
	ErrDuplicatePassport = errors.New("duplicate_passport")
	ErrInvalidINN        = errors.New("invalid_inn")
	ErrDuplicateINN      = errors.New("duplicate_inn")

	// The two ways editing the service list goes wrong. Both need their own
	// sentence: "conflict" leaves the operator guessing whether they typed a
	// duplicate or hit something they are not allowed to remove.
	ErrDuplicateCategory = errors.New("duplicate_category")
	ErrCategoryInUse     = errors.New("category_in_use")

	// ErrSubscriptionExpired is a firm trying to take a job with no months left
	// and no free ones. Separate from ErrInsufficientBalance because the fix is
	// different: a firm does not top up a wallet, it buys another month.
	ErrSubscriptionExpired = errors.New("subscription_expired")

	// ErrCompanyUnavailable is the customer's side of the same fact: the firm is
	// listed but cannot take the job today.
	ErrCompanyUnavailable = errors.New("company_unavailable")

	// The two ways an upload is refused. Both were ErrBadRequest, which reaches
	// the phone as "invalid request" — a sentence that tells somebody whose
	// photo just failed nothing about the one thing they can do next, which is
	// pick a smaller or differently formatted one.
	ErrFileTooLarge     = errors.New("file_too_large")
	ErrUnsupportedMedia = errors.New("unsupported_media")

	// ErrPriceNotSet is a term nobody has priced: no tariff card of that length
	// and no price named in the request. Selling it anyway would book a
	// subscription for nothing and record it as paid.
	ErrPriceNotSet = errors.New("price_not_set")
)

// matchRadiusKm is the smart-matchmaking radius.
const matchRadiusKm = 15.0

// Store wraps a PostgreSQL database with domain methods.
type Store struct {
	db  *sql.DB
	cfg config.Config
	// blobs is where uploaded images are written. Nil when UPLOAD_DIR is unset,
	// which puts the bytes back in the database — see internal/blob.
	blobs    *blob.Store
	settings models.Settings
}

// New creates a store backed by the given database, seeds categories if empty,
// and loads settings. blobs may be nil, meaning uploads stay in Postgres.
func New(cfg config.Config, db *sql.DB, blobs *blob.Store) *Store {
	s := &Store{
		db:    db,
		cfg:   cfg,
		blobs: blobs,
		settings: models.Settings{
			CommissionPercent:      cfg.CommissionPercent,
			MinBalance:             cfg.MinBalance,
			FreeOrderLimit:         cfg.FreeOrderLimit,
			CompanyMonthlyPrice:    cfg.CompanyMonthlyPrice,
			CompanyDiscountPercent: cfg.CompanyDiscountPercent,
			SubscriptionWarnDays:   cfg.SubscriptionWarnDays,
		},
	}
	s.seed()
	s.loadSettings()
	// After the settings, because the starting tariffs are derived from the
	// monthly price the office has actually configured.
	s.seedSubscriptionPlans()
	// Last, because the fixtures need the categories, the settings and the
	// tariffs that the three calls above put in place.
	if cfg.SeedDemo {
		s.seedDemoProviders()
	}
	return s
}

// Ping reports whether the database is reachable, for the health endpoint.
func (s *Store) Ping() error { return s.db.Ping() }

// ---- JSON helpers -----------------------------------------------------------

func marshalJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func unmarshalStrings(s string) []string {
	var out []string
	_ = json.Unmarshal([]byte(s), &out)
	if out == nil {
		out = []string{}
	}
	return out
}

// ---- Seeding ----------------------------------------------------------------

type catSeed struct{ key, uz, ru, en, icon string }

// The icon column is legacy. The apps draw their own glyph, chosen from the
// category's stable key (mobile/src/lib/category-icon.ts), because a picture is
// one more thing the office would have had to answer for before it could add a
// category — and a name is what a category actually is. Nothing reads this
// value; the seeds keep it only so the column is not silently empty.
var categorySeeds = []catSeed{
	{"banquet_helper", "Bazmga yordamchi", "Помощник на банкете", "Banquet helper", "confetti"},
	{"gardening", "Bog' ishlari", "Садовые работы", "Gardening", "tree"},
	{"mover_coal", "Ko'k tushuruvchi", "Разгрузка", "Mover", "truck"},
	{"loader", "Ortuvchi", "Грузчик", "Loader", "package"},
	{"post_repair_clean", "Remontdan keyingi tozalash", "Уборка после ремонта", "Post-repair cleaning", "broom"},
	{"master_helper", "Ustaga yordamchi", "Помощник мастера", "Master helper", "wrench"},
	{"room_clean", "Xona tozalash", "Уборка комнат", "Room cleaning", "spray-bottle"},
}

func (s *Store) seed() {
	// Only seed categories if table is empty.
	var count int
	s.db.QueryRow("SELECT COUNT(*) FROM categories").Scan(&count)
	if count > 0 {
		return
	}
	// A starting list, not the list: the office edits these rows like any other
	// from the panel, and this never runs again once any category exists.
	for i, c := range categorySeeds {
		s.db.Exec("INSERT INTO categories (id, key, name_uz, name_ru, name_en, icon, sort) VALUES ($1, $2, $3, $4, $5, $6, $7)",
			id.New(), c.key, c.uz, c.ru, c.en, c.icon, i)
	}

	// Seed admin from config.
	adminPhone := s.cfg.AdminPhone
	if adminPhone != "" && adminPhone[0] != '+' {
		adminPhone = "+" + adminPhone
	}
	adminID := id.New()
	s.db.Exec("INSERT INTO users (id, phone, gmail, name, roles) VALUES ($1, $2, $3, $4, $5)",
		adminID, adminPhone, s.cfg.AdminEmail, "Admin", `["customer","admin"]`)
	s.db.Exec("INSERT INTO wallets (user_id) VALUES ($1)", adminID)
}

func (s *Store) loadSettings() {
	loadInt := func(key string, dst *int) {
		var val string
		if s.db.QueryRow("SELECT value FROM settings WHERE key=$1", key).Scan(&val) != nil {
			return
		}
		var v int
		if json.Unmarshal([]byte(val), &v) == nil {
			*dst = v
		}
	}
	loadInt64 := func(key string, dst *int64) {
		var val string
		if s.db.QueryRow("SELECT value FROM settings WHERE key=$1", key).Scan(&val) != nil {
			return
		}
		var v int64
		if json.Unmarshal([]byte(val), &v) == nil {
			*dst = v
		}
	}
	loadInt("commission_percent", &s.settings.CommissionPercent)
	loadInt64("min_balance", &s.settings.MinBalance)
	loadInt("free_order_limit", &s.settings.FreeOrderLimit)
	loadInt64("company_monthly_price", &s.settings.CompanyMonthlyPrice)
	loadInt("company_discount_percent", &s.settings.CompanyDiscountPercent)
	loadInt("subscription_warn_days", &s.settings.SubscriptionWarnDays)
}

// ---- Users ------------------------------------------------------------------

func scanUser(row interface{ Scan(dest ...any) error }) (*models.User, error) {
	var u models.User
	var roles string
	err := row.Scan(&u.ID, &u.Phone, &u.Gmail, &u.Name, &u.AvatarURL, &u.PushToken,
		&roles, &u.Active, &u.CreatedAt)
	if err != nil {
		return nil, err
	}
	u.Roles = unmarshalStrings(roles)
	return &u, nil
}

// GetUserByEmail returns a user by gmail/email field.
func (s *Store) GetUserByEmail(email string) (*models.User, bool) {
	u, err := scanUser(s.db.QueryRow(
		"SELECT id, phone, gmail, name, avatar_url, push_token, roles, active, created_at FROM users WHERE gmail=$1", email))
	if err != nil {
		return nil, false
	}
	return u, true
}

// GetUser returns a core user (no hydrated profiles).
func (s *Store) GetUser(userID string) (*models.User, bool) {
	u, err := scanUser(s.db.QueryRow(
		"SELECT id, phone, gmail, name, avatar_url, push_token, roles, active, created_at FROM users WHERE id=$1", userID))
	if err != nil {
		return nil, false
	}
	return u, true
}

// UserView returns a fully hydrated user (profiles + wallet), for GET /me.
func (s *Store) UserView(userID string) (*models.User, bool) {
	u, ok := s.GetUser(userID)
	if !ok {
		return nil, false
	}
	if wp, ok := s.GetWorkerByUser(userID); ok {
		u.WorkerProfile = wp
	}
	if cp, ok := s.getCompanyByUser(userID); ok {
		u.CompanyProfile = cp
	}
	if w := s.getWallet(userID); w != nil {
		u.Wallet = w
	}
	return u, true
}

// GetOrCreateUserByPhone finds or creates a customer account.
func (s *Store) GetOrCreateUserByPhone(phone string) (*models.User, bool) {
	phone = normalizePhone(phone)
	u, err := scanUser(s.db.QueryRow(
		"SELECT id, phone, gmail, name, avatar_url, push_token, roles, active, created_at FROM users WHERE phone=$1", phone))
	if err == nil {
		return u, false
	}
	newID := id.New()
	s.db.Exec("INSERT INTO users (id, phone, roles) VALUES ($1, $2, $3)",
		newID, phone, `["customer"]`)
	s.db.Exec("INSERT INTO wallets (user_id, min_balance) VALUES ($1, $2)", newID, s.settings.MinBalance)
	u, _ = s.GetUser(newID)
	return u, true
}

// GetUserByPhone returns a user by phone number.
func (s *Store) GetUserByPhone(phone string) (*models.User, bool) {
	phone = normalizePhone(phone)
	u, err := scanUser(s.db.QueryRow(
		"SELECT id, phone, gmail, name, avatar_url, push_token, roles, active, created_at FROM users WHERE phone=$1", phone))
	if err != nil {
		return nil, false
	}
	return u, true
}

// GetPasswordHash returns the stored password hash for a user.
func (s *Store) GetPasswordHash(userID string) string {
	var hash string
	s.db.QueryRow("SELECT password_hash FROM users WHERE id=$1", userID).Scan(&hash)
	return hash
}

// SetUserName updates the display name for a user.
func (s *Store) SetUserName(userID, name string) {
	s.db.Exec("UPDATE users SET name=$1 WHERE id=$2", name, userID)
}

// SetPasswordHash updates the password hash for a user.
func (s *Store) SetPasswordHash(userID, hash string) {
	s.db.Exec("UPDATE users SET password_hash=$1 WHERE id=$2", hash, userID)
}

// ---- Push Token -------------------------------------------------------------

func (s *Store) SetPushToken(userID, token string) error {
	res, err := s.db.Exec("UPDATE users SET push_token=$1 WHERE id=$2", token, userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetDeviceToken stores the device's own FCM registration token — the one that
// lets the server send a notification Android renders without waking the app.
//
// Cleared from any other account first: a phone that gets handed over, or an
// account switched on a shared device, leaves the token pointing at whoever
// signed in last. Without this, the previous owner keeps getting the new one's
// jobs, on a device that is no longer theirs.
func (s *Store) SetDeviceToken(userID, token string) error {
	if token == "" {
		return nil
	}
	s.db.Exec("UPDATE users SET fcm_token='' WHERE fcm_token=$1 AND id<>$2", token, userID)
	res, err := s.db.Exec("UPDATE users SET fcm_token=$1 WHERE id=$2", token, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetDeviceToken returns the user's FCM registration token, or "".
func (s *Store) GetDeviceToken(userID string) string {
	var token string
	s.db.QueryRow("SELECT fcm_token FROM users WHERE id=$1", userID).Scan(&token)
	return token
}

// ClearDeviceToken forgets an FCM token Firebase has reported as unregistered.
func (s *Store) ClearDeviceToken(token string) {
	if token == "" {
		return
	}
	s.db.Exec("UPDATE users SET fcm_token='' WHERE fcm_token=$1", token)
}

// ClearPushToken forgets a token Expo has told us is dead, whoever holds it.
//
// Keyed by the token and not by the user on purpose: the report comes back from
// Expo asynchronously, long after the request that sent the push, and the token
// is the only thing that identifies the device. Reinstalling the app issues a
// new token and leaves the old row pointing at nothing.
func (s *Store) ClearPushToken(token string) {
	if token == "" {
		return
	}
	s.db.Exec("UPDATE users SET push_token='' WHERE push_token=$1", token)
}

func (s *Store) GetPushToken(userID string) string {
	var token string
	s.db.QueryRow("SELECT push_token FROM users WHERE id=$1", userID).Scan(&token)
	return token
}

// ---- Avatar -----------------------------------------------------------------

func (s *Store) SetAvatarURL(userID, url string) (*models.User, error) {
	res, err := s.db.Exec("UPDATE users SET avatar_url=$1 WHERE id=$2", url, userID)
	if err != nil {
		return nil, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil, ErrNotFound
	}
	u, _ := s.GetUser(userID)
	return u, nil
}

// ---- Refresh Tokens ---------------------------------------------------------

func (s *Store) StoreRefreshToken(token, userID string) {
	s.db.Exec("INSERT INTO refresh_tokens (token, user_id) VALUES ($1, $2) ON CONFLICT (token) DO UPDATE SET user_id=$2", token, userID)
}

func (s *Store) ValidateRefreshToken(token string) (string, bool) {
	var uid string
	err := s.db.QueryRow("SELECT user_id FROM refresh_tokens WHERE token=$1", token).Scan(&uid)
	if err != nil {
		return "", false
	}
	return uid, true
}

func (s *Store) RevokeRefreshToken(token string) {
	s.db.Exec("DELETE FROM refresh_tokens WHERE token=$1", token)
}

func (s *Store) RevokeAllRefreshTokens(userID string) {
	s.db.Exec("DELETE FROM refresh_tokens WHERE user_id=$1", userID)
}

// ---- OTP --------------------------------------------------------------------

// OTPTTL is how long a verification code stays usable. Short enough that an
// intercepted SMS goes stale quickly, long enough to read one off a second
// handset and type it in.
const OTPTTL = 2 * time.Minute

// SetOTP stores the code for a phone, replacing any previous one and restarting
// its two-minute clock.
func (s *Store) SetOTP(phone, code string) {
	s.db.Exec(`INSERT INTO otps (phone, code, created_at) VALUES ($1, $2, NOW())
		ON CONFLICT (phone) DO UPDATE SET code=$2, created_at=NOW()`, phone, code)
}

// VerifyOTP consumes the code for a phone and reports whether it was valid and
// still inside OTPTTL.
//
// Check and delete are a single statement on purpose: two requests racing with
// the same code can't both come away thinking they won it, and expiry is judged
// by the database clock so it can't drift from the issue time written by SetOTP.
func (s *Store) VerifyOTP(phone, code string) bool {
	var consumed string
	err := s.db.QueryRow(`DELETE FROM otps
		WHERE phone=$1 AND code=$2 AND created_at > NOW() - make_interval(secs => $3)
		RETURNING phone`, phone, code, OTPTTL.Seconds()).Scan(&consumed)
	return err == nil
}

// Categories live in categories.go — the list is admin-editable, which is more
// than a pair of readers' worth of rules.

// ---- Notifications ----------------------------------------------------------

func (s *Store) AddNotification(n *models.Notification) *models.Notification {
	n.ID = id.New()
	n.CreatedAt = time.Now()
	data := "{}"
	if n.Data != nil {
		b, _ := json.Marshal(n.Data)
		data = string(b)
	}
	s.db.Exec("INSERT INTO notifications (id, user_id, type, title, body, data, read, created_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)",
		n.ID, n.UserID, n.Type, n.Title, n.Body, data, n.Read, n.CreatedAt)
	return n
}

// MarkNotificationsRead clears the unread flag. With no ids the whole inbox is
// marked — which is what opening the list means.
func (s *Store) MarkNotificationsRead(userID string, ids []string) {
	if len(ids) == 0 {
		s.db.Exec("UPDATE notifications SET read=TRUE WHERE user_id=$1 AND read=FALSE", userID)
		return
	}
	for _, nid := range ids {
		s.db.Exec("UPDATE notifications SET read=TRUE WHERE user_id=$1 AND id=$2", userID, nid)
	}
}

func (s *Store) ListNotifications(userID string, since time.Time) []models.Notification {
	var rows *sql.Rows
	var err error
	if since.IsZero() {
		rows, err = s.db.Query(
			"SELECT id, user_id, type, title, body, data, read, created_at FROM notifications WHERE user_id=$1 ORDER BY created_at DESC",
			userID)
	} else {
		rows, err = s.db.Query(
			"SELECT id, user_id, type, title, body, data, read, created_at FROM notifications WHERE user_id=$1 AND created_at>$2 ORDER BY created_at DESC",
			userID, since)
	}
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []models.Notification
	for rows.Next() {
		var n models.Notification
		var data string
		rows.Scan(&n.ID, &n.UserID, &n.Type, &n.Title, &n.Body, &data, &n.Read, &n.CreatedAt)
		var parsed any
		if json.Unmarshal([]byte(data), &parsed) == nil {
			n.Data = parsed
		}
		out = append(out, n)
	}
	return out
}

// ---- Wallet -----------------------------------------------------------------

func (s *Store) getWallet(userID string) *models.Wallet {
	var w models.Wallet
	err := s.db.QueryRow("SELECT balance, credit_limit, currency, min_balance FROM wallets WHERE user_id=$1", userID).
		Scan(&w.Balance, &w.CreditLimit, &w.Currency, &w.MinBalance)
	if err != nil {
		return nil
	}
	return &w
}

// ---- Settings ---------------------------------------------------------------

func (s *Store) GetSettings() models.Settings {
	return s.settings
}

func (s *Store) UpdateSettings(p SettingsPatch) models.Settings {
	save := func(key string, v any) {
		s.db.Exec("INSERT INTO settings (key, value) VALUES ($1, $2) ON CONFLICT (key) DO UPDATE SET value=$2",
			key, marshalJSON(v))
	}
	if p.CommissionPercent != nil {
		s.settings.CommissionPercent = *p.CommissionPercent
		save("commission_percent", *p.CommissionPercent)
	}
	if p.MinBalance != nil {
		s.settings.MinBalance = *p.MinBalance
		save("min_balance", *p.MinBalance)
	}
	// A negative free-order limit would mean "owes us jobs"; zero is legitimate
	// and means the trial is switched off for everybody signed up from now on.
	if p.FreeOrderLimit != nil && *p.FreeOrderLimit >= 0 {
		s.settings.FreeOrderLimit = *p.FreeOrderLimit
		save("free_order_limit", *p.FreeOrderLimit)
	}
	if p.CompanyMonthlyPrice != nil && *p.CompanyMonthlyPrice >= 0 {
		s.settings.CompanyMonthlyPrice = *p.CompanyMonthlyPrice
		save("company_monthly_price", *p.CompanyMonthlyPrice)
	}
	if p.CompanyDiscountPercent != nil && *p.CompanyDiscountPercent >= 0 && *p.CompanyDiscountPercent <= 100 {
		s.settings.CompanyDiscountPercent = *p.CompanyDiscountPercent
		save("company_discount_percent", *p.CompanyDiscountPercent)
	}
	if p.SubscriptionWarnDays != nil && *p.SubscriptionWarnDays >= 0 {
		s.settings.SubscriptionWarnDays = *p.SubscriptionWarnDays
		save("subscription_warn_days", *p.SubscriptionWarnDays)
	}
	return s.settings
}

// ---- Admin helpers ----------------------------------------------------------

func (s *Store) ListUsers(query string) []models.User {
	var rows *sql.Rows
	var err error
	if query == "" {
		rows, err = s.db.Query("SELECT id, phone, gmail, name, avatar_url, push_token, roles, active, created_at FROM users ORDER BY created_at DESC LIMIT 100")
	} else {
		like := "%" + query + "%"
		rows, err = s.db.Query("SELECT id, phone, gmail, name, avatar_url, push_token, roles, active, created_at FROM users WHERE phone ILIKE $1 OR name ILIKE $1 OR gmail ILIKE $1 ORDER BY created_at DESC LIMIT 100", like)
	}
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []models.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err == nil {
			out = append(out, *u)
		}
	}
	// Hydrate the profiles + wallet: the admin screens filter and act on them,
	// and a bare user row can't tell a worker from a customer.
	for i := range out {
		if wp, ok := s.GetWorkerByUser(out[i].ID); ok {
			out[i].WorkerProfile = wp
		}
		if cp, ok := s.getCompanyByUser(out[i].ID); ok {
			out[i].CompanyProfile = cp
		}
		if w := s.getWallet(out[i].ID); w != nil {
			out[i].Wallet = w
		}
	}
	return out
}

// ---- Payments ---------------------------------------------------------------

// CreatePayment records money that has already changed hands — an admin
// crediting an account, a subscription sold at the counter. Written as settled,
// because there is nothing left to wait for.
//
// Money arriving through the gateway does not come this way: it goes through
// OpenPayment and SettlePayment in payment.go, which is where the once-only
// guarantee lives.
func (s *Store) CreatePayment(userID, provider string, amount int64, purpose string) *models.Payment {
	if purpose == "" {
		purpose = models.PurposeWallet
	}
	now := time.Now()
	p := &models.Payment{
		ID:        id.New(),
		UserID:    userID,
		Provider:  provider,
		Amount:    amount,
		Status:    models.PaymentSuccess,
		Purpose:   purpose,
		PaidAt:    &now,
		CreatedAt: now,
	}
	s.db.Exec(`INSERT INTO payments (id, user_id, provider, amount, status, created_at, purpose, paid_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		p.ID, p.UserID, p.Provider, p.Amount, p.Status, p.CreatedAt, p.Purpose, now)
	return p
}

func (s *Store) ListPayments() []models.Payment {
	rows, err := s.db.Query("SELECT " + paymentCols + " FROM payments ORDER BY created_at DESC LIMIT 500")
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []models.Payment
	for rows.Next() {
		if p, err := scanPayment(rows); err == nil {
			out = append(out, *p)
		}
	}
	return out
}

// ---- helpers ----------------------------------------------------------------

func containsStr(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}

func (s *Store) addRoleInternal(userID, role string) {
	u, ok := s.GetUser(userID)
	if !ok {
		return
	}
	if !u.HasRole(role) {
		u.Roles = append(u.Roles, role)
		s.db.Exec("UPDATE users SET roles=$1 WHERE id=$2", marshalJSON(u.Roles), userID)
	}
}

// sortOrdersDesc sorts orders by created_at descending.
func sortOrdersDesc(orders []models.Order) {
	sort.Slice(orders, func(i, j int) bool { return orders[i].CreatedAt.After(orders[j].CreatedAt) })
}
