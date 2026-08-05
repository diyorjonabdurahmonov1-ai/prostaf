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

// POST /wallet/topup
func (s *Server) handleTopup(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	var body struct {
		Provider string `json:"provider"`
		Amount   int64  `json:"amount"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	// In dev mode the top-up is credited instantly.
	payment, wallet, err := s.store.Topup(u.ID, body.Provider, body.Amount, s.cfg.DevMode)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	resp := map[string]any{"ok": true, "payment": payment, "wallet": wallet}
	if !s.cfg.DevMode {
		// A real integration would return the provider checkout URL here.
		resp["payment_url"] = "https://pay." + body.Provider + ".invalid/checkout/" + payment.ID
	}
	writeJSON(w, http.StatusOK, resp)
}
