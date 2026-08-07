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

		// A firm is described like a worker now: which services it offers and
		// where it sits. Existing rows keep 0,0 — treated as "no pin set", so a
		// firm that predates this stays visible in the nearby list.
		`ALTER TABLE companies ADD COLUMN IF NOT EXISTS category_ids TEXT NOT NULL DEFAULT '[]'`,
		`ALTER TABLE companies ADD COLUMN IF NOT EXISTS lat DOUBLE PRECISION NOT NULL DEFAULT 0`,
		`ALTER TABLE companies ADD COLUMN IF NOT EXISTS lng DOUBLE PRECISION NOT NULL DEFAULT 0`,

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

		// Accounts the office created before the role was assigned at creation
		// time carry a profile but no matching role, so the admin list files them
		// under "customer". Re-derive the role from the profile that exists.
		`UPDATE users u SET roles = (
			SELECT jsonb_agg(DISTINCT r)::text
			FROM jsonb_array_elements(u.roles::jsonb || '["worker"]'::jsonb) AS r
		)
		WHERE EXISTS (SELECT 1 FROM workers w WHERE w.user_id = u.id)
		  AND NOT (u.roles::jsonb @> '["worker"]'::jsonb)`,

		`UPDATE users u SET roles = (
			SELECT jsonb_agg(DISTINCT r)::text
			FROM jsonb_array_elements(u.roles::jsonb || '["company"]'::jsonb) AS r
		)
		WHERE EXISTS (SELECT 1 FROM companies c WHERE c.user_id = u.id)
		  AND NOT (u.roles::jsonb @> '["company"]'::jsonb)`,

		// A rating belongs to one reputation, not to an account.
		//
		// One person can be a worker, a firm and a customer at once. Without this
		// column there was nowhere to file a score: `Rate` wrote every star to
		// both the worker and the company aggregate if the account had both, a
		// firm rating its customer moved that customer's own worker average, and
		// the two review lists served the same comments to both profiles.
		`ALTER TABLE ratings ADD COLUMN IF NOT EXISTS target_role TEXT NOT NULL DEFAULT ''`,

		// Backfill from the order each rating hangs off: rated the customer →
		// 'customer', otherwise the job's own kind. Only touches rows written
		// before the column existed.
		`UPDATE ratings r SET target_role = CASE
			WHEN r.to_user_id = o.customer_id THEN 'customer'
			WHEN o.company_id <> ''           THEN 'company'
			ELSE 'worker'
		END
		FROM orders o
		WHERE o.id = r.order_id AND r.target_role = ''`,

		// A rating whose order is gone cannot be placed; treat it as a provider
		// score so it is not silently dropped from every list.
		`UPDATE ratings SET target_role = 'worker' WHERE target_role = ''`,

		// Rebuild both aggregates from the ratings themselves. The stored numbers
		// were accumulated by the double-write above, so an account holding two
		// profiles carries an average of jobs it did under the other hat — no
		// incremental fix reaches that, only a recount. Idempotent, and cheap at
		// this size: after this the column is simply the truth about the table.
		`UPDATE workers w SET
			rating = COALESCE((SELECT AVG(r.stars) FROM ratings r
				WHERE r.to_user_id = w.user_id AND r.target_role = 'worker'), 0),
			ratings_count = (SELECT COUNT(*) FROM ratings r
				WHERE r.to_user_id = w.user_id AND r.target_role = 'worker')`,

		`UPDATE companies c SET
			rating = COALESCE((SELECT AVG(r.stars) FROM ratings r
				WHERE r.to_user_id = c.user_id AND r.target_role = 'company'), 0),
			ratings_count = (SELECT COUNT(*) FROM ratings r
				WHERE r.to_user_id = c.user_id AND r.target_role = 'company')`,

		// Indexes for common queries.
		`CREATE INDEX IF NOT EXISTS idx_orders_customer ON orders(customer_id)`,
		`CREATE INDEX IF NOT EXISTS idx_orders_company  ON orders(company_id)`,
		`CREATE INDEX IF NOT EXISTS idx_orders_worker   ON orders(worker_id)`,
		`CREATE INDEX IF NOT EXISTS idx_notifications_user ON notifications(user_id, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_ratings_target ON ratings(to_user_id, target_role, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_workers_user ON workers(user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_companies_user ON companies(user_id)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			log.Fatalf("db: migrate: %v\n  sql: %s", err, s)
		}
	}
}
