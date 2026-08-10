# PROSTAFF Backend (Go)

Multilingual marketplace API connecting customers with daily workers and service
companies. Implements the shared contract in `../API_CONTRACT.md`.

**Standard library only — zero external dependencies.** JWT (HS256), UUIDv4,
and realtime (SSE) are all implemented by hand with `crypto/*`, `net/http`, and
`encoding/json`. It builds and runs fully offline.

## Requirements

- Go 1.22+ (developed on Go 1.26). Uses `http.ServeMux` method+path routing.

## Run

```bash
cd backend
go run ./cmd/server
```

The API listens on `http://localhost:8080`, all routes under `/api/v1`.
Graceful shutdown on Ctrl-C / SIGTERM.

```bash
go build ./...   # compiles clean
go vet ./...     # vets clean
```

## Configuration (environment variables)

| Var                  | Default                         | Meaning                                      |
| -------------------- | ------------------------------- | -------------------------------------------- |
| `PORT`               | `8080`                          | HTTP port                                    |
| `JWT_SECRET`         | `prostaff-dev-secret-change-me` | HS256 signing secret                         |
| `DEV_MODE`           | `true`                          | Return `dev_code` on OTP; instant top-ups    |
| `ESKIZ_BASE_URL`     | ``                              | Base URL of the Eskiz SMS API                |
| `ESKIZ_TOKEN`        | ``                              | Optional pre-issued bearer token             |
| `ESKIZ_EMAIL`        | ``                              | Eskiz login email used for token exchange    |
| `ESKIZ_PASSWORD`     | ``                              | Eskiz login password used for token exchange |
| `ESKIZ_SENDER`       | `4546`                          | Sender id; a custom name needs Eskiz approval |
| `COMMISSION_PERCENT` | `10`                            | Commission % deducted from worker on accept  |
| `MIN_BALANCE`        | `10000`                         | Default wallet minimum balance (UZS)         |
| `SEED_DEMO`          | `false`                         | Create test workers/firms (see below)        |

## Seeded data

Seeded on first run into an empty database, then persisted like anything else.

Categories (7): `banquet_helper`, `gardening`, `mover_coal`, `loader`,
`post_repair_clean`, `master_helper`, `room_clean` (uz/ru/en names + emoji icon).

The admin account from `ADMIN_PHONE` (default `+998900000000`).

### Demo providers (`SEED_DEMO=true`)

With the flag set, `internal/store/demo.go` puts 8 workers (`+99890111000X`) and
5 firms (`+99890222000X`) on the platform, all online, scattered 2–12 km around
the centre of Tashkent so the radius filter has something to filter. Every one of
them signs in with the password `demo1234`.

The seeder is idempotent — a phone that already carries a profile is skipped — so
the flag can stay set across restarts. It is off by default and independent of
`DEV_MODE`, which defaults on: inventing bookable providers on a real database is
not something a default should do.

Any new phone auto-creates a `customer` on OTP verify.

## Key behaviours

- **Auth**: `POST /auth/otp/request` → a random 4-digit code, sent by SMS through
  Eskiz and returned as `dev_code` in dev. The code is **valid for 2 minutes** and
  single-use; check and delete happen in one statement so two requests can't both
  spend the same code. One message per phone per 60 s, which puts a resend halfway
  through the code's life.
  `POST /auth/otp/verify` → `{ token, refresh_token, expires_in, user }`; `token` is
  an HS256 JWT valid for 15 minutes, renewed via `POST /auth/refresh` (the refresh
  token is rotated on every use). `POST /auth/otp/verify` is the only way an
  account is opened — it takes an optional `name`/`password` so sign-up finishes
  in the same call, and `POST /auth/login` then signs in with phone + password.
  With `ESKIZ_BASE_URL` empty no SMS is sent — that is how dev runs without an
  Eskiz account.
- **Password reset**: three steps, and no password is ever generated for the user
  or carried over SMS. `POST /auth/forgot-password` sends a code →
  `POST /auth/forgot-password/verify` spends it for a 10-minute `reset_token` →
  `POST /auth/reset-password` sets the password the user chose and revokes every
  older session. The reset token is scoped: it is signed with a `pwreset:` subject,
  so the authed middleware and the SSE stream both fail to resolve it to a user and
  reject it.
- **Worker**: created by an admin (`POST /admin/workers`) — there is no
  self-service sign-up. The worker themselves may only `PATCH /worker/profile`
  with `price_min`/`price_max`/`category_ids`; every other field belongs to the
  admin (`PATCH /admin/workers/{id}`). Companies work the same way
  (`POST /admin/companies`).
- **Broadcast dispatch**: `POST /orders` creates the order _and_ rings everyone
  eligible within **15 km** (Haversine) — online, not banned, right category,
  right gender, not already on a job, and able to cover the commission. Status
  goes `invited`, `invite_expires_at = now+120s`, SSE `order.invite` + a
  high-priority push go to each worker (customer phone masked). While the window
  is open a background loop re-broadcasts every **12 s**, so a worker who comes
  online mid-search still gets the call; decliners are recorded and never rung
  twice for the same order. `POST /orders/{id}/dispatch` re-opens the search on
  an order nobody took.
- **First accept wins**: the assignment is one guarded
  `UPDATE ... WHERE worker_id='' AND status IN ('matching','invited')`, so of N
  simultaneous taps exactly one succeeds — the losers get **409** and only the
  winner pays commission. The winner's phone is revealed to the customer (SSE
  `order.accepted`) and the other invitees get SSE `order.taken`. If nobody
  accepts inside the window the order flips to `expired`.
- **Commission**: on accept, `worker.price_min * commission_percent / 100` is
  deducted from the worker's wallet. A worker may accept only when
  `balance >= min_balance` **or** `credit_limit > 0`. Insufficient funds → **402**.
- **Wallet**: `POST /wallet/topup {provider, amount}` credits instantly in dev
  and records a `Payment`.
- **Admin**: users search/edit, role add/remove, worker/company ban + credit
  limit, push (SSE `admin.push` + stored `Notification`), order analytics
  (`day|month|year` buckets with commission revenue), finance overview, and
  `PATCH /admin/settings`. All `/admin/*` require the `admin` role.

## Realtime (SSE)

`GET /api/v1/events?token=<jwt>` — `text/event-stream`, one stream per user.
Authenticates via the `token` query param (EventSource can't set headers).
Each event is a line `data: {"type":...,"data":...}\n\n`; a `:keepalive`
comment is sent every 20s; the connection is cleaned up on disconnect.

Event types: `order.invite`, `order.accepted`, `order.expired`, `order.status`,
`admin.push`.

REST poll fallbacks (for React Native without SSE):
`GET /worker/orders?since=`, `GET /orders/{id}`, `GET /notifications?since=`.

## Errors

`{ "error": { "code": string, "message": string } }` with the proper HTTP
status (400/401/402/403/404/409/500).

## Example: full customer → worker flow (curl)

```bash
BASE=http://localhost:8080/api/v1

# 1. Customer logs in
CODE=$(curl -s -X POST $BASE/auth/otp/request -d '{"phone":"+998705550001"}' | grep -o '"dev_code":"[0-9]*"' | sed 's/[^0-9]//g')
CUST=$(curl -s -X POST $BASE/auth/otp/verify -d "{\"phone\":\"+998705550001\",\"code\":\"$CODE\"}" | grep -o '"token":"[^"]*"' | sed 's/"token":"//;s/"//')

# 2. Worker logs in (use seeded +998901111111 which is already online + funded)
WCODE=$(curl -s -X POST $BASE/auth/otp/request -d '{"phone":"+998901111111"}' | grep -o '"dev_code":"[0-9]*"' | sed 's/[^0-9]//g')
WK=$(curl -s -X POST $BASE/auth/otp/verify -d "{\"phone\":\"+998901111111\",\"code\":\"$WCODE\"}" | grep -o '"token":"[^"]*"' | sed 's/"token":"//;s/"//')

# 3. Find the room_clean category id
CAT=$(curl -s $BASE/categories -H "Authorization: Bearer $CUST" | grep -o '"id":"[0-9a-f-]*","key":"room_clean"' | sed 's/"id":"//;s/","key.*//')

# 4. Customer creates an order in Tashkent — this also broadcasts it. The reply
#    already carries invited_worker_ids (every worker within 15 km) and a 120s
#    invite_expires_at; the customer picks nobody.
ORDER=$(curl -s -X POST $BASE/orders -H "Authorization: Bearer $CUST" \
  -d "{\"category_id\":\"$CAT\",\"gender\":\"any\",\"description\":\"Xona tozalash\",\"price\":150000,\"phone\":\"+998705550001\",\"lat\":41.311,\"lng\":69.240}")
OID=$(echo "$ORDER" | grep -o '"id":"[^"]*"' | head -1 | sed 's/"id":"//;s/"//')

# 5. The worker sees it in their incoming list
curl -s $BASE/worker/orders -H "Authorization: Bearer $WK"

# 6. Worker accepts (wins the order, commission deducted, phones revealed).
#    Any other worker accepting after this gets 409.
curl -s -X POST $BASE/worker/orders/$OID/accept -H "Authorization: Bearer $WK"

# 7. Customer confirms accepted state + worker phone
curl -s $BASE/orders/$OID -H "Authorization: Bearer $CUST"

# Nobody took it? Search again (fresh 120s window, everyone re-rung):
#   curl -s -X POST $BASE/orders/$OID/dispatch -H "Authorization: Bearer $CUST"

# Realtime alternative to steps 6-7: the customer opens the SSE stream and
# receives order.accepted automatically:
#   curl -N "$BASE/events?token=$CUST"
```

## Project layout

```
backend/
  go.mod                       module prostaff, go 1.26
  cmd/server/main.go           config load, store seed, HTTP server + graceful shutdown
  internal/config/config.go    env configuration
  internal/models/models.go    entity shapes (JSON tags match the contract)
  internal/id/id.go            UUIDv4 via crypto/rand
  internal/geo/geo.go          Haversine distance
  internal/auth/jwt.go         HS256 JWT sign/verify (stdlib)
  internal/sse/hub.go          in-process SSE hub (userID -> subscriber channels)
  internal/store/
    store.go                   core store, seeding, users, categories, OTP, notifications
    worker.go                  worker + company profiles
    order.go                   order lifecycle, matchmaking, invite/accept/expire, rating
    wallet.go                  wallet + payments
    admin.go                   admin ops, analytics, finance, settings
  internal/api/
    server.go                  router (method+path patterns)
    middleware.go              recover, logging, CORS, auth, role guard
    respond.go                 JSON + error-envelope helpers
    auth.go me.go worker.go company.go orders.go wallet.go admin.go events.go
```
