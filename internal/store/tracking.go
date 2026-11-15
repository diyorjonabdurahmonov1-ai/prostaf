package store

import (
	"database/sql"
	"time"

	"prostaff/internal/models"
)

// Trail is where the worker last said they were on the way to a customer.
type Trail struct {
	Lat, Lng float64
	At       time.Time
}

// validPoint rejects what a phone can report by accident: nothing at all (0,0)
// and numbers that are not coordinates.
func validPoint(lat, lng float64) bool {
	if lat < -90 || lat > 90 || lng < -180 || lng > 180 {
		return false
	}
	return lat != 0 || lng != 0
}

// MarkDeparted is the worker saying "I'm on my way". It starts the customer's
// live map; the first position (when the phone has one) rides along.
func (s *Store) MarkDeparted(orderID, workerUserID string, lat, lng float64) (*models.Order, error) {
	o, ok := s.GetOrder(orderID)
	if !ok {
		return nil, ErrNotFound
	}
	if o.WorkerID == "" || o.WorkerID != workerUserID {
		return nil, ErrForbidden
	}
	if o.DepartedAt != nil || o.ArrivedAt != nil {
		return o, nil
	}
	res, err := s.db.Exec(
		`UPDATE orders SET departed_at=NOW()
		 WHERE id=$1 AND worker_id=$2 AND status IN ('accepted','in_progress')
		   AND departed_at IS NULL AND arrived_at IS NULL`,
		orderID, workerUserID)
	if err != nil {
		return nil, ErrConflict
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrConflict
	}
	if validPoint(lat, lng) {
		_ = s.SetTrail(orderID, workerUserID, lat, lng)
	}
	o, _ = s.GetOrder(orderID)
	return o, nil
}

// SetTrail stores the worker's latest position for an order they are on the way
// to. Only while they are en route: before setting off and after arriving, the
// customer has no business knowing where the worker's phone is.
func (s *Store) SetTrail(orderID, workerUserID string, lat, lng float64) error {
	if !validPoint(lat, lng) {
		return ErrBadRequest
	}
	res, err := s.db.Exec(
		`UPDATE orders SET trail_lat=$3, trail_lng=$4, trail_at=NOW()
		 WHERE id=$1 AND worker_id=$2 AND status IN ('accepted','in_progress')
		   AND departed_at IS NOT NULL AND arrived_at IS NULL`,
		orderID, workerUserID, lat, lng)
	if err != nil {
		return ErrConflict
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrConflict
	}
	return nil
}

// GetTrail returns the last reported position, or false when there is none.
func (s *Store) GetTrail(orderID string) (Trail, bool) {
	var lat, lng sql.NullFloat64
	var at sql.NullTime
	err := s.db.QueryRow(`SELECT trail_lat, trail_lng, trail_at FROM orders WHERE id=$1`, orderID).Scan(&lat, &lng, &at)
	if err != nil || !lat.Valid || !lng.Valid || !at.Valid {
		return Trail{}, false
	}
	return Trail{Lat: lat.Float64, Lng: lng.Float64, At: at.Time}, true
}
