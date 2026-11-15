package api

import (
	"net/http"
	"time"

	"prostaff/internal/models"
)

// How old a position may be before the customer's map treats it as "signal
// lost" rather than "right here".
const trailStaleAfter = 90 * time.Second

type pointBody struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

// POST /worker/orders/{id}/depart — "I'm on my way". Starts the customer's live
// map. The phone's first position may ride along; it is optional.
func (s *Server) handleWorkerDepart(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	var body pointBody
	if !decodeJSON(w, r, &body) { // an empty body is fine
		return
	}
	before, _ := s.store.GetOrder(r.PathValue("id"))
	order, err := s.store.MarkDeparted(r.PathValue("id"), u.ID, body.Lat, body.Lng)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	s.store.WithBatch(order)
	s.notifyOrderStatus(order)
	if before != nil && before.DepartedAt == nil {
		s.notify(order.CustomerID, "order.departed", "Ishchi yo'lga chiqdi",
			"Ishchi sizga qarab yo'lga chiqdi. Xaritada kuzatishingiz mumkin.", map[string]any{"order_id": order.ID})
	}
	writeJSON(w, http.StatusOK, maskForWorker(*order, u.ID))
}

// POST /worker/orders/{id}/position — the worker's phone reporting where it is
// while they are on the way. Accepted only between "on my way" and "arrived".
func (s *Server) handleWorkerPosition(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	var body pointBody
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := s.store.SetTrail(r.PathValue("id"), u.ID, body.Lat, body.Lng); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GET /orders/{id}/tracking — the customer's live view of the worker coming to
// them. Only the order's own customer (and the office) can read it: a worker's
// position is not for anyone else, and not for the customer before the worker
// has set off or after they have arrived.
func (s *Server) handleOrderTracking(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	order, ok := s.store.GetOrder(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "order not found")
		return
	}
	if order.CustomerID != u.ID && !u.HasRole(models.RoleAdmin) {
		writeError(w, http.StatusForbidden, "forbidden", "not your order")
		return
	}
	out := map[string]any{
		"status":      order.Status,
		"departed_at": order.DepartedAt,
		"arrived_at":  order.ArrivedAt,
		"dest":        map[string]any{"lat": order.Lat, "lng": order.Lng},
	}
	active := order.Status == models.StatusAccepted || order.Status == models.StatusInProgress
	if active && order.DepartedAt != nil && order.ArrivedAt == nil {
		if t, ok := s.store.GetTrail(order.ID); ok {
			out["position"] = map[string]any{"lat": t.Lat, "lng": t.Lng, "at": t.At}
			out["stale"] = time.Since(t.At) > trailStaleAfter
		}
	}
	writeJSON(w, http.StatusOK, out)
}
