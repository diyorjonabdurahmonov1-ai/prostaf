package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"prostaff/internal/store"
)

// writeJSON serialises v as JSON with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

// writeError writes the contract error envelope { error: { code, message } }.
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"code": code, "message": message},
	})
}

// writeStoreError maps a store sentinel error to the proper HTTP status.
//
// The messages are shown to the person who tapped the button — the app prints
// `error.message` straight onto the screen — so they are in the user's language
// and say what to do next. A refusal that reads "request conflicts with current
// state" is indistinguishable, from the other side of the glass, from a button
// that simply does not work.
func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrCompanyBusy):
		writeError(w, http.StatusConflict, "company_busy",
			"Bu firma hozir boshqa buyurtma ustida ishlamoqda. Ish tugagach qayta urinib ko'ring.")
	case errors.Is(err, store.ErrCompanyBanned):
		writeError(w, http.StatusForbidden, "company_banned",
			"Bu firma vaqtincha buyurtma qabul qilmaydi.")
	case errors.Is(err, store.ErrOwnCompany):
		writeError(w, http.StatusForbidden, "own_company",
			"O'z firmangizga buyurtma bera olmaysiz.")
	case errors.Is(err, store.ErrOrderNotSaved):
		writeError(w, http.StatusInternalServerError, "order_not_saved",
			"Buyurtma saqlanmadi. Qayta urinib ko'ring.")
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", "request conflicts with current state")
	case errors.Is(err, store.ErrForbidden):
		writeError(w, http.StatusForbidden, "forbidden", "not allowed")
	case errors.Is(err, store.ErrBadRequest):
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request")
	case errors.Is(err, store.ErrInsufficientBalance):
		writeError(w, http.StatusPaymentRequired, "insufficient_balance", "top up your wallet or request a credit limit")
	default:
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

// decodeJSON decodes the request body into dst. On failure it writes a 400 and
// returns false. An empty body is treated as an empty object.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.Body == nil {
		return true
	}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		if err.Error() == "EOF" {
			return true
		}
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return false
	}
	return true
}
