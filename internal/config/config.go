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

		DBHost:     env("DB_HOST", "localhost"),
		DBPort:     env("DB_PORT", "5432"),
		DBUser:     env("DB_USER", "prostaff"),
		DBPassword: env("DB_PASSWORD", "prostaff"),
		DBName:     env("DB_NAME", "prostaff"),
		DBSSLMode:  env("DB_SSLMODE", "disable"),

		AdminEmail:    env("ADMIN_EMAIL", "admin@prostaff.uz"),
		AdminPassword: env("ADMIN_PASSWORD", "admin123"),
		AdminPhone:    env("ADMIN_PHONE", "998900000000"),

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
