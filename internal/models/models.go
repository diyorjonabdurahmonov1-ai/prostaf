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
	DistanceKm *float64  `json:"distance_km,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
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
	Email     string    `json:"email"`
	Website   string    `json:"website"`
	CreatedAt time.Time `json:"created_at"`
}

// Order is a job request.
type Order struct {
	ID            string `json:"id"`
	CustomerID    string `json:"customer_id"`
	WorkerID      string `json:"worker_id,omitempty"`
	CompanyID     string `json:"company_id,omitempty"`
	CategoryID    string `json:"category_id"`
	Gender        string `json:"gender"`
	Description   string `json:"description"`
	ImageURL      string `json:"image_url,omitempty"`
	CustomerName  string `json:"customer_name,omitempty"`
	CustomerPhone string `json:"customer_phone"`
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

// Wallet is a user's balance ledger snapshot.
type Wallet struct {
	Balance     int64  `json:"balance"`
	CreditLimit int64  `json:"credit_limit"`
	Currency    string `json:"currency"`
	MinBalance  int64  `json:"min_balance"`
}

// Payment is a top-up record.
type Payment struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	Provider  string    `json:"provider"`
	Amount    int64     `json:"amount"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
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
}
