package api

import "net/http"

// GET /wallet
func (s *Server) handleGetWallet(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	wallet, ok := s.store.GetWallet(u.ID)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "wallet not found")
		return
	}
	writeJSON(w, http.StatusOK, wallet)
}

// Topping up lives in payments.go now — it opens a real transaction at the
// gateway rather than inventing a checkout URL.
