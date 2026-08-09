// Package models defines the entity shapes exchanged over the REST API.
// JSON field names mirror the shared API contract exactly.
package models

import "time"

// Roles.
const (
	RoleCustomer = "customer"
	RoleWorker   = "worker"
	RoleCompany  = "company"
	RoleAdmin    = "admin"
)

// Order statuses.
const (
	StatusPending    = "pending"
	StatusMatching   = "matching"
	StatusInvited    = "invited"
	StatusAccepted   = "accepted"
	StatusInProgress = "in_progress"
	StatusCompleted  = "completed"
	StatusCancelled  = "cancelled"
	StatusExpired    = "expired"
)

// User is an account. worker_profile / company_profile / wallet are hydrated
// on read (GET /me) and omitted otherwise.
type User struct {
	ID             string          `json:"id"`
	Phone          string          `json:"phone"`
	Gmail          string          `json:"gmail,omitempty"`
	Name           string          `json:"name,omitempty"`
	AvatarURL      string          `json:"avatar_url,omitempty"`
	Roles          []string        `json:"roles"`
	Active         bool            `json:"active"`
	PushToken      string          `json:"push_token,omitempty"`
	WorkerProfile  *WorkerProfile  `json:"worker_profile,omitempty"`
	CompanyProfile *CompanyProfile `json:"company_profile,omitempty"`
	Wallet         *Wallet         `json:"wallet,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
}

// HasRole reports whether the user holds the given role.
func (u *User) HasRole(role string) bool {
	for _, r := range u.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// Category is a worker service category (seeded).
type Category struct {
	ID     string `json:"id"`
	Key    string `json:"key"`
	NameUz string `json:"name_uz"`
	NameRu string `json:"name_ru"`
	NameEn string `json:"name_en"`
	Icon   string `json:"icon"`
}

// WorkerProfile is a worker's public + operational data.
//
// Everything from AvatarURL down is what a customer reads before calling
// somebody to their home: a face, a few words, how long they have done the job
// and where they are based. It is presentational only — matchmaking still runs
// on Lat/Lng, CategoryIDs and Online.
type WorkerProfile struct {
	ID           string   `json:"id"`
	UserID       string   `json:"user_id"`
	FullName     string   `json:"full_name"`
	Phone        string   `json:"phone"`
	Gender       string   `json:"gender"`
	Age          int      `json:"age"`
	PriceMin     int64    `json:"price_min"`
	PriceMax     int64    `json:"price_max"`
	Lat          float64  `json:"lat"`
	Lng          float64  `json:"lng"`
	CategoryIDs  []string `json:"category_ids"`
	Online       bool     `json:"online"`
	Banned       bool     `json:"banned"`
	CreditLimit  int64    `json:"credit_limit"`
	Rating       float64  `json:"rating"`
	RatingsCount int      `json:"ratings_count"`
	// AvatarURL is the worker's own photo, separate from the account avatar:
	// the person a customer is about to let into their home is the profile
	// they are looking at, not whatever picture the account holder set.
	AvatarURL string `json:"avatar_url"`
	// About is a short self-description shown on the public profile.
	About string `json:"about"`
	// ExperienceYears is how long they have worked in these categories.
	ExperienceYears int `json:"experience_years"`
	// Address is a human-readable base ("Chilonzor, Toshkent"), for people who
	// read a district name faster than a pin on a map.
	Address string `json:"address"`
	// Images is a small portfolio of previous work.
	Images []string `json:"images"`
	// DistanceKm is populated only in matchmaking results.
	DistanceKm *float64 `json:"distance_km,omitempty"`
	// PassportSeries is the worker's passport (AA1234567), taken by the office
	// when the account is opened and unique across the platform.
	//
	// It is not presentation and it is not matchmaking: it is the identity the
	// free-trial counter below is filed under, so three free jobs stay three
	// however many times a profile is deleted and opened again. Stripped from
	// every public read — a customer browsing workers has no business with it.
	PassportSeries string `json:"passport_series,omitempty"`
	// FreeOrdersUsed / FreeOrdersLimit are the trial jobs: a new worker may
	// accept FreeOrdersLimit of them with an empty wallet, after which the
	// balance rules apply as normal.
	FreeOrdersUsed  int       `json:"free_orders_used"`
	FreeOrdersLimit int       `json:"free_orders_limit"`
	CreatedAt       time.Time `json:"created_at"`
}

// FreeOrdersLeft is how many trial jobs the worker still has.
//
// None at all without a passport series, and that is not a detail — it is the
// same rule the ledger already enforced, finally stated in the one place that
// decides anything.
//
// The trial is counted against the document, not the profile, so that deleting
// a worker and opening them again does not hand out three more jobs. A profile
// with no passport therefore has no ledger row and can never have one: its
// `used` count reads 0 for ever. This method used to subtract that permanent 0
// from the limit and answer "three left" every single time, which is how a
// legacy worker ended up with an unlimited trial — the acceptance gate saw a
// free job available and never once looked at the wallet, so the balance ran as
// far negative as the worker cared to take it, while the counter on their panel
// sat at three of three because nothing was ever written down.
func (wp *WorkerProfile) FreeOrdersLeft() int {
	if wp.PassportSeries == "" {
		return 0
	}
	if n := wp.FreeOrdersLimit - wp.FreeOrdersUsed; n > 0 {
		return n
	}
	return 0
}

// CompanyProfile is a service company's data.
type CompanyProfile struct {
	ID          string   `json:"id"`
	UserID      string   `json:"user_id"`
	Name        string   `json:"name"`
	Phone       string   `json:"phone"`
	Description string   `json:"description"`
	Images      []string `json:"images"`
	// Price is the company's service fee in so'm. Required — customers order at
	// this price, they do not name their own.
	Price int64 `json:"price"`
	// CategoryIDs is what the firm actually does. A company is never broadcast
	// to the way a worker is, so this does not gate dispatch — it is how a
	// customer browsing by service finds the firm at all.
	CategoryIDs []string `json:"category_ids"`
	// Lat/Lng is where the office is. Same purpose as Address, for the half of
	// customers who read a pin faster than a street name, and what the nearby
	// filter on GET /companies measures from.
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
	// DistanceKm is how far the office is from the customer asking. Set only by
	// the nearby listing — a profile fetched on its own has nobody to measure
	// from — and left out for a firm that never dropped a pin.
	DistanceKm   *float64 `json:"distance_km,omitempty"`
	Online       bool     `json:"online"`
	Banned       bool     `json:"banned"`
	Rating       float64  `json:"rating"`
	RatingsCount int      `json:"ratings_count"`
	// AvatarURL is the firm's logo. Images stays the work gallery.
	AvatarURL string `json:"avatar_url"`
	// Address is where the office is, in words.
	Address string `json:"address"`
	// WorkingHours is free text ("Dush–Shan 9:00–18:00") rather than a
	// structured schedule: firms describe their hours in their own words and a
	// customer only ever reads the line.
	WorkingHours string `json:"working_hours"`
	// EmployeesCount and ExperienceYears are the two numbers customers actually
	// ask for on the phone — how big the firm is and how long it has existed.
	EmployeesCount  int `json:"employees_count"`
	ExperienceYears int `json:"experience_years"`
	// Email and Website are optional contact channels.
	Email   string `json:"email"`
	Website string `json:"website"`
	// INN is the firm's tax number, the company-side twin of a worker's passport
	// series: taken by the office, unique across the platform, and what the free
	// jobs below are counted against. Kept out of public reads.
	INN string `json:"inn,omitempty"`
	// FreeOrdersUsed / FreeOrdersLimit are the trial jobs a firm may take before
	// it has to hold a subscription.
	FreeOrdersUsed  int `json:"free_orders_used"`
	FreeOrdersLimit int `json:"free_orders_limit"`
	// SubscriptionUntil is when the firm's paid period ends; nil means it has
	// never bought one. A firm pays by the month rather than a commission per
	// job, so this date — not a wallet balance — is what lets it take work.
	SubscriptionUntil *time.Time `json:"subscription_until,omitempty"`
	// SubscriptionActive and SubscriptionDaysLeft are the same date as the app
	// needs to read it: whether the firm may accept work at all, and how long it
	// has before that stops being true. Derived on read, never stored.
	SubscriptionActive   bool      `json:"subscription_active"`
	SubscriptionDaysLeft int       `json:"subscription_days_left"`
	CreatedAt            time.Time `json:"created_at"`
}

// FreeOrdersLeft is how many trial jobs the firm still has. Nothing without an
// INN, for the reason spelled out on the worker's copy of this.
func (cp *CompanyProfile) FreeOrdersLeft() int {
	if cp.INN == "" {
		return 0
	}
	if n := cp.FreeOrdersLimit - cp.FreeOrdersUsed; n > 0 {
		return n
	}
	return 0
}

// CanTakeWork reports whether the firm is allowed to accept an order: it still
// has trial jobs, or its subscription has not run out.
func (cp *CompanyProfile) CanTakeWork() bool {
	return cp.FreeOrdersLeft() > 0 || cp.SubscriptionActive
}

// SubscriptionPlan is one tariff a firm can buy.
//
// The plans differ in length and nothing else — every subscribed firm gets the
// same platform — so the only thing a longer plan buys is a lower price per
// month. The price is therefore stored per plan rather than derived from a
// monthly rate: "three months for 200 000" is the office's number, not a
// calculation.
type SubscriptionPlan struct {
	ID     string `json:"id"`
	Months int    `json:"months"`
	// Price is the whole amount for the whole term, in so'm.
	Price int64 `json:"price"`
	// OldPrice is what the card shows struck through — presentational only, and
	// never charged. Zero means no strike-through.
	OldPrice int64 `json:"old_price,omitempty"`
	// Title is an optional label ("Yillik"); empty lets the app name the plan
	// from its length.
	Title  string `json:"title,omitempty"`
	Active bool   `json:"active"`
	Sort   int    `json:"sort"`
	// PerMonth and DiscountPercent are derived on read: what the plan works out
	// to per month, and how much less that is than the old price. Both exist so
	// the card does not have to do arithmetic the server can do once.
	PerMonth        int64     `json:"per_month"`
	DiscountPercent int       `json:"discount_percent"`
	CreatedAt       time.Time `json:"created_at"`
}

// Derive fills in the computed fields. Called on every read, so a plan whose
// price the office just changed reports the right discount immediately.
func (p *SubscriptionPlan) Derive() {
	if p.Months > 0 {
		p.PerMonth = p.Price / int64(p.Months)
	}
	p.DiscountPercent = 0
	if p.OldPrice > p.Price && p.OldPrice > 0 {
		p.DiscountPercent = int((p.OldPrice - p.Price) * 100 / p.OldPrice)
	}
}

// CompanySubscription is one payment for months of access, with the price and
// discount that were in force the day it was taken.
type CompanySubscription struct {
	ID              string    `json:"id"`
	CompanyID       string    `json:"company_id"`
	Months          int       `json:"months"`
	PricePerMonth   int64     `json:"price_per_month"`
	DiscountPercent int       `json:"discount_percent"`
	Amount          int64     `json:"amount"`
	StartsAt        time.Time `json:"starts_at"`
	EndsAt          time.Time `json:"ends_at"`
	CreatedAt       time.Time `json:"created_at"`
}

// Order is a job request.
type Order struct {
	ID            string `json:"id"`
	CustomerID    string `json:"customer_id"`
	WorkerID      string `json:"worker_id,omitempty"`
	CompanyID     string `json:"company_id,omitempty"`
	CategoryID    string `json:"category_id"`
	Gender        string `json:"gender"`
	Description string `json:"description"`
	// ImageURL is the first photo, kept for apps built before an order could
	// carry more than one. Always the head of Images when there is any.
	ImageURL string `json:"image_url,omitempty"`
	// Images is every photo the customer attached — often a better description
	// of the job than the words are.
	Images        []string `json:"images"`
	CustomerName  string   `json:"customer_name,omitempty"`
	CustomerPhone string   `json:"customer_phone"`
	// Price is what the job pays, in so'm. The customer sets it when calling
	// workers; for a company order it is copied from the company's service fee.
	Price       int64   `json:"price"`
	WorkerPhone string  `json:"worker_phone,omitempty"`
	Lat         float64 `json:"lat"`
	Lng         float64 `json:"lng"`
	Status      string  `json:"status"`
	// InvitedWorkerIDs is everyone the order has been broadcast to so far. The
	// customer does not pick from it — it is how many phones are ringing.
	InvitedWorkerIDs []string   `json:"invited_worker_ids"`
	InviteExpiresAt  *time.Time `json:"invite_expires_at,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`

	// RejectedWorkerIDs are the workers who declined; later broadcast waves skip
	// them. Internal only, never serialised.
	RejectedWorkerIDs []string `json:"-"`

	// CommissionAmount is the commission collected from the worker on accept.
	// Internal only (used for analytics/finance), never serialised.
	CommissionAmount int64 `json:"-"`
}

// Rating is a two-way review tied to an order.
type Rating struct {
	ID         string `json:"id"`
	OrderID    string `json:"order_id"`
	FromUserID string `json:"from_user_id"`
	ToUserID   string `json:"to_user_id"`
	// TargetRole is the hat the rated account was wearing on this job:
	// RoleWorker, RoleCompany or RoleCustomer.
	//
	// One account can hold all three. Without this the score had nowhere to be
	// filed: a star earned on a worker job was added to the firm's average as
	// well, and a star a firm gave its *customer* landed on that customer's own
	// worker profile. Ratings and their comments are separated by this column.
	TargetRole string    `json:"target_role"`
	Stars      int       `json:"stars"`
	Comment    string    `json:"comment,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// Review is a rating as the public profile shows it: the stars and the words,
// plus enough about the author to make them a person rather than an anonymous
// score. Ratings are stored two-way; this is the read side, and it is only ever
// served for the party being reviewed.
//
// The author's phone and account id stay out of it. A customer reading a
// worker's profile is entitled to what was said about the work, not to a
// directory of everyone who has hired them.
type Review struct {
	ID           string    `json:"id"`
	OrderID      string    `json:"order_id"`
	Stars        int       `json:"stars"`
	Comment      string    `json:"comment,omitempty"`
	AuthorName   string    `json:"author_name"`
	AuthorAvatar string    `json:"author_avatar,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// OrderCustomer is the person who placed a job, as the provider deciding
// whether to take it is allowed to see them.
//
// A worker answering an invite had nothing to answer about the other party: a
// price, a pin on a map and a sentence of description, with no way to tell a
// regular from a first-timer, or from someone three previous workers have rated
// one star. This is that missing half of the decision — and deliberately not
// the whole account: the phone number stays out until the job is actually
// theirs, which is the same rule the order itself follows.
type OrderCustomer struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	AvatarURL string `json:"avatar_url,omitempty"`
	// Phone is only filled once the provider holds the order. Before that it is
	// empty, exactly as it is on the masked order.
	Phone string `json:"phone,omitempty"`
	// Rating is the average of the stars this account has been given *as a
	// customer* — not as a worker or a firm it may also be.
	Rating       float64   `json:"rating"`
	RatingsCount int       `json:"ratings_count"`
	OrdersCount  int       `json:"orders_count"`
	MemberSince  time.Time `json:"member_since"`
}

// Wallet is a user's balance ledger snapshot.
type Wallet struct {
	Balance     int64  `json:"balance"`
	CreditLimit int64  `json:"credit_limit"`
	Currency    string `json:"currency"`
	MinBalance  int64  `json:"min_balance"`
}

// Payment purposes. What the money bought, which a callback carrying only an
// amount and an id could not otherwise tell.
const (
	PurposeWallet       = "wallet"
	PurposeSubscription = "subscription"
)

// Payment statuses.
const (
	PaymentPending  = "pending"
	PaymentSuccess  = "success"
	PaymentCanceled = "canceled"
	// PaymentFailed is a payment that could not even be opened at the gateway,
	// or one the gateway refused. Distinct from canceled, which is a person
	// changing their mind.
	PaymentFailed = "failed"
)

// Payment is one movement of money, from the moment it is opened at the gateway
// to the moment a signed callback settles it.
type Payment struct {
	ID       string `json:"id"`
	UserID   string `json:"user_id"`
	Provider string `json:"provider"`
	Amount   int64  `json:"amount"`
	Status   string `json:"status"`
	// Purpose is PurposeWallet or PurposeSubscription; Months is set only for
	// the latter, and is what gets added when the payment settles.
	Purpose string `json:"purpose"`
	Months  int    `json:"months,omitempty"`
	// ExternalID is the gateway's own transaction id — the key a callback
	// arrives under, and what makes settling it idempotent.
	ExternalID string `json:"external_id,omitempty"`
	// PayURL is the hosted invoice page for this payment. Only ever returned in
	// the response that opened it; it is not stored.
	PayURL string `json:"pay_url,omitempty"`
	// Error is what went wrong, in words, for the office to read. Set on a
	// failure and also on a payment that settled with something odd about it —
	// an amount that disagreed with what was asked for, months that were paid
	// for but could not be applied. Empty on an ordinary payment.
	Error     string     `json:"error,omitempty"`
	PaidAt    *time.Time `json:"paid_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

// Notification is a stored push message.
type Notification struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	Type      string    `json:"type"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	Data      any       `json:"data,omitempty"`
	Read      bool      `json:"read"`
	CreatedAt time.Time `json:"created_at"`
}

// Settings holds admin-tunable platform values.
type Settings struct {
	CommissionPercent int   `json:"commission_percent"`
	MinBalance        int64 `json:"min_balance"`
	// FreeOrderLimit is how many jobs a new worker or firm may accept before the
	// money rules apply.
	FreeOrderLimit int `json:"free_order_limit"`
	// CompanyMonthlyPrice is what one month of a firm's subscription costs, and
	// CompanyDiscountPercent the discount offered on it by default. Both are the
	// admin's to set; a single payment may override either.
	CompanyMonthlyPrice    int64 `json:"company_monthly_price"`
	CompanyDiscountPercent int   `json:"company_discount_percent"`
	// SubscriptionWarnDays is how long before a subscription expires the firm is
	// warned — seven, so it has a week to pay.
	SubscriptionWarnDays int `json:"subscription_warn_days"`
}

// Alert levels.
const (
	AlertInfo    = "info"
	AlertWarning = "warning"
	AlertDanger  = "danger"
)

// Alert types.
const (
	AlertTrialLeft            = "trial.left"
	AlertTrialExhausted       = "trial.exhausted"
	AlertLowBalance           = "wallet.low_balance"
	AlertSubscriptionExpiring = "subscription.expiring"
	AlertSubscriptionExpired  = "subscription.expired"
	AlertSubscriptionMissing  = "subscription.missing"
)

// Alert is a standing condition the app shows as a banner, as opposed to a
// Notification, which is one thing that happened at one moment.
//
// The difference matters: "your three free jobs are gone and your balance is
// under 10 000 so'm" is not news that arrives once, it is the state the account
// is in until money is added, and a worker who dismissed the push still has to
// see it. So it is computed on read (GET /alerts) rather than stored.
type Alert struct {
	Type  string `json:"type"`
	Level string `json:"level"`
	Title string `json:"title"`
	Body  string `json:"body"`
	// Action is what the banner's button should do — "topup" opens the wallet,
	// "subscription" points the firm at the office. Empty means "no button".
	Action string `json:"action,omitempty"`
	// Role is the hat the alert is about, so a person holding both a worker and a
	// firm profile can be shown the right banner on the right panel.
	Role string `json:"role"`
}
