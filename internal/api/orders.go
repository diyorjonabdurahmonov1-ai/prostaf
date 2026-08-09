package api

import (
	"net/http"
	"time"

	"prostaff/internal/models"
	"prostaff/internal/push"
	"prostaff/internal/sse"
	"prostaff/internal/store"
)

// POST /orders
func (s *Server) handleCreateOrder(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	var body struct {
		CategoryID  string `json:"category_id"`
		Gender      string `json:"gender"`
		Description string `json:"description"`
		// Both are accepted: an app built before an order could carry several
		// photos sends `image_url`, and the store reconciles the two.
		Images   []string `json:"images"`
		ImageURL string   `json:"image_url"`
		Phone    string   `json:"phone"`
		Price       int64   `json:"price"`
		Lat         float64 `json:"lat"`
		Lng         float64 `json:"lng"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Phone == "" {
		body.Phone = u.Phone
	}
	order, err := s.store.CreateOrder(u.ID, store.OrderInput{
		CategoryID:  body.CategoryID,
		Gender:      body.Gender,
		Description: body.Description,
		Images:      body.Images,
		ImageURL:    body.ImageURL,
		Phone:       body.Phone,
		Price:       body.Price,
		Lat:         body.Lat,
		Lng:         body.Lng,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	// The customer never picks a worker: the order goes straight out to every
	// worker nearby and the first to accept takes it.
	order = s.dispatch(order.ID, order)
	writeJSON(w, http.StatusCreated, order)
}

// GET /orders (caller's own orders — as customer + as company owner)
func (s *Server) handleListOrders(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	orders := s.store.ListOrdersByCustomer(u.ID)
	// Also include orders directed at the user's company.
	companyOrders := s.store.ListCompanyOrders(u.ID)
	seen := make(map[string]bool, len(orders))
	for _, o := range orders {
		seen[o.ID] = true
	}
	for _, o := range companyOrders {
		if !seen[o.ID] {
			orders = append(orders, o)
		}
	}
	if orders == nil {
		orders = []models.Order{}
	}
	writeJSON(w, http.StatusOK, orders)
}

// GET /orders/{id}
func (s *Server) handleGetOrder(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	order, ok := s.store.GetOrder(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "order not found")
		return
	}
	// Check if the user owns the company this order is directed to.
	isCompanyOwner := false
	if order.CompanyID != "" {
		if cp, ok := s.store.GetCompanyByID(order.CompanyID); ok && cp.UserID == u.ID {
			isCompanyOwner = true
		}
	}
	switch {
	case order.CustomerID == u.ID || order.WorkerID == u.ID || isCompanyOwner || u.HasRole(models.RoleAdmin):
		writeJSON(w, http.StatusOK, order)
	default:
		// Invited worker may view a masked copy.
		if wp, ok := s.store.GetWorkerByUser(u.ID); ok && contains(order.InvitedWorkerIDs, wp.ID) {
			writeJSON(w, http.StatusOK, maskForWorker(*order, u.ID))
			return
		}
		writeError(w, http.StatusForbidden, "forbidden", "not your order")
	}
}

// POST /orders/{id}/dispatch — search again for an order nobody took.
func (s *Server) handleRedispatch(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	orderID := r.PathValue("id")
	order, err := s.store.Redispatch(orderID, u.ID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.dispatch(orderID, order))
}

// dispatch broadcasts an open order to every nearby worker and keeps the search
// running in the background.
//
// The first wave goes out inline so the response already carries who is being
// called; after that a background loop re-broadcasts every DispatchInterval,
// which is what lets a worker who comes online (or finishes a job) 30 seconds
// into the search still get the call. When the window closes with nobody having
// accepted, the order expires and the customer is told.
//
// fallback is returned if the order vanished underneath us.
func (s *Server) dispatch(orderID string, fallback *models.Order) *models.Order {
	order, workerUserIDs, searching := s.store.DispatchWave(orderID)
	if order == nil {
		return fallback
	}
	s.notifyInvited(order, workerUserIDs)
	if searching {
		go s.dispatchLoop(orderID)
		// The loop only wakes every DispatchInterval, so on its own the order
		// could sit "searching" for a few seconds past a countdown the customer
		// is watching tick to zero. This closes the window on the second.
		time.AfterFunc(store.InviteTTL+time.Second, func() { s.expireIfDue(orderID) })
	}
	return order
}

// expireIfDue closes a search whose window has run out and tells the customer.
// Safe to call at any time: an order that was taken, or whose window was
// re-opened, is left alone.
//
// The notification is the point of this, not the socket event. A search nobody
// answered used to end in silence — the row quietly moved to the finished tab
// and a customer who had put their phone down never learned that no worker was
// coming. "Nobody took it" is news, and it is the one outcome the customer has
// to act on.
func (s *Server) expireIfDue(orderID string) {
	if expired, changed := s.store.ExpireIfSearching(orderID); changed {
		s.publish(expired.CustomerID, sse.Event{
			Type: "order.expired",
			Data: map[string]any{"order": expired},
		})
		s.notify(expired.CustomerID, "order.expired",
			"Buyurtmangizni hech kim qabul qilmadi",
			"Yaqin atrofda bo'sh ishchi topilmadi. Buyurtmani qaytadan yuboring yoki narxni oshiring.",
			map[string]any{"order_id": expired.ID})
	}
}

// dispatchLoop keeps re-broadcasting until the order is taken or the window
// closes. It is bounded by the invite window, so it always ends.
func (s *Server) dispatchLoop(orderID string) {
	for {
		time.Sleep(store.DispatchInterval)
		order, workerUserIDs, searching := s.store.DispatchWave(orderID)
		if order == nil {
			return
		}
		s.notifyInvited(order, workerUserIDs)
		if !searching {
			break
		}
	}
	// Backstop for the precise timer armed in dispatch().
	s.expireIfDue(orderID)
}

// notifyInvited rings the workers a wave just went out to: a realtime event for
// open apps and a high-priority push for closed ones. The customer's phone is
// masked until somebody accepts.
func (s *Server) notifyInvited(order *models.Order, workerUserIDs []string) {
	if order == nil || len(workerUserIDs) == 0 {
		return
	}
	masked := *order
	masked.CustomerPhone = ""

	// One event, every worker in the wave. The socket half encodes the payload
	// once; ringing a hundred phones is a hundred writes, not a hundred
	// serialisations.
	s.publishMany(workerUserIDs, sse.Event{
		Type: "order.invite",
		Data: map[string]any{"order": masked, "expires_at": order.InviteExpiresAt},
	})

	var pushMessages []push.Message
	for _, wuid := range workerUserIDs {
		// Stored, not notified: the push for this wave goes out below in one
		// batched call, and going through notify() would ring every worker
		// twice for the same job.
		s.record(wuid, "order.invite", "Yangi buyurtma!", order.Description,
			map[string]any{"order_id": order.ID})
		// High priority + time-sensitive so it rings through Focus modes; the
		// TTL matches the search window — a late invite is just noise.
		if tok := s.store.GetPushToken(wuid); tok != "" {
			pushMessages = append(pushMessages, push.Urgent(
				tok,
				"Yangi buyurtma!",
				order.Description,
				map[string]any{"type": "order.invite", "order_id": order.ID},
				int(store.InviteTTL.Seconds()),
			))
		}
	}
	push.Send(pushMessages...)
}

// POST /orders/{id}/complete
func (s *Server) handleComplete(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	order, err := s.store.Complete(r.PathValue("id"), u.ID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	s.notifyOrderStatus(order)
	writeJSON(w, http.StatusOK, order)
}

// POST /orders/{id}/cancel
func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	order, err := s.store.Cancel(r.PathValue("id"), u.ID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	s.notifyOrderStatus(order)
	writeJSON(w, http.StatusOK, order)
}

// POST /orders/{id}/rate (two-way)
func (s *Server) handleRate(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	var body struct {
		Stars   int    `json:"stars"`
		Comment string `json:"comment"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	rating, err := s.store.Rate(r.PathValue("id"), u.ID, body.Stars, body.Comment)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, rating)
}

// notifyOrderStatus pushes an order.status event to both parties.
func (s *Server) notifyOrderStatus(order *models.Order) {
	ev := sse.Event{Type: "order.status", Data: map[string]any{"order": order}}
	s.publish(order.CustomerID, ev)
	if order.WorkerID != "" {
		s.publish(order.WorkerID, ev)
	}
}

func contains(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}
