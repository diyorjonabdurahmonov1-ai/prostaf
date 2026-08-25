#!/usr/bin/env bash
#
# A bare Ubuntu 24.04 box -> a machine that runs PROSTAFF and accepts deploys
# from GitHub Actions. Run it once, as root, on a server with nothing on it:
#
#   ssh root@<host>
#   apt-get update && apt-get install -y git
#   git clone <repo> /tmp/prostaff && cd /tmp/prostaff   # or scp this one file
#   DOMAIN=api.prostaff.uz bash scripts/server-setup.sh
#
# It is idempotent — running it again fixes whatever drifted and changes
# nothing else. That matters more than it sounds: the first run usually stops
# half way, at the point where GitHub has to be told about a key, and the
# second run has to be able to pick up exactly there.
#
# What it does not do is start the application. The last mile is .env — the SMS
# provider, the payment gateway, the Firebase key — and none of that can be
# guessed here. The script writes a .env with the secrets it can generate
# safely and prints what is left.
#
# Settings, all optional except DOMAIN:
#
#   DOMAIN         the hostname Caddy gets a certificate for. Its A record must
#                  already point here — Let's Encrypt proves the domain by
#                  connecting back to it.
#   REPO_URL       ssh by default, because the repository is private and a
#                  read-only deploy key is the smallest credential that works.
#   DEPLOY_USER    the account CI logs in as (default: deploy)
#   APP_DIR        where the checkout lives (default: /opt/prostaff/backend)
#   DEPLOY_PUBKEY  the public half of the CI key, authorised for DEPLOY_USER.
#                  Skip it and you are told how to add it by hand.
#   SWAP_SIZE      default 4G; "0" to skip
#   TIMEZONE       default Asia/Tashkent
#   START          "true" to build and start at the end

set -euo pipefail

DOMAIN="${DOMAIN:-}"
REPO_URL="${REPO_URL:-git@github.com:SamandarTurgunboyev/prostaf.git}"
DEPLOY_USER="${DEPLOY_USER:-deploy}"
APP_DIR="${APP_DIR:-/opt/prostaff/backend}"
DEPLOY_PUBKEY="${DEPLOY_PUBKEY:-}"
SWAP_SIZE="${SWAP_SIZE:-4G}"
TIMEZONE="${TIMEZONE:-Asia/Tashkent}"
START="${START:-false}"

step() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
note() { printf '    %s\n' "$*"; }
warn() { printf '\033[33m    ! %s\033[0m\n' "$*" >&2; }

[ "$(id -u)" -eq 0 ] || { echo "run this as root" >&2; exit 1; }

if ! grep -q 'ID=ubuntu' /etc/os-release 2>/dev/null; then
	warn "this was written for Ubuntu 24.04; continuing anyway"
fi

export DEBIAN_FRONTEND=noninteractive

# ── Base system ──────────────────────────────────────────────────
step "base packages"
apt-get update -qq
# curl, git and ca-certificates are needed by the steps below; ufw and fail2ban
# are the two things a server on a public IP should not be without;
# unattended-upgrades is what keeps openssl patched on a box nobody logs into
# for months at a time.
#
# sudo, openssh-client and cron are named explicitly rather than assumed. A full
# Ubuntu server image has all three, and the minimal cloud images that several
# providers now hand out by default have none of them — openssh-*server* being
# installed is not the same as ssh-keygen existing, which is the sort of thing
# you find out from a script dying two thirds of the way through.
apt-get install -y -qq \
	ca-certificates curl git gnupg \
	ufw fail2ban unattended-upgrades \
	sudo openssh-client cron \
	openssl >/dev/null
systemctl enable --now cron >/dev/null 2>&1 || warn "cron did not start — the nightly backup will not run"
note "installed"

timedatectl set-timezone "$TIMEZONE" 2>/dev/null || warn "could not set timezone to $TIMEZONE"
note "timezone: $(timedatectl show -p Timezone --value 2>/dev/null || echo unknown)"

# Security updates, applied by the machine itself. No automatic reboot: a
# kernel upgrade that reboots the server at 6am unannounced is a worse outcome
# than a kernel that is a week old.
cat > /etc/apt/apt.conf.d/20auto-upgrades <<'EOF'
APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Unattended-Upgrade "1";
EOF
note "unattended security upgrades: on"

# fail2ban's Debian default reads /var/log/auth.log, which a minimal 24.04
# image does not have — ssh logs go to the journal only, and the jail then
# silently protects nothing.
cat > /etc/fail2ban/jail.local <<'EOF'
[DEFAULT]
backend = systemd

[sshd]
enabled = true
maxretry = 5
bantime = 1h
EOF
systemctl enable --now fail2ban >/dev/null 2>&1 || warn "fail2ban did not start"
note "fail2ban: watching sshd"

# ── Swap ─────────────────────────────────────────────────────────
step "swap"
if [ "$SWAP_SIZE" = "0" ]; then
	note "skipped"
elif swapon --show | grep -q .; then
	note "already on: $(swapon --show=NAME,SIZE --noheadings | tr '\n' ' ')"
else
	# 4 GB of RAM is little enough that a brief spike should not be fatal, and
	# swap is what makes it survivable instead of an OOM kill of whichever
	# process the kernel likes least — which is usually the API, mid-request.
	fallocate -l "$SWAP_SIZE" /swapfile 2>/dev/null \
		|| dd if=/dev/zero of=/swapfile bs=1M count=$(( ${SWAP_SIZE%G} * 1024 )) status=none
	chmod 600 /swapfile
	mkswap /swapfile >/dev/null
	swapon /swapfile
	grep -q '^/swapfile' /etc/fstab || echo '/swapfile none swap sw 0 0' >> /etc/fstab
	# Swap as a safety net, not as a habit: the default of 60 has the kernel
	# paging out a process that is about to be asked for a response.
	sysctl -qw vm.swappiness=10
	grep -q '^vm.swappiness' /etc/sysctl.conf || echo 'vm.swappiness=10' >> /etc/sysctl.conf
	note "$SWAP_SIZE added"
fi

# ── Docker ───────────────────────────────────────────────────────
step "docker"
if command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
	note "already installed: $(docker --version)"
else
	# Docker's own repository rather than Ubuntu's docker.io package: the
	# compose plugin is what every script here calls as `docker compose`, and
	# the distribution package does not ship it.
	install -m 0755 -d /etc/apt/keyrings
	curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
	chmod a+r /etc/apt/keyrings/docker.asc
	echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo "$VERSION_CODENAME") stable" \
		> /etc/apt/sources.list.d/docker.list
	apt-get update -qq
	apt-get install -y -qq docker-ce docker-ce-cli containerd.io \
		docker-buildx-plugin docker-compose-plugin >/dev/null
	note "installed: $(docker --version)"
fi

# Log caps for anything started outside compose. docker-compose.yml sets its
# own per service; this is the safety net, because the json-file driver's
# default is unbounded and a server filling its disk with the logs of the thing
# that was working fine is a genuinely common way to lose one.
if [ ! -f /etc/docker/daemon.json ]; then
	mkdir -p /etc/docker
	cat > /etc/docker/daemon.json <<'EOF'
{
  "log-driver": "json-file",
  "log-opts": { "max-size": "10m", "max-file": "3" }
}
EOF
	systemctl restart docker
	note "daemon defaults written"
fi
systemctl enable docker >/dev/null 2>&1 || true

# ── The deploy account ───────────────────────────────────────────
step "deploy account: $DEPLOY_USER"
if id "$DEPLOY_USER" >/dev/null 2>&1; then
	note "exists"
else
	# No password at all. The only way in is a key, which is the point.
	adduser --disabled-password --gecos "" "$DEPLOY_USER" >/dev/null
	note "created"
fi
# Membership of the docker group is effectively root on this box. That is
# accepted here because it is a single-purpose server whose only job is to run
# these containers — but it is also why CI logs in as this account and not as
# root: the key lives in a GitHub secret, and a key that is also root is a
# larger thing to lose.
usermod -aG docker "$DEPLOY_USER"

DEPLOY_HOME="$(getent passwd "$DEPLOY_USER" | cut -d: -f6)"
install -d -m 700 -o "$DEPLOY_USER" -g "$DEPLOY_USER" "$DEPLOY_HOME/.ssh"
AUTH_KEYS="$DEPLOY_HOME/.ssh/authorized_keys"
touch "$AUTH_KEYS"

# Whatever key root is logged in with, so a human can reach this account too.
# Being locked out of the deploy user while debugging a deploy is a bad hour.
if [ -s /root/.ssh/authorized_keys ]; then
	while read -r key; do
		[ -n "$key" ] || continue
		grep -qF "$key" "$AUTH_KEYS" || echo "$key" >> "$AUTH_KEYS"
	done < /root/.ssh/authorized_keys
fi
if [ -n "$DEPLOY_PUBKEY" ]; then
	grep -qF "$DEPLOY_PUBKEY" "$AUTH_KEYS" || echo "$DEPLOY_PUBKEY" >> "$AUTH_KEYS"
	note "CI key authorised"
else
	warn "no DEPLOY_PUBKEY given — add the CI public key to $AUTH_KEYS yourself"
fi
chmod 600 "$AUTH_KEYS"
chown "$DEPLOY_USER:$DEPLOY_USER" "$AUTH_KEYS"
note "authorised keys: $(grep -c . "$AUTH_KEYS" || true)"

# ── The checkout ─────────────────────────────────────────────────
step "checkout: $APP_DIR"
install -d -o "$DEPLOY_USER" -g "$DEPLOY_USER" "$(dirname "$APP_DIR")"

# GitHub's host key, pinned rather than scanned. ssh-keyscan trusts whatever
# answers on the day, which is the one thing known_hosts exists to stop. If
# GitHub ever rotates this the clone fails loudly rather than quietly trusting
# a stranger; the current fingerprints are published at docs.github.com, under
# "GitHub's SSH key fingerprints".
KNOWN="$DEPLOY_HOME/.ssh/known_hosts"
GH_KEY="github.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"
touch "$KNOWN"
grep -qF "$GH_KEY" "$KNOWN" || echo "$GH_KEY" >> "$KNOWN"
chown "$DEPLOY_USER:$DEPLOY_USER" "$KNOWN"

# The key this server pulls the repository with. Separate from the key CI logs
# in with, and read-only on GitHub's side: a server that can push is a server
# that can rewrite what it is asked to deploy.
REPO_KEY="$DEPLOY_HOME/.ssh/id_ed25519"
if [ ! -f "$REPO_KEY" ]; then
	sudo -u "$DEPLOY_USER" -H ssh-keygen -t ed25519 -N "" -C "prostaff-server@$(hostname)" -f "$REPO_KEY" >/dev/null
	note "repository key generated"
fi

CLONE_PENDING=""
if [ -d "$APP_DIR/.git" ]; then
	note "already cloned"
elif sudo -u "$DEPLOY_USER" -H git clone --quiet "$REPO_URL" "$APP_DIR" 2>/dev/null; then
	note "cloned $REPO_URL"
else
	CLONE_PENDING=1
	warn "could not clone $REPO_URL — the repository is private and this key is not on it yet"
fi

# ── .env ─────────────────────────────────────────────────────────
if [ -z "$CLONE_PENDING" ]; then
	step ".env"
	ENV_FILE="$APP_DIR/.env"
	if [ -f "$ENV_FILE" ]; then
		# Never rewritten. DB_PASSWORD in particular is baked into the Postgres
		# volume the first time it starts, so changing it here afterwards does
		# not change the database's password — it only stops the API connecting.
		note "already exists, left alone"
	else
		cp "$APP_DIR/.env.example" "$ENV_FILE"
		JWT="$(openssl rand -hex 32)"
		DBPASS="$(openssl rand -hex 16)"
		sed -i \
			-e "s|^DEV_MODE=.*|DEV_MODE=false|" \
			-e "s|^JWT_SECRET=.*|JWT_SECRET=$JWT|" \
			-e "s|^DB_PASSWORD=.*|DB_PASSWORD=$DBPASS|" \
			-e "s|^DOMAIN=.*|DOMAIN=$DOMAIN|" \
			"$ENV_FILE"
		# After sed, not before: `sed -i` writes a new file and the old owner
		# goes with the old inode. Left as root's, this is a .env that the
		# account running docker compose cannot read — which surfaces as
		# Postgres refusing to start for want of a password, some way from the
		# thing that actually went wrong.
		chown "$DEPLOY_USER:$DEPLOY_USER" "$ENV_FILE"
		chmod 600 "$ENV_FILE"
		note "written, with a generated JWT_SECRET and DB_PASSWORD"
		[ -n "$DOMAIN" ] || warn "DOMAIN is empty — Caddy will refuse to start until it is set"
	fi
	# The Firebase key's home. Compose bind-mounts it read-only, and Docker
	# would otherwise create it root-owned on first start.
	#
	# 755, and the key inside it has to be 644 — which looks wrong for a
	# credential and is not. A bind mount carries host uids straight through,
	# and the process inside the container is alpine's `prostaff` (a system uid
	# of its own, nothing to do with this account), so a file only the owner can
	# read is a file the server cannot read. It fails quietly, too: no Firebase
	# key means push silently falls back to Expo, which on Android delivers
	# nothing at all once the app's process has been closed. The host is
	# single-purpose and has two accounts, both of which already have the
	# database password, so this trades very little.
	install -d -m 755 -o "$DEPLOY_USER" -g "$DEPLOY_USER" "$APP_DIR/secrets"
fi

# ── Firewall ─────────────────────────────────────────────────────
step "firewall"
SSH_PORT="$(sshd -T 2>/dev/null | awk '/^port /{print $2; exit}')"
SSH_PORT="${SSH_PORT:-22}"
# ssh first, and before enabling. The other order locks you out of the machine
# you are standing on.
ufw allow "$SSH_PORT/tcp" >/dev/null
ufw allow 80/tcp >/dev/null
ufw allow 443/tcp >/dev/null
ufw --force enable >/dev/null
note "open: $SSH_PORT, 80, 443"
# Worth knowing: Docker writes iptables rules of its own and a published port
# bypasses ufw entirely. Here only Caddy publishes anything, on the two ports
# that are meant to be open — which is why docker-compose.yml gives the API and
# Postgres no `ports:` at all. Adding one puts it on the internet whatever ufw
# has been told.

# ── ssh hardening ────────────────────────────────────────────────
step "ssh"
if [ -s "$AUTH_KEYS" ]; then
	# 00- and not 99-: sshd takes the FIRST value it sees for a keyword, and
	# Ubuntu's cloud images ship a 50-cloud-init.conf that turns password
	# authentication back on. A file sorting after that one would be read and
	# then ignored.
	cat > /etc/ssh/sshd_config.d/00-prostaff.conf <<'EOF'
# Keys only. Passwords on a public IP are guessed, continuously, by everybody.
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin prohibit-password
EOF
	if sshd -t 2>/dev/null; then
		systemctl restart ssh 2>/dev/null || systemctl restart sshd
		note "password authentication off, key login only"
	else
		rm -f /etc/ssh/sshd_config.d/00-prostaff.conf
		warn "sshd rejected the config; left as it was"
	fi
else
	warn "no authorised keys for $DEPLOY_USER — password login left on, or you would be locked out"
fi

# ── Nightly backup ───────────────────────────────────────────────
if [ -z "$CLONE_PENDING" ]; then
	step "backup"
	install -m 755 "$APP_DIR/scripts/backup.sh" /usr/local/bin/prostaff-backup
	# 03:17 rather than 03:00, because every cron job on every server runs at
	# the top of the hour. Root, because the job talks to the docker socket.
	cat > /etc/cron.d/prostaff-backup <<EOF
SHELL=/bin/sh
PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
17 3 * * * root COMPOSE_DIR=$APP_DIR /usr/local/bin/prostaff-backup >> /var/log/prostaff-backup.log 2>&1
EOF
	chmod 644 /etc/cron.d/prostaff-backup
	note "nightly at 03:17 -> /var/backups/prostaff"
	warn "same disk as the database — add an offsite copy before this counts as a backup"
fi

# ── Start ────────────────────────────────────────────────────────
if [ -z "$CLONE_PENDING" ] && [ "$START" = "true" ]; then
	step "starting"
	# Built here, once, because the server has no registry credentials of its
	# own — every deploy after this one pulls an image CI has already built.
	sudo -u "$DEPLOY_USER" -H sh -c "cd '$APP_DIR' && docker compose up -d --build"
	sudo -u "$DEPLOY_USER" -H sh -c "cd '$APP_DIR' && docker compose ps"
fi

# ── What is left ─────────────────────────────────────────────────
PUBLIC_IP="$(curl -fsS --max-time 5 https://api.ipify.org 2>/dev/null || hostname -I | awk '{print $1}')"

# The SSH_KNOWN_HOSTS secret, pre-baked, because typing it out from a keyscan on
# your own machine is the step people skip. Scanning ourselves over the public
# address is the honest version — it proves the port is actually reachable —
# with a fall back to localhost for a box that cannot route to its own IP.
KNOWN_LINE="$(ssh-keyscan -t ed25519 -p "$SSH_PORT" "$PUBLIC_IP" 2>/dev/null | grep . | head -n1 || true)"
if [ -z "$KNOWN_LINE" ]; then
	KNOWN_LINE="$(ssh-keyscan -t ed25519 -p "$SSH_PORT" localhost 2>/dev/null | grep . | head -n1 | sed "s|^[^ ]*|$PUBLIC_IP|" || true)"
fi
[ -n "$KNOWN_LINE" ] || KNOWN_LINE="(run: ssh-keyscan $PUBLIC_IP)"

printf '\n\033[1m==> done\033[0m\n\n'

if [ -n "$CLONE_PENDING" ]; then
	cat <<EOF
The server is set up. It has no copy of the code yet, because the repository is
private. Add this as a read-only deploy key — GitHub, the repository, Settings,
Deploy keys, Add deploy key, leaving "Allow write access" unchecked:

$(cat "$REPO_KEY.pub")

Then run this script again. It picks up from exactly here.

EOF
	exit 1
fi

cat <<EOF
Left to do, in order:

1. Fill in $APP_DIR/.env — as $DEPLOY_USER, not root:
     ESKIZ_*                   SMS, or nobody can log in
     ATMOS_*                   payments; leave empty and they stay refused
     ALLOWED_ORIGINS           the admin panel's origin
     FCM_SERVICE_ACCOUNT_FILE  put the key in $APP_DIR/secrets/, chmod 644 —
                               the container runs as a uid of its own and
                               cannot read a 600 file through a bind mount
   DEV_MODE, JWT_SECRET, DB_PASSWORD and DOMAIN are already set.

2. Point ${DOMAIN:-the domain}'s A record at $PUBLIC_IP and wait for it to
   resolve. Caddy proves the domain by being connected back to on port 80;
   until that works there is no certificate.

3. Start it:
     sudo -u $DEPLOY_USER -i sh -c 'cd $APP_DIR && docker compose up -d --build'
     sudo -u $DEPLOY_USER -i sh -c 'cd $APP_DIR && docker compose logs -f api'

4. Wire up CI — the repository, Settings, Secrets and variables, Actions:

     SSH_HOST          $PUBLIC_IP
     SSH_USER          $DEPLOY_USER
     SSH_PORT          $SSH_PORT
     SSH_PRIVATE_KEY   the private half of the CI key authorised above
     SSH_KNOWN_HOSTS   the line below

       $KNOWN_LINE

   From then on every push to main runs the tests, builds an image and deploys
   it. Nothing else reaches this machine.

Health, once it is up:  curl https://${DOMAIN:-<domain>}/api/v1/healthz
EOF
