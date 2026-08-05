// Package db manages the PostgreSQL database connection and schema migrations.
package db

import (
	"database/sql"
	"log"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Open connects to PostgreSQL and runs migrations.
// dsn example: "postgres://user:pass@localhost:5432/prostaff?sslmode=disable"
func Open(dsn string) *sql.DB {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		log.Fatalf("db: open: %v", err)
	}
	if err := db.Ping(); err != nil {
		log.Fatalf("db: ping: %v", err)
	}
	migrate(db)
	return db
}

func migrate(db *sql.DB) {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id         TEXT PRIMARY KEY,
			phone      TEXT UNIQUE NOT NULL,
			gmail      TEXT NOT NULL DEFAULT '',
			name       TEXT NOT NULL DEFAULT '',
			avatar_url TEXT NOT NULL DEFAULT '',
			push_token TEXT NOT NULL DEFAULT '',
			roles      TEXT NOT NULL DEFAULT '["customer"]',
			active     BOOLEAN NOT NULL DEFAULT TRUE,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,

		`CREATE TABLE IF NOT EXISTS categories (
			id      TEXT PRIMARY KEY,
			key     TEXT UNIQUE NOT NULL,
			name_uz TEXT NOT NULL,
			name_ru TEXT NOT NULL,
			name_en TEXT NOT NULL,
			icon    TEXT NOT NULL DEFAULT ''
		)`,

		`CREATE TABLE IF NOT EXISTS workers (
			id            TEXT PRIMARY KEY,
			user_id       TEXT UNIQUE NOT NULL REFERENCES users(id),
			full_name     TEXT NOT NULL,
			phone         TEXT NOT NULL,
			gender        TEXT NOT NULL DEFAULT 'male',
			age           INTEGER NOT NULL DEFAULT 0,
			price_min     BIGINT NOT NULL DEFAULT 0,
			price_max     BIGINT NOT NULL DEFAULT 0,
			lat           DOUBLE PRECISION NOT NULL DEFAULT 0,
			lng           DOUBLE PRECISION NOT NULL DEFAULT 0,
			category_ids  TEXT NOT NULL DEFAULT '[]',
			online        BOOLEAN NOT NULL DEFAULT FALSE,
			banned        BOOLEAN NOT NULL DEFAULT FALSE,
			credit_limit  BIGINT NOT NULL DEFAULT 0,
			rating        DOUBLE PRECISION NOT NULL DEFAULT 0,
			ratings_count INTEGER NOT NULL DEFAULT 0,
			created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,

		`CREATE TABLE IF NOT EXISTS companies (
			id            TEXT PRIMARY KEY,
			user_id       TEXT UNIQUE NOT NULL REFERENCES users(id),
			name          TEXT NOT NULL,
			phone         TEXT NOT NULL DEFAULT '',
			description   TEXT NOT NULL DEFAULT '',
			images        TEXT NOT NULL DEFAULT '[]',
			online        BOOLEAN NOT NULL DEFAULT FALSE,
			banned        BOOLEAN NOT NULL DEFAULT FALSE,
			rating        DOUBLE PRECISION NOT NULL DEFAULT 0,
			ratings_count INTEGER NOT NULL DEFAULT 0,
			created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,

		`CREATE TABLE IF NOT EXISTS wallets (
			user_id      TEXT PRIMARY KEY REFERENCES users(id),
			balance      BIGINT NOT NULL DEFAULT 0,
			credit_limit BIGINT NOT NULL DEFAULT 0,
			currency     TEXT NOT NULL DEFAULT 'UZS',
			min_balance  BIGINT NOT NULL DEFAULT 10000
		)`,

		`CREATE TABLE IF NOT EXISTS orders (
			id                 TEXT PRIMARY KEY,
			customer_id        TEXT NOT NULL REFERENCES users(id),
			worker_id          TEXT NOT NULL DEFAULT '',
			company_id         TEXT NOT NULL DEFAULT '',
			category_id        TEXT NOT NULL DEFAULT '',
			gender             TEXT NOT NULL DEFAULT 'any',
			description        TEXT NOT NULL DEFAULT '',
			image_url          TEXT NOT NULL DEFAULT '',
			customer_phone     TEXT NOT NULL DEFAULT '',
			worker_phone       TEXT NOT NULL DEFAULT '',
			lat                DOUBLE PRECISION NOT NULL DEFAULT 0,
			lng                DOUBLE PRECISION NOT NULL DEFAULT 0,
			status             TEXT NOT NULL DEFAULT 'pending',
			invited_worker_ids TEXT NOT NULL DEFAULT '[]',
			invite_expires_at  TIMESTAMPTZ,
			commission_amount  BIGINT NOT NULL DEFAULT 0,
			created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,

		`CREATE TABLE IF NOT EXISTS ratings (
			id           TEXT PRIMARY KEY,
			order_id     TEXT NOT NULL,
			from_user_id TEXT NOT NULL,
			to_user_id   TEXT NOT NULL,
			stars        INTEGER NOT NULL,
			comment      TEXT NOT NULL DEFAULT '',
			created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,

		`CREATE TABLE IF NOT EXISTS payments (
			id         TEXT PRIMARY KEY,
			user_id    TEXT NOT NULL REFERENCES users(id),
			provider   TEXT NOT NULL,
			amount     BIGINT NOT NULL,
			status     TEXT NOT NULL DEFAULT 'pending',
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,

		`CREATE TABLE IF NOT EXISTS notifications (
			id         TEXT PRIMARY KEY,
			user_id    TEXT NOT NULL REFERENCES users(id),
			type       TEXT NOT NULL DEFAULT '',
			title      TEXT NOT NULL DEFAULT '',
			body       TEXT NOT NULL DEFAULT '',
			data       TEXT NOT NULL DEFAULT '{}',
			read       BOOLEAN NOT NULL DEFAULT FALSE,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,

		`CREATE TABLE IF NOT EXISTS refresh_tokens (
			token   TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id)
		)`,

		`CREATE TABLE IF NOT EXISTS otps (
			phone TEXT PRIMARY KEY,
			code  TEXT NOT NULL
		)`,

		`CREATE TABLE IF NOT EXISTS settings (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,

		// Password column for phone+password auth (added after initial schema).
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS password_hash TEXT NOT NULL DEFAULT ''`,

		// Customer name carried on the order so a company sees who is calling
		// without a second lookup (added after initial schema).
		`ALTER TABLE orders ADD COLUMN IF NOT EXISTS customer_name TEXT NOT NULL DEFAULT ''`,

		// Money on the job: the customer names a price for worker orders, and a
		// company publishes its service fee that its orders are booked at.
		`ALTER TABLE orders ADD COLUMN IF NOT EXISTS price BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE companies ADD COLUMN IF NOT EXISTS price BIGINT NOT NULL DEFAULT 0`,

		// Broadcast dispatch: the order rings every nearby worker at once and
		// keeps ringing new arrivals until somebody takes it. Whoever declined is
		// remembered so a later wave never rings the same phone twice.
		`ALTER TABLE orders ADD COLUMN IF NOT EXISTS rejected_worker_ids TEXT NOT NULL DEFAULT '[]'`,

		// The public face of a provider. A customer picks who comes to their home
		// from these fields, so they are readable text and a photo rather than the
		// operational numbers the rest of the row carries.
		`ALTER TABLE workers ADD COLUMN IF NOT EXISTS avatar_url TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE workers ADD COLUMN IF NOT EXISTS about TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE workers ADD COLUMN IF NOT EXISTS experience_years INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE workers ADD COLUMN IF NOT EXISTS address TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE workers ADD COLUMN IF NOT EXISTS images TEXT NOT NULL DEFAULT '[]'`,

		`ALTER TABLE companies ADD COLUMN IF NOT EXISTS avatar_url TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE companies ADD COLUMN IF NOT EXISTS address TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE companies ADD COLUMN IF NOT EXISTS working_hours TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE companies ADD COLUMN IF NOT EXISTS employees_count INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE companies ADD COLUMN IF NOT EXISTS experience_years INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE companies ADD COLUMN IF NOT EXISTS email TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE companies ADD COLUMN IF NOT EXISTS website TEXT NOT NULL DEFAULT ''`,

		// Uploaded images live in the database, not on disk: the server runs on a
		// host with an ephemeral filesystem, and a photo that disappears on the
		// next deploy is worse than no photo at all. Rows are small (a picker
		// image at 0.6 quality is well under a megabyte) and read by id only.
		`CREATE TABLE IF NOT EXISTS files (
			id         TEXT PRIMARY KEY,
			user_id    TEXT NOT NULL DEFAULT '',
			mime       TEXT NOT NULL DEFAULT 'image/jpeg',
			bytes      BYTEA NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,

		// A verification code is only good for a couple of minutes; without an
		// issue time an old row would stay valid forever (added after initial
		// schema). Existing rows are treated as issued now — they are about to be
		// replaced by the next request anyway.
		`ALTER TABLE otps ADD COLUMN IF NOT EXISTS created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()`,

		// Indexes for common queries.
		`CREATE INDEX IF NOT EXISTS idx_orders_customer ON orders(customer_id)`,
		`CREATE INDEX IF NOT EXISTS idx_orders_company  ON orders(company_id)`,
		`CREATE INDEX IF NOT EXISTS idx_orders_worker   ON orders(worker_id)`,
		`CREATE INDEX IF NOT EXISTS idx_notifications_user ON notifications(user_id, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_workers_user ON workers(user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_companies_user ON companies(user_id)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			log.Fatalf("db: migrate: %v\n  sql: %s", err, s)
		}
	}
}
