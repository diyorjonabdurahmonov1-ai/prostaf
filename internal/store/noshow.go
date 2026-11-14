package store

import (
	"time"

	"prostaff/internal/id"
	"prostaff/internal/models"
)

// What happens between a worker taking a job and the job being done: the
// commission coming back when somebody else calls it off, the worker saying
// they have arrived, and the customer saying nobody came.

// refundAcceptance gives back what the worker paid to take the order: the
// commission to their wallet, or the free job if that is what paid for it.
// It reads and clears the amount in one statement, so it cannot pay twice.
func (s *Store) refundAcceptance(orderID, workerUserID string) (int64, bool) {
	var amount int64
	var onTrial bool
	err := s.db.QueryRow(`
		WITH old AS (SELECT commission_amount AS c, free_trial_used AS t FROM orders WHERE id=$1 FOR UPDATE)
		UPDATE orders SET commission_refunded = commission_refunded + old.c, commission_amount = 0,
		                  free_trial_used = FALSE
		FROM old WHERE orders.id = $1
		RETURNING old.c, old.t`, orderID).Scan(&amount, &onTrial)
	if err != nil {
		return 0, false
	}
	if amount > 0 {
		s.db.Exec("UPDATE wallets SET balance = balance + $1 WHERE user_id = $2", amount, workerUserID)
	}
	if onTrial {
		if wp, ok := s.GetWorkerByUser(workerUserID); ok {
			if key := WorkerTrialKey(wp.PassportSeries); key != "" {
				s.db.Exec("UPDATE free_trials SET used = used - 1 WHERE key = $1 AND used > 0", key)
			}
		}
	}
	return amount, onTrial
}

// setNoShowAfter works out from when the customer may report a no-show.
func (s *Store) setNoShowAfter(o *models.Order) {
	if o.Status != models.StatusAccepted || o.WorkerID == "" || o.CompanyID != "" ||
		o.ArrivedAt != nil || o.RespondedAt == nil {
		return
	}
	minutes := s.settings.NoShowMinutes
	if minutes < 1 {
		minutes = defaultNoShowMinutes
	}
	t := o.RespondedAt.Add(time.Duration(minutes) * time.Minute)
	o.NoShowAfter = &t
}

// MarkArrived is the worker saying they have reached the customer. After it
// the customer can no longer report that nobody came.
func (s *Store) MarkArrived(orderID, workerUserID string) (*models.Order, error) {
	o, ok := s.GetOrder(orderID)
	if !ok {
		return nil, ErrNotFound
	}
	if o.WorkerID == "" || o.WorkerID != workerUserID {
		return nil, ErrForbidden
	}
	if o.ArrivedAt != nil {
		return o, nil
	}
	res, err := s.db.Exec(
		`UPDATE orders SET arrived_at=NOW()
		 WHERE id=$1 AND worker_id=$2 AND status IN ('accepted','in_progress') AND arrived_at IS NULL`,
		orderID, workerUserID)
	if err != nil {
		return nil, ErrConflict
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrConflict
	}
	o, _ = s.GetOrder(orderID)
	return o, nil
}

// ReportNoShow is the customer saying the worker who took the job never came.
//
// The job is taken off that worker and put straight back into the search for
// everybody else. The worker keeps nothing back — the commission they paid is
// forfeited — and the report is counted against them for the office to see.
//
// Only after the waiting time, and never once the worker has said they
// arrived: at that point it is a disagreement, and that is the office's to
// settle, not a button's. It returns the re-opened order and the user id of
// the worker it was taken from.
func (s *Store) ReportNoShow(orderID, customerID string) (*models.Order, string, error) {
	o, ok := s.GetOrder(orderID)
	if !ok {
		return nil, "", ErrNotFound
	}
	if o.CustomerID != customerID {
		return nil, "", ErrForbidden
	}
	if o.NoShowAfter == nil {
		return nil, "", ErrConflict
	}
	if time.Now().Before(*o.NoShowAfter) {
		return nil, "", ErrTooEarly
	}
	// A team still being gathered has not been sent anywhere yet.
	if o.BatchID != "" {
		if b, ok := s.BatchState(o.BatchID); ok && !b.Ready() {
			return nil, "", ErrConflict
		}
	}
	worker := o.WorkerID
	dropped := []string{}
	if wp, ok := s.GetWorkerByUser(worker); ok {
		dropped = append(dropped, wp.ID)
	}
	expires := time.Now().Add(InviteTTL)
	res, err := s.db.Exec(
		`UPDATE orders SET status=$1, invited_worker_ids='[]', rejected_worker_ids=$2, invite_expires_at=$3,
		        worker_id='', worker_phone='', responded_at=NULL, closed_at=NULL,
		        commission_forfeited=commission_forfeited+commission_amount, commission_amount=0,
		        free_trial_used=FALSE
		 WHERE id=$4 AND status='accepted' AND worker_id=$5 AND arrived_at IS NULL`,
		models.StatusMatching, marshalJSON(dropped), expires, orderID, worker)
	if err != nil {
		return nil, "", ErrConflict
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, "", ErrConflict
	}
	s.db.Exec("INSERT INTO worker_no_shows (id, order_id, worker_user_id) VALUES ($1,$2,$3)",
		id.New(), orderID, worker)
	o, _ = s.GetOrder(orderID)
	return o, worker, nil
}

// NoShowCount is how many times customers reported this worker never came.
func (s *Store) NoShowCount(workerUserID string) int {
	var n int
	s.db.QueryRow("SELECT COUNT(*) FROM worker_no_shows WHERE worker_user_id=$1", workerUserID).Scan(&n)
	return n
}
