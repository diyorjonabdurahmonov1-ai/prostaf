package api

import (
	"fmt"
	"net/http"

	"prostaff/internal/models"
	"prostaff/internal/sse"
)

// GET /app/version — the oldest app version still allowed, and where to get a
// newer one. Public: an app too old to work must still be able to ask.
func (s *Server) handleAppVersion(w http.ResponseWriter, _ *http.Request) {
	st := s.store.GetSettings()
	writeJSON(w, http.StatusOK, map[string]any{
		"min_version": st.MinAppVersion,
		"android_url": st.AndroidStoreURL,
		"ios_url":     st.IOSStoreURL,
	})
}

// notifyRefund tells a worker that a cancellation gave back what they paid to
// take the job. Without it the money reappears in the wallet unexplained.
func (s *Server) notifyRefund(order *models.Order) {
	if order == nil || order.WorkerID == "" {
		return
	}
	data := map[string]any{"order_id": order.ID}
	switch {
	case order.RefundedCommission > 0:
		s.notify(order.WorkerID, "wallet.refund", "Komissiya qaytarildi",
			fmt.Sprintf("Buyurtma bekor qilingani uchun %d so'm hamyoningizga qaytarildi.", order.RefundedCommission), data)
	case order.TrialRestored:
		s.notify(order.WorkerID, "wallet.refund", "Bepul ish qaytarildi",
			"Buyurtma bekor qilingani uchun sarflangan bepul ishingiz qaytarildi.", data)
	}
}

// POST /worker/orders/{id}/arrived — "I am at the customer's".
func (s *Server) handleWorkerArrived(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	before, _ := s.store.GetOrder(r.PathValue("id"))
	order, err := s.store.MarkArrived(r.PathValue("id"), u.ID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	s.store.WithBatch(order)
	s.notifyOrderStatus(order)
	if before != nil && before.ArrivedAt == nil {
		s.notify(order.CustomerID, "order.arrived", "Ishchi yetib keldi",
			"Ishchi manzilingizga yetib kelganini bildirdi.", map[string]any{"order_id": order.ID})
	}
	writeJSON(w, http.StatusOK, maskForWorker(*order, u.ID))
}

// POST /orders/{id}/no-show — the customer reports that the worker who took
// the job never came; the job goes back out to everybody else.
func (s *Server) handleNoShow(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	order, worker, err := s.store.ReportNoShow(r.PathValue("id"), u.ID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	// The worker's app drops the job, and they are told why.
	gone := *order
	gone.Status = models.StatusCancelled
	gone.CustomerPhone = ""
	gone.WorkerID = worker
	s.publish(worker, sse.Event{Type: "order.status", Data: map[string]any{"order": gone}})
	s.notify(worker, "order.no_show", "Buyurtma sizdan olindi",
		"Mijoz belgilangan vaqtda yetib kelmaganingizni bildirdi. Buyurtma boshqa ishchilarga yuborildi, komissiya qaytarilmaydi.",
		map[string]any{"order_id": order.ID})
	order = s.dispatch(order.ID, order)
	s.store.WithBatch(order)
	s.notifyOrderStatus(order)
	writeJSON(w, http.StatusOK, order)
}
