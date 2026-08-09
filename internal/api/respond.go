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
	case errors.Is(err, store.ErrCompanyUnavailable):
		writeError(w, http.StatusConflict, "company_unavailable",
			"Bu firma hozircha buyurtma qabul qilmayapti. Boshqa firmani tanlang.")
	case errors.Is(err, store.ErrPriceNotSet):
		writeError(w, http.StatusConflict, "price_not_set",
			"Bu muddat uchun tarif yo'q. Tarif qo'shing yoki narxni qo'lda kiriting.")
	case errors.Is(err, store.ErrSubscriptionExpired):
		writeError(w, http.StatusPaymentRequired, "subscription_expired",
			"Obuna muddati tugagan. Buyurtma qabul qilish uchun obunani yangilang.")
	case errors.Is(err, store.ErrInvalidPassport):
		writeError(w, http.StatusBadRequest, "invalid_passport",
			"Passport seriyasi noto'g'ri. Namuna: AA1234567.")
	case errors.Is(err, store.ErrDuplicatePassport):
		writeError(w, http.StatusConflict, "duplicate_passport",
			"Bu passport seriyasi allaqachon ro'yxatdan o'tgan.")
	case errors.Is(err, store.ErrInvalidINN):
		writeError(w, http.StatusBadRequest, "invalid_inn",
			"INN noto'g'ri. INN 9 ta raqamdan iborat bo'lishi kerak.")
	case errors.Is(err, store.ErrDuplicateINN):
		writeError(w, http.StatusConflict, "duplicate_inn",
			"Bu INN allaqachon ro'yxatdan o'tgan.")
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
		// The exact figure lives on the alert banner, which knows the setting;
		// this only has to say which of the two things went wrong.
		writeError(w, http.StatusPaymentRequired, "insufficient_balance",
			"Hisobingizda yetarli mablag' yo'q. Bepul ishlar tugagan — buyurtma olish uchun hisobingizni to'ldiring.")
	default:
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

// errGatewayUnavailable is the payment gateway refusing or failing to answer.
// Separate from a store error because nothing is wrong with the request — the
// only useful thing to say is "try again".
var errGatewayUnavailable = errors.New("gateway_unavailable")

// writePaymentError maps a checkout failure to something the payer can act on.
func writePaymentError(w http.ResponseWriter, err error) {
	if errors.Is(err, errGatewayUnavailable) {
		writeError(w, http.StatusBadGateway, "gateway_unavailable",
			"To'lov tizimiga ulanib bo'lmadi. Bir necha daqiqadan so'ng qayta urinib ko'ring.")
		return
	}
	writeStoreError(w, err)
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
