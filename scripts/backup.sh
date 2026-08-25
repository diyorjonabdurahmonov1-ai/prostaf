#!/bin/sh
#
# One night's backup of everything PROSTAFF cannot be rebuilt without: the
# database, and the photos.
#
# The two have to be taken together and restored together. A pg_dump on its own
# restores a database full of rows naming files that are not on the disk, which
# does not look like a failed restore — it looks like every avatar on the
# platform going blank at once, some hours later, to everyone.
#
# Install it as a nightly job on the host, not in a container:
#
#   cp scripts/backup.sh /usr/local/bin/prostaff-backup
#   chmod +x /usr/local/bin/prostaff-backup
#   crontab -e
#   17 3 * * * COMPOSE_DIR=/opt/prostaff/backend /usr/local/bin/prostaff-backup >> /var/log/prostaff-backup.log 2>&1
#
# 03:17 rather than 03:00 because every cron job on every server runs at the top
# of the hour and the disk has other things to do then.
#
# What this is not: an offsite backup. A copy on the same disk survives a bad
# migration and a dropped table, and does not survive the provider losing the
# volume — which is the failure that ends companies. Once this is running, add
# a line that ships $BACKUP_DIR somewhere else (rclone to any S3-compatible
# bucket is a one-liner) and check a restore, on a fresh box, before you need it.

set -eu

COMPOSE_DIR="${COMPOSE_DIR:-$(cd "$(dirname "$0")/.." && pwd)}"
BACKUP_DIR="${BACKUP_DIR:-/var/backups/prostaff}"
# How many days of backups to keep. Fourteen is a fortnight of mornings to
# notice something went wrong in, at roughly the size of one dump a day.
KEEP_DAYS="${KEEP_DAYS:-14}"
DB_USER="${DB_USER:-prostaff}"
DB_NAME="${DB_NAME:-prostaff}"
# The Docker volume holding the photos. `docker compose` prefixes volume names
# with the project, which is the compose directory's name unless COMPOSE_PROJECT_NAME
# says otherwise.
UPLOADS_VOLUME="${UPLOADS_VOLUME:-$(basename "$COMPOSE_DIR")_uploads}"

stamp="$(date +%F-%H%M)"
mkdir -p "$BACKUP_DIR"
cd "$COMPOSE_DIR"

db_out="$BACKUP_DIR/db-$stamp.sql.gz"
uploads_out="$BACKUP_DIR/uploads-$stamp.tar.gz"

# Written to .part and renamed only once the command has succeeded. A backup run
# that is killed half way through — the machine reboots, the disk fills — must
# not leave behind a truncated file named exactly like a good one, because the
# night you reach for it is the night you are not in a position to check.
echo "backup: dumping $DB_NAME"
docker compose exec -T db pg_dump -U "$DB_USER" "$DB_NAME" | gzip > "$db_out.part"
mv "$db_out.part" "$db_out"

echo "backup: archiving $UPLOADS_VOLUME"
docker run --rm \
	-v "$UPLOADS_VOLUME":/data:ro \
	-v "$BACKUP_DIR":/out \
	alpine tar czf "/out/$(basename "$uploads_out").part" -C /data .
mv "$uploads_out.part" "$uploads_out"

# A dump of an empty database is a few hundred bytes and compresses to almost
# nothing, so a suspiciously small file is the signal that the dump ran against
# the wrong container, or that DB_NAME is wrong, or that Postgres was still
# starting. Loud, because a backup that fails quietly is worse than none: it is
# none, plus the belief that there is one.
db_size="$(wc -c < "$db_out")"
if [ "$db_size" -lt 10000 ]; then
	echo "backup: WARNING — $db_out is only $db_size bytes, which is not a populated database" >&2
	exit 1
fi

# Old copies go only after the new ones are safely written.
find "$BACKUP_DIR" -name 'db-*.sql.gz' -mtime "+$KEEP_DAYS" -delete
find "$BACKUP_DIR" -name 'uploads-*.tar.gz' -mtime "+$KEEP_DAYS" -delete
# Parts left by a previous run that died. Kept a day so they can be looked at.
find "$BACKUP_DIR" -name '*.part' -mtime +1 -delete

echo "backup: done — $(du -sh "$db_out" "$uploads_out" | tr '\n' ' ')"
