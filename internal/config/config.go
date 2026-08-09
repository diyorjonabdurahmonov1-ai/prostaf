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

	// Database (individual fields assembled into a DSN).
	DBHost     string
	DBPort     string
	DBUser     string
	DBPassword string
	DBName     string
	DBSSLMode  string

	// Admin seed account.
	AdminEmail    string
	AdminPassword string
	AdminPhone    string

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

		DBHost:     env("DB_HOST", "localhost"),
		DBPort:     env("DB_PORT", "5432"),
		DBUser:     env("DB_USER", "prostaff"),
		DBPassword: env("DB_PASSWORD", "prostaff"),
		DBName:     env("DB_NAME", "prostaff"),
		DBSSLMode:  env("DB_SSLMODE", "disable"),

		AdminEmail:    env("ADMIN_EMAIL", "admin@prostaff.uz"),
		AdminPassword: env("ADMIN_PASSWORD", "admin123"),
		AdminPhone:    env("ADMIN_PHONE", "998900000000"),

		AtmosBaseURL:        env("ATMOS_BASE_URL", ""),
		AtmosCheckoutURL:    env("ATMOS_CHECKOUT_URL", "https://checkout.pays.uz/invoice/get"),
		AtmosConsumerKey:    env("ATMOS_CONSUMER_KEY", ""),
		AtmosConsumerSecret: env("ATMOS_CONSUMER_SECRET", ""),
		AtmosStoreID:        env("ATMOS_STORE_ID", ""),
		AtmosTerminalID:     env("ATMOS_TERMINAL_ID", ""),
		AtmosAPIKey:         env("ATMOS_API_KEY", ""),
		AtmosRedirectURL:    env("ATMOS_REDIRECT_URL", ""),

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
