package api

import (
	"fmt"
	"net/http"
	"strconv"

	"prostaff/internal/models"
	"prostaff/internal/sse"
	"prostaff/internal/store"
)

// GET /workers/nearby?lat=&lng=&radius_km= — online workers around a point.
func (s *Server) handleNearbyWorkers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	lat, _ := strconv.ParseFloat(q.Get("lat"), 64)
	lng, _ := strconv.ParseFloat(q.Get("lng"), 64)
	radius, _ := strconv.ParseFloat(q.Get("radius_km"), 64)
	u := userFrom(r)
	found := s.store.NearbyWorkers(lat, lng, radius, u.ID)
	out := make([]models.WorkerProfile, 0, len(found))
	for _, wp := range found {
		out = append(out, s.publicWorker(u, wp))
	}
	writeJSON(w, http.StatusOK, out)
}

// GET /workers/{id} — public worker profile.
func (s *Server) handleGetWorker(w http.ResponseWriter, r *http.Request) {
	wp, ok := s.store.GetWorkerByID(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "worker not found")
		return
	}
	writeJSON(w, http.StatusOK, s.publicWorker(userFrom(r), *wp))
}

// publicWorker is a worker record as somebody browsing is allowed to see it —
// which is everything except the phone number.
//
// A directory of reachable workers is exactly what the platform sells, and
// printing the number on the profile handed it out for free: a customer could
// read it off the search results and ring the worker directly, and the job that
// followed was invisible to the platform and earned it nothing. Worse for the
// worker, who is then negotiating without the order, the price or the record.
//
// The number appears the moment it is needed and not before: when this worker
// accepted an order from this customer. It stays visible on the finished job,
// because "call them back about yesterday" is a real thing people do.
// The passport series never leaves the office. It is on the record so the
// platform can count three free jobs per person, and it is nobody's business but
// the admin's and the worker's own — not a customer's, not even a customer who
// has hired them.
func (s *Server) publicWorker(viewer *models.User, wp models.WorkerProfile) models.WorkerProfile {
	if viewer == nil {
		wp.Phone = ""
		wp.PassportSeries = ""
		return wp
	}
	if wp.UserID == viewer.ID || viewer.HasRole(models.RoleAdmin) {
		return wp
	}
	wp.PassportSeries = ""
	if s.store.HasHiredWorker(viewer.ID, wp.UserID) {
		return wp
	}
	wp.Phone = ""
	return wp
}

// GET /workers/{id}/reviews — what customers wrote about this worker.
//
// Separate from the profile rather than embedded in it: the profile is fetched
// on every list tap and a worker with two hundred jobs behind them would drag
// every one of those comments along with it.
//
// `{id}` is the worker profile id, the same one the profile route takes, while
// ratings are keyed by account — hence the lookup before the query.
func (s *Server) handleWorkerReviews(w http.ResponseWriter, r *http.Request) {
	wp, ok := s.store.GetWorkerByID(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "worker not found")
		return
	}
	writeJSON(w, http.StatusOK, s.store.ListReviews(wp.UserID, models.RoleWorker, 50))
}

// PATCH /worker/profile
//
// The worker owns their terms (price, categories) and how they present
// themselves (photo, bio, experience, area, work gallery). Identity — name,
// phone, age, gender — stays with the admin who opened the account.
func (s *Server) handlePatchWorkerProfile(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	var body struct {
		PriceMin    *int64    `json:"price_min"`
		PriceMax    *int64    `json:"price_max"`
		CategoryIDs *[]string `json:"category_ids"`

		AvatarURL       *string   `json:"avatar_url"`
		About           *string   `json:"about"`
		ExperienceYears *int      `json:"experience_years"`
		Address         *string   `json:"address"`
		Images          *[]string `json:"images"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	wp, err := s.store.UpdateWorkerProfile(u.ID, store.WorkerPatch{
		PriceMin:        body.PriceMin,
		PriceMax:        body.PriceMax,
		CategoryIDs:     body.CategoryIDs,
		AvatarURL:       body.AvatarURL,
		About:           body.About,
		ExperienceYears: body.ExperienceYears,
		Address:         body.Address,
		Images:          body.Images,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, wp)
}

// POST /worker/status
func (s *Server) handleWorkerStatus(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	var body struct {
		Online bool     `json:"online"`
		Lat    *float64 `json:"lat"`
		Lng    *float64 `json:"lng"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	wp, err := s.store.SetWorkerStatus(u.ID, body.Online, body.Lat, body.Lng)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	// Mirror the toggle into the socket hub, so dispatch can pick recipients
	// from memory instead of asking the database about every worker in turn.
	if wp.Online && !wp.Banned {
		s.ws.SetPresence(u.ID, presenceOf(wp.Lat, wp.Lng, wp.CategoryIDs, wp.Gender, true))
	} else {
		s.ws.ClearPresence(u.ID)
	}
	writeJSON(w, http.StatusOK, wp)
}

// GET /worker/orders?since=<rfc3339>
func (s *Server) handleWorkerOrders(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	since := parseSince(r.URL.Query().Get("since"))
	orders := s.store.ListWorkerOrders(u.ID, since)
	out := make([]models.Order, 0, len(orders))
	for _, o := range orders {
		out = append(out, maskForWorker(o, u.ID))
	}
	writeJSON(w, http.StatusOK, out)
}

// POST /worker/orders/{id}/accept
func (s *Server) handleWorkerAccept(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	orderID := r.PathValue("id")
	order, wp, err := s.store.Accept(orderID, u.ID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	// Whether the job now has everybody it needs. A one-person order is full on
	// the first yes, which is what "taken" always meant; a five-person one is
	// not, and the difference decides both what the customer is told and whether
	// the other workers keep their card.
	full := len(order.AcceptedWorkers) >= order.WorkersNeeded

	// Notify the customer that a worker accepted (reveal worker phone).
	s.publish(order.CustomerID, sse.Event{
		Type: "order.accepted",
		Data: map[string]any{"order": order, "worker": wp, "phone": wp.Phone, "full": full},
	})

	// Everyone else the order was offered to.
	//
	// On a full job they lost the race, and the offer comes off their screen
	// rather than leaving them ringing at work that is gone. On a job still
	// short of its crew there is no race to lose — four places are open — so
	// they get the updated order and keep their card. Sending `order.taken`
	// there would have cleared the invite from every other phone the moment the
	// first of five accepted, and the job would never have found the other four.
	rest := *order
	rest.CustomerPhone = ""
	// A fresh slice, and by index: ranging by value would have blanked a copy
	// and posted every crew member's number to every other invited worker.
	crew := make([]models.OrderWorker, len(order.AcceptedWorkers))
	copy(crew, order.AcceptedWorkers)
	for i := range crew {
		crew[i].Phone = ""
	}
	rest.AcceptedWorkers = crew
	event := "order.status"
	if full {
		event = "order.taken"
	}
	for _, pid := range order.InvitedWorkerIDs {
		otherID, ok := s.store.WorkerUserID(pid)
		if !ok || otherID == u.ID {
			continue
		}
		s.publish(otherID, sse.Event{
			Type: event,
			Data: map[string]any{"order": rest},
		})
	}

	// And tell the customer for keeps: a push for the phone in their pocket and
	// a row in their notification list for when they come back to it. On a crew
	// job the count is the news — "3 / 5" is what they are waiting on.
	body := wp.FullName + " buyurtmangizni qabul qildi. Telefon raqami: " + wp.Phone
	if order.WorkersNeeded > 1 {
		body = fmt.Sprintf("%s qabul qildi (%d / %d). Telefon raqami: %s",
			wp.FullName, len(order.AcceptedWorkers), order.WorkersNeeded, wp.Phone)
	}
	s.notify(order.CustomerID, "order.accepted", "Buyurtma qabul qilindi!", body,
		map[string]any{"order_id": order.ID})
	// If that job was the last free one, say so now rather than letting the
	// worker discover it when their phone goes quiet.
	s.notifyTrialSpent(u.ID, wp.FreeOrdersLeft(), models.RoleWorker)
	writeJSON(w, http.StatusOK, order)
}

// POST /worker/orders/{id}/reject
//
// One worker declining does not end the search — the order keeps ringing the
// other workers nearby until somebody takes it or the window closes.
//
// Unless there is nobody else. An order that named its workers can run out of
// people to ask, and when the last of them declines the customer hears about it
// now rather than watching a countdown finish on a search that is already over.
// The distinction matters to them: "nobody was free" invites trying again in ten
// minutes, "they said no" invites picking somebody else.
func (s *Server) handleWorkerReject(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	order, exhausted, err := s.store.Reject(r.PathValue("id"), u.ID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if exhausted {
		s.publish(order.CustomerID, sse.Event{
			Type: "order.rejected",
			Data: map[string]any{"order": order},
		})
		s.notify(order.CustomerID, "order.rejected",
			"Buyurtmangiz rad etildi",
			"Siz tanlagan ishchi buyurtmani qabul qilmadi. Boshqa ishchi tanlang yoki narxni oshiring.",
			map[string]any{"order_id": order.ID})
	}
	writeJSON(w, http.StatusOK, maskForWorker(*order, u.ID))
}

// maskForWorker hides the customer phone unless this worker holds a place on
// the order (contract: the customer's number is revealed only on accept).
//
// The crew is masked too, and separately: a worker who took one of five places
// is told who else is coming, because they are working alongside them, but a
// job does not hand out four phone numbers to each of the five people on it.
// The customer gets the numbers — they are the one who has to ring the people
// arriving at their house.
func maskForWorker(o models.Order, workerUserID string) models.Order {
	mine := false
	for _, ow := range o.AcceptedWorkers {
		if ow.WorkerID == workerUserID {
			mine = true
			break
		}
	}
	// An order carrying no crew list has not been hydrated (the older read
	// paths); fall back to the first-accepter column it does carry.
	if !mine && len(o.AcceptedWorkers) == 0 {
		mine = o.WorkerID == workerUserID
	}
	if !mine {
		o.CustomerPhone = ""
	}
	crew := make([]models.OrderWorker, 0, len(o.AcceptedWorkers))
	for _, ow := range o.AcceptedWorkers {
		if ow.WorkerID != workerUserID {
			ow.Phone = ""
		}
		crew = append(crew, ow)
	}
	o.AcceptedWorkers = crew
	return o
}

// maskForCompany applies that same contract to a firm.
//
// It did not, and the difference was not a decision anybody made — it fell out
// of a firm owning the order row from the moment the customer picked the firm,
// while a worker is only ever *invited* to one. So the phone was on the card
// before the firm had agreed to anything: a firm could read every number that
// came in and call them all back directly, declining each job in the app. The
// worker side has never allowed that.
//
// Holding the job is the line, and status is what says so — a company order
// carries its company_id from creation, so that cannot be the test.
func maskForCompany(o models.Order) models.Order {
	switch o.Status {
	case models.StatusAccepted, models.StatusInProgress, models.StatusCompleted:
		return o
	}
	o.CustomerPhone = ""
	return o
}
