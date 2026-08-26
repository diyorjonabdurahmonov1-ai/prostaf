#!/bin/sh
#
# One deploy. Runs on the server, piped in over ssh by
# .github/workflows/deploy.yml — and safe to run by hand, which is what a
# rollback is:
#
#   IMAGE=ghcr.io/samandarturgunboyev/prostaf IMAGE_TAG=sha-abc123def456 \
#     sh scripts/deploy.sh
#
# What a deploy is here: fast-forward the checkout so that the compose file and
# the Caddyfile match the commit, pull the image the workflow has just built,
# and recreate whatever differs. Normally that is the API alone — `up -d`
# leaves Postgres and Caddy running untouched unless their configuration
# actually changed, and neither restarting Postgres nor asking Let's Encrypt
# for another certificate is something to do on every push.

set -eu

COMPOSE_DIR="${COMPOSE_DIR:-/opt/prostaff/backend}"
IMAGE="${IMAGE:?IMAGE is required, e.g. ghcr.io/samandarturgunboyev/prostaf}"
IMAGE_TAG="${IMAGE_TAG:?IMAGE_TAG is required, e.g. sha-abc123def456}"
# How long to wait for the new container to report itself healthy. The
# Dockerfile's healthcheck starts asking after 10s at 30s intervals, and the
# first thing the server does is connect to Postgres and run migrations.
HEALTH_TIMEOUT="${HEALTH_TIMEOUT:-120}"
# Images kept on the disk. A rollback is only instant for as long as the image
# it rolls back to is still here; three is a couple of deploys' worth of room
# on a box where disk is the first thing to run out.
KEEP_IMAGES="${KEEP_IMAGES:-3}"

cd "$COMPOSE_DIR"

# The compose file and the Caddyfile are in the repo, so a deploy that only
# swapped the image would run new code under an old configuration — and the
# gap would show up as whichever setting the commit had changed silently not
# taking effect.
if [ -n "$(git status --porcelain)" ]; then
	echo "deploy: WARNING — uncommitted changes on the server, discarding:"
	git status --porcelain
fi
git fetch --quiet origin main
git reset --quiet --hard origin/main
# .env and secrets/ are gitignored, so neither of them is touched by that.

# A token that lives as long as the workflow run that sent it. Deliberately not
# a credential stored on the server: one of those makes a compromise of the box
# a compromise of the registry as well.
if [ -n "${GHCR_TOKEN:-}" ]; then
	echo "$GHCR_TOKEN" | docker login ghcr.io -u "${GHCR_USER:-x}" --password-stdin >/dev/null
fi

export IMAGE IMAGE_TAG
echo "deploy: pulling $IMAGE:$IMAGE_TAG"
docker compose pull --quiet api

echo "deploy: starting"
docker compose up -d --remove-orphans

# Wait for the container to say it is actually serving. `restart:
# unless-stopped` means one that crashes on boot is restarted, and restarted,
# and restarted: without this check a deploy that cannot reach Postgres or has
# a broken migration reports success here and is discovered by a user instead.
cid="$(docker compose ps -q api)"
healthy=""
waited=0
while [ "$waited" -lt "$HEALTH_TIMEOUT" ]; do
	state="$(docker inspect -f '{{.State.Health.Status}}' "$cid" 2>/dev/null || echo starting)"
	case "$state" in
	healthy)
		healthy=yes
		break
		;;
	unhealthy)
		break
		;;
	esac
	waited=$((waited + 2))
	sleep 2
done

if [ -z "$healthy" ]; then
	echo "deploy: FAILED — api did not become healthy in ${HEALTH_TIMEOUT}s" >&2
	docker compose logs --tail=80 api >&2
	echo >&2
	echo "deploy: to go back, with the previous tag from the last successful run:" >&2
	echo "  cd $COMPOSE_DIR && IMAGE='$IMAGE' IMAGE_TAG=<previous-sha> docker compose up -d api" >&2
	exit 1
fi
echo "deploy: api healthy"

# The other half of the check, through Caddy — which is what the phone actually
# talks to. This is what catches a certificate that did not renew or a proxy
# pointed at the wrong port, neither of which makes the container unhealthy. A
# warning and not a failure: a server resolving its own public domain is common
# but not guaranteed, and that is not a reason to fail a good deploy.
domain="$(sed -n 's/^DOMAIN=//p' .env 2>/dev/null | tr -d '"' | head -n1)"
# A DOMAIN of `:80` — a bare port — is Caddy serving plain http with no name to
# reach it by, which is how a server on an IP address alone runs. There is no
# https URL to check then, so the check goes over http on the loopback: still
# through Caddy, so it still catches a proxy pointed at the wrong port, and it
# does not fail every deploy with a warning about a certificate nobody asked
# for.
case "$domain" in
"") url="" ;;
:80) url="http://localhost/api/v1/healthz" ;;
:*) url="http://localhost:${domain#:}/api/v1/healthz" ;;
*) url="https://$domain/api/v1/healthz" ;;
esac
if [ -n "$url" ]; then
	if curl -fsS --max-time 10 "$url" >/dev/null 2>&1; then
		echo "deploy: $url ok"
	else
		echo "deploy: WARNING — the container is healthy but $url did not answer" >&2
	fi
fi

# Old images, newest $KEEP_IMAGES kept. `latest` is excluded because it is a
# second name for one of the tags already in the list, and removing a name the
# compose file's default refers to would leave a manual `docker compose up`
# with nothing to start.
docker images "$IMAGE" --format '{{.Tag}} {{.CreatedAt}}' \
	| grep -v '^latest ' \
	| sort -k2 -r \
	| tail -n "+$((KEEP_IMAGES + 1))" \
	| cut -d' ' -f1 \
	| while read -r old; do
		[ -n "$old" ] || continue
		docker rmi "$IMAGE:$old" >/dev/null 2>&1 || true
	done
docker image prune -f >/dev/null

if [ -n "${GHCR_TOKEN:-}" ]; then
	docker logout ghcr.io >/dev/null 2>&1 || true
fi

echo "deploy: done — $IMAGE:$IMAGE_TAG"
