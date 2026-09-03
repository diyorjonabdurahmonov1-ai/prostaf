package api

import (
	"errors"
	"log"
	"net/http"
	"strings"

	"prostaff/internal/atmos"
	"prostaff/internal/models"
)

// Paying with a card the app collected itself, rather than on a page at the
// gateway.
//
// # Why this exists
//
// The documented hosted invoice page lives on a host that stopped answering —
// checkout.pays.uz and test-checkout.pays.uz both refuse TCP from everywhere,
// including from this server. A payer sent there sees a browser error, and no
// amount of care on our side changes that. The API gateway, meanwhile, has
// never missed: it issues tokens and opens transactions exactly as documented.
// So the payer stays in the app and this server drives the same three calls the
// page would have made.
//
// # What that costs, and what is done about it
//
// The card number now passes through this process. It is a parameter and
// nothing else: never stored, never logged, never put on a struct that outlives
// the request. `atmos.Client.post` is the single place a request body is
// built, which is the one place that could ever leak one.
//
// # What has not changed
//
// The amount is still fixed when the payment row is opened, so a client cannot
// name its own price. Settlement is still the single guarded UPDATE in
// `SettlePayment`, so a payment credited here cannot be credited again by the
// callback that follows — Atmos sends one either way, and it is welcome to.

// POST /payments/{id}/card — body { card_number, expiry }.
//
// Hands the card to Atmos, which texts the cardholder a one-time code. Nothing
// is debited by this call.
func (s *Server) handlePaymentCard(w http.ResponseWriter, r *http.Request) {
	payment, ok := s.payableFor(w, r)
	if !ok {
		return
	}
	var body struct {
		CardNumber string `json:"card_number"`
		Expiry     string `json:"expiry"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	card := digitsOnly(body.CardNumber)
	if len(card) < 16 || len(card) > 19 {
		writeError(w, http.StatusBadRequest, "bad_card", "Karta raqamini to'liq kiriting.")
		return
	}
	expiry, valid := atmos.NormalizeExpiry(body.Expiry)
	if !valid {
		writeError(w, http.StatusBadRequest, "bad_expiry",
			"Kartaning amal qilish muddatini OO/YY ko'rinishida kiriting.")
		return
	}

	if err := s.atmos.PreApply(r.Context(), payment.ExternalID, card, expiry); err != nil {
		s.writeAtmosError(w, payment.ID, "pre-apply", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"payment_id": payment.ID,
		// The app shows an SMS-code screen next. Said explicitly rather than
		// implied by a 200, so a build that later gains saved cards — which are
		// confirmed without a code — can tell the two apart.
		"otp_required": true,
	})
}

// POST /payments/{id}/otp — body { otp }.
//
// The call that moves the money. Credits the payer here rather than waiting for
// the callback: Atmos has already confirmed the debit in its reply, and making
// somebody watch a spinner until a webhook lands is a worse experience for no
// extra certainty. The callback still arrives and still settles nothing twice.
func (s *Server) handlePaymentOTP(w http.ResponseWriter, r *http.Request) {
	payment, ok := s.payableFor(w, r)
	if !ok {
		return
	}
	var body struct {
		OTP string `json:"otp"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	otp := digitsOnly(body.OTP)
	if otp == "" {
		writeError(w, http.StatusBadRequest, "bad_otp", "SMS kodni kiriting.")
		return
	}

	paid, err := s.atmos.Apply(r.Context(), payment.ExternalID, otp)
	if err != nil {
		s.writeAtmosError(w, payment.ID, "apply", err)
		return
	}

	settled, ok := s.store.SettlePayment(payment.ExternalID)
	if !ok {
		// The card has been debited and this server cannot record it. That is
		// the one outcome worth shouting about: either the callback beat us here
		// (harmless — it credited the same payment) or the row moved underneath
		// us, and only a person can tell which.
		current, known := s.store.GetPaymentByExternalID(payment.ExternalID)
		if known && current.Status == models.PaymentSuccess {
			writeJSON(w, http.StatusOK, s.paidResponse(current))
			return
		}
		log.Printf("atmos: PAID BUT NOT SETTLED — payment %s (transaction %s, %d tiyin)",
			payment.ID, paid.TransactionID, paid.AmountTiyin)
		s.store.NotePaymentErrorByID(payment.ID,
			"Karta yechildi, lekin to'lov yozib olinmadi. Qo'lda tekshiring.")
		writeError(w, http.StatusConflict, "settle_failed",
			"To'lov o'tdi, lekin hisobga yozishda xatolik bo'ldi. Operatorga murojaat qiling.")
		return
	}

	// Checked after settling for the same reason the callback checks it after
	// settling: a mismatch must not be a way to keep a payment pending. The
	// wallet is credited with what was asked for, never with the gateway's
	// number, and the disagreement is written down.
	if som := paid.AmountTiyin / 100; paid.AmountTiyin%100 != 0 || som != settled.Amount {
		log.Printf("atmos: WARNING amount mismatch on transaction %s: gateway %d tiyin, expected %d so'm",
			paid.TransactionID, paid.AmountTiyin, settled.Amount)
		s.store.NotePaymentError(payment.ExternalID,
			"Summa mos kelmadi. Hisobga kutilgan summa yozildi.")
	}

	s.applyPayment(settled)
	writeJSON(w, http.StatusOK, s.paidResponse(settled))
}

// POST /payments/{id}/resend-otp — the code never arrived.
func (s *Server) handlePaymentResendOTP(w http.ResponseWriter, r *http.Request) {
	payment, ok := s.payableFor(w, r)
	if !ok {
		return
	}
	if err := s.atmos.RetryOTP(r.Context(), payment.ExternalID); err != nil {
		s.writeAtmosError(w, payment.ID, "retry-otp", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// paidResponse is what the app redraws the wallet from once a payment lands.
func (s *Server) paidResponse(p *models.Payment) map[string]any {
	out := map[string]any{"ok": true, "payment": p}
	if wallet, ok := s.store.GetWallet(p.UserID); ok {
		out["wallet"] = wallet
	}
	return out
}

// payableFor loads the payment a card request is about, or answers for itself.
//
// Every check here is the same question asked once: is this caller allowed to
// push this particular payment forward, right now. A settled payment is called
// out by name rather than lumped in with "not found", because an app that
// retried a request whose first attempt succeeded should be told it worked.
func (s *Server) payableFor(w http.ResponseWriter, r *http.Request) (*models.Payment, bool) {
	if !s.atmos.Enabled() {
		writeError(w, http.StatusServiceUnavailable, "payments_unavailable",
			"To'lov tizimi sozlanmagan. Operatorga murojaat qiling.")
		return nil, false
	}
	u := userFrom(r)
	p, found := s.store.GetPayment(r.PathValue("id"))
	if !found || p.UserID != u.ID {
		writeError(w, http.StatusNotFound, "not_found", "To'lov topilmadi.")
		return nil, false
	}
	if p.Status == models.PaymentSuccess {
		writeError(w, http.StatusConflict, "already_paid", "Bu to'lov allaqachon amalga oshirilgan.")
		return nil, false
	}
	if p.Status != models.PaymentPending {
		writeError(w, http.StatusConflict, "payment_closed", "Bu to'lov yopilgan. Yangisini boshlang.")
		return nil, false
	}
	if p.ExternalID == "" {
		// Opened but never registered at the gateway. Nothing to pre-apply
		// against, and retrying this row would only fail again.
		writeError(w, http.StatusConflict, "payment_not_opened",
			"To'lov tizimida ochilmagan. Yangisini boshlang.")
		return nil, false
	}
	return p, true
}

// writeAtmosError separates the gateway saying no from the gateway not
// answering.
//
// A refusal carries a sentence Atmos wrote for the cardholder — "wrong code",
// "insufficient funds" — and that sentence is worth more to the payer than
// anything this server could invent, so it is passed through. A transport
// failure has nothing to tell them and everything to tell the log.
func (s *Server) writeAtmosError(w http.ResponseWriter, paymentID, op string, err error) {
	var refusal *atmos.APIError
	if errors.As(err, &refusal) {
		log.Printf("atmos: %s refused for payment %s: code %s: %s",
			op, paymentID, refusal.Code, refusal.Description)
		// The payment is left pending on purpose: a wrong code or a declined
		// card is something the payer can fix on the next attempt, and failing
		// the row would make them start over for a typo.
		writeError(w, http.StatusBadRequest, "payment_refused", refusal.Message())
		return
	}
	log.Printf("atmos: %s failed for payment %s: %v", op, paymentID, err)
	writeError(w, http.StatusBadGateway, "gateway_unavailable",
		"To'lov tizimiga ulanib bo'lmadi. Bir necha daqiqadan so'ng qayta urinib ko'ring.")
}

// digitsOnly strips the spaces and dashes people type into a card field.
func digitsOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
