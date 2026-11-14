package api

import (
	"net/http"
	"strings"

	"prostaff/internal/models"
	"prostaff/internal/sse"
)

// adminOrderJSON answers an office action with the order as the office's list
// shows it — names joined in — so the screen can redraw from the response.
func (s *Server) adminOrderJSON(w http.ResponseWriter, id string) {
	ao, ok := s.store.GetAdminOrder(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "order not found")
		return
	}
	writeJSON(w, http.StatusOK, ao)
}

// GET /admin/orders/{id}
func (s *Server) handleAdminGetOrder(w http.ResponseWriter, r *http.Request) {
	s.adminOrderJSON(w, r.PathValue("id"))
}

// POST /admin/orders/{id}/cancel — body { reason }.
func (s *Server) handleAdminCancelOrder(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Reason string `json:"reason"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	// Read before it changes: the phones still ringing have to be told.
	before, _ := s.store.GetOrder(r.PathValue("id"))
	order, err := s.store.AdminCancel(r.PathValue("id"), in.Reason)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	s.withdrawOffer(before)
	s.settleBatch(order)
	s.notifyOrderStatus(order)
	s.notifyRefund(order)
	body := "Buyurtma administrator tomonidan bekor qilindi."
	if reason := strings.TrimSpace(in.Reason); reason != "" {
		body += " Sabab: " + reason
	}
	data := map[string]any{"order_id": order.ID}
	s.notify(order.CustomerID, "order.cancelled", "Buyurtma bekor qilindi", body, data)
	if order.WorkerID != "" {
		s.notify(order.WorkerID, "order.cancelled", "Buyurtma bekor qilindi", body, data)
	}
	s.adminOrderJSON(w, order.ID)
}

// POST /admin/orders/{id}/complete
func (s *Server) handleAdminCompleteOrder(w http.ResponseWriter, r *http.Request) {
	order, err := s.store.AdminComplete(r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	s.notifyOrderStatus(order)
	s.adminOrderJSON(w, order.ID)
}

// POST /admin/orders/{id}/dispatch — search again.
func (s *Server) handleAdminDispatchOrder(w http.ResponseWriter, r *http.Request) {
	order, err := s.store.AdminRedispatch(r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	order = s.dispatch(order.ID, order)
	s.notifyOrderStatus(order)
	s.adminOrderJSON(w, order.ID)
}

// POST /admin/orders/{id}/assign — body { worker_id } (the worker's user id).
func (s *Server) handleAdminAssignOrder(w http.ResponseWriter, r *http.Request) {
	var in struct {
		WorkerID string `json:"worker_id"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.WorkerID) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "worker_id is required")
		return
	}
	before, _ := s.store.GetOrder(r.PathValue("id"))
	order, _, err := s.store.AdminAssign(r.PathValue("id"), strings.TrimSpace(in.WorkerID))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	s.withdrawOffer(before)
	s.settleBatch(order)
	s.notifyOrderStatus(order)
	data := map[string]any{"order_id": order.ID}
	s.notify(order.WorkerID, "order.assigned", "Sizga buyurtma biriktirildi",
		"Administrator sizga yangi buyurtma biriktirdi. Tafsilotlarni ilovada ko'ring.", data)
	s.notify(order.CustomerID, "order.accepted", "Ishchi topildi",
		"Buyurtmangizga ishchi biriktirildi.", data)
	s.adminOrderJSON(w, order.ID)
}

// withdrawOffer takes an order's card off every phone it was still ringing,
// when the office has settled it some other way.
func (s *Server) withdrawOffer(before *models.Order) {
	if before == nil {
		return
	}
	switch before.Status {
	case models.StatusMatching, models.StatusInvited:
	default:
		return
	}
	taken := *before
	taken.CustomerPhone = ""
	for _, pid := range before.InvitedWorkerIDs {
		if wuid, ok := s.store.WorkerUserID(pid); ok {
			s.publish(wuid, sse.Event{Type: "order.taken", Data: map[string]any{"order": taken}})
		}
	}
}

// POST /admin/push/broadcast — body { title, body, audience, only_online }.
//
// The office app used to do this itself, one request per user; a few hundred
// users is a few hundred requests from a phone that may lock halfway through.
func (s *Server) handleAdminBroadcast(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Title      string `json:"title"`
		Body       string `json:"body"`
		Audience   string `json:"audience"`
		OnlyOnline bool   `json:"only_online"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	in.Title, in.Body = strings.TrimSpace(in.Title), strings.TrimSpace(in.Body)
	if in.Title == "" || in.Body == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "title and body are required")
		return
	}
	switch in.Audience {
	case "workers", "companies", "customers", "all":
	default:
		writeError(w, http.StatusBadRequest, "bad_request", "audience must be workers, companies, customers or all")
		return
	}
	ids := s.store.BroadcastRecipients(in.Audience, in.OnlyOnline)
	// Sent in the background: the answer should not wait on hundreds of pushes.
	go func() {
		for _, id := range ids {
			s.publish(id, sse.Event{Type: "admin.push", Data: map[string]any{"title": in.Title, "body": in.Body}})
			s.notify(id, "admin.push", in.Title, in.Body, nil)
		}
	}()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "recipients": len(ids)})
}
