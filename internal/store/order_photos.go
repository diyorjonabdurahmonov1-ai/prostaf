package store

import (
	"database/sql"
	"log"
	"strconv"
	"strings"
	"time"

	"prostaff/internal/models"
)

// Deleting a finished job's photos, and only a job's photos.
//
// The platform's disk is filled by one thing. A profile photo is written once
// and read for as long as the account exists; an order photo is looked at by
// the worker deciding whether to take the job, once, and then never again — and
// there are six of them per job and a new job every few minutes. At a hundred
// thousand accounts the avatars are tens of gigabytes and the job photos are a
// terabyte, so the job photos are what has to go.
//
// The whole design is one column. `files.order_id` is stamped when a photo is
// attached to an order and is empty on every other upload, so the delete is
// `WHERE order_id = $1` — an indexed lookup that cannot, by construction, reach
// an avatar. The alternative (matching order image URLs against every profile
// column) is both slower and wrong the first time the two happen to overlap.

// terminalOrderStatuses are the states a job does not come back from. Photos
// are only ever deleted for an order in one of these.
var terminalOrderStatuses = []string{
	models.StatusCompleted,
	models.StatusCancelled,
	models.StatusExpired,
}

// fileIDFromURL pulls the file id out of an upload URL.
//
// Uploads are handed back as `https://host/api/v1/files/<id>` (see
// api.handleUpload), so the id is the last segment. Anything else — a URL from
// somewhere else entirely, a seeded avatar pointing at a stock photo — yields
// something that matches no row, and the queries below simply do nothing with
// it. Query parameters and a trailing slash are tolerated because a client that
// appends a cache-buster is not wrong.
func fileIDFromURL(rawURL string) string {
	u := strings.TrimSpace(rawURL)
	if u == "" {
		return ""
	}
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	u = strings.TrimRight(u, "/")
	if i := strings.LastIndex(u, "/"); i >= 0 {
		u = u[i+1:]
	}
	// An id is a UUIDv4 from internal/id. Checking the shape keeps a stray path
	// segment from being sent to the database as a candidate id.
	if len(u) != 36 {
		return ""
	}
	return u
}

// tagOrderPhotos records that these photos belong to this order, so that
// closing the order can delete them.
//
// Three conditions decide whether a file is claimable, and each is a way the
// wrong photo would otherwise be deleted:
//
//   - `user_id = uploader` — a customer may only attach their own uploads. Not
//     a tidiness rule: without it, putting somebody else's avatar URL in an
//     order and then cancelling the order deletes that person's photo.
//   - an empty `order_id` — a file already belonging to an order is not moved
//     to a second one, so cancelling the second cannot blank the first.
//   - not currently a profile photo of the uploader — the one case the first
//     condition cannot catch, since those files are the customer's own. Each
//     check is keyed on the single user id, so it reads one row per table
//     rather than scanning the platform.
//
// Failure is logged, never returned: the order is already written and worth
// more than its cleanup. An untagged photo is a file that is kept forever,
// which is the behaviour this whole file replaces — a safe way to fail.
func (s *Store) tagOrderPhotos(orderID, uploaderID string, urls []string) {
	if !s.cfg.PurgeOrderPhotos || orderID == "" || uploaderID == "" {
		return
	}
	ids := make([]string, 0, len(urls))
	seen := make(map[string]bool, len(urls))
	for _, u := range urls {
		fileID := fileIDFromURL(u)
		if fileID == "" || seen[fileID] {
			continue
		}
		seen[fileID] = true
		ids = append(ids, fileID)
	}
	if len(ids) == 0 {
		return
	}

	// $1 order, $2 uploader, $3… the file ids.
	args := make([]any, 0, len(ids)+2)
	args = append(args, orderID, uploaderID)
	placeholders := make([]string, len(ids))
	for i, fileID := range ids {
		placeholders[i] = "$" + strconv.Itoa(i+3)
		args = append(args, fileID)
	}

	// `LIKE '%' || f.id` rather than an equality on the URL: the row stores the
	// full URL and the host in it depends on how the app reached the server, so
	// the id is the only part that can be compared reliably.
	q := `UPDATE files f SET order_id = $1
	      WHERE f.id IN (` + strings.Join(placeholders, ",") + `)
	        AND f.user_id = $2
	        AND f.order_id = ''
	        AND NOT EXISTS (SELECT 1 FROM users u
	              WHERE u.id = $2 AND u.avatar_url LIKE '%' || f.id)
	        AND NOT EXISTS (SELECT 1 FROM workers w
	              WHERE w.user_id = $2
	                AND (w.avatar_url LIKE '%' || f.id OR w.images LIKE '%' || f.id || '%'))
	        AND NOT EXISTS (SELECT 1 FROM companies c
	              WHERE c.user_id = $2
	                AND (c.avatar_url LIKE '%' || f.id OR c.images LIKE '%' || f.id || '%'))`

	if _, err := s.db.Exec(q, args...); err != nil {
		log.Printf("order photos: could not tag %d file(s) for order %s: %v", len(ids), orderID, err)
	}
}

// PurgeOrderPhotos deletes the photos of one order and empties its photo list.
// Returns how many files were removed.
//
// The order of operations is what makes an interrupted run harmless: the bytes
// go first, then the row, then the order's URLs. A crash anywhere leaves rows
// or URLs pointing at something already deleted — a 404 on a photo of a job
// that is over — and the next sweep finishes the job. The reverse order would
// clear the URLs first and leave files nothing points at and nothing will ever
// look for, which is a leak no sweep can find.
func (s *Store) PurgeOrderPhotos(orderID string) int {
	if !s.cfg.PurgeOrderPhotos || orderID == "" {
		return 0
	}

	rows, err := s.db.Query(
		`SELECT id, COALESCE(path,'') FROM files WHERE order_id = $1`, orderID)
	if err != nil {
		log.Printf("order photos: listing files of order %s: %v", orderID, err)
		return 0
	}
	type stored struct{ id, path string }
	var files []stored
	for rows.Next() {
		var f stored
		if err := rows.Scan(&f.id, &f.path); err != nil {
			rows.Close()
			log.Printf("order photos: scanning files of order %s: %v", orderID, err)
			return 0
		}
		files = append(files, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		log.Printf("order photos: reading files of order %s: %v", orderID, err)
		return 0
	}

	removed := 0
	for _, f := range files {
		if f.path != "" {
			if err := s.blobs.Remove(f.path); err != nil {
				// Leave the row alone: it still names a file that is still there,
				// and the next sweep will try again. Dropping the row here would
				// make the file unreachable and undeletable at the same time.
				log.Printf("order photos: %s (%s): %v", f.id, f.path, err)
				continue
			}
		}
		if _, err := s.db.Exec("DELETE FROM files WHERE id = $1", f.id); err != nil {
			log.Printf("order photos: deleting row %s: %v", f.id, err)
			continue
		}
		removed++
	}

	// Unconditional, and deliberately not skipped when nothing was removed: an
	// order whose files were deleted by an earlier interrupted run still has the
	// dead URLs on it, and this is what finally clears them. Without it the
	// sweeper would find the same order on every pass, forever.
	if _, err := s.db.Exec(
		`UPDATE orders SET images = '[]', image_url = '' WHERE id = $1`, orderID); err != nil {
		log.Printf("order photos: clearing order %s: %v", orderID, err)
	}
	return removed
}

// purgeOnClose deletes a job's photos the moment the job ends, which is what
// happens with no grace period configured. With one set this does nothing and
// the sweeper picks the order up once the days have passed.
//
// Called on the request that closes the order. The deletes are a handful of
// unlinks and one small transaction, so they stay on that request rather than
// going to a goroutine: the customer's phone gets its reply a millisecond later
// and the platform never has work in flight that a restart could lose.
//
// It empties the photo fields on the order it is given, so that the reply to
// the request that closed the job describes the job as it now is. Skipping that
// hands the app a list of URLs that were deleted a moment ago, and it renders
// them as broken images until something refetches.
func (s *Store) purgeOnClose(o *models.Order) {
	if o == nil || !s.cfg.PurgeOrderPhotos || s.cfg.OrderPhotoGraceDays > 0 {
		return
	}
	s.PurgeOrderPhotos(o.ID)
	o.Images = []string{}
	o.ImageURL = ""
}

// purgeSweepLimit is how many orders one sweep will clean.
//
// A cap rather than "all of them" because the first run after this ships has a
// backlog of every order ever closed, and deleting a hundred thousand files in
// one pass is a stall long enough to look like an outage. At this size the
// backlog clears over a few sweeps and steady state is a handful per pass.
const purgeSweepLimit = 500

// PurgeClosedOrderPhotos deletes the photos of orders that are over, and
// returns how many files it removed.
//
// Closing an order deletes its photos directly — this is the net underneath
// that. It catches the orders closed in bulk by ExpireStaleSearches, the ones
// whose delete failed on a full or briefly unwritable disk, and, on the first
// run after this ships, every order that was closed before any of it existed.
func (s *Store) PurgeClosedOrderPhotos(grace time.Duration) int {
	if !s.cfg.PurgeOrderPhotos {
		return 0
	}

	// `closed_at IS NULL` is an order closed before the column existed. Those
	// are the backlog and are as stale as an order gets, so the grace period
	// does not apply to them.
	rows, err := s.db.Query(
		`SELECT id FROM orders
		 WHERE status = ANY($1::text[])
		   AND images <> '[]'
		   AND (closed_at IS NULL OR closed_at <= NOW() - $2::interval)
		 ORDER BY closed_at NULLS FIRST
		 LIMIT $3`,
		pgTextArray(terminalOrderStatuses),
		strconv.FormatInt(int64(grace/time.Second), 10)+" seconds",
		purgeSweepLimit)
	if err != nil {
		log.Printf("order photos: sweep query: %v", err)
		return 0
	}
	var ids []string
	for rows.Next() {
		var orderID string
		if err := rows.Scan(&orderID); err != nil {
			rows.Close()
			log.Printf("order photos: sweep scan: %v", err)
			return 0
		}
		ids = append(ids, orderID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		log.Printf("order photos: sweep read: %v", err)
		return 0
	}

	removed := 0
	for _, orderID := range ids {
		removed += s.PurgeOrderPhotos(orderID)
	}
	if removed > 0 {
		log.Printf("order photos: removed %d file(s) from %d closed order(s)", removed, len(ids))
	}
	return removed
}

// CountPurgeableOrders is how many closed orders are still carrying photos.
// Reported at startup so a backlog is a number in the log rather than a
// surprise on the disk graph.
func (s *Store) CountPurgeableOrders() int {
	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM orders WHERE status = ANY($1::text[]) AND images <> '[]'`,
		pgTextArray(terminalOrderStatuses)).Scan(&n); err != nil && err != sql.ErrNoRows {
		return 0
	}
	return n
}

// pgTextArray renders a string slice as a Postgres array literal.
//
// The driver is reached through database/sql, which has no notion of an array
// parameter, so `= ANY($1)` needs the literal form. The only values passed here
// are the status constants above, but the quoting is done properly regardless —
// a helper that is only safe for its current callers stops being safe the first
// time somebody finds it useful somewhere else.
func pgTextArray(vals []string) string {
	quoted := make([]string, len(vals))
	for i, v := range vals {
		quoted[i] = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v) + `"`
	}
	return "{" + strings.Join(quoted, ",") + "}"
}
