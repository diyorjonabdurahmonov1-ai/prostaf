package api

import (
	"net/http"
	"strconv"

	"prostaff/internal/models"
	"prostaff/internal/push"
	"prostaff/internal/sse"
	"prostaff/internal/store"
)

// GET /workers/nearby?lat=&lng=&radius_km= — online workers around a point.
func (s *Server) handleNearbyWorkers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	lat, _ := strconv.ParseFloat(q.Get("lat"), 64)
	lng, _ := strconv.ParseFloat(q.Get("lng"), 64)
	radius, _ := strconv.ParseFloat(q.Get("radius_km"), 64)
	writeJSON(w, http.StatusOK, s.store.NearbyWorkers(lat, lng, radius, userFrom(r).ID))
}

// GET /workers/{id} — public worker profile.
func (s *Server) handleGetWorker(w http.ResponseWriter, r *http.Request) {
	wp, ok := s.store.GetWorkerByID(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "worker not found")
		return
	}
	writeJSON(w, http.StatusOK, wp)
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
	writeJSON(w, http.StatusOK, s.store.ListReviews(wp.UserID, 50))
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
	// Notify the customer that a worker accepted (reveal worker phone).
	s.publish(order.CustomerID, sse.Event{
		Type: "order.accepted",
		Data: map[string]any{"order": order, "worker": wp, "phone": order.WorkerPhone},
	})
	// Everyone else the order was broadcast to lost the race — take the offer
	// off their screen instead of leaving them ringing at a job that is gone.
	taken := *order
	taken.CustomerPhone = ""
	for _, pid := range order.InvitedWorkerIDs {
		loserID, ok := s.store.WorkerUserID(pid)
		if !ok || loserID == u.ID {
			continue
		}
		s.publish(loserID, sse.Event{
			Type: "order.taken",
			Data: map[string]any{"order": taken},
		})
	}
	// Push notification to customer.
	if tok := s.store.GetPushToken(order.CustomerID); tok != "" {
		push.Send(push.Message{
			To:    tok,
			Title: "Buyurtma qabul qilindi!",
			Body:  wp.FullName + " buyurtmangizni qabul qildi",
			Data:  map[string]any{"type": "order.accepted", "order_id": order.ID},
		})
	}
	writeJSON(w, http.StatusOK, order)
}

// POST /worker/orders/{id}/reject
//
// One worker declining does not end the search — the order keeps ringing the
// other workers nearby until somebody takes it or the window closes.
func (s *Server) handleWorkerReject(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	order, err := s.store.Reject(r.PathValue("id"), u.ID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, maskForWorker(*order, u.ID))
}

// maskForWorker hides the customer phone unless the order is assigned to the
// worker (contract: customer phone is revealed only on accept).
func maskForWorker(o models.Order, workerUserID string) models.Order {
	if o.WorkerID != workerUserID {
		o.CustomerPhone = ""
	}
	return o
}
