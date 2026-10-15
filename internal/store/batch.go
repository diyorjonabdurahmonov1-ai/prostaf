package store

import (
	"prostaff/internal/models"
)

/**
 * The orders of one "I need N workers" request, taken as a whole.
 *
 * Asked for four, two found: the two who said yes should wait rather than set
 * off for a job that may yet be called off for want of hands. So a batch is
 * "ready" only once every place the customer still wants is filled — or the
 * customer chose to start with the workers found so far — and until then the
 * workers who accepted do not get the customer's phone.
 *
 * Readiness is derived from the rows, not stored as a counter: a place is
 * filled (accepted, in progress, completed), withdrawn by the customer
 * (cancelled with nobody on it), or still open (anything else — searching,
 * expired, declined, or dropped by the worker who had it). `batch_started`
 * pins it once reached, so a worker dropping out later does not snatch the
 * phone back from the ones already on their way.
 */

// BatchState is where a multi-worker request stands.
type BatchState struct {
	// Found is how many places are filled.
	Found int
	// Open is how many places the customer still wants and nobody holds.
	Open int
	// Started is set once the batch went ahead — every place filled, or the
	// customer started with the workers found so far.
	Started bool
}

// Ready reports whether the workers who accepted may start.
func (b BatchState) Ready() bool {
	return b.Started || b.Open == 0
}

// BatchState reads the request batchID belongs to. False when there is none.
func (s *Store) BatchState(batchID string) (BatchState, bool) {
	var b BatchState
	if batchID == "" {
		return b, false
	}
	var total int
	err := s.db.QueryRow(`
		SELECT COUNT(*),
		       COUNT(*) FILTER (WHERE status IN ('accepted','in_progress','completed')),
		       COUNT(*) FILTER (WHERE NOT (status IN ('accepted','in_progress','completed')
		                                   OR (status = 'cancelled' AND worker_id = ''))),
		       COALESCE(BOOL_OR(batch_started), FALSE)
		FROM orders WHERE batch_id = $1`, batchID).
		Scan(&total, &b.Found, &b.Open, &b.Started)
	if err != nil || total == 0 {
		return BatchState{}, false
	}
	return b, true
}

// decorateBatch fills the derived batch fields on an order. cache, when given,
// is shared across one listing so siblings are read once per request.
func (s *Store) decorateBatch(o *models.Order, cache map[string]BatchState) {
	if o == nil || o.BatchID == "" {
		return
	}
	b, ok := cache[o.BatchID]
	if !ok {
		if b, ok = s.BatchState(o.BatchID); !ok {
			return
		}
		if cache != nil {
			cache[o.BatchID] = b
		}
	}
	ready := b.Ready()
	o.BatchReady = &ready
	o.BatchFound = b.Found
}

// WithBatch re-reads the derived batch fields of an order the caller already
// holds — after an accept or a cancel has moved the request on.
func (s *Store) WithBatch(o *models.Order) {
	s.decorateBatch(o, nil)
}

// decorateBatches is decorateBatch over a listing.
func (s *Store) decorateBatches(orders []models.Order) {
	cache := map[string]BatchState{}
	for i := range orders {
		s.decorateBatch(&orders[i], cache)
	}
}

// BatchOrders returns every order of a request, oldest first, decorated.
func (s *Store) BatchOrders(batchID string) []models.Order {
	if batchID == "" {
		return nil
	}
	rows, err := s.db.Query("SELECT "+orderCols+" FROM orders WHERE batch_id=$1 ORDER BY created_at", batchID)
	if err != nil {
		return nil
	}
	var out []models.Order
	for rows.Next() {
		if o, err := s.scanOrder(rows); err == nil {
			out = append(out, *o)
		}
	}
	rows.Close()
	s.decorateBatches(out)
	return out
}

// SettleBatch pins a request as started once its last open place is filled,
// and reports whether this call is the one that did it — the caller then owes
// the waiting workers the news that they can go.
func (s *Store) SettleBatch(batchID string) bool {
	b, ok := s.BatchState(batchID)
	if !ok || b.Started || b.Open > 0 || b.Found == 0 {
		return false
	}
	res, err := s.db.Exec(
		"UPDATE orders SET batch_started=TRUE WHERE batch_id=$1 AND NOT batch_started", batchID)
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n > 0
}

// StartBatch is the customer's "start with the workers I have": every place
// still searching, expired or declined is closed, so nobody else is rung, and
// the request is pinned as started, which releases the customer's phone to
// the workers who accepted.
//
// Returns the orders of the request as they now are, and the ids of the ones
// this call closed. At least one worker must have been found — starting with
// nobody is a cancellation, which is what /orders/{id}/cancel is for.
func (s *Store) StartBatch(batchID, customerID string) ([]models.Order, []string, error) {
	orders := s.BatchOrders(batchID)
	if len(orders) == 0 {
		return nil, nil, ErrNotFound
	}
	for _, o := range orders {
		if o.CustomerID != customerID {
			return nil, nil, ErrForbidden
		}
	}
	if b, _ := s.BatchState(batchID); b.Found == 0 {
		return nil, nil, ErrConflict
	}

	// The photos of the closed places are left alone here: a request's photos
	// are stamped on whichever of its orders claimed them first, and the
	// workers who are starting still need to see them.
	rows, err := s.db.Query(
		`UPDATE orders SET status=$1, invite_expires_at=NULL, closed_at=NOW()
		 WHERE batch_id=$2 AND worker_id=''
		   AND status IN ('pending','matching','invited','expired','rejected')
		 RETURNING id`,
		models.StatusCancelled, batchID)
	if err != nil {
		return nil, nil, ErrConflict
	}
	var closed []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			closed = append(closed, id)
		}
	}
	rows.Close()

	if _, err := s.db.Exec("UPDATE orders SET batch_started=TRUE WHERE batch_id=$1", batchID); err != nil {
		return nil, nil, ErrConflict
	}
	return s.BatchOrders(batchID), closed, nil
}
