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
| `UPLOAD_DIR`         | ``                              | Directory for uploaded photos; empty = keep them in Postgres |
| `UPLOAD_MIGRATE`     | `false`                         | Move the photos still in Postgres onto `UPLOAD_DIR`, once, at startup |
| `MAX_UPLOAD_MB`      | `3`                             | Cap on one uploaded image                    |
| `PURGE_ORDER_PHOTOS` | `true`                          | Delete a job's photos once the job is over; profile photos are never touched |
| `ORDER_PHOTO_GRACE_DAYS` | `0`                         | Days to keep a finished job's photos; `0` deletes them as it closes |
| `DB_MAX_OPEN_CONNS`  | `30`                            | Cap on the Postgres connection pool — Go's own default is unlimited |
| `DB_MAX_IDLE_CONNS`  | `5`                             | Connections kept open between requests       |
| `SMS_HOURLY_BUDGET`  | `300`                           | Messages the platform will send in an hour, all callers together; `0` removes the cap |
| `RATE_LIMIT_PER_MIN` | `240`                           | Requests one signed-in account may make per minute, bursting to a minute's worth; `0` switches it off |
| `DOMAIN`             | ``                              | Domain Caddy gets a TLS certificate for (deploy only) |

## Seeded data

Seeded on first run into an empty database, then persisted like anything else.

Categories (7): `banquet_helper`, `gardening`, `mover_coal`, `loader`,
`post_repair_clean`, `master_helper`, `room_clean` (uz/ru/en names). The apps
derive each category's glyph from its `key`; the `icon` column is legacy and
unread, so the admin form asks for a name and nothing else.
A starting list, not the list — the admin panel adds, renames, reorders and
retires them (`/admin/categories`), and this seed never runs again once any
category exists. A category in use is retired (`active: false`) rather than
deleted, so the orders already filed under it keep a name to show; only an
unreferenced row can be removed. `GET /categories` therefore returns retired rows
too, and anything offering a *choice* filters on `active`.

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

## Uploaded photos

`POST /uploads` takes the base64 an image picker produces and answers with a
URL; `GET /api/v1/files/{id}` serves it back, public and cached forever, because
an `<Image>` tag sends no `Authorization` header and the random uuid in the URL
is the capability. Avatars, company logos and order photos are all this.

Where the bytes live is the one thing that changes between hosts:

| `UPLOAD_DIR` | Bytes go to | Right when |
| ------------ | ----------- | ---------- |
| empty        | the `files.bytes` column | the filesystem is wiped on every deploy (Render, Fly without a volume) |
| a path       | that directory, sharded `f4/7a/f47ac10b-….jpg` | the server has a disk that survives a restart |

Both are served under the same URL, so this is a deployment decision and not an
API one — nothing in either app changes, and neither does a stored `avatar_url`.

**Moving an existing platform to disk.** Set `UPLOAD_DIR`, then once:

```bash
UPLOAD_MIGRATE=true docker compose up -d api    # logs "moved N image(s)"
docker compose exec db psql -U prostaff -d prostaff -c 'VACUUM FULL files'
```

The migration is idempotent and safe to interrupt: each file is on disk before
its row stops pointing at the bytes. The `VACUUM FULL` is not optional if you
are doing this to reclaim space — Postgres marks the pages free but does not
hand them back to the filesystem, so `df` shows no change until it runs. It
takes an exclusive lock on the table, so do it while nobody is uploading.

Leave `UPLOAD_MIGRATE` off afterwards. With it off the server counts what is
left in the database and says so at startup, which is how you notice.

**Backups.** Once photos are on disk, `pg_dump` alone is no longer a backup — a
restore from it gives you rows naming files that are not there, which reaches
users as every avatar going blank at once. Both halves, together:

```bash
docker compose exec -T db pg_dump -U prostaff prostaff | gzip > db-$(date +%F).sql.gz
docker run --rm -v prostaff_uploads:/data -v "$PWD":/out alpine \
  tar czf /out/uploads-$(date +%F).tar.gz -C /data .
```

`scripts/backup.sh` is those two commands as a nightly job — written to `.part`
and renamed only on success, pruned after `KEEP_DAYS`, and loud when the dump
comes out too small to be a real database. Install it on the host:

```bash
cp scripts/backup.sh /usr/local/bin/prostaff-backup && chmod +x /usr/local/bin/prostaff-backup
# 17 3 * * * COMPOSE_DIR=/opt/prostaff/backend /usr/local/bin/prostaff-backup >> /var/log/prostaff-backup.log 2>&1
```

It writes to the same machine, which is protection against a bad migration and
none at all against losing the server. Ship `/var/backups/prostaff` somewhere
else as well, and restore one onto a spare box once — an untested backup is a
belief, not a backup.

### Order photos are deleted when the job is

Photos are the only thing here that grows without a ceiling. A profile photo is
written once and read for as long as the account exists; an order photo is
looked at by the worker deciding whether to take the job, and then never again —
and there are up to six of them per job. At a hundred thousand accounts the
avatars are tens of gigabytes and the job photos are a terabyte.

So they are deleted as the order reaches `completed`, `cancelled` or `expired`.
Profile photos — avatars, a worker's portfolio, a firm's gallery — are never
touched.

What makes that safe is a column rather than a rule: `files.order_id` is stamped
when a photo is attached to an order, and is empty on every other upload, so the
delete is `WHERE order_id = $1`. It cannot reach an avatar because an avatar has
no order on it. A file is only claimable by the account that uploaded it, and
never if it is currently that account's own avatar or portfolio image, which
closes the two ways the wrong photo could otherwise be picked.

| Variable | Default | |
| -------- | ------- | - |
| `PURGE_ORDER_PHOTOS` | `true` | Off keeps every photo forever — the old behaviour, and the fastest way to fill a small disk |
| `ORDER_PHOTO_GRACE_DAYS` | `0` | Days to hold a finished job's photos. `0` deletes them as the job closes |

Set the grace period to 3 or 7 if the office handles complaints: the photos are
the only record of what the job looked like, and a customer ringing up the next
day about work they are unhappy with has nothing to show once they are gone.

Closing an order deletes its own photos, on that request. A sweeper every 15
minutes covers what that cannot — searches expired in bulk by a restart, a
delete that hit a full disk, and the whole backlog of orders closed before any
of this existed, 500 orders at a time. The startup log says how many are
waiting.

Deleting rows does not shrink Postgres, so after the first big sweep:

```bash
docker compose exec db psql -U prostaff -d prostaff -c 'VACUUM FULL files'
```

## Deploying on a server

`docker-compose.yml` runs Postgres, the API and Caddy; `Caddyfile` terminates
TLS. Caddy is the only thing holding a host port.

```bash
cp .env.example .env        # DEV_MODE=false, a real JWT_SECRET, DB_PASSWORD, DOMAIN
docker compose up -d --build
docker compose logs -f api
```

On a server that is bare — a fresh Ubuntu 24.04 and nothing else — that comes
after `scripts/server-setup.sh`, which installs Docker and everything else the
box is missing. See [CI/CD → Setting it up](#setting-it-up).

### Sizing

`docker-compose.yml` is tuned for a 4 GB / 4 vCPU VPS, which is what the
platform is expected to run on at a hundred thousand accounts. That number is
registrations, not people using the app at once — the concurrent figure is a few
percent of it, and a Go process holding four thousand WebSockets is a couple of
hundred megabytes of buffers.

| | |
| - | - |
| Postgres | 1.8 GB limit, 1 GB `shared_buffers`, 60 connections |
| API | 900 MB limit, `GOMEMLIMIT=700MiB`, pool capped at 30 connections |
| Caddy | 200 MB limit |
| Left for page cache | ~1 GB — what keeps photos being served out of memory |

The two caps that matter most are the ones Go and Postgres do not set for
themselves. `DB_MAX_OPEN_CONNS` bounds the pool, which Go leaves unlimited: with
no cap a traffic spike opens a Postgres backend process per concurrent query
until the machine is out of memory and the kernel starts killing things.
`GOMEMLIMIT` tells the garbage collector how big the container is, which it
otherwise reads off the host and grows straight past.

**Swap.** Four gigabytes is little enough that a brief spike shouldn't be fatal,
and swap is what makes it survivable rather than an OOM kill. Once, on the host:

```bash
fallocate -l 4G /swapfile && chmod 600 /swapfile
mkswap /swapfile && swapon /swapfile
echo '/swapfile none swap sw 0 0' >> /etc/fstab
sysctl -w vm.swappiness=10          # swap as a safety net, not a habit
echo 'vm.swappiness=10' >> /etc/sysctl.conf
```

**What the search costs.** Every order still looking for somebody re-broadcasts
every `DispatchInterval`, and each wave is a fixed three queries: one read of
the online workers, then two batched lookups about the few the category and
radius left. It used to be one query per online worker per wave, so the cost
grew with the size of the city times the number of people ordering at once —
ten simultaneous searches across six hundred online workers was around a
thousand queries a second and is now under three. This is the number that
decides when a bigger box stops helping, so it is worth knowing which shape it
has.

**What to watch.** Disk is the first thing to run out — it is photos rather than
rows now, at up to `MAX_UPLOAD_MB` each. Move up a plan at 70% disk, at RAM
steadily above 85%, or when swap is in constant use rather than occasional.

**Throttles.** Three, none of them per IP address — carriers here put whole
cities behind one, so counting requests by address locks out subscribers rather
than attackers (`internal/api/ratelimit.go`):

- Five wrong codes on a phone number, or ten wrong passwords, and that number is
  refused for fifteen minutes. A right answer clears the count. Without it a
  six-digit code that lives two minutes is a space small enough to search.
- `SMS_HOURLY_BUDGET` caps what the platform will spend on messages in an hour.
  The per-phone cooldown does nothing about a generated list of a thousand
  numbers being used once each, which costs exactly as much. `eskiz: hourly SMS
  budget spent` in the log means either real growth past the cap or somebody
  walking such a list — worth looking at either way.
- `RATE_LIMIT_PER_MIN` caps one signed-in account, keyed by user id, bursting to
  a minute's worth at once so a phone reconnecting and refetching every screen
  is not mistaken for an attack.

All three are counters in this process's memory. Running a second API container
would give an attacker two allowances — the point at which they need to move
somewhere shared, which is a real cost and not one worth paying yet.

Two things that are easy to get wrong, both of which look like a broken app
rather than a broken deploy:

- **The proxy must send `X-Forwarded-Proto`.** The server builds upload URLs
  from it (`publicBase`, `internal/api/files.go`). Without it every URL comes
  back `http://`, Android blocks cleartext, and photos upload fine and never
  display. Caddy sends it by default; nginx needs
  `proxy_set_header X-Forwarded-Proto $scheme;` written out.
- **`UPLOAD_DIR` must be a volume, not a container path.** A directory inside
  the container is as ephemeral as the host we moved off; the photos would
  survive exactly until the next `docker compose up --build`.

## CI/CD

`.github/workflows/deploy.yml`. Every push to `main`: `go vet`, `go build`,
`go test`; then a Docker image; then the server. A pull request gets the tests
and stops there — only `main` reaches a machine.

The image is built by the runner and not by the server. Compiling Go wants
about a gigabyte and a couple of minutes, and the box it would want them from
is the 4 GB one already running Postgres under a 1.8 GB limit and the API under
900 MB. A build competing with those is a common way for an unattended deploy
to end in the kernel killing the process that was serving perfectly well. Built
on the runner it costs the server nothing, the deploy is a pull rather than a
compile, and the image being replaced stays on the disk to go back to.

Each image is tagged twice: `latest`, and `sha-<commit>`. The second is the one
that matters — an immutable name for exactly what is running, which is what you
want in front of you when something has to be undone quickly.

### Setting it up

Start with the key CI logs in with, on your own machine. No passphrase, because
nothing can type one:

```bash
ssh-keygen -t ed25519 -C "github-actions" -f ~/.ssh/prostaff_deploy -N ""
cat ~/.ssh/prostaff_deploy.pub
```

**Then the server.** `scripts/server-setup.sh` takes a box with nothing on it
but Ubuntu 24.04 and makes it one that can run this and accept a deploy: Docker
from Docker's own apt repository (Ubuntu's `docker.io` package ships no
`compose` plugin, which every script here calls), 4 GB of swap, a `deploy`
account in the docker group, ufw with ssh/80/443 and nothing else, fail2ban,
unattended security upgrades, key-only ssh, the checkout under
`/opt/prostaff/backend`, a `.env` carrying a generated `JWT_SECRET` and
`DB_PASSWORD`, and the nightly backup in cron.

```bash
ssh root@<host>
apt-get update && apt-get install -y git
git clone <repo> /tmp/prostaff && cd /tmp/prostaff
DOMAIN=api.prostaff.uz DEPLOY_PUBKEY="$(cat ~/.ssh/prostaff_deploy.pub)" \
  bash scripts/server-setup.sh
```

It is idempotent, and it needs to be: the first run stops half way. The
repository is private, so the script generates a read-only deploy key for the
server, prints it, and waits — add it under **Settings → Deploy keys**, leaving
write access unchecked, then run the same command again and it picks up from
there.

That key is not optional. Step 1 of a deploy is the server fast-forwarding its
own checkout, so that the compose file and the `Caddyfile` match the commit
being deployed; an account that cannot `git fetch` fails there, and the place to
discover that is now rather than in a deploy at midnight.

What the script cannot do is fill in the rest of `.env` — Eskiz, Atmos, the
Firebase key in `secrets/` — so it prints what is left, along with the A record
to point at the box and the first `docker compose up -d --build`. That first
build happens on the server because it has no registry credentials of its own.
Every deploy after it is a pull.

Then in **Settings → Secrets and variables → Actions**:

| Secret | Value |
| - | - |
| `SSH_HOST` | the server's IP or hostname |
| `SSH_USER` | `deploy` |
| `SSH_PRIVATE_KEY` | all of `~/.ssh/prostaff_deploy`, `BEGIN`/`END` lines included |
| `SSH_KNOWN_HOSTS` | the line `server-setup.sh` printed, or `ssh-keyscan <host>` |
| `SSH_PORT` | only if ssh is not on 22 |

The registry needs no secret. `GITHUB_TOKEN` may write packages belonging to
its own repository, and the same short-lived token is handed to the server for
the pull and expires with the run — deliberately, because a stored registry
credential makes a compromise of the server a compromise of the registry too.
The consequence is that the server cannot pull on its own; a manual rollback
uses an image already on the disk, which is what `KEEP_IMAGES` is for.

A pinned `SSH_KNOWN_HOSTS` rather than `ssh-keyscan` inside the job: scanning at
deploy time trusts whatever answers on the day, which is the one thing the
known_hosts file exists to prevent.

### What a deploy does

`scripts/deploy.sh`, piped to the server over ssh:

1. `git reset --hard origin/main` — the compose file and the `Caddyfile` are in
   the repo, so an image swapped under an old configuration would run new code
   with whichever setting the commit changed silently not taking effect. `.env`
   and `secrets/` are gitignored and untouched.
2. `docker compose pull api`, then `up -d`. Postgres and Caddy are left running
   unless their configuration actually changed — neither restarting a database
   nor asking Let's Encrypt for another certificate is a thing to do on every
   push.
3. Waits for the container's healthcheck. `restart: unless-stopped` means a
   container that crashes on boot is restarted and restarted, so without this a
   deploy that cannot reach Postgres reports success and is found by a user
   instead. On failure it prints the last 80 log lines and the rollback command.
4. Checks `https://$DOMAIN/api/v1/healthz` through Caddy — the half that catches
   a certificate that did not renew, which does not make the container
   unhealthy. A warning, not a failure.
5. Keeps the newest `KEEP_IMAGES` (3) and prunes the rest.

### Rolling back

The tag of a good deploy is in that run's log. On the server:

```bash
cd /opt/prostaff/backend
IMAGE=ghcr.io/samandarturgunboyev/prostaf IMAGE_TAG=sha-abc123def456 docker compose up -d api
```

Seconds, because the image is already there. Reverting the commit is still what
puts `main` and the server back in agreement — this is the part that stops the
bleeding, not the fix.

### Two things CI does not gate on yet

- **`go test -race`.** It reports a real race, but in a test rather than in the
  server: `push.Send` delivers on a background goroutine, and
  `TestABadFCMKeyDoesNotCostTheUserTheirToken` returns while that goroutine is
  still reading `expoURL`, which `t.Cleanup` is by then restoring. The
  production path writes `expoURL` once at startup and never again. Worth
  fixing — the race detector is most of the value of having tests for anything
  concurrent, and it is switched off here for one test's tidiness.
- **`gofmt`.** It lists most of the tree, because those files were written with
  CRLF line endings — a line-ending problem and not a formatting one. To settle
  it, add a `.gitattributes` containing `*.go text eol=lf`, run
  `git add --renormalize .`, and drop the `continue-on-error` from the workflow.
  It is one large, mechanical diff, best taken on its own rather than mixed into
  a change somebody will later want to read.

## Project layout

```
backend/
  go.mod                       module prostaff, go 1.26
  cmd/server/main.go           config load, store seed, HTTP server + graceful shutdown
  internal/config/config.go    env configuration
  internal/models/models.go    entity shapes (JSON tags match the contract)
  internal/id/id.go            UUIDv4 via crypto/rand
  internal/geo/geo.go          Haversine distance
  internal/blob/blob.go        uploaded photos on disk (sharded, atomic writes)
  internal/auth/jwt.go         HS256 JWT sign/verify (stdlib)
  internal/sse/hub.go          in-process SSE hub (userID -> subscriber channels)
  internal/store/
    store.go                   core store, seeding, users, OTP, notifications
    categories.go              the service list: admin CRUD, retire-vs-delete rule
    worker.go                  worker + company profiles
    order.go                   order lifecycle, matchmaking, invite/accept/expire, rating
    wallet.go                  wallet + payments
    file.go                    uploaded images: disk or database, and the move between
    admin.go                   admin ops, analytics, finance, settings
  internal/api/
    server.go                  router (method+path patterns)
    middleware.go              recover, logging, CORS, auth, role guard
    respond.go                 JSON + error-envelope helpers
    auth.go me.go worker.go company.go orders.go wallet.go admin.go events.go
```
