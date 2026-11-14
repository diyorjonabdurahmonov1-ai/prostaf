package store

import (
	"database/sql"
	"log"
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
	// BatchID groups orders placed as one request — see models.Order.
	BatchID string
	// BatchSize is how many orders that request was split into. Zero reads as one.
	BatchSize int
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

// busyWorkers is WorkerIsBusy asked about a list of workers at once.
//
// The dispatch sweep asks it about everyone the radius and category filters
// left, every DispatchInterval, for every order searching — which as one query
// per worker was the single most expensive thing the platform did per unit of
// traffic. As one query it is an index scan over the handful of orders that are
// currently live.
//
// An error is logged and answered with "nobody is busy". A worker who is on a
// job and gets rung anyway is refused by Accept and loses a card; a sweep that
// treats a database hiccup as "everyone is busy" dispatches to nobody, which
// nothing downstream would report as anything but an empty city.
func (s *Store) busyWorkers(workerUserIDs []string) map[string]bool {
	busy := make(map[string]bool, len(workerUserIDs))
	if len(workerUserIDs) == 0 {
		return busy
	}
	rows, err := s.db.Query(
		`SELECT DISTINCT worker_id FROM orders
		 WHERE worker_id = ANY($1) AND status IN ('accepted','in_progress')`,
		workerUserIDs)
	if err != nil {
		log.Printf("store: busy sweep failed, dispatching without it: %v", err)
		return busy
	}
	defer rows.Close()
	for rows.Next() {
		var userID string
		if rows.Scan(&userID) == nil {
			busy[userID] = true
		}
	}
	return busy
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
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM orders
		 WHERE customer_id=$1 AND worker_id=$2
		   AND status IN ('accepted','in_progress','completed')`,
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

func (s *Store) scanOrder(row interface{ Scan(dest ...any) error }) (*models.Order, error) {
	var o models.Order
	var invitedIDs, rejectedIDs, images, targetIDs string
	var inviteExpires, responded sql.NullTime
	err := row.Scan(&o.ID, &o.CustomerID, &o.WorkerID, &o.CompanyID, &o.CategoryID,
		&o.Gender, &o.Description, &o.ImageURL, &o.CustomerPhone, &o.WorkerPhone,
		&o.Lat, &o.Lng, &o.Status, &invitedIDs, &inviteExpires, &o.CommissionAmount,
		&o.CreatedAt, &o.CustomerName, &o.Price, &rejectedIDs, &images, &targetIDs,
		&o.BatchID, &o.BatchSize, &o.CancelledBy, &o.CancelReason, &responded)
	if err != nil {
		return nil, err
	}
	if responded.Valid {
		o.RespondedAt = &responded.Time
	}
	// A row from before the column existed, or a lone order: one of one.
	if o.BatchSize < 1 {
		o.BatchSize = 1
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

const orderCols = "id, customer_id, worker_id, company_id, category_id, gender, description, image_url, customer_phone, worker_phone, lat, lng, status, invited_worker_ids, invite_expires_at, commission_amount, created_at, customer_name, price, rejected_worker_ids, images, target_worker_ids, batch_id, batch_size"

// orderReadCols is what scanOrder reads: the columns an order is written with,
// plus the ones only ever set later in its life.
const orderReadCols = orderCols + ", cancelled_by, cancel_reason, responded_at"

// CreateOrder creates a pending order for a customer.
func (s *Store) CreateOrder(customerID string, in OrderInput) (*models.Order, error) {
	// Bookable, not merely existing: a category the office retired keeps its name
	// on the orders already filed under it, but takes no new ones.
	if in.CategoryID == "" || !s.categoryBookable(in.CategoryID) {
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
		BatchID:           in.BatchID,
		BatchSize:         max(in.BatchSize, 1),
		InviteExpiresAt:   &expires,
		CreatedAt:         time.Now(),
	}
	normalizeOrderPhotos(o)
	if _, err := s.db.Exec("INSERT INTO orders ("+orderCols+") VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24)",
		o.ID, o.CustomerID, "", "", o.CategoryID, o.Gender, o.Description, o.ImageURL,
		o.CustomerPhone, "", o.Lat, o.Lng, o.Status, "[]", expires, 0, o.CreatedAt, o.CustomerName, o.Price, "[]",
		marshalJSON(o.Images), marshalJSON(o.TargetWorkerIDs), o.BatchID, o.BatchSize); err != nil {
		log.Printf("store: order insert failed: %v", err)
		return nil, ErrOrderNotSaved
	}
	// After the insert, so a photo is never marked as belonging to an order that
	// failed to save — that file would then be deleted by nothing and kept
	// forever, which is the leak the tagging exists to close.
	s.tagOrderPhotos(o.ID, customerID, o.Images)
	return o, nil
}

// CompanyOrderInput is a job addressed to one firm rather than broadcast.
type CompanyOrderInput struct {
	Description string
	Phone       string
	Name        string
	Images      []string
	ImageURL    string
	Lat         float64
	Lng         float64
}

// CreateCompanyOrder books a job with a named firm.
//
// Nothing is dispatched: the customer picked the firm, the order is addressed to
// it, and it sits at `pending` until the firm answers. The price is the firm's
// own published fee rather than something the customer names — that is the
// difference between hiring a company and calling for a worker.
func (s *Store) CreateCompanyOrder(customerID, companyID string, in CompanyOrderInput) (*models.Order, error) {
	cp, ok := s.GetCompanyByID(companyID)
	if !ok {
		return nil, ErrNotFound
	}
	if cp.Banned || !cp.Online {
		return nil, ErrCompanyUnavailable
	}
	// A firm that could not accept the job if it wanted to must not be offered
	// it: the customer would sit waiting on an answer that cannot come.
	if !cp.CanTakeWork() {
		return nil, ErrCompanyUnavailable
	}
	// No "one job at a time" here, unlike a worker: a firm is a team, and
	// holding it to a single job turned every other customer away for the
	// length of it.
	if cp.Price <= 0 {
		return nil, ErrPriceNotSet
	}

	customerName := in.Name
	if customerName == "" {
		if u, ok := s.GetUser(customerID); ok {
			customerName = u.Name
		}
	}

	o := &models.Order{
		ID:                id.New(),
		CustomerID:        customerID,
		CompanyID:         cp.ID,
		Description:       in.Description,
		ImageURL:          in.ImageURL,
		Images:            in.Images,
		CustomerName:      customerName,
		CustomerPhone:     in.Phone,
		Price:             cp.Price,
		Lat:               in.Lat,
		Lng:               in.Lng,
		Gender:            "any",
		Status:            models.StatusPending,
		InvitedWorkerIDs:  []string{},
		RejectedWorkerIDs: []string{},
		TargetWorkerIDs:   []string{},
		CreatedAt:         time.Now(),
	}
	normalizeOrderPhotos(o)
	if _, err := s.db.Exec("INSERT INTO orders ("+orderCols+") VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24)",
		o.ID, o.CustomerID, "", o.CompanyID, "", o.Gender, o.Description, o.ImageURL,
		o.CustomerPhone, "", o.Lat, o.Lng, o.Status, "[]", nil, 0, o.CreatedAt, o.CustomerName, o.Price, "[]",
		marshalJSON(o.Images), "[]", "", 1); err != nil {
		log.Printf("store: company order insert failed: %v", err)
		return nil, ErrOrderNotSaved
	}
	s.tagOrderPhotos(o.ID, customerID, o.Images)
	return o, nil
}

// CompanyRespond is a firm taking or declining a job addressed to it.
//
// Accepting reveals the firm's number to the customer and spends one of its
// trial jobs when it has no subscription; declining ends the order, because a
// company order was never broadcast and there is nobody else to ask.
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
		o.InviteExpiresAt = nil
		res, err := s.db.Exec(
			`UPDATE orders SET status=$1, invite_expires_at=NULL, cancelled_by='company',
			        responded_at=NOW(), closed_at=NOW()
			 WHERE id=$2 AND status=$3`,
			models.StatusCancelled, orderID, models.StatusPending)
		if err != nil {
			return nil, ErrConflict
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, ErrConflict
		}
		o.CancelledBy = CancelledByCompany
		return o, nil
	}

	if cp.Banned || !cp.CanTakeWork() {
		return nil, ErrSubscriptionExpired
	}
	// Guarded, for the same reason the worker's accept is: two taps from the
	// same firm on two devices must settle to one answer.
	res, err := s.db.Exec(
		`UPDATE orders SET status=$1, worker_phone=$2, responded_at=NOW() WHERE id=$3 AND status=$4`,
		models.StatusAccepted, cp.Phone, orderID, models.StatusPending)
	if err != nil {
		return nil, ErrConflict
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrConflict
	}
	// A subscribed firm pays nothing per job; one on its trial spends one.
	s.chargeCompanyAcceptance(cp)

	o.Status = models.StatusAccepted
	o.WorkerPhone = cp.Phone
	o.InviteExpiresAt = nil
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

// GetOrder returns one order by id.
func (s *Store) GetOrder(orderID string) (*models.Order, bool) {
	o, err := s.scanOrder(s.db.QueryRow("SELECT "+orderReadCols+" FROM orders WHERE id=$1", orderID))
	if err != nil {
		return nil, false
	}
	s.decorateBatch(o, nil)
	return o, true
}

// ListOrdersByCustomer returns a customer's own orders, newest first.
func (s *Store) ListOrdersByCustomer(customerID string) []models.Order {
	rows, err := s.db.Query("SELECT "+orderReadCols+" FROM orders WHERE customer_id=$1 ORDER BY created_at DESC", customerID)
	if err != nil {
		return nil
	}
	var out []models.Order
	for rows.Next() {
		o, err := s.scanOrder(rows)
		if err == nil {
			out = append(out, *o)
		}
	}
	// Closed before the batch reads, which query the table again.
	rows.Close()
	s.decorateBatches(out)
	return out
}

// ListCompanyOrders returns orders directed at a company, newest first.
func (s *Store) ListCompanyOrders(companyUserID string) []models.Order {
	// Get company profile ID from user ID.
	cp, ok := s.getCompanyByUser(companyUserID)
	if !ok {
		return nil
	}
	rows, err := s.db.Query("SELECT "+orderReadCols+" FROM orders WHERE company_id=$1 ORDER BY created_at DESC", cp.ID)
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
	rows, err := s.db.Query("SELECT " + orderReadCols + " FROM orders ORDER BY created_at DESC")
	if err != nil {
		return nil
	}
	var out []models.Order
	for rows.Next() {
		o, err := s.scanOrder(rows)
		if err != nil {
			continue
		}
		if !since.IsZero() && !o.CreatedAt.After(since) {
			continue
		}
		if o.WorkerID == workerUserID {
			out = append(out, *o)
			continue
		}
		// Invited, but somebody else answered first — the offer is over.
		if o.WorkerID != "" {
			continue
		}
		if containsStr(o.InvitedWorkerIDs, wp.ID) {
			out = append(out, *o)
		}
	}
	// Closed before the batch reads, which query the table again.
	rows.Close()
	s.decorateBatches(out)
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
	return matchesOrderWithin(o, wp, matchRadiusKm)
}

// matchesOrderWithin is matchesOrder with the radius the office has set.
func matchesOrderWithin(o *models.Order, wp *models.WorkerProfile, radiusKm float64) bool {
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
	return geo.DistanceKm(o.Lat, o.Lng, wp.Lat, wp.Lng) <= radiusKm
}

/**
 * Workers this order's siblings have already got on the phone.
 *
 * "I need five helpers" is five orders, and each of them broadcasts. Left alone
 * that means every nearby worker is rung five times for what is, to them, one
 * decision: five cards, five pushes, and four of them dead the moment they
 * answer the first. The point of five orders is five *workers*, not five
 * requests each.
 *
 * So an order skips anybody a sibling is currently waiting on. Only siblings
 * still searching hold their workers — once one is accepted, expired or
 * declined, the people it was ringing are released and the next wave of the
 * remaining orders picks them up. That is what makes this converge rather than
 * deadlock when there are fewer workers around than the customer asked for:
 * with two workers and five orders, two get answered and the other three run
 * their window out, which is the honest outcome.
 */
func (s *Store) batchEngagedWorkers(o *models.Order) (map[string]bool, int) {
	engaged := map[string]bool{}
	if o.BatchID == "" {
		return engaged, 0
	}
	rows, err := s.db.Query(
		`SELECT invited_worker_ids FROM orders
		 WHERE batch_id=$1 AND id<>$2 AND status IN ('pending','matching','invited')`,
		o.BatchID, o.ID)
	if err != nil {
		return engaged, 0
	}
	defer rows.Close()
	siblings := 0
	for rows.Next() {
		var invited string
		if rows.Scan(&invited) != nil {
			continue
		}
		siblings++
		for _, id := range unmarshalStrings(invited) {
			engaged[id] = true
		}
	}
	return engaged, siblings
}

// candidatesFor returns every worker who could take this order right now:
// online, not banned, eligible under matchesOrder, not already on a job, not
// already ringing for a sibling order, and able to afford the commission —
// nearest first.
//
// It costs a fixed three queries rather than a number that grows with the city.
// The obvious way to write it is a cursor over the online workers with the two
// remaining questions asked per row, and that is what this was: 1 + 2n round
// trips, every DispatchInterval, for every order still searching. Six hundred
// workers online and ten open searches is around a thousand queries a second,
// spent almost entirely on workers the radius filter was about to drop anyway.
// So the rules that need nothing but the row run first, and the two lookups are
// asked once, for the handful that survived them.
func (s *Store) candidatesFor(o *models.Order) []models.WorkerProfile {
	// One read for the whole sweep rather than a query per candidate.
	engaged, _ := s.batchEngagedWorkers(o)

	shortlist := s.shortlistFor(o, engaged)
	if len(shortlist) == 0 {
		return nil
	}

	// The two facts that live in other tables. Both are filters against noise
	// rather than gates on the job: Accept asks the same two questions again,
	// and it is the answer there that decides who actually gets the work. That
	// is why both lookups fail open — ringing a worker who turns out to be busy
	// costs one dead card on one phone, while failing closed is an order that
	// silently rings nobody and expires looking like the platform is empty.
	userIDs := make([]string, 0, len(shortlist))
	needWallet := make([]string, 0, len(shortlist))
	for i := range shortlist {
		userIDs = append(userIDs, shortlist[i].UserID)
		// A worker still inside the trial never has their balance looked at, so
		// there is no reason to fetch it.
		if shortlist[i].FreeOrdersLeft() == 0 {
			needWallet = append(needWallet, shortlist[i].UserID)
		}
	}
	busy := s.busyWorkers(userIDs)
	solvent := s.walletsCanAccept(needWallet)

	out := make([]models.WorkerProfile, 0, len(shortlist))
	for _, wp := range shortlist {
		// Already on a job — not available until it is finished.
		if busy[wp.UserID] {
			continue
		}
		// A worker who could not accept if they tried is pure noise on the
		// broadcast — but a brand-new one with free jobs left can accept with an
		// empty wallet, which is the whole point of the trial, so this asks the
		// same question Accept will ask rather than only looking at the money.
		if wp.FreeOrdersLeft() == 0 && !solvent[wp.UserID] {
			continue
		}
		dist := geo.DistanceKm(o.Lat, o.Lng, wp.Lat, wp.Lng)
		wp.DistanceKm = &dist
		out = append(out, wp)
	}
	sort.Slice(out, func(i, j int) bool { return *out[i].DistanceKm < *out[j].DistanceKm })
	return out
}

// shortlistFor is the first pass of the sweep: the online workers this order is
// allowed to ring, on the rules that need nothing beyond the row itself.
//
// It is a function of its own so that the cursor is closed before candidatesFor
// asks the database anything more. A rows loop holds a pool connection open for
// as long as it runs, and querying inside one means every dispatching goroutine
// holds two of the thirty the pool is capped at (DB_MAX_OPEN_CONNS) — which
// under a burst of simultaneous searches is a pool deadlocking against itself.
func (s *Store) shortlistFor(o *models.Order, engaged map[string]bool) []models.WorkerProfile {
	rows, err := s.db.Query("SELECT " + workerCols + " FROM workers WHERE online=TRUE AND banned=FALSE")
	if err != nil {
		return nil
	}
	defer rows.Close()
	radius := s.radiusKm()
	var out []models.WorkerProfile
	for rows.Next() {
		wp, err := s.scanWorker(rows)
		if err != nil {
			continue
		}
		if !matchesOrderWithin(o, wp, radius) {
			continue
		}
		// A sibling order of the same request is already waiting on them.
		if engaged[wp.ID] {
			continue
		}
		out = append(out, *wp)
	}
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

	// Read once: the sweep is a full scan of the online workers plus a busy check
	// each, and it was being run twice per wave.
	candidates := s.candidatesFor(o)

	/**
	 * How many new workers this wave may claim.
	 *
	 * A lone order takes everybody: that is the broadcast, and the whole point is
	 * that the first free phone answers fast.
	 *
	 * An order that is one of five behaves differently, because its siblings need
	 * workers too. Left to take all ten nearby workers, the first order of the
	 * batch would engage every one of them and the other four would find nobody —
	 * so the five would fill strictly one after another, each waiting for the
	 * previous to be answered. Sharing the free workers out means all five search
	 * at once, which is what asking for five people at once should do.
	 *
	 * Rounded up, and never below one: an order that can only reach a single
	 * worker must still be allowed to reach them.
	 */
	share := 0
	if o.BatchID != "" {
		// How many others are sharing the pool. `BatchSize` is what the request
		// said it was splitting into and is right from the very first wave, when
		// the sibling rows may not be committed yet; the count of siblings
		// actually searching is right later on, once some have been answered and
		// the rest are what is left. Whichever is larger is the safe divisor —
		// under-counting is what lets one order take everybody.
		_, searching := s.batchEngagedWorkers(o)
		siblings := max(o.BatchSize-1, searching)
		if siblings > 0 {
			available := len(candidates)
			share = (available + siblings) / (siblings + 1)
			if share < 1 {
				share = 1
			}
		}
	}

	// Nearest first. The candidates come sorted by distance, so capping a wave
	// rings the closest few and leaves the rest for the next wave, twelve
	// seconds on, if nobody has taken the job by then. Workers the customer
	// picked by hand are all rung at once: there is nobody to prefer among them.
	if wave := s.settings.DispatchWaveSize; wave > 0 && len(o.TargetWorkerIDs) == 0 {
		if share == 0 || share > wave {
			share = wave
		}
	}

	var newProfileIDs, newUserIDs []string
	for _, wp := range candidates {
		if containsStr(o.InvitedWorkerIDs, wp.ID) || containsStr(o.RejectedWorkerIDs, wp.ID) {
			continue
		}
		if share > 0 && len(newProfileIDs) >= share {
			break
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
	return s.reopen(o)
}

// reopen puts an order nobody is working on back into the search.
func (s *Store) reopen(o *models.Order) (*models.Order, error) {
	// A firm's order is a request to that one firm; there is nobody to search.
	if o.CompanyID != "" {
		return nil, ErrConflict
	}
	expires := time.Now().Add(InviteTTL)

	// The worker who took the job dropped it. The order is cancelled with their
	// name still on it, and "find me somebody else" has to mean exactly that:
	// the search re-opens for everyone except the one who walked away.
	if o.Status == models.StatusCancelled && o.WorkerID != "" && o.CancelledBy != CancelledByCustomer &&
		o.CancelledBy != CancelledByAdmin {
		dropped := []string{}
		if wp, ok := s.GetWorkerByUser(o.WorkerID); ok {
			dropped = append(dropped, wp.ID)
		}
		res, err := s.db.Exec(
			`UPDATE orders SET status=$1, invited_worker_ids='[]', rejected_worker_ids=$2, invite_expires_at=$3,
			        worker_id='', worker_phone='', cancelled_by='', cancel_reason='', responded_at=NULL, closed_at=NULL
			 WHERE id=$4 AND status='cancelled' AND worker_id=$5`,
			models.StatusMatching, marshalJSON(dropped), expires, o.ID, o.WorkerID)
		if err != nil {
			return nil, ErrConflict
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, ErrConflict
		}
		o.Status = models.StatusMatching
		o.InvitedWorkerIDs = []string{}
		o.RejectedWorkerIDs = dropped
		o.InviteExpiresAt = &expires
		o.WorkerID, o.WorkerPhone = "", ""
		o.CancelledBy, o.CancelReason, o.RespondedAt = "", "", nil
		return o, nil
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
	res, err := s.db.Exec(
		`UPDATE orders SET status=$1, invited_worker_ids='[]', rejected_worker_ids='[]', invite_expires_at=$2,
		        closed_at=NULL
		 WHERE id=$3 AND worker_id='' AND status IN ('pending','matching','invited','expired','rejected')`,
		models.StatusMatching, expires, o.ID)
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
//
// One order, one worker — deliberately. A job needing five people is five
// orders, not one order with five places on it. The multi-place version was
// built and taken back out: a half-filled job left the workers who *had*
// accepted charged their commission for an evening that was one person short,
// with no way to price or refund the difference. Splitting the job splits the
// money question with it — each order is taken or it is not, and nobody is ever
// owed a partial refund.
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
	if !s.canWorkerAccept(wp) {
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
		`UPDATE orders SET status=$1, worker_id=$2, worker_phone=$3, invite_expires_at=NULL, commission_amount=$4,
		        responded_at=NOW()
		 WHERE id=$5 AND worker_id='' AND status IN ('matching','invited')`,
		models.StatusAccepted, workerUserID, wp.Phone, commission, orderID)
	if err != nil {
		return nil, nil, ErrConflict
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Somebody else was a fraction of a second faster.
		return nil, nil, ErrConflict
	}
	// Only the winner pays — and a worker still inside their three free jobs
	// spends one of those instead of the commission. The order row is corrected
	// afterwards rather than before, because whether the job is free is only
	// settled by the atomic consume inside the call.
	charged := s.chargeWorkerAcceptance(wp, commission)
	if charged != commission {
		s.db.Exec("UPDATE orders SET commission_amount=$1 WHERE id=$2", charged, orderID)
	}

	o.Status = models.StatusAccepted
	o.WorkerID = workerUserID
	o.WorkerPhone = wp.Phone
	o.InviteExpiresAt = nil
	o.CommissionAmount = charged
	// The profile handed back is what the app redraws its banner from, so it has
	// to know a trial job was just spent.
	wp.FreeOrdersUsed = s.freeOrdersUsed(WorkerTrialKey(wp.PassportSeries))
	return o, wp, nil
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

// ExpireIfSearching flips an order to expired only if the search is still
// running and its window has actually run out. An order somebody already took
// is left alone, and so is one the customer just re-opened — otherwise a
// dispatch loop finishing a fraction of a second late would kill a search that
// had only just started again.
func (s *Store) ExpireIfSearching(orderID string) (*models.Order, bool) {
	res, err := s.db.Exec(
		`UPDATE orders SET status=$1, invite_expires_at=NULL, closed_at=NOW()
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
	// A search that found nobody is as finished as a job that was done, and its
	// photos are worth exactly as much — nothing. This is the common case on a
	// quiet night, and the one that would otherwise fill the disk fastest.
	s.purgeOnClose(o)
	return o, true
}

// ExpireStaleSearches closes searches whose window ran out while the server was
// down — their dispatch loops died with the process, so nothing else would ever
// finish them. Returns how many were closed.
func (s *Store) ExpireStaleSearches() int64 {
	// Photos are not touched here: this closes an unbounded number of orders in
	// one statement, and the sweeper in cmd/server is what cleans up after it.
	res, err := s.db.Exec(
		`UPDATE orders SET status=$1, invite_expires_at=NULL, closed_at=NOW()
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
	return s.complete(o)
}

// complete closes a job that is actually under way.
//
// It used to write "completed" over whatever was there, so a cancelled order,
// or one still looking for a worker, could be closed as done — and then rated,
// and counted as a finished job in every report. Only a job somebody took can
// be finished. Asking twice is not an error: the second answer is the first.
func (s *Store) complete(o *models.Order) (*models.Order, error) {
	if o.Status == models.StatusCompleted {
		return o, nil
	}
	res, err := s.db.Exec(
		`UPDATE orders SET status=$1, invite_expires_at=NULL, closed_at=NOW()
		 WHERE id=$2 AND status IN ('accepted','in_progress')`,
		models.StatusCompleted, o.ID)
	if err != nil {
		return nil, ErrConflict
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrConflict
	}
	o.Status = models.StatusCompleted
	o.InviteExpiresAt = nil
	// The job is over, so its photos are: see order_photos.go. Nothing here can
	// touch a profile picture — only files stamped with this order's id go.
	s.purgeOnClose(o)
	return o, nil
}

// CompleteByWorker is the worker holding a job saying it is done.
//
// Only the customer could close a job, so a customer who never tapped "Ish
// tugadi" left the worker stuck on it — and a worker on a job is offered no
// other work. The worker may now close their own job, once it is actually
// theirs and under way; the customer is told and can rate it as before.
//
// Not while a multi-worker request is still gathering: that worker has not
// been sent to the job yet, so there is nothing to finish.
func (s *Store) CompleteByWorker(orderID, workerUserID string) (*models.Order, error) {
	o, ok := s.GetOrder(orderID)
	if !ok {
		return nil, ErrNotFound
	}
	if o.WorkerID == "" || o.WorkerID != workerUserID {
		return nil, ErrForbidden
	}
	if o.BatchReady != nil && !*o.BatchReady {
		return nil, ErrConflict
	}
	res, err := s.db.Exec(
		`UPDATE orders SET status=$1, invite_expires_at=NULL, closed_at=NOW()
		 WHERE id=$2 AND worker_id=$3 AND status IN ('accepted','in_progress')`,
		models.StatusCompleted, orderID, workerUserID)
	if err != nil {
		return nil, ErrConflict
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrConflict
	}
	o.Status = models.StatusCompleted
	o.InviteExpiresAt = nil
	s.purgeOnClose(o)
	return o, nil
}

// Who called an order off — models.Order.CancelledBy.
const (
	CancelledByCustomer = "customer"
	CancelledByWorker   = "worker"
	CancelledByCompany  = "company"
	CancelledByAdmin    = "admin"
)

// Cancel marks an order cancelled, and records who did it.
//
// Who may cancel depends on where the order is. The customer may withdraw it
// at any point before it is finished — including a search that already ran out,
// which is how a place in a multi-worker request is given up. The worker or
// firm holding it may drop it once it is theirs. Nobody may cancel a finished
// job: that used to be possible, and turned a completed, rated order back into
// a cancelled one.
func (s *Store) Cancel(orderID, actorID string) (*models.Order, error) {
	o, ok := s.GetOrder(orderID)
	if !ok {
		return nil, ErrNotFound
	}
	by := ""
	switch {
	case o.CustomerID == actorID:
		by = CancelledByCustomer
	case o.WorkerID != "" && o.WorkerID == actorID:
		by = CancelledByWorker
	case o.CompanyID != "":
		if cp, ok := s.GetCompanyByID(o.CompanyID); ok && cp.UserID == actorID {
			by = CancelledByCompany
		}
	}
	if by == "" {
		return nil, ErrForbidden
	}
	return s.cancel(o, by, "")
}

// cancel is the one place an order becomes cancelled.
func (s *Store) cancel(o *models.Order, by, reason string) (*models.Order, error) {
	// Asking twice is not an error, and does not rewrite who cancelled first.
	if o.Status == models.StatusCancelled {
		return o, nil
	}
	allowed := []string{models.StatusAccepted, models.StatusInProgress}
	switch by {
	case CancelledByCustomer, CancelledByAdmin:
		allowed = append(allowed, models.StatusPending, models.StatusMatching, models.StatusInvited,
			models.StatusExpired, models.StatusRejected)
	case CancelledByCompany:
		allowed = append(allowed, models.StatusPending)
	}
	res, err := s.db.Exec(
		`UPDATE orders SET status=$1, invite_expires_at=NULL, closed_at=NOW(), cancelled_by=$2, cancel_reason=$3
		 WHERE id=$4 AND status = ANY($5)`,
		models.StatusCancelled, by, reason, o.ID, allowed)
	if err != nil {
		return nil, ErrConflict
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrConflict
	}
	o.Status = models.StatusCancelled
	o.InviteExpiresAt = nil
	o.CancelledBy = by
	o.CancelReason = reason
	s.purgeOnClose(o)
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
