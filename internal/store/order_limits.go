package store

import (
	"math"
	"prostaff/internal/models"
	"strings"
	"unicode/utf8"
)

// Limits on what a customer may put into an order or a rating.
//
// Every field below used to be taken as typed. A price of 9 quintillion would
// overflow the commission arithmetic, a description of a few megabytes was
// pushed to every worker's phone, and nothing stopped one account from filing
// hundreds of open orders and ringing the whole city with them.
const (
	// maxOrderPrice is a billion so'm: far above any real job, far below the
	// point where percentages of it stop fitting in an int64.
	maxOrderPrice = 1_000_000_000
	// maxOrderDescription is characters, not bytes.
	maxOrderDescription = 2000
	maxOrderPhone       = 20
	maxBatchIDLen       = 64
	maxBatchSizeAllowed = 20
	// maxOpenOrders is how many unfinished orders one customer may have at a time.
	maxOpenOrders = 30
	// maxRatingComment is characters, not bytes.
	maxRatingComment = 500
)

// validateOrderInput refuses what no real order contains. It does not know the
// database, so it can be tested on its own.
func validateOrderInput(in OrderInput) error {
	if in.Price <= 0 || in.Price > maxOrderPrice {
		return ErrBadRequest
	}
	if utf8.RuneCountInString(in.Description) > maxOrderDescription {
		return ErrBadRequest
	}
	if len(strings.TrimSpace(in.Phone)) > maxOrderPhone {
		return ErrBadRequest
	}
	if math.IsNaN(in.Lat) || math.IsNaN(in.Lng) || math.IsInf(in.Lat, 0) || math.IsInf(in.Lng, 0) ||
		in.Lat < -90 || in.Lat > 90 || in.Lng < -180 || in.Lng > 180 {
		return ErrBadRequest
	}
	if len(in.BatchID) > maxBatchIDLen || in.BatchSize < 0 || in.BatchSize > maxBatchSizeAllowed {
		return ErrBadRequest
	}
	return nil
}

// cleanRatingComment trims a comment and keeps it to a sensible length.
func cleanRatingComment(c string) string {
	c = strings.TrimSpace(c)
	if utf8.RuneCountInString(c) > maxRatingComment {
		r := []rune(c)
		c = string(r[:maxRatingComment])
	}
	return c
}

// openOrderCount is how many unfinished orders this customer has.
func (s *Store) openOrderCount(customerID string) int {
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM orders WHERE customer_id=$1
		AND status IN ('pending','matching','invited','accepted','in_progress')`, customerID).Scan(&n)
	return n
}

// batchBelongsToOther reports whether this batch id is already used by somebody
// else's orders. The id is minted by the app, so it could otherwise be guessed
// or copied to attach an order to another customer's request.
func (s *Store) batchBelongsToOther(batchID, customerID string) bool {
	if batchID == "" {
		return false
	}
	var other int
	s.db.QueryRow(`SELECT COUNT(*) FROM orders WHERE batch_id=$1 AND customer_id<>$2`, batchID, customerID).Scan(&other)
	return other > 0
}

// insertRatingOnce writes a rating unless this person already rated this order.
func (s *Store) insertRatingOnce(r *models.Rating) error {
	tx, err := s.db.Begin()
	if err != nil {
		return ErrConflict
	}
	defer tx.Rollback()
	tx.Exec(`SELECT pg_advisory_xact_lock(hashtext($1))`, "rate:"+r.OrderID+":"+r.FromUserID)
	var exists bool
	tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM ratings WHERE order_id=$1 AND from_user_id=$2)`,
		r.OrderID, r.FromUserID).Scan(&exists)
	if exists {
		return ErrAlreadyRated
	}
	if _, err := tx.Exec(`INSERT INTO ratings (id, order_id, from_user_id, to_user_id, target_role, stars, comment, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		r.ID, r.OrderID, r.FromUserID, r.ToUserID, r.TargetRole, r.Stars, r.Comment, r.CreatedAt); err != nil {
		return ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return ErrConflict
	}
	return nil
}

// highestMinPrice is the largest minimum price, among the given worker
// profiles, that sits above price — 0 when every one of them accepts it.
func (s *Store) highestMinPrice(workerIDs []string, price int64) int64 {
	var top int64
	for _, id := range workerIDs {
		var min int64
		if s.db.QueryRow("SELECT price_min FROM workers WHERE id=$1", id).Scan(&min) != nil {
			continue
		}
		if min > price && min > top {
			top = min
		}
	}
	return top
}
