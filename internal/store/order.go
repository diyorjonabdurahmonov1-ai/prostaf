package store

import (
	"database/sql"
	"sort"
	"time"

	"prostaff/internal/geo"
	"prostaff/internal/id"
	"prostaff/internal/models"
)

// InviteTTL is how long the search for a worker stays open before the order
// auto-expires.
const InviteTTL = 120 * time.Second

// DispatchInterval is how often an open order re-broadcasts, picking up workers
// who came online or finished a job after the first wave went out.
const DispatchInterval = 12 * time.Second

// OrderInput is the customer order-creation payload.
type OrderInput struct {
	CategoryID  string
	Gender      string
	Description string
	ImageURL    string
	Phone       string
	Price       int64
	Lat         float64
	Lng         float64
}

// A worker or company is "busy" while one of their orders is accepted or
// in_progress; until it is completed they cannot be given another job.

// WorkerIsBusy reports whether the worker is already on a job.
func (s *Store) WorkerIsBusy(workerUserID string) bool {
	var n int
	err := s.db.QueryRow(
		"SELECT COUNT(*) FROM orders WHERE worker_id=$1 AND status IN ('accepted','in_progress')",
		workerUserID).Scan(&n)
	return err == nil && n > 0
}

// CompanyIsBusy reports whether the company is already on a job.
func (s *Store) CompanyIsBusy(companyID string) bool {
	var n int
	err := s.db.QueryRow(
		"SELECT COUNT(*) FROM orders WHERE company_id=$1 AND status IN ('accepted','in_progress')",
		companyID).Scan(&n)
	return err == nil && n > 0
}

func (s *Store) scanOrder(row interface{ Scan(dest ...any) error }) (*models.Order, error) {
	var o models.Order
	var invitedIDs, rejectedIDs string
	var inviteExpires sql.NullTime
	err := row.Scan(&o.ID, &o.CustomerID, &o.WorkerID, &o.CompanyID, &o.CategoryID,
		&o.Gender, &o.Description, &o.ImageURL, &o.CustomerPhone, &o.WorkerPhone,
		&o.Lat, &o.Lng, &o.Status, &invitedIDs, &inviteExpires, &o.CommissionAmount,
		&o.CreatedAt, &o.CustomerName, &o.Price, &rejectedIDs)
	if err != nil {
		return nil, err
	}
	o.InvitedWorkerIDs = unmarshalStrings(invitedIDs)
	o.RejectedWorkerIDs = unmarshalStrings(rejectedIDs)
	if inviteExpires.Valid {
		o.InviteExpiresAt = &inviteExpires.Time
	}
	return &o, nil
}

const orderCols = "id, customer_id, worker_id, company_id, category_id, gender, description, image_url, customer_phone, worker_phone, lat, lng, status, invited_worker_ids, invite_expires_at, commission_amount, created_at, customer_name, price, rejected_worker_ids"

// CreateOrder creates a pending order for a customer.
func (s *Store) CreateOrder(customerID string, in OrderInput) (*models.Order, error) {
	if in.CategoryID == "" || !s.categoryExists(in.CategoryID) {
		return nil, ErrBadRequest
	}
	if in.Gender == "" {
		in.Gender = "any"
	}
	if in.Gender != "male" && in.Gender != "female" && in.Gender != "any" {
		return nil, ErrBadRequest
	}
	// The customer names what the job pays — a worker should never be called
	// without knowing the price.
	if in.Price <= 0 {
		return nil, ErrBadRequest
	}
	// Carry the customer's name on the order so the worker sees who called.
	var customerName string
	if u, ok := s.GetUser(customerID); ok {
		customerName = u.Name
	}
	// The order starts searching the moment it is created — nobody picks a
	// worker, the order goes out to every worker nearby (see DispatchWave).
	expires := time.Now().Add(InviteTTL)
	o := &models.Order{
		ID:                id.New(),
		CustomerID:        customerID,
		CategoryID:        in.CategoryID,
		Gender:            in.Gender,
		Description:       in.Description,
		ImageURL:          in.ImageURL,
		CustomerName:      customerName,
		CustomerPhone:     in.Phone,
		Price:             in.Price,
		Lat:               in.Lat,
		Lng:               in.Lng,
		Status:            models.StatusMatching,
		InvitedWorkerIDs:  []string{},
		RejectedWorkerIDs: []string{},
		InviteExpiresAt:   &expires,
		CreatedAt:         time.Now(),
	}
	s.db.Exec("INSERT INTO orders ("+orderCols+") VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)",
		o.ID, o.CustomerID, "", "", o.CategoryID, o.Gender, o.Description, o.ImageURL,
		o.CustomerPhone, "", o.Lat, o.Lng, o.Status, "[]", expires, 0, o.CreatedAt, o.CustomerName, o.Price, "[]")
	return o, nil
}

// CompanyOrderInput is the payload for a direct order to a company. The company
// needs everything it takes to show up at the door: who, where, and a photo.
type CompanyOrderInput struct {
	Description string
	Phone       string
	Name        string
	ImageURL    string
	Lat         float64
	Lng         float64
}

// CreateCompanyOrder creates a direct order to a company.
func (s *Store) CreateCompanyOrder(customerID, companyID string, in CompanyOrderInput) (*models.Order, error) {
	cp, ok := s.GetCompanyByID(companyID)
	if !ok {
		return nil, ErrNotFound
	}
	if cp.Banned {
		return nil, ErrForbidden
	}
	// You cannot hire yourself.
	if cp.UserID == customerID {
		return nil, ErrForbidden
	}
	// A company already on a job takes no new orders until it finishes.
	if s.CompanyIsBusy(cp.ID) {
		return nil, ErrConflict
	}
	// Fall back to the account name when the form left it blank.
	name := in.Name
	if name == "" {
		if u, ok := s.GetUser(customerID); ok {
			name = u.Name
		}
	}
	o := &models.Order{
		ID:            id.New(),
		CustomerID:    customerID,
		CompanyID:     companyID,
		Gender:        "any",
		Description:   in.Description,
		ImageURL:      in.ImageURL,
		CustomerName:  name,
		CustomerPhone: in.Phone,
		// The company publishes its fee; the customer books at that price.
		Price:             cp.Price,
		Lat:               in.Lat,
		Lng:               in.Lng,
		Status:            models.StatusPending,
		InvitedWorkerIDs:  []string{},
		RejectedWorkerIDs: []string{},
		CreatedAt:         time.Now(),
	}
	s.db.Exec("INSERT INTO orders ("+orderCols+") VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)",
		o.ID, o.CustomerID, "", o.CompanyID, "", o.Gender, o.Description, o.ImageURL,
		o.CustomerPhone, "", o.Lat, o.Lng, o.Status, "[]", nil, 0, o.CreatedAt, o.CustomerName, o.Price, "[]")
	return o, nil
}

// CompanyRespond lets a company accept or reject an order addressed to it.
// A company never searches for workers — it simply takes the job or declines.
func (s *Store) CompanyRespond(orderID, companyUserID string, accept bool) (*models.Order, error) {
	o, ok := s.GetOrder(orderID)
	if !ok {
		return nil, ErrNotFound
	}
	cp, ok := s.getCompanyByUser(companyUserID)
	if !ok || o.CompanyID != cp.ID {
		return nil, ErrForbidden
	}
	if o.Status != models.StatusPending {
		return nil, ErrConflict
	}
	if !accept {
		o.Status = models.StatusCancelled
		s.db.Exec("UPDATE orders SET status=$1 WHERE id=$2", o.Status, orderID)
		return o, nil
	}
	if cp.Banned {
		return nil, ErrForbidden
	}
	if s.CompanyIsBusy(cp.ID) {
		return nil, ErrConflict
	}
	if !s.walletCanAccept(companyUserID) {
		return nil, ErrInsufficientBalance
	}
	commission := o.Price * int64(s.settings.CommissionPercent) / 100
	s.deductCommission(companyUserID, commission)

	o.Status = models.StatusAccepted
	o.WorkerPhone = cp.Phone
	o.CommissionAmount = commission
	s.db.Exec("UPDATE orders SET status=$1, worker_phone=$2, commission_amount=$3 WHERE id=$4",
		o.Status, o.WorkerPhone, commission, orderID)
	return o, nil
}

// GetOrder returns an order.
func (s *Store) GetOrder(orderID string) (*models.Order, bool) {
	o, err := s.scanOrder(s.db.QueryRow("SELECT "+orderCols+" FROM orders WHERE id=$1", orderID))
	if err != nil {
		return nil, false
	}
	return o, true
}

// ListOrdersByCustomer returns a customer's own orders, newest first.
func (s *Store) ListOrdersByCustomer(customerID string) []models.Order {
	rows, err := s.db.Query("SELECT "+orderCols+" FROM orders WHERE customer_id=$1 ORDER BY created_at DESC", customerID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []models.Order
	for rows.Next() {
		o, err := s.scanOrder(rows)
		if err == nil {
			out = append(out, *o)
		}
	}
	return out
}

// ListCompanyOrders returns orders directed at a company, newest first.
func (s *Store) ListCompanyOrders(companyUserID string) []models.Order {
	// Get company profile ID from user ID.
	cp, ok := s.getCompanyByUser(companyUserID)
	if !ok {
		return nil
	}
	rows, err := s.db.Query("SELECT "+orderCols+" FROM orders WHERE company_id=$1 ORDER BY created_at DESC", cp.ID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []models.Order
	for rows.Next() {
		o, err := s.scanOrder(rows)
		if err == nil {
			out = append(out, *o)
		}
	}
	return out
}

// ListWorkerOrders returns orders a worker is invited to or assigned.
func (s *Store) ListWorkerOrders(workerUserID string, since time.Time) []models.Order {
	wp, ok := s.GetWorkerByUser(workerUserID)
	if !ok {
		return nil
	}
	// We need to check both worker_id and invited_worker_ids (JSON array).
	rows, err := s.db.Query("SELECT " + orderCols + " FROM orders ORDER BY created_at DESC")
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []models.Order
	for rows.Next() {
		o, err := s.scanOrder(rows)
		if err != nil {
			continue
		}
		if !since.IsZero() && !o.CreatedAt.After(since) {
			continue
		}
		if o.WorkerID == workerUserID || containsStr(o.InvitedWorkerIDs, wp.ID) {
			out = append(out, *o)
		}
	}
	return out
}

// candidatesFor returns every worker who could take this order right now:
// online, not banned, does this category, matches the requested gender, is not
// already on a job, can afford the commission, and is inside the match radius —
// nearest first.
func (s *Store) candidatesFor(o *models.Order) []models.WorkerProfile {
	rows, err := s.db.Query("SELECT " + workerCols + " FROM workers WHERE online=TRUE AND banned=FALSE")
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []models.WorkerProfile
	for rows.Next() {
		wp, err := s.scanWorker(rows)
		if err != nil {
			continue
		}
		// You cannot be called to your own job.
		if wp.UserID == o.CustomerID {
			continue
		}
		if !containsStr(wp.CategoryIDs, o.CategoryID) {
			continue
		}
		// Gender filter: if the order asks for male/female, only those match.
		if o.Gender != "any" && wp.Gender != o.Gender {
			continue
		}
		// Already on a job — not available until it is finished.
		if s.WorkerIsBusy(wp.UserID) {
			continue
		}
		// An empty wallet cannot accept, so ringing that phone is pure noise.
		if !s.walletCanAccept(wp.UserID) {
			continue
		}
		d := geo.DistanceKm(o.Lat, o.Lng, wp.Lat, wp.Lng)
		if d > matchRadiusKm {
			continue
		}
		dist := d
		wp.DistanceKm = &dist
		out = append(out, *wp)
	}
	sort.Slice(out, func(i, j int) bool { return *out[i].DistanceKm < *out[j].DistanceKm })
	return out
}

// DispatchWave broadcasts an open order to every worker nearby who has not been
// rung for it yet. Nobody picks a worker: all matching phones ring at once and
// the first to accept takes the job.
//
// It returns the order, the user IDs newly notified in this wave, and whether
// the search is still open — the caller keeps calling it every DispatchInterval
// so workers who come online mid-search are pulled in too.
func (s *Store) DispatchWave(orderID string) (*models.Order, []string, bool) {
	o, ok := s.GetOrder(orderID)
	if !ok {
		return nil, nil, false
	}
	switch o.Status {
	case models.StatusPending, models.StatusMatching, models.StatusInvited:
	default:
		// Taken, cancelled or already expired — stop searching.
		return o, nil, false
	}
	if o.InviteExpiresAt == nil || !time.Now().Before(*o.InviteExpiresAt) {
		return o, nil, false
	}

	var newProfileIDs, newUserIDs []string
	for _, wp := range s.candidatesFor(o) {
		if containsStr(o.InvitedWorkerIDs, wp.ID) || containsStr(o.RejectedWorkerIDs, wp.ID) {
			continue
		}
		newProfileIDs = append(newProfileIDs, wp.ID)
		newUserIDs = append(newUserIDs, wp.UserID)
	}
	if len(newProfileIDs) == 0 {
		// Nobody new this round; keep the window open in case somebody appears.
		return o, nil, true
	}

	invited := append(append([]string{}, o.InvitedWorkerIDs...), newProfileIDs...)
	// Guarded update: if the order was taken or cancelled between the read and
	// here, it must not be resurrected — and those workers must not be rung.
	res, err := s.db.Exec(
		`UPDATE orders SET status=$1, invited_worker_ids=$2
		 WHERE id=$3 AND worker_id='' AND status IN ('pending','matching','invited')`,
		models.StatusInvited, marshalJSON(invited), orderID)
	if err != nil {
		return o, nil, false
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return o, nil, false
	}
	o.Status = models.StatusInvited
	o.InvitedWorkerIDs = invited
	return o, newUserIDs, true
}

// Redispatch re-opens the search on an order nobody took, giving every nearby
// worker — including those who declined the first time — a fresh ring.
func (s *Store) Redispatch(orderID, customerID string) (*models.Order, error) {
	o, ok := s.GetOrder(orderID)
	if !ok {
		return nil, ErrNotFound
	}
	if o.CustomerID != customerID {
		return nil, ErrForbidden
	}
	switch o.Status {
	case models.StatusPending, models.StatusMatching, models.StatusInvited, models.StatusExpired:
	default:
		return nil, ErrConflict
	}
	expires := time.Now().Add(InviteTTL)
	res, err := s.db.Exec(
		`UPDATE orders SET status=$1, invited_worker_ids='[]', rejected_worker_ids='[]', invite_expires_at=$2
		 WHERE id=$3 AND worker_id=''`,
		models.StatusMatching, expires, orderID)
	if err != nil {
		return nil, ErrConflict
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrConflict
	}
	o.Status = models.StatusMatching
	o.InvitedWorkerIDs = []string{}
	o.RejectedWorkerIDs = []string{}
	o.InviteExpiresAt = &expires
	return o, nil
}

// Accept hands the order to the first worker who takes it. Every other worker
// the order was broadcast to loses the race here: the assignment is a single
// guarded UPDATE, so exactly one of N simultaneous taps can win.
func (s *Store) Accept(orderID, workerUserID string) (*models.Order, *models.WorkerProfile, error) {
	o, ok := s.GetOrder(orderID)
	if !ok {
		return nil, nil, ErrNotFound
	}
	wp, ok := s.GetWorkerByUser(workerUserID)
	if !ok {
		return nil, nil, ErrForbidden
	}
	if o.WorkerID != "" || (o.Status != models.StatusInvited && o.Status != models.StatusMatching) {
		return nil, nil, ErrConflict
	}
	if o.InviteExpiresAt != nil && time.Now().After(*o.InviteExpiresAt) {
		s.db.Exec("UPDATE orders SET status=$1, invite_expires_at=NULL WHERE id=$2", models.StatusExpired, orderID)
		return nil, nil, ErrConflict
	}
	if !containsStr(o.InvitedWorkerIDs, wp.ID) {
		return nil, nil, ErrForbidden
	}
	if wp.Banned {
		return nil, nil, ErrForbidden
	}
	// One job at a time — finish the current one first.
	if s.WorkerIsBusy(workerUserID) {
		return nil, nil, ErrConflict
	}
	if !s.walletCanAccept(workerUserID) {
		return nil, nil, ErrInsufficientBalance
	}
	// Commission is taken on the agreed price of this job.
	base := o.Price
	if base <= 0 {
		base = wp.PriceMin
	}
	commission := base * int64(s.settings.CommissionPercent) / 100

	// The winner is decided by the database, not by the order of the checks
	// above — two workers reaching this line together both pass every check.
	res, err := s.db.Exec(
		`UPDATE orders SET status=$1, worker_id=$2, worker_phone=$3, invite_expires_at=NULL, commission_amount=$4
		 WHERE id=$5 AND worker_id='' AND status IN ('matching','invited')`,
		models.StatusAccepted, workerUserID, wp.Phone, commission, orderID)
	if err != nil {
		return nil, nil, ErrConflict
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Somebody else was a fraction of a second faster.
		return nil, nil, ErrConflict
	}
	// Only the winner pays the commission.
	s.deductCommission(workerUserID, commission)

	o.Status = models.StatusAccepted
	o.WorkerID = workerUserID
	o.WorkerPhone = wp.Phone
	o.InviteExpiresAt = nil
	o.CommissionAmount = commission
	return o, wp, nil
}

// Reject drops a worker from the order. The search itself carries on until the
// window closes — one worker saying no is not the customer's answer.
func (s *Store) Reject(orderID, workerUserID string) (*models.Order, error) {
	o, ok := s.GetOrder(orderID)
	if !ok {
		return nil, ErrNotFound
	}
	wp, ok := s.GetWorkerByUser(workerUserID)
	if !ok {
		return nil, ErrForbidden
	}
	if !containsStr(o.InvitedWorkerIDs, wp.ID) {
		return nil, ErrForbidden
	}
	invited := make([]string, 0, len(o.InvitedWorkerIDs))
	for _, wid := range o.InvitedWorkerIDs {
		if wid != wp.ID {
			invited = append(invited, wid)
		}
	}
	// Remembered as declined so the next wave does not ring them again.
	rejected := append(append([]string{}, o.RejectedWorkerIDs...), wp.ID)
	o.InvitedWorkerIDs = invited
	o.RejectedWorkerIDs = rejected
	s.db.Exec("UPDATE orders SET invited_worker_ids=$1, rejected_worker_ids=$2 WHERE id=$3",
		marshalJSON(invited), marshalJSON(rejected), orderID)
	return o, nil
}

// ExpireIfSearching flips an order to expired only if the search is still
// running and its window has actually run out. An order somebody already took
// is left alone, and so is one the customer just re-opened — otherwise a
// dispatch loop finishing a fraction of a second late would kill a search that
// had only just started again.
func (s *Store) ExpireIfSearching(orderID string) (*models.Order, bool) {
	res, err := s.db.Exec(
		`UPDATE orders SET status=$1, invite_expires_at=NULL
		 WHERE id=$2 AND worker_id='' AND status IN ('pending','matching','invited')
		   AND (invite_expires_at IS NULL OR invite_expires_at <= NOW())`,
		models.StatusExpired, orderID)
	if err != nil {
		return nil, false
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, false
	}
	o, ok := s.GetOrder(orderID)
	if !ok {
		return nil, false
	}
	return o, true
}

// ExpireStaleSearches closes searches whose window ran out while the server was
// down — their dispatch loops died with the process, so nothing else would ever
// finish them. Returns how many were closed.
func (s *Store) ExpireStaleSearches() int64 {
	res, err := s.db.Exec(
		`UPDATE orders SET status=$1, invite_expires_at=NULL
		 WHERE worker_id='' AND status IN ('matching','invited')
		   AND invite_expires_at IS NOT NULL AND invite_expires_at <= NOW()`,
		models.StatusExpired)
	if err != nil {
		return 0
	}
	n, _ := res.RowsAffected()
	return n
}

// Complete marks an order completed. Only the customer who placed the order may
// declare the work finished — a worker cannot close their own job.
func (s *Store) Complete(orderID, actorID string) (*models.Order, error) {
	o, ok := s.GetOrder(orderID)
	if !ok {
		return nil, ErrNotFound
	}
	if o.CustomerID != actorID {
		return nil, ErrForbidden
	}
	o.Status = models.StatusCompleted
	o.InviteExpiresAt = nil
	s.db.Exec("UPDATE orders SET status=$1, invite_expires_at=NULL WHERE id=$2",
		models.StatusCompleted, orderID)
	return o, nil
}

// Cancel marks an order cancelled.
func (s *Store) Cancel(orderID, actorID string) (*models.Order, error) {
	return s.setTerminal(orderID, actorID, models.StatusCancelled)
}

func (s *Store) setTerminal(orderID, actorID, status string) (*models.Order, error) {
	o, ok := s.GetOrder(orderID)
	if !ok {
		return nil, ErrNotFound
	}
	// Allow customer, worker, or company owner.
	isCompanyOwner := false
	if o.CompanyID != "" {
		if cp, ok := s.GetCompanyByID(o.CompanyID); ok && cp.UserID == actorID {
			isCompanyOwner = true
		}
	}
	if o.CustomerID != actorID && o.WorkerID != actorID && !isCompanyOwner {
		return nil, ErrForbidden
	}
	o.Status = status
	o.InviteExpiresAt = nil
	s.db.Exec("UPDATE orders SET status=$1, invite_expires_at=NULL WHERE id=$2", status, orderID)
	return o, nil
}

// Rate records a two-way rating.
func (s *Store) Rate(orderID, fromUserID string, stars int, comment string) (*models.Rating, error) {
	if stars < 1 || stars > 5 {
		return nil, ErrBadRequest
	}
	o, ok := s.GetOrder(orderID)
	if !ok {
		return nil, ErrNotFound
	}
	var toUserID string
	switch fromUserID {
	case o.CustomerID:
		toUserID = o.WorkerID
	case o.WorkerID:
		toUserID = o.CustomerID
	default:
		return nil, ErrForbidden
	}
	if toUserID == "" {
		return nil, ErrConflict
	}
	r := &models.Rating{
		ID:         id.New(),
		OrderID:    orderID,
		FromUserID: fromUserID,
		ToUserID:   toUserID,
		Stars:      stars,
		Comment:    comment,
		CreatedAt:  time.Now(),
	}
	s.db.Exec("INSERT INTO ratings (id, order_id, from_user_id, to_user_id, stars, comment, created_at) VALUES ($1,$2,$3,$4,$5,$6,$7)",
		r.ID, r.OrderID, r.FromUserID, r.ToUserID, r.Stars, r.Comment, r.CreatedAt)

	// Update the target worker's rating aggregate.
	if wp, ok := s.GetWorkerByUser(toUserID); ok {
		total := wp.Rating*float64(wp.RatingsCount) + float64(stars)
		newCount := wp.RatingsCount + 1
		newRating := total / float64(newCount)
		s.db.Exec("UPDATE workers SET rating=$1, ratings_count=$2 WHERE user_id=$3", newRating, newCount, toUserID)
	}
	return r, nil
}

// ListReviews returns what has been said *about* a user, newest first.
//
// The aggregate on the profile (4.7 ★, 12 ratings) says how it went; this says
// why. Reviews with no words still come back — the star alone is a data point,
// and dropping them would quietly make a profile look better than its score.
//
// The author's name comes from the users table, so a customer who later renames
// their account is not frozen under the name they had that day.
func (s *Store) ListReviews(toUserID string, limit int) []models.Review {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := s.db.Query(`
		SELECT r.id, r.order_id, r.stars, r.comment, r.created_at,
		       COALESCE(u.name, ''), COALESCE(u.avatar_url, '')
		FROM ratings r
		LEFT JOIN users u ON u.id = r.from_user_id
		WHERE r.to_user_id = $1
		ORDER BY r.created_at DESC
		LIMIT $2`, toUserID, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()

	out := []models.Review{}
	for rows.Next() {
		var v models.Review
		if err := rows.Scan(&v.ID, &v.OrderID, &v.Stars, &v.Comment, &v.CreatedAt,
			&v.AuthorName, &v.AuthorAvatar); err != nil {
			continue
		}
		out = append(out, v)
	}
	return out
}
