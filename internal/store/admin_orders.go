package store

import (
	"strings"

	"prostaff/internal/models"
)

// What the office may do to an order that has gone wrong: call it off, close
// it, send it round again, or hand it to a worker by name. Each goes through
// the same guarded transitions the apps use, so the office cannot put an order
// into a state the apps could not have.

// AdminCancel calls an order off from the office, with the operator's reason.
func (s *Store) AdminCancel(orderID, reason string) (*models.Order, error) {
	o, ok := s.GetOrder(orderID)
	if !ok {
		return nil, ErrNotFound
	}
	if o.Status == models.StatusCancelled {
		return nil, ErrConflict
	}
	reason = strings.TrimSpace(reason)
	if len(reason) > 500 {
		reason = reason[:500]
	}
	return s.cancel(o, CancelledByAdmin, reason)
}

// AdminComplete closes a job that is under way, for the customer and worker
// who both forgot to.
func (s *Store) AdminComplete(orderID string) (*models.Order, error) {
	o, ok := s.GetOrder(orderID)
	if !ok {
		return nil, ErrNotFound
	}
	return s.complete(o)
}

// AdminRedispatch re-opens the search on an order nobody is working on.
func (s *Store) AdminRedispatch(orderID string) (*models.Order, error) {
	o, ok := s.GetOrder(orderID)
	if !ok {
		return nil, ErrNotFound
	}
	return s.reopen(o)
}

// AdminAssign hands an order nobody is working on to one worker, by the
// worker's user id (a profile id is accepted too).
//
// The worker is charged exactly as if they had accepted it themselves: a free
// trial job is spent first, otherwise the commission comes off the wallet. So
// the same rules apply — not banned, not already on another job, and able to
// pay the commission.
func (s *Store) AdminAssign(orderID, worker string) (*models.Order, *models.WorkerProfile, error) {
	o, ok := s.GetOrder(orderID)
	if !ok {
		return nil, nil, ErrNotFound
	}
	if o.CompanyID != "" {
		return nil, nil, ErrConflict
	}
	wp, ok := s.GetWorkerByUser(worker)
	if !ok {
		uid, found := s.WorkerUserID(worker)
		if !found {
			return nil, nil, ErrNotFound
		}
		if wp, ok = s.GetWorkerByUser(uid); !ok {
			return nil, nil, ErrNotFound
		}
	}
	if wp.Banned || wp.UserID == o.CustomerID {
		return nil, nil, ErrForbidden
	}
	if s.WorkerIsBusy(wp.UserID) {
		return nil, nil, ErrConflict
	}
	if !s.canWorkerAccept(wp) {
		return nil, nil, ErrInsufficientBalance
	}
	base := o.Price
	if base <= 0 {
		base = wp.PriceMin
	}
	commission := base * int64(s.settings.CommissionPercent) / 100
	// Whatever left the order without a worker — expired, declined, dropped —
	// reopen clears it; an order still searching is taken as it stands.
	switch o.Status {
	case models.StatusPending, models.StatusMatching, models.StatusInvited:
	default:
		if _, err := s.reopen(o); err != nil {
			return nil, nil, err
		}
	}
	res, err := s.db.Exec(
		`UPDATE orders SET status=$1, worker_id=$2, worker_phone=$3, invite_expires_at=NULL,
		        commission_amount=$4, responded_at=NOW()
		 WHERE id=$5 AND worker_id='' AND status IN ('pending','matching','invited')`,
		models.StatusAccepted, wp.UserID, wp.Phone, commission, orderID)
	if err != nil {
		return nil, nil, ErrConflict
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, nil, ErrConflict
	}
	// Charged only once the order is theirs, as in Accept.
	if charged := s.chargeWorkerAcceptance(wp, commission); charged != commission {
		s.db.Exec("UPDATE orders SET commission_amount=$1 WHERE id=$2", charged, orderID)
	}
	o, _ = s.GetOrder(orderID)
	return o, wp, nil
}

// BroadcastRecipients is the user ids a message from the office goes to.
// audience is "workers", "companies", "customers" or "all"; onlyOnline narrows
// workers and companies to those taking orders right now. Deactivated accounts,
// banned providers and the office's own accounts are left out.
func (s *Store) BroadcastRecipients(audience string, onlyOnline bool) []string {
	var q string
	switch audience {
	case "workers":
		q = `SELECT u.id FROM users u JOIN workers w ON w.user_id = u.id
		     WHERE u.active AND NOT w.banned`
		if onlyOnline {
			q += " AND w.online"
		}
	case "companies":
		q = `SELECT u.id FROM users u JOIN companies c ON c.user_id = u.id
		     WHERE u.active AND NOT c.banned`
		if onlyOnline {
			q += " AND c.online"
		}
	case "customers":
		q = `SELECT id FROM users WHERE active AND roles LIKE '%"customer"%' AND roles NOT LIKE '%"admin"%'`
	case "all":
		q = `SELECT id FROM users WHERE active AND roles NOT LIKE '%"admin"%'`
	default:
		return nil
	}
	rows, err := s.db.Query(q)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			out = append(out, id)
		}
	}
	return out
}
