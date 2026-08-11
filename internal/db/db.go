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

		// The device's own FCM registration token, next to the Expo one.
		//
		// They are not interchangeable. `push_token` routes through Expo's push
		// service, which hands Android a data-only message that only becomes a
		// notification once the app's process picks it up — so it shows nothing
		// at all with the app closed. `fcm_token` is the raw device token, which
		// lets the server send a real notification payload that Android renders
		// on its own. Both are kept: iOS and any build without a Firebase
		// service account still go out through Expo.
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS fcm_token TEXT NOT NULL DEFAULT ''`,

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

		// The category list is the office's to edit now, not a fixture compiled
		// into the binary. Retiring one is a flag rather than a delete: an order
		// taken last year still points at its category and still has to be able
		// to show a name. Existing rows are active and unsorted, which is the
		// list exactly as it was.
		`ALTER TABLE categories ADD COLUMN IF NOT EXISTS active BOOLEAN NOT NULL DEFAULT TRUE`,
		`ALTER TABLE categories ADD COLUMN IF NOT EXISTS sort INTEGER NOT NULL DEFAULT 0`,

		// …and then in its own words instead. Picking from the fixed category list
		// is gone from the firm forms: it never covered what a firm actually
		// sells, and it was the one field that could make an otherwise complete
		// profile refuse to save. category_ids stays on the row so the firms that
		// filled it in before keep their line on screen.
		`ALTER TABLE companies ADD COLUMN IF NOT EXISTS services TEXT NOT NULL DEFAULT ''`,

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

		// Who the provider actually is, on paper.
		//
		// The office takes a worker's passport series and a firm's INN when it
		// opens the account, and both are unique across the platform. That is what
		// makes the three free jobs below safe to give away: the counter hangs off
		// the document, so the same person cannot collect another three by having
		// their profile deleted and re-created under a new phone number.
		//
		// Existing rows keep '' — a record from before this was asked for has no
		// document on file, and the partial indexes below let any number of them
		// coexist while still refusing a second worker with the same passport.
		`ALTER TABLE workers   ADD COLUMN IF NOT EXISTS passport_series TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE companies ADD COLUMN IF NOT EXISTS inn             TEXT NOT NULL DEFAULT ''`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_workers_passport
			ON workers(passport_series) WHERE passport_series <> ''`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_companies_inn
			ON companies(inn) WHERE inn <> ''`,

		// The free-trial ledger. `key` is the document it belongs to, prefixed by
		// the kind of provider ('worker:AA1234567', 'company:123456789'), so a
		// person who is both a worker and a firm gets one trial per hat.
		//
		// Deliberately not a column on workers/companies: the profile row is the
		// thing an admin can delete, and this has to outlive it.
		`CREATE TABLE IF NOT EXISTS free_trials (
			key        TEXT PRIMARY KEY,
			used       INTEGER NOT NULL DEFAULT 0,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,

		// A firm's account is not a wallet that commission is taken from — it is a
		// subscription with an end date. `subscription_until` is that date; NULL
		// means the firm has never paid and is still on its free jobs.
		//
		// `subscription_warned_at` remembers that the "7 days left" message has
		// already gone out for the current period, so the daily check does not
		// repeat it every morning for a week.
		`ALTER TABLE companies ADD COLUMN IF NOT EXISTS subscription_until     TIMESTAMPTZ`,
		`ALTER TABLE companies ADD COLUMN IF NOT EXISTS subscription_warned_at TIMESTAMPTZ`,

		// Every month a firm bought, and what it paid for it. The admin sets the
		// monthly price and may discount it, so the numbers of the day have to be
		// stored on the row rather than recomputed from today's settings.
		`CREATE TABLE IF NOT EXISTS company_subscriptions (
			id               TEXT PRIMARY KEY,
			company_id       TEXT NOT NULL,
			months           INTEGER NOT NULL,
			price_per_month  BIGINT NOT NULL,
			discount_percent INTEGER NOT NULL DEFAULT 0,
			amount           BIGINT NOT NULL,
			starts_at        TIMESTAMPTZ NOT NULL,
			ends_at          TIMESTAMPTZ NOT NULL,
			created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_company_subs ON company_subscriptions(company_id, created_at)`,

		// The tariffs a firm chooses between.
		//
		// They differ in length, not in what they include — every subscribed firm
		// gets the same platform — so a longer one is simply cheaper per month.
		// That is why the price is stored per plan rather than computed from a
		// monthly rate: "three months for 200 000" is a number the office decides,
		// not one arithmetic should be deriving.
		//
		// `old_price` is what the card shows struck through. It is presentational
		// and never charged; zero means no strike-through.
		`CREATE TABLE IF NOT EXISTS subscription_plans (
			id         TEXT PRIMARY KEY,
			months     INTEGER NOT NULL,
			price      BIGINT NOT NULL,
			old_price  BIGINT NOT NULL DEFAULT 0,
			title      TEXT NOT NULL DEFAULT '',
			active     BOOLEAN NOT NULL DEFAULT TRUE,
			sort       INTEGER NOT NULL DEFAULT 0,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_plans_active ON subscription_plans(active, sort)`,

		// Firms that were already on the platform when billing arrived get a
		// month, once.
		//
		// Without this the rule would apply retroactively: every existing firm has
		// no INN (so no free jobs) and no subscription (so no paid ones), and the
		// deploy that introduced this would have stopped all of them taking work
		// with no warning and nothing they could do about it. A month is long
		// enough for the office to call each of them and sell them a real one, and
		// the seven-day notice fires inside it exactly as it does for anybody
		// else.
		//
		// The flag below is what makes it once rather than once per restart —
		// `subscription_until IS NULL` alone would hand thirty free days to every
		// firm opened on a trial after this shipped, on the next deploy.
		`UPDATE companies SET subscription_until = NOW() + INTERVAL '30 days'
		 WHERE subscription_until IS NULL
		   AND NOT EXISTS (SELECT 1 FROM settings WHERE key = 'company_billing_grace_applied')`,
		`INSERT INTO settings (key, value) VALUES ('company_billing_grace_applied', 'true')
		 ON CONFLICT (key) DO NOTHING`,

		// A payment is now a real thing with a life: opened at the gateway,
		// pending while the payer is on Atmos's page, and settled — once — by a
		// signed callback.
		//
		// `external_id` is Atmos's transaction id and carries a unique index,
		// which is what makes a repeated callback (Atmos retries) harmless: the
		// second one matches a row that is no longer pending and credits nothing.
		//
		// `purpose` and `months` are what the money was for. Without them a
		// callback knows an amount and an account and has no idea whether it just
		// bought a worker some balance or a firm three months.
		`ALTER TABLE payments ADD COLUMN IF NOT EXISTS external_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE payments ADD COLUMN IF NOT EXISTS purpose     TEXT NOT NULL DEFAULT 'wallet'`,
		`ALTER TABLE payments ADD COLUMN IF NOT EXISTS months      INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE payments ADD COLUMN IF NOT EXISTS paid_at     TIMESTAMPTZ`,
		// Why a payment did not work.
		//
		// Until this column existed, a failure was a line in the server log and
		// nothing else: the person who had just been charged, or had just failed
		// to be, could ring the office and nobody there could see anything at all.
		// The gateway refusing, a callback whose signature did not verify, an
		// amount that disagreed with what was asked for — all of it lands here,
		// next to the payment it happened to.
		`ALTER TABLE payments ADD COLUMN IF NOT EXISTS error       TEXT NOT NULL DEFAULT ''`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_payments_external
			ON payments(external_id) WHERE external_id <> ''`,
		`CREATE INDEX IF NOT EXISTS idx_payments_user ON payments(user_id, created_at)`,

		// An order carries a set of photos, not one.
		//
		// A job is often explained better by three pictures than by a paragraph —
		// the broken thing, the room it is in, the way in — and one slot meant the
		// customer picked whichever single frame they thought was most important
		// and the worker arrived without the rest.
		//
		// `image_url` stays and keeps holding the first photo: apps built before
		// this read that field and nothing else, and an order that shows them no
		// picture at all is worse than one that shows them the main one.
		`ALTER TABLE orders ADD COLUMN IF NOT EXISTS images TEXT NOT NULL DEFAULT '[]'`,
		// Existing orders keep their single photo by moving it into the list.
		`UPDATE orders SET images = json_build_array(image_url)::text
		 WHERE images = '[]' AND image_url <> ''`,

		// The workers a customer picked by hand.
		//
		// The platform's default is a broadcast: the customer describes the job
		// and every matching worker nearby is rung at once. That is the right
		// default and a poor fit for the other thing people want, which is to
		// look through who is around and call three of them in particular — a
		// neighbour they have used before, the two with the best reviews.
		//
		// Empty means the broadcast, so every order written before this behaves
		// exactly as it did. Non-empty narrows the search to these worker
		// profiles and to nobody else, however many other workers would match.
		`ALTER TABLE orders ADD COLUMN IF NOT EXISTS target_worker_ids TEXT NOT NULL DEFAULT '[]'`,

		// The orders that were placed together as one request.
		//
		// "I need five helpers" is five orders — that is what keeps each worker's
		// commission tied to a job they actually got, instead of leaving somebody
		// charged for a half-staffed evening. But five orders broadcast
		// independently rang every nearby worker five times: a worker looking for
		// one job saw five cards and got five pushes, and four of them were for
		// work they could not take once they had answered the first.
		//
		// Sharing an id is what lets dispatch treat them as one request and offer
		// each worker only one of them at a time. Empty for an ordinary single
		// order, which is every order written before this.
		`ALTER TABLE orders ADD COLUMN IF NOT EXISTS batch_id TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS idx_orders_batch ON orders(batch_id) WHERE batch_id <> ''`,

		// How many orders the request was split into.
		//
		// Carried rather than counted, because the five POSTs arrive together:
		// the first of them to reach dispatch can see none of its siblings in the
		// table yet, conclude it is alone, and claim every free worker — leaving
		// the other four to ring nobody and time out while the customer waits for
		// five people. Knowing the size up front is what lets the very first wave
		// take a fair share instead.
		`ALTER TABLE orders ADD COLUMN IF NOT EXISTS batch_size INTEGER NOT NULL DEFAULT 1`,

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
