package store

import (
	"database/sql"
	"fmt"
	"log"
	"sort"
	"strings"
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
	// Images is every photo attached to the job. ImageURL is what an older app
	// sends instead of a list; whichever arrives, the first photo ends up in
	// both so no reader has to know which client made the order.
	Images   []string
	ImageURL string
	Phone    string
	Price    int64
	Lat      float64
	Lng      float64
	// TargetWorkerIDs narrows the search to workers the customer picked by hand.
	// Empty is the ordinary broadcast — see models.Order.
	TargetWorkerIDs []string
	// WorkersNeeded is how many people the job takes. Zero reads as one.
	WorkersNeeded int
}

// A worker or company is "busy" while one of their orders is accepted or
// in_progress; until it is completed they cannot be given another job.

// WorkerIsBusy reports whether the worker is already on a job.
// Read through order_workers rather than orders.worker_id: on a job that takes
// five people only the first of them is named in that column, and the other four
// would have looked free while they were on their way to a banquet.
func (s *Store) WorkerIsBusy(workerUserID string) bool {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM order_workers ow
		 JOIN orders o ON o.id = ow.order_id
		 WHERE ow.worker_id=$1 AND o.status IN ('accepted','in_progress')`,
		workerUserID).Scan(&n)
	return err == nil && n > 0
}

// HasHiredWorker reports whether this worker ever took a job from this
// customer. It is the gate on the worker's phone number: the platform hands it
// over once the two are actually working together, and never for a browser
// scrolling the list.
//
// Completed jobs count. The order is over, but calling back about the work is
// not a way around the platform — it is the same job.
func (s *Store) HasHiredWorker(customerID, workerUserID string) bool {
	if customerID == "" || workerUserID == "" {
		return false
	}
	// Through order_workers: on a five-person job `orders.worker_id` names one
	// of the five, so four people who worked in this customer's home would have
	// gone on hiding their number from them afterwards.
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM order_workers ow
		 JOIN orders o ON o.id = ow.order_id
		 WHERE o.customer_id=$1 AND ow.worker_id=$2
		   AND o.status IN ('accepted','in_progress','completed')`,
		customerID, workerUserID).Scan(&n)
	return err == nil && n > 0
}

// HasHiredCompany is HasHiredWorker for a firm. Company orders are keyed by the
// company profile id rather than a user id, which is why it cannot share the
// query above.
func (s *Store) HasHiredCompany(customerID, companyID string) bool {
	if customerID == "" || companyID == "" {
		return false
	}
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM orders
		 WHERE customer_id=$1 AND company_id=$2
		   AND status IN ('accepted','in_progress','completed')`,
		customerID, companyID).Scan(&n)
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
	var invitedIDs, rejectedIDs, images, targetIDs string
	var inviteExpires sql.NullTime
	err := row.Scan(&o.ID, &o.CustomerID, &o.WorkerID, &o.CompanyID, &o.CategoryID,
		&o.Gender, &o.Description, &o.ImageURL, &o.CustomerPhone, &o.WorkerPhone,
		&o.Lat, &o.Lng, &o.Status, &invitedIDs, &inviteExpires, &o.CommissionAmount,
		&o.CreatedAt, &o.CustomerName, &o.Price, &rejectedIDs, &images, &targetIDs,
		&o.WorkersNeeded)
	if err != nil {
		return nil, err
	}
	// A row written before the column existed reads as 0, which no order ever
	// means — every job takes at least one person.
	if o.WorkersNeeded < 1 {
		o.WorkersNeeded = 1
	}
	o.InvitedWorkerIDs = unmarshalStrings(invitedIDs)
	o.RejectedWorkerIDs = unmarshalStrings(rejectedIDs)
	o.Images = unmarshalStrings(images)
	o.TargetWorkerIDs = unmarshalStrings(targetIDs)
	// An order written before photos were a list still has its one in the old
	// column; the reader is where that difference stops mattering.
	if len(o.Images) == 0 && o.ImageURL != "" {
		o.Images = []string{o.ImageURL}
	}
	if inviteExpires.Valid {
		o.InviteExpiresAt = &inviteExpires.Time
	}
	return &o, nil
}

const orderCols = "id, customer_id, worker_id, company_id, category_id, gender, description, image_url, customer_phone, worker_phone, lat, lng, status, invited_worker_ids, invite_expires_at, commission_amount, created_at, customer_name, price, rejected_worker_ids, images, target_worker_ids, workers_needed"

/**
 * The crew, attached to an order that has been read.
 *
 * A join rather than a column, so this is a second query — deliberately not
 * folded into `scanOrder`, which runs inside a `rows.Next()` loop in half a
 * dozen listings and would turn each of them into a query per row. Callers that
 * hand an order to somebody ask for the crew explicitly.
 */
func (s *Store) loadOrderWorkers(o *models.Order) {
	if o == nil {
		return
	}
	o.AcceptedWorkers = s.OrderWorkers(o.ID)
}

// OrderWorkers is everyone who has accepted this job, oldest first.
func (s *Store) OrderWorkers(orderID string) []models.OrderWorker {
	out := []models.OrderWorker{}
	rows, err := s.db.Query(
		`SELECT worker_id, worker_name, phone, created_at FROM order_workers
		 WHERE order_id=$1 ORDER BY created_at`, orderID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var ow models.OrderWorker
		if rows.Scan(&ow.WorkerID, &ow.Name, &ow.Phone, &ow.CreatedAt) == nil {
			out = append(out, ow)
		}
	}
	return out
}

/**
 * The crews for a whole listing, in one query.
 *
 * The per-order loader is fine for a single read and wrong inside a loop: the
 * customer's order list is thirty rows on a busy account, and hydrating it a row
 * at a time is thirty round trips for a screen that already had its data.
 */
func (s *Store) loadOrderWorkersBatch(orders []models.Order) {
	if len(orders) == 0 {
		return
	}
	placeholders := make([]string, len(orders))
	args := make([]any, len(orders))
	for i := range orders {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		args[i] = orders[i].ID
		orders[i].AcceptedWorkers = []models.OrderWorker{}
	}
	rows, err := s.db.Query(
		`SELECT order_id, worker_id, worker_name, phone, created_at FROM order_workers
		 WHERE order_id IN (`+strings.Join(placeholders, ",")+`) ORDER BY created_at`, args...)
	if err != nil {
		return
	}
	defer rows.Close()

	byOrder := map[string][]models.OrderWorker{}
	for rows.Next() {
		var orderID string
		var ow models.OrderWorker
		if rows.Scan(&orderID, &ow.WorkerID, &ow.Name, &ow.Phone, &ow.CreatedAt) == nil {
			byOrder[orderID] = append(byOrder[orderID], ow)
		}
	}
	for i := range orders {
		if crew, ok := byOrder[orders[i].ID]; ok {
			orders[i].AcceptedWorkers = crew
		}
	}
}

// countOrderWorkers is how many have accepted so far.
func (s *Store) countOrderWorkers(orderID string) int {
	var n int
	s.db.QueryRow("SELECT COUNT(*) FROM order_workers WHERE order_id=$1", orderID).Scan(&n)
	return n
}

// OrderIsFull reports whether the job already has everybody it needs.
func (s *Store) OrderIsFull(o *models.Order) bool {
	return s.countOrderWorkers(o.ID) >= o.WorkersNeeded
}

// WorkerOnOrder reports whether this worker holds a place on the job.
func (s *Store) WorkerOnOrder(orderID, workerUserID string) bool {
	var n int
	s.db.QueryRow("SELECT COUNT(*) FROM order_workers WHERE order_id=$1 AND worker_id=$2",
		orderID, workerUserID).Scan(&n)
	return n > 0
}

// maxWorkersPerOrder caps one job. Past this it is a staffing contract, not an
// order, and the office should be arranging it rather than the app.
const maxWorkersPerOrder = 20

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
	// Who the order is allowed to reach.
	//
	// Names that are not worker profiles are dropped rather than refused: the
	// list comes from a screen the customer was looking at moments ago, and one
	// entry going stale — a profile the office removed in between — is no reason
	// to throw away an order they have finished writing. A list that turns out
	// to be entirely stale falls through to the broadcast, which is the same
	// thing the app would have done had they never picked anybody.
	targets := s.existingWorkerIDs(in.TargetWorkerIDs)
	if len(targets) > maxTargetWorkers {
		targets = targets[:maxTargetWorkers]
	}

	needed := in.WorkersNeeded
	if needed < 1 {
		needed = 1
	}
	if needed > maxWorkersPerOrder {
		needed = maxWorkersPerOrder
	}
	// Naming five workers is asking for five people. Left unreconciled, an order
	// could ring exactly the three workers the customer chose while claiming to
	// need five, and then keep searching for two who were never allowed to be
	// found — a countdown that could not finish, by construction.
	if len(targets) > 0 && needed > len(targets) {
		needed = len(targets)
	}

	// The order starts searching the moment it is created — with no targets it
	// goes out to every matching worker nearby (see DispatchWave).
	expires := time.Now().Add(InviteTTL)
	o := &models.Order{
		ID:                id.New(),
		CustomerID:        customerID,
		CategoryID:        in.CategoryID,
		Gender:            in.Gender,
		Description:       in.Description,
		ImageURL:          in.ImageURL,
		Images:            in.Images,
		CustomerName:      customerName,
		CustomerPhone:     in.Phone,
		Price:             in.Price,
		Lat:               in.Lat,
		Lng:               in.Lng,
		Status:            models.StatusMatching,
		InvitedWorkerIDs:  []string{},
		RejectedWorkerIDs: []string{},
		TargetWorkerIDs:   targets,
		WorkersNeeded:     needed,
		AcceptedWorkers:   []models.OrderWorker{},
		InviteExpiresAt:   &expires,
		CreatedAt:         time.Now(),
	}
	normalizeOrderPhotos(o)
	if _, err := s.db.Exec("INSERT INTO orders ("+orderCols+") VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23)",
		o.ID, o.CustomerID, "", "", o.CategoryID, o.Gender, o.Description, o.ImageURL,
		o.CustomerPhone, "", o.Lat, o.Lng, o.Status, "[]", expires, 0, o.CreatedAt, o.CustomerName, o.Price, "[]",
		marshalJSON(o.Images), marshalJSON(o.TargetWorkerIDs), o.WorkersNeeded); err != nil {
		log.Printf("store: order insert failed: %v", err)
		return nil, ErrOrderNotSaved
	}
	return o, nil
}

// maxOrderPhotos is what one job can usefully carry. Past this it is not a
// description any more, it is an album, and every reader of the order pays for
// it in bytes.
const maxOrderPhotos = 6

// normalizeOrderPhotos makes the list and the single field agree, whichever one
// the client actually sent.
//
// Old apps post `image_url`; new ones post `images`. Rather than making every
// reader — the worker's card, the firm's panel, the admin list — know which
// kind of client made the order, the two are reconciled once, here: the list is
// the truth and `image_url` is always its first entry.
func normalizeOrderPhotos(o *models.Order) {
	if len(o.Images) == 0 && o.ImageURL != "" {
		o.Images = []string{o.ImageURL}
	}
	if len(o.Images) > maxOrderPhotos {
		o.Images = o.Images[:maxOrderPhotos]
	}
	if o.Images == nil {
		o.Images = []string{}
	}
	if len(o.Images) > 0 {
		o.ImageURL = o.Images[0]
	} else {
		o.ImageURL = ""
	}
}

// CompanyOrderInput is the payload for a direct order to a company. The company
// needs everything it takes to show up at the door: who, where, and a photo.
type CompanyOrderInput struct {
	Description string
	Phone       string
	Name        string
	Images      []string
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
		return nil, ErrCompanyBanned
	}
	// You cannot hire yourself.
	if cp.UserID == customerID {
		return nil, ErrOwnCompany
	}
	// A company already on a job takes no new orders until it finishes.
	//
	// Worth knowing when this fires unexpectedly: only the customer can close a
	// job (Complete checks CustomerID), so a firm whose last customer never
	// tapped "Ish tugadi" stays busy indefinitely and refuses every order after
	// it. That is why this now says so rather than returning a bare conflict.
	if s.CompanyIsBusy(cp.ID) {
		return nil, ErrCompanyBusy
	}
	// A firm whose subscription has run out stays on the list — it is a real
	// business a customer may want to find — but it cannot be sent a job it is
	// not allowed to accept. Refusing here is the honest version of that: the
	// customer is told now, rather than after waiting on an order the firm can
	// only ever decline.
	if !cp.CanTakeWork() {
		return nil, ErrCompanyUnavailable
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
		Images:        in.Images,
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
	// The error is checked, unlike almost every other write in this file. An
	// ignored Exec here meant a failed insert still returned the order object
	// and a 201: the customer got the "sent!" screen, the firm got nothing, and
	// there was no record anywhere of an order that never existed. A write that
	// did not happen has to be an error the caller can see.
	normalizeOrderPhotos(o)
	if _, err := s.db.Exec("INSERT INTO orders ("+orderCols+") VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)",
		o.ID, o.CustomerID, "", o.CompanyID, "", o.Gender, o.Description, o.ImageURL,
		o.CustomerPhone, "", o.Lat, o.Lng, o.Status, "[]", nil, 0, o.CreatedAt, o.CustomerName, o.Price, "[]",
		marshalJSON(o.Images)); err != nil {
		log.Printf("store: company order insert failed: %v", err)
		return nil, ErrOrderNotSaved
	}
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
	// A firm pays by the month, not by the job: what gates it here is whether it
	// still holds a subscription, or is inside its free trial jobs. There is no
	// commission on a company order at all — the month has already been paid for
	// — so the wallet is not consulted and nothing is deducted.
	if !cp.CanTakeWork() {
		return nil, ErrSubscriptionExpired
	}
	s.chargeCompanyAcceptance(cp)

	o.Status = models.StatusAccepted
	o.WorkerPhone = cp.Phone
	s.db.Exec("UPDATE orders SET status=$1, worker_phone=$2 WHERE id=$3",
		o.Status, o.WorkerPhone, orderID)
	return o, nil
}

// GetOrder returns an order.
func (s *Store) GetOrder(orderID string) (*models.Order, bool) {
	o, err := s.scanOrder(s.db.QueryRow("SELECT "+orderCols+" FROM orders WHERE id=$1", orderID))
	if err != nil {
		return nil, false
	}
	// Hydrated here rather than at each call site: every reader of a single
	// order wants to know who is on it, and one that quietly did not would show
	// a five-person job as unstaffed.
	s.loadOrderWorkers(o)
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
	s.loadOrderWorkersBatch(out)
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
//
// An invitation is not a claim. A broadcast order rings every nearby worker at
// once, and the first one to answer takes it — so the row keeps naming all of
// them in invited_worker_ids long after somebody else has the job. Returning it
// to the losers put a job they will never do into their feed, where the app
// filed it under "accepted" because that is what its status now says: every
// worker who had been rung was shown an active job belonging to one of them.
//
// Whoever holds the order sees it; everybody else it was merely offered to
// stops seeing it the moment it is taken.
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
		// A place on the job is a job, whether or not this worker was the first
		// to take it — on a five-person order only the first is in `worker_id`.
		if s.WorkerOnOrder(o.ID, workerUserID) {
			s.loadOrderWorkers(o)
			out = append(out, *o)
			continue
		}
		if !containsStr(o.InvitedWorkerIDs, wp.ID) {
			continue
		}
		// Invited, and the job still has room. A one-person order fills on the
		// first yes, which is the old behaviour exactly; a five-person one stays
		// on everybody's screen until the fifth.
		switch o.Status {
		case models.StatusPending, models.StatusMatching, models.StatusInvited:
		default:
			continue
		}
		if s.OrderIsFull(o) {
			continue
		}
		s.loadOrderWorkers(o)
		out = append(out, *o)
	}
	return out
}

// maxTargetWorkers caps a hand-picked list. Past this the customer is not
// choosing anybody, they are broadcasting the long way round — and the whole
// point of naming workers is that the phones which ring are ones they meant.
const maxTargetWorkers = 10

// existingWorkerIDs keeps the ids that are real worker profiles, in the order
// they were given and without repeats.
func (s *Store) existingWorkerIDs(ids []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		var n int
		if s.db.QueryRow("SELECT COUNT(*) FROM workers WHERE id=$1", id).Scan(&n) != nil || n == 0 {
			continue
		}
		out = append(out, id)
	}
	return out
}

// matchesOrder reports whether this worker is somebody the order may ring, on
// the rules that need nothing beyond the two records in hand.
//
// Split out from candidatesFor so it can be tested: the rest of that function
// is a database cursor and two lookups, and this is where the decision that
// actually matters is made.
//
// An order that names its workers is judged differently. Category, gender and
// radius are *matchmaking* rules — they exist to decide who to ring when nobody
// has said. Here somebody has: the customer read these profiles and chose them.
// Applying the rules on top would silently drop people they picked on purpose
// (the neighbour who is filed under a different category, the worker a street
// past the radius) and leave them watching a search that rings nobody, with no
// way to find out why. What still applies is everything the worker could not
// answer anyway — banned, offline, already on a job, unable to pay the
// commission — which is checked by the caller.
func matchesOrder(o *models.Order, wp *models.WorkerProfile) bool {
	// You cannot be called to your own job.
	if wp.UserID == o.CustomerID {
		return false
	}
	if len(o.TargetWorkerIDs) > 0 {
		return containsStr(o.TargetWorkerIDs, wp.ID)
	}
	if !containsStr(wp.CategoryIDs, o.CategoryID) {
		return false
	}
	// Gender filter: if the order asks for male/female, only those match.
	if o.Gender != "any" && wp.Gender != o.Gender {
		return false
	}
	return geo.DistanceKm(o.Lat, o.Lng, wp.Lat, wp.Lng) <= matchRadiusKm
}

// candidatesFor returns every worker who could take this order right now:
// online, not banned, eligible under matchesOrder, not already on a job and able
// to afford the commission — nearest first.
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
		if !matchesOrder(o, wp) {
			continue
		}
		// Already holds a place on this very job — a five-person order must not
		// ring somebody it has already got.
		if s.WorkerOnOrder(o.ID, wp.UserID) {
			continue
		}
		// Already on a job — not available until it is finished.
		if s.WorkerIsBusy(wp.UserID) {
			continue
		}
		// A worker who could not accept if they tried is pure noise on the
		// broadcast — but a brand-new one with free jobs left can accept with an
		// empty wallet, which is the whole point of the trial, so this asks the
		// same question Accept will ask rather than only looking at the money.
		if !s.canWorkerAccept(wp) {
			continue
		}
		dist := geo.DistanceKm(o.Lat, o.Lng, wp.Lat, wp.Lng)
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
	// Guarded update: if the order was filled or cancelled between the read and
	// here, it must not be resurrected — and those workers must not be rung.
	//
	// Status is the whole guard. It used to also require `worker_id=''`, which
	// was the same question while a job took one person and is the wrong one
	// now: the first of five acceptances writes that column, and the wave that
	// should go looking for the other four would have found the order already
	// spoken for and stopped.
	res, err := s.db.Exec(
		`UPDATE orders SET status=$1, invited_worker_ids=$2
		 WHERE id=$3 AND status IN ('pending','matching','invited')`,
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
	case models.StatusPending, models.StatusMatching, models.StatusInvited,
		models.StatusExpired,
		// A search everybody declined is exactly the case "try again" is for:
		// the UPDATE below clears rejected_worker_ids, so the second run rings
		// them afresh — and without this the one order that most needs re-opening
		// would be the one order that could not be.
		models.StatusRejected:
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

// Accept gives a worker a place on the job.
//
// A job takes as many people as it says it takes. For the ordinary one-person
// order this is what it always was — the first yes wins and everybody else gets
// a conflict — because "full" and "taken" are the same thing when the job needs
// one. For a banquet that needs five, the first four acceptances leave the
// search running and the fifth closes it.
//
// The place is claimed inside a transaction that locks the order row, because
// the check that matters ("is there room?") is a count of another table: two
// workers reading four-of-five at the same moment would both see room and both
// take the fifth place. The lock is what makes exactly one of them right.
func (s *Store) Accept(orderID, workerUserID string) (*models.Order, *models.WorkerProfile, error) {
	o, ok := s.GetOrder(orderID)
	if !ok {
		return nil, nil, ErrNotFound
	}
	wp, ok := s.GetWorkerByUser(workerUserID)
	if !ok {
		return nil, nil, ErrForbidden
	}
	if o.Status != models.StatusInvited && o.Status != models.StatusMatching {
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
	if !s.canWorkerAccept(wp) {
		return nil, nil, ErrInsufficientBalance
	}
	// Commission is taken on the agreed price of this job. Each worker on a
	// five-person job is paid the price and pays commission on it — the price is
	// per person, which is what the customer was shown when they set it.
	base := o.Price
	if base <= 0 {
		base = wp.PriceMin
	}
	commission := base * int64(s.settings.CommissionPercent) / 100

	filled, err := s.claimPlace(o, wp, commission)
	if err != nil {
		return nil, nil, err
	}

	// Only workers who got a place pay — and one still inside their three free
	// jobs spends one of those instead of the commission. Settled after the
	// claim, because whether the job is free is only decided by the atomic
	// consume inside the call.
	charged := s.chargeWorkerAcceptance(wp, commission)
	if charged != commission {
		s.db.Exec("UPDATE order_workers SET commission=$1 WHERE order_id=$2 AND worker_id=$3",
			charged, orderID, workerUserID)
		if o.WorkerID == workerUserID {
			s.db.Exec("UPDATE orders SET commission_amount=$1 WHERE id=$2", charged, orderID)
		}
	}

	// Re-read: the claim moved the status, the first-accepter columns and the
	// crew, and the caller hands this straight to both the worker and the
	// customer.
	if fresh, ok := s.GetOrder(orderID); ok {
		o = fresh
	}
	s.loadOrderWorkers(o)
	if o.WorkerID == workerUserID {
		o.CommissionAmount = charged
	}
	_ = filled
	// The profile handed back is what the app redraws its banner from, so it has
	// to know a trial job was just spent.
	wp.FreeOrdersUsed = s.freeOrdersUsed(WorkerTrialKey(wp.PassportSeries))
	return o, wp, nil
}

/**
 * claimPlace takes one of the job's places for this worker, or fails.
 *
 * Everything here is one transaction over a locked order row. The question it
 * answers — "is there still room?" — is a count of `order_workers`, so without
 * the lock two workers can both read four-of-five and both take the fifth
 * place: a customer who asked for five would get six people and pay six.
 *
 * Returns whether this claim was the one that filled the job.
 */
func (s *Store) claimPlace(o *models.Order, wp *models.WorkerProfile, commission int64) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, ErrConflict
	}
	defer tx.Rollback()

	// The lock, and the state as of holding it — the order may have been filled
	// or cancelled between the checks above and here.
	var needed int
	var status, firstWorker string
	if err := tx.QueryRow(
		`SELECT workers_needed, status, worker_id FROM orders WHERE id=$1 FOR UPDATE`,
		o.ID).Scan(&needed, &status, &firstWorker); err != nil {
		return false, ErrConflict
	}
	if needed < 1 {
		needed = 1
	}
	if status != models.StatusInvited && status != models.StatusMatching {
		return false, ErrConflict
	}

	var taken int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM order_workers WHERE order_id=$1`, o.ID).Scan(&taken); err != nil {
		return false, ErrConflict
	}
	if taken >= needed {
		return false, ErrConflict
	}

	// ON CONFLICT rather than a prior check: the primary key is what makes a
	// double tap idempotent, and a worker already on the job has not found a
	// second place on it.
	res, err := tx.Exec(
		`INSERT INTO order_workers (order_id, worker_id, worker_name, phone, commission)
		 VALUES ($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`,
		o.ID, wp.UserID, wp.FullName, wp.Phone, commission)
	if err != nil {
		return false, ErrConflict
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, ErrConflict
	}
	taken++

	// The first worker to accept also lands in the order's own columns. Every
	// screen written before a job could take five people reads `worker_id` and
	// `worker_phone`, and showing them nobody is worse than showing them one of
	// the five.
	if firstWorker == "" {
		if _, err := tx.Exec(
			`UPDATE orders SET worker_id=$1, worker_phone=$2, commission_amount=$3 WHERE id=$4`,
			wp.UserID, wp.Phone, commission, o.ID); err != nil {
			return false, ErrConflict
		}
	}

	// Full: the search is over. Short of full: it keeps running, so the order
	// stays `invited` with its window open and the dispatch loop keeps looking
	// for the people still missing.
	full := taken >= needed
	if full {
		if _, err := tx.Exec(
			`UPDATE orders SET status=$1, invite_expires_at=NULL WHERE id=$2`,
			models.StatusAccepted, o.ID); err != nil {
			return false, ErrConflict
		}
	}
	if err := tx.Commit(); err != nil {
		return false, ErrConflict
	}
	return full, nil
}

// Reject drops a worker from the order. On a broadcast the search carries on
// until the window closes — one worker saying no is not the customer's answer.
//
// The second return value says whether this decline was the *last* one: true
// when the order named its workers and every one of them has now refused, in
// which case the order has been closed as rejected here and the caller owes the
// customer the news.
func (s *Store) Reject(orderID, workerUserID string) (*models.Order, bool, error) {
	o, ok := s.GetOrder(orderID)
	if !ok {
		return nil, false, ErrNotFound
	}
	wp, ok := s.GetWorkerByUser(workerUserID)
	if !ok {
		return nil, false, ErrForbidden
	}
	if !containsStr(o.InvitedWorkerIDs, wp.ID) {
		return nil, false, ErrForbidden
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

	/**
	 * A search with nobody left to ask is over.
	 *
	 * On a broadcast one decline means nothing — the order keeps ringing, and a
	 * worker who comes online in the next minute is pulled in by the dispatch
	 * loop, so ending it early would throw away a job somebody was about to
	 * take. On an order that named its workers the set cannot grow: once every
	 * one of them has declined there is no wave left to run, and leaving the
	 * order "searching" meant the customer watched a two-minute countdown for a
	 * search that was already finished, and was then told nobody was free —
	 * when in fact the person they chose had answered, and said no.
	 */
	if s.allTargetsRejected(o) {
		res, err := s.db.Exec(
			`UPDATE orders SET status=$1, invite_expires_at=NULL
			 WHERE id=$2 AND worker_id='' AND status IN ('pending','matching','invited')`,
			models.StatusRejected, orderID)
		if err == nil {
			if n, _ := res.RowsAffected(); n > 0 {
				o.Status = models.StatusRejected
				o.InviteExpiresAt = nil
				return o, true, nil
			}
		}
	}
	return o, false, nil
}

// allTargetsRejected reports whether a hand-picked order has run out of people
// to ask. Always false for a broadcast, whose candidate set is open-ended.
func (s *Store) allTargetsRejected(o *models.Order) bool {
	if len(o.TargetWorkerIDs) == 0 {
		return false
	}
	for _, id := range o.TargetWorkerIDs {
		if !containsStr(o.RejectedWorkerIDs, id) {
			return false
		}
	}
	return true
}

// ExpireIfSearching closes a search whose window has run out. An order somebody
// already took is left alone, and so is one the customer just re-opened —
// otherwise a dispatch loop finishing a fraction of a second late would kill a
// search that had only just started again.
//
// The second return value is whether anybody is coming. A five-person job that
// found three is not a failed order: three people accepted, they are on their
// way, and telling the customer "nobody took it" would be false and would offer
// them a "try again" that re-opens a job three workers are already committed to.
// Such an order closes as `accepted` — short-staffed, but a job. Only a search
// that found nobody at all expires.
func (s *Store) ExpireIfSearching(orderID string) (*models.Order, bool) {
	// `worker_id=''` is not the test any more: it names the first of five, so a
	// partly-staffed job would have slipped past this and stayed "searching"
	// forever with a dead window.
	taken := s.countOrderWorkers(orderID)
	status := models.StatusExpired
	if taken > 0 {
		status = models.StatusAccepted
	}
	res, err := s.db.Exec(
		`UPDATE orders SET status=$1, invite_expires_at=NULL
		 WHERE id=$2 AND status IN ('pending','matching','invited')
		   AND (invite_expires_at IS NULL OR invite_expires_at <= NOW())`,
		status, orderID)
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
	s.loadOrderWorkers(o)
	// True means "this call closed the search" — the caller reads the resulting
	// status to decide what to tell the customer, because the two outcomes are
	// different news and only one of them is bad.
	return o, true
}

// ExpireStaleSearches closes searches whose window ran out while the server was
// down — their dispatch loops died with the process, so nothing else would ever
// finish them. Returns how many were closed.
func (s *Store) ExpireStaleSearches() int64 {
	// Same two outcomes as ExpireIfSearching, in one pass: a search that found
	// nobody expires, and one that found somebody closes as the (possibly
	// short-staffed) job it is. Keyed off order_workers rather than
	// `worker_id`, which only ever names the first of a crew.
	settled := `AND status IN ('matching','invited')
		    AND invite_expires_at IS NOT NULL AND invite_expires_at <= NOW()`

	accepted, err := s.db.Exec(
		`UPDATE orders SET status=$1, invite_expires_at=NULL
		 WHERE EXISTS (SELECT 1 FROM order_workers ow WHERE ow.order_id = orders.id) `+settled,
		models.StatusAccepted)
	if err != nil {
		return 0
	}
	expired, err := s.db.Exec(
		`UPDATE orders SET status=$1, invite_expires_at=NULL
		 WHERE NOT EXISTS (SELECT 1 FROM order_workers ow WHERE ow.order_id = orders.id) `+settled,
		models.StatusExpired)
	if err != nil {
		n, _ := accepted.RowsAffected()
		return n
	}
	a, _ := accepted.RowsAffected()
	e, _ := expired.RowsAffected()
	return a + e
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
	// A company order has no worker on it — the firm's own account is the other
	// side of the job, so it is rated and it rates back exactly like a worker.
	companyUserID := ""
	if o.CompanyID != "" {
		if cp, ok := s.GetCompanyByID(o.CompanyID); ok {
			companyUserID = cp.UserID
		}
	}
	// Who is being rated, and as what. The role comes from the order, not from
	// what profiles the account happens to own: a job with a worker_id was a
	// worker's job, a job with a company_id was a firm's, and whoever rates the
	// customer back is rating them as a customer.
	var toUserID, toRole string
	switch {
	case fromUserID == o.CustomerID:
		if o.WorkerID != "" {
			toUserID, toRole = o.WorkerID, models.RoleWorker
		} else {
			toUserID, toRole = companyUserID, models.RoleCompany
		}
	case o.WorkerID != "" && fromUserID == o.WorkerID:
		toUserID, toRole = o.CustomerID, models.RoleCustomer
	case companyUserID != "" && fromUserID == companyUserID:
		toUserID, toRole = o.CustomerID, models.RoleCustomer
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
		TargetRole: toRole,
		Stars:      stars,
		Comment:    comment,
		CreatedAt:  time.Now(),
	}
	s.db.Exec("INSERT INTO ratings (id, order_id, from_user_id, to_user_id, target_role, stars, comment, created_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)",
		r.ID, r.OrderID, r.FromUserID, r.ToUserID, r.TargetRole, r.Stars, r.Comment, r.CreatedAt)

	// Exactly one aggregate moves, the one for the hat that was worn.
	//
	// This used to be two independent `if`s, so an account registered as both
	// worker and firm had every star written to both averages — and a customer
	// who also worked saw their worker rating shift every time a firm rated them
	// as a customer. A customer has no score of their own to keep, so that case
	// stores the rating and stops.
	switch toRole {
	case models.RoleWorker:
		if wp, ok := s.GetWorkerByUser(toUserID); ok {
			total := wp.Rating*float64(wp.RatingsCount) + float64(stars)
			newCount := wp.RatingsCount + 1
			s.db.Exec("UPDATE workers SET rating=$1, ratings_count=$2 WHERE user_id=$3",
				total/float64(newCount), newCount, toUserID)
		}
	case models.RoleCompany:
		if cp, ok := s.getCompanyByUser(toUserID); ok {
			total := cp.Rating*float64(cp.RatingsCount) + float64(stars)
			newCount := cp.RatingsCount + 1
			s.db.Exec("UPDATE companies SET rating=$1, ratings_count=$2 WHERE user_id=$3",
				total/float64(newCount), newCount, toUserID)
		}
	}
	return r, nil
}

// CustomerCard is the public half of the account that placed an order: who
// they are, how many jobs they have put out, and how the providers who did
// those jobs rated them afterwards.
//
// The phone is never included here — the caller adds it back only when the
// order is already theirs, which keeps one rule in one place.
func (s *Store) CustomerCard(userID string) (*models.OrderCustomer, bool) {
	c := &models.OrderCustomer{ID: userID}
	err := s.db.QueryRow(`
		SELECT COALESCE(u.name, ''), COALESCE(u.avatar_url, ''), u.created_at,
		       (SELECT COUNT(*) FROM orders o WHERE o.customer_id = u.id),
		       (SELECT COALESCE(AVG(r.stars), 0) FROM ratings r
		          WHERE r.to_user_id = u.id AND r.target_role = $2),
		       (SELECT COUNT(*) FROM ratings r
		          WHERE r.to_user_id = u.id AND r.target_role = $2)
		FROM users u WHERE u.id = $1`, userID, models.RoleCustomer).
		Scan(&c.Name, &c.AvatarURL, &c.MemberSince, &c.OrdersCount, &c.Rating, &c.RatingsCount)
	if err != nil {
		return nil, false
	}
	return c, true
}

// ListReviews returns what has been said about a user *in one role*, newest
// first. Pass RoleWorker for a worker profile, RoleCompany for a firm.
//
// The role is not optional. One account can be a worker and a firm at once, and
// without the filter both profiles served the same list of comments: a customer
// reading a firm's page saw complaints about a plumbing job the same account
// did as an individual, and vice versa. The two pages are two reputations.
//
// The aggregate on the profile (4.7 ★, 12 ratings) says how it went; this says
// why. Reviews with no words still come back — the star alone is a data point,
// and dropping them would quietly make a profile look better than its score.
//
// The author's name comes from the users table, so a customer who later renames
// their account is not frozen under the name they had that day.
func (s *Store) ListReviews(toUserID, role string, limit int) []models.Review {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := s.db.Query(`
		SELECT r.id, r.order_id, r.stars, r.comment, r.created_at,
		       COALESCE(u.name, ''), COALESCE(u.avatar_url, '')
		FROM ratings r
		LEFT JOIN users u ON u.id = r.from_user_id
		WHERE r.to_user_id = $1 AND r.target_role = $2
		ORDER BY r.created_at DESC
		LIMIT $3`, toUserID, role, limit)
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
