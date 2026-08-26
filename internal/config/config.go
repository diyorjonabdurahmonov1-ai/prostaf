// Package config loads runtime configuration from environment variables.
// All values have sensible defaults so the server runs with zero setup.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config holds all runtime settings.
type Config struct {
	Port              string
	JWTSecret         string
	DevMode           bool
	CommissionPercent int
	MinBalance        int64

	// FreeOrderLimit is how many jobs a brand-new provider may accept before the
	// money rules apply — three, so somebody the office just signed up can try
	// the app out with an empty wallet. Counted per identity document (a worker's
	// passport, a firm's INN), never per account, so deleting the profile and
	// opening it again does not hand out three more.
	FreeOrderLimit int
	// CompanyMonthlyPrice is the default price of one month of a firm's
	// subscription. A firm does not pay commission per job — it pays for months,
	// and the admin sets what a month costs.
	CompanyMonthlyPrice int64
	// CompanyDiscountPercent is the discount offered on a subscription by
	// default. The admin can override it on each payment.
	CompanyDiscountPercent int
	// SubscriptionWarnDays is how many days before a subscription runs out the
	// firm is warned.
	SubscriptionWarnDays int

	// UploadDir is the directory uploaded images are written to. Empty keeps
	// them in Postgres as BYTEA, which is right on a host whose filesystem is
	// wiped every deploy and wrong on a VPS: there the photos are the fastest
	// growing thing in the database and the slowest part of every backup.
	//
	// Set it to a path on a disk that survives a restart (a Docker volume, not
	// a directory inside the container) and new uploads go there. Rows already
	// carrying bytes keep being served from the database, so turning this on
	// breaks nothing; UploadMigrate moves them across.
	UploadDir string
	// UploadMigrate moves the images still held in the database onto UploadDir
	// once, at startup. Off by default because it rewrites rows, and a rewrite
	// is not something a server should do to a database because it restarted.
	UploadMigrate bool
	// MaxUploadBytes caps one uploaded image. A picker photo at 0.6 quality is
	// well under a megabyte; the cap is what stops a client that forgot to
	// compress — or one that is not a client at all — from filling the disk.
	MaxUploadBytes int64

	// PurgeOrderPhotos deletes a job's photos once the job is over.
	//
	// Order photos are the only thing on the platform that grows without a
	// ceiling: every job adds up to six of them and nothing ever removed one. A
	// profile photo is different — it is read every time somebody looks at the
	// provider, for as long as the account exists — so nothing here touches an
	// avatar or a portfolio image. Only the photos of the job itself, and only
	// once the job is completed, cancelled or expired.
	PurgeOrderPhotos bool
	// OrderPhotoGraceDays holds a finished job's photos back for this many days
	// before deleting them. Zero deletes them as the job closes.
	//
	// The reason to set it: a photo is the only evidence of what the job looked
	// like, and a customer disputing the work a day later has nothing to show
	// the office once it is gone. Days rather than hours because that is the
	// timescale a complaint actually arrives on.
	OrderPhotoGraceDays int

	// Database (individual fields assembled into a DSN).
	DBHost     string
	DBPort     string
	DBUser     string
	DBPassword string
	DBName     string
	DBSSLMode  string

	// DBMaxOpenConns caps how many connections the pool opens to Postgres.
	//
	// Go's default is unlimited, which is a memory bomb on a small server: each
	// connection is a backend process on the Postgres side with a work_mem
	// allowance of its own, so a burst of traffic turns into hundreds of them and
	// the box runs out of RAM long before it runs out of CPU. Capped, the burst
	// queues in Go instead — slower for a moment, and still serving.
	//
	// Keep it below the server's own max_connections, which also has to leave
	// room for psql and pg_dump.
	DBMaxOpenConns int
	// DBMaxIdleConns is how many of those are kept open between requests. Go's
	// default is 2, which on a busy server means constantly reconnecting.
	DBMaxIdleConns int

	// SMSHourlyBudget caps how many verification messages the platform will
	// send in an hour, all callers together. The per-phone cooldown already
	// stops one number being milked; this is what stops a generated list of a
	// thousand numbers from being milked one message each. Set it a little above
	// a busy hour of real sign-ups. Zero removes the cap.
	SMSHourlyBudget int
	// RateLimitPerMin is how many requests one signed-in account may make per
	// minute, bursting to a minute's worth at once. It is deliberately not a
	// per-address limit: carriers here NAT whole cities behind one IP, so
	// counting by address locks out subscribers rather than attackers. Zero
	// switches it off.
	RateLimitPerMin int

	// Admin seed account.
	AdminEmail    string
	AdminPassword string
	AdminPhone    string

	// SeedDemo fills an empty platform with test workers and firms — see
	// internal/store/demo.go. Off by default, and deliberately not tied to
	// DevMode: that one defaults on, and inventing bookable providers on a real
	// database is not something a default should do.
	SeedDemo bool

	// Atmos payment gateway. With ATMOS_BASE_URL empty the server behaves as it
	// did before there was a gateway: DEV_MODE credits a top-up straight away,
	// and outside DEV_MODE a top-up is refused rather than silently pretending.
	//
	// Paths and the signature recipe are per-merchant, so they live here rather
	// than compiled in — see internal/atmos for what to confirm against your own
	// documentation.
	AtmosBaseURL        string
	AtmosCheckoutURL    string
	AtmosConsumerKey    string
	AtmosConsumerSecret string
	AtmosStoreID        string
	AtmosTerminalID     string
	// AtmosAPIKey is the key callbacks are signed with — a different secret from
	// the consumer one, out of the merchant cabinet.
	AtmosAPIKey string
	// AtmosRedirectURL is the deep link Atmos returns the payer's browser to.
	AtmosRedirectURL string
	// AtmosCallbackIPs optionally narrows who may post a callback, as a
	// comma-separated list of CIDRs. Atmos documents its own range as
	// 92.63.207.0/24 and asks merchants to accept only it.
	//
	// Empty means "anyone may post", which is not the hole it sounds like: the
	// signature is the control that actually protects the endpoint, and it is
	// checked either way. This is a second lock, and it is off by default
	// because a wrong entry here is worse than no entry — it would refuse the
	// real gateway silently, and the symptom is money that moves at Atmos and
	// never arrives. Turn it on once the log has shown you the address that
	// callbacks genuinely come from.
	AtmosCallbackIPs string

	// Firebase Cloud Messaging, for push that survives the app being closed.
	//
	// With neither of these set the server keeps sending through Expo exactly as
	// before — which works while the app is running and shows nothing once it is
	// not, because Expo hands Android a data-only message. Supply the Firebase
	// service account key (Console → Project settings → Service accounts →
	// Generate new private key) either as a path or as the JSON itself, and the
	// server sends a real notification payload that Android renders on its own.
	FCMServiceAccountFile string
	FCMServiceAccountJSON string

	// Eskiz SMS provider.
	EskizBaseURL  string
	EskizToken    string
	EskizEmail    string
	EskizPassword string
	EskizSender   string

	// CORS.
	AllowedOrigins []string
}

// DatabaseURL builds a PostgreSQL DSN from the individual DB_* fields.
func (c Config) DatabaseURL() string {
	return fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=%s",
		c.DBUser, c.DBPassword, c.DBHost, c.DBPort, c.DBName, c.DBSSLMode)
}

// Load reads configuration from the environment, applying defaults.
func Load() Config {
	origins := env("ALLOWED_ORIGINS", "http://localhost:3000,http://localhost:5173")
	var originList []string
	for _, o := range strings.Split(origins, ",") {
		o = strings.TrimSpace(o)
		if o != "" {
			originList = append(originList, o)
		}
	}

	return Config{
		Port:              env("PORT", "8000"),
		JWTSecret:         env("JWT_SECRET", "prostaff-dev-secret-change-me"),
		DevMode:           envBool("DEV_MODE", true),
		CommissionPercent: envInt("COMMISSION_PERCENT", 10),
		MinBalance:        int64(envInt("MIN_BALANCE", 10000)),

		FreeOrderLimit:         envInt("FREE_ORDER_LIMIT", 3),
		CompanyMonthlyPrice:    int64(envInt("COMPANY_MONTHLY_PRICE", 300000)),
		CompanyDiscountPercent: envInt("COMPANY_DISCOUNT_PERCENT", 0),
		SubscriptionWarnDays:   envInt("SUBSCRIPTION_WARN_DAYS", 7),

		UploadDir:      env("UPLOAD_DIR", ""),
		UploadMigrate:  envBool("UPLOAD_MIGRATE", false),
		MaxUploadBytes: uploadCap(),

		PurgeOrderPhotos:    envBool("PURGE_ORDER_PHOTOS", true),
		OrderPhotoGraceDays: max(envInt("ORDER_PHOTO_GRACE_DAYS", 0), 0),

		DBHost:     env("DB_HOST", "localhost"),
		DBPort:     env("DB_PORT", "5432"),
		DBUser:     env("DB_USER", "prostaff"),
		DBPassword: env("DB_PASSWORD", "prostaff"),
		DBName:     env("DB_NAME", "prostaff"),
		DBSSLMode:  env("DB_SSLMODE", "disable"),

		// Sized for the smallest server the platform is expected to run on: a
		// 4 GB VPS with Postgres in a container next to the API. Thirty
		// connections is well past what the request rate needs and well under
		// what the memory allows.
		DBMaxOpenConns: max(envInt("DB_MAX_OPEN_CONNS", 30), 1),
		DBMaxIdleConns: max(envInt("DB_MAX_IDLE_CONNS", 5), 1),

		SMSHourlyBudget: max(envInt("SMS_HOURLY_BUDGET", 300), 0),
		RateLimitPerMin: max(envInt("RATE_LIMIT_PER_MIN", 240), 0),

		AdminEmail:    env("ADMIN_EMAIL", "admin@prostaff.uz"),
		AdminPassword: env("ADMIN_PASSWORD", "admin123"),
		AdminPhone:    env("ADMIN_PHONE", "998900000000"),

		SeedDemo: envBool("SEED_DEMO", false),

		AtmosBaseURL:        env("ATMOS_BASE_URL", ""),
		AtmosCheckoutURL:    env("ATMOS_CHECKOUT_URL", "https://checkout.pays.uz/invoice/get"),
		AtmosConsumerKey:    env("ATMOS_CONSUMER_KEY", ""),
		AtmosConsumerSecret: env("ATMOS_CONSUMER_SECRET", ""),
		AtmosStoreID:        env("ATMOS_STORE_ID", ""),
		AtmosTerminalID:     env("ATMOS_TERMINAL_ID", ""),
		AtmosAPIKey:         env("ATMOS_API_KEY", ""),
		AtmosRedirectURL:    env("ATMOS_REDIRECT_URL", ""),
		AtmosCallbackIPs:    env("ATMOS_CALLBACK_IPS", ""),

		FCMServiceAccountFile: env("FCM_SERVICE_ACCOUNT_FILE", ""),
		FCMServiceAccountJSON: env("FCM_SERVICE_ACCOUNT_JSON", ""),

		EskizBaseURL:  env("ESKIZ_BASE_URL", ""),
		EskizToken:    env("ESKIZ_TOKEN", ""),
		EskizEmail:    env("ESKIZ_EMAIL", ""),
		EskizPassword: env("ESKIZ_PASSWORD", ""),
		// 4546 is Eskiz's shared sender id, the only one that works without
		// per-account approval. An alphanumeric sender ("PROSTAFF") has to be
		// registered in my.eskiz.uz first or every message is rejected.
		EskizSender: env("ESKIZ_SENDER", "4546"),

		AllowedOrigins: originList,
	}
}

// uploadCap reads MAX_UPLOAD_MB into a byte count.
//
// Zero and negative are rejected rather than honoured: `MAX_UPLOAD_MB=0` in a
// compose file is somebody blanking a value they did not need, and taking it
// literally would refuse every photo on the platform with "invalid request".
func uploadCap() int64 {
	mb := envInt("MAX_UPLOAD_MB", 3)
	if mb <= 0 {
		mb = 3
	}
	return int64(mb) << 20
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func envInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
