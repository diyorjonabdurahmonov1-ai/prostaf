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
// So the payer stays in the app and this server drives the calls the page would
// have made.
//
// # Two gateways, one card field
//
// Atmos settles local and international cards through entirely separate
// modules, and the card number decides which:
//
//	UzCard / Humo   /merchant/pay/pre-apply -> SMS code -> apply
//	Visa / MC       /mps/pay/transaction/*  -> 3DS page -> apply
//
// Routing this wrongly is not a mistake a payer can retry their way out of. The
// merchant API's answer to a Visa number is "Karta topilmadi" — the card is not
// missing, it was never in that processing — which reads as "your card is
// broken" to somebody holding a perfectly good card. So the brand is decided
// before anything is sent (atmos.DetectBrand), and an international card is
// refused with a plain sentence when MPS is not configured for the account.
//
// # What that costs, and what is done about it
//
// The card number, and now the CVC2 for international cards, pass through this
// process. They are parameters and nothing else: never stored, never logged,
// never put on a struct that outlives the request. `atmos.Client.post` is the
// single place a request body is built, which is the one place that could ever
// leak one.
//
// # What has not changed
//
// The amount is still fixed when the payment row is opened, so a client cannot
// name its own price. Settlement is still the single guarded UPDATE in
// `SettlePayment`, so a payment credited here cannot be credited again by the
// callback that follows — Atmos sends one either way, and it is welcome to.

// providerMPS marks a payment as being settled through the Visa/Mastercard
// module rather than the merchant API. Written to the row when the card is
// handed over, and read back at the confirm step — see store.SetPaymentProvider
// for why it cannot be worked out again later.
const providerMPS = "atmos_mps"

// POST /payments/{id}/card — body { card_number, expiry, cvv }.
//
// Hands the card to Atmos. What happens next depends on the card:
//
//   - a local card gets an SMS code texted to the cardholder, and the app moves
//     to the code screen. Nothing is debited by this call.
//   - an international card is authorised and comes back with a 3DS page at the
//     payer's own bank, which the app opens. Nothing is captured until
//     /confirm.
//
// The response says which in `next`, rather than leaving the app to infer it
// from which fields happen to be present.
func (s *Server) handlePaymentCard(w http.ResponseWriter, r *http.Request) {
	payment, ok := s.payableFor(w, r)
	if !ok {
		return
	}
	var body struct {
		CardNumber string `json:"card_number"`
		Expiry     string `json:"expiry"`
		// CVV is the three digits on the back, and only international cards
		// have any use for it. Accepted under either name because "cvv" is what
		// the card says and "cvc2" is what Atmos calls it.
		CVV  string `json:"cvv"`
		CVC2 string `json:"cvc2"`
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

	switch brand := atmos.DetectBrand(card); {
	case brand == atmos.BrandLocal:
		s.payWithLocalCard(w, r, payment, card, expiry)
	case brand.UsesMPS():
		cvv := digitsOnly(body.CVV)
		if cvv == "" {
			cvv = digitsOnly(body.CVC2)
		}
		s.payWithInternationalCard(w, r, payment, card, body.Expiry, cvv, brand)
	default:
		// Neither processing would recognise it, so it is refused here rather
		// than by whichever one it was guessed into. Saying which cards *are*
		// taken turns a dead end into something the payer can act on.
		writeError(w, http.StatusBadRequest, "unsupported_card",
			"Bu karta turini qabul qila olmaymiz. UzCard, Humo, Visa yoki Mastercard kartasini kiriting.")
	}
}

// payWithLocalCard is the UzCard and Humo path: Atmos texts the cardholder a
// code, and /otp spends it.
func (s *Server) payWithLocalCard(w http.ResponseWriter, r *http.Request, payment *models.Payment, card, expiryYYmm string) {
	// A payment previously attempted with an international card points at an
	// MPS transaction, which pre-apply knows nothing about. Send it back to a
	// merchant-API transaction before using one.
	if payment.Provider == providerMPS {
		tx, err := s.atmos.CreateTransaction(r.Context(), payment.Amount, payment.ID)
		if err != nil {
			s.writeAtmosError(w, payment.ID, "create", err)
			return
		}
		if !s.retarget(w, payment, tx.ID, "atmos") {
			return
		}
	}

	if err := s.atmos.PreApply(r.Context(), payment.ExternalID, card, expiryYYmm); err != nil {
		s.writeAtmosError(w, payment.ID, "pre-apply", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"payment_id": payment.ID,
		"brand":      string(atmos.BrandLocal),
		// What the app is to do next, named rather than implied by a 200 — the
		// international path also answers 200 and means something else
		// entirely.
		"next": "otp",
		// Kept for builds shipped before `next` existed, which read this.
		"otp_required": true,
	})
}

// payWithInternationalCard is the Visa and Mastercard path.
//
// It opens a fresh MPS transaction for the payment: the one made when the
// payment row was created belongs to the merchant API, and the two gateways do
// not share transaction ids. The row is re-pointed at the MPS one so the
// callback, and settlement, still find their way home.
func (s *Server) payWithInternationalCard(w http.ResponseWriter, r *http.Request, payment *models.Payment, card, expiryInput, cvv string, brand atmos.Brand) {
	if !s.atmos.MPSEnabled() {
		// Said in full rather than as "card refused". The card is fine and
		// retrying it will not help; the account simply has not been set up for
		// international cards, and the payer needs to know to use a different
		// card rather than to try again on a different day.
		writeError(w, http.StatusBadRequest, "card_not_supported",
			"Hozircha Visa va Mastercard kartalari qabul qilinmaydi. UzCard yoki Humo kartasidan foydalaning.")
		return
	}
	if len(cvv) < 3 || len(cvv) > 4 {
		writeError(w, http.StatusBadRequest, "bad_cvv",
			"Kartaning orqasidagi 3 xonali CVV kodni kiriting.")
		return
	}
	// MPS wants MMYY where the merchant API wants YYmm — the same four digits,
	// reversed, with no way to tell a wrong one from a right one by looking.
	expiry, valid := atmos.ExpiryMMYY(expiryInput)
	if !valid {
		writeError(w, http.StatusBadRequest, "bad_expiry",
			"Kartaning amal qilish muddatini OO/YY ko'rinishida kiriting.")
		return
	}

	txID, err := s.atmos.MPSPreCreate(r.Context(), payment.Amount, payment.ID)
	if err != nil {
		s.writeAtmosError(w, payment.ID, "mps pre-create", err)
		return
	}
	if !s.retarget(w, payment, txID, providerMPS) {
		return
	}

	tx, err := s.atmos.MPSCreate(r.Context(), txID, card, expiry, cvv,
		"CARD", clientIP(r), payment.ID)
	if err != nil {
		s.writeAtmosError(w, payment.ID, "mps create", err)
		return
	}

	// No 3DS page means the card's bank did not ask for one, so there is nothing
	// for the payer to do and the transaction can be applied straight away.
	// Sending them to an empty URL to "confirm" would strand them on a blank
	// page with their money authorised and never captured.
	if strings.TrimSpace(tx.RedirectURI) == "" {
		s.applyMPSAndRespond(w, r, payment)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"payment_id": payment.ID,
		"brand":      string(brand),
		"next":       "3ds",
		// The app opens this, waits for the payer to come back, then calls
		// /confirm. It is the bank's page, not Atmos's.
		"redirect_url": tx.RedirectURI,
		"masked_pan":   tx.MaskedPAN,
	})
}

// POST /payments/{id}/confirm — the payer has finished at their bank's 3DS
// page.
//
// The call that moves the money for an international card, and the counterpart
// of /otp for a local one. It takes no body: everything it needs is on the row,
// and there is deliberately nothing here a client could assert about whether
// authentication succeeded — Atmos is asked, not told.
func (s *Server) handlePaymentConfirm(w http.ResponseWriter, r *http.Request) {
	payment, ok := s.payableFor(w, r)
	if !ok {
		return
	}
	if payment.Provider != providerMPS {
		// A local card is confirmed with the code that was texted, not here.
		writeError(w, http.StatusConflict, "otp_required",
			"Bu to'lov SMS kod bilan tasdiqlanadi.")
		return
	}
	s.applyMPSAndRespond(w, r, payment)
}

// applyMPSAndRespond captures an authorised MPS transaction and credits the
// payer, or explains why it could not.
func (s *Server) applyMPSAndRespond(w http.ResponseWriter, r *http.Request, payment *models.Payment) {
	paid, err := s.atmos.MPSApply(r.Context(), payment.ExternalID)
	if err != nil {
		s.writeAtmosError(w, payment.ID, "mps apply", err)
		return
	}
	s.creditConfirmedPayment(w, payment, paid)
}

// POST /payments/{id}/otp — body { otp }.
//
// The call that moves the money for a local card. Credits the payer here rather
// than waiting for the callback: Atmos has already confirmed the debit in its
// reply, and making somebody watch a spinner until a webhook lands is a worse
// experience for no extra certainty. The callback still arrives and still
// settles nothing twice.
func (s *Server) handlePaymentOTP(w http.ResponseWriter, r *http.Request) {
	payment, ok := s.payableFor(w, r)
	if !ok {
		return
	}
	if payment.Provider == providerMPS {
		// An international card is confirmed at the bank's own 3DS page. There
		// is no code and none was sent, so asking the payer for one would have
		// them hunting through an SMS inbox that will never fill.
		writeError(w, http.StatusConflict, "confirm_required",
			"Bu to'lov bank sahifasida tasdiqlanadi, SMS kod yuborilmaydi.")
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
	s.creditConfirmedPayment(w, payment, paid)
}

// creditConfirmedPayment records a debit Atmos has confirmed, and is the one
// place either card path lands.
//
// Shared rather than duplicated because everything after "the money moved" is
// identical for both gateways, and because the two awkward cases below — a
// settle that finds the row already settled, and an amount that disagrees — are
// exactly the ones that would rot in a copy.
func (s *Server) creditConfirmedPayment(w http.ResponseWriter, payment *models.Payment, paid *atmos.Payment) {
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
	//
	// A zero is not a disagreement — MPS's apply reply carries no amount at all,
	// having stated it at create — so only a figure Atmos actually gave is
	// compared.
	if som := paid.AmountTiyin / 100; paid.AmountTiyin > 0 &&
		(paid.AmountTiyin%100 != 0 || som != settled.Amount) {
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
	if payment.Provider == providerMPS {
		writeError(w, http.StatusConflict, "confirm_required",
			"Bu to'lov bank sahifasida tasdiqlanadi, SMS kod yuborilmaydi.")
		return
	}
	if err := s.atmos.RetryOTP(r.Context(), payment.ExternalID); err != nil {
		s.writeAtmosError(w, payment.ID, "retry-otp", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// retarget points a payment row at a transaction on the other gateway, and
// records which one — keeping the two writes together so a row can never name a
// transaction from one module and a provider from the other.
//
// Both fields are updated in memory as well, because the caller goes on to use
// `payment` for the rest of the request.
func (s *Server) retarget(w http.ResponseWriter, payment *models.Payment, externalID, provider string) bool {
	if err := s.store.AttachExternalID(payment.ID, externalID); err != nil {
		log.Printf("atmos: attach transaction %s to payment %s: %v", externalID, payment.ID, err)
		writeError(w, http.StatusBadGateway, "gateway_unavailable",
			"To'lovni ochishda xatolik bo'ldi. Qaytadan urinib ko'ring.")
		return false
	}
	if err := s.store.SetPaymentProvider(payment.ID, provider); err != nil {
		// The row now names a transaction whose gateway it cannot remember,
		// which is the one state the confirm step cannot recover from. Failed
		// deliberately rather than left to be guessed at: nothing has been
		// charged yet, so starting again costs the payer a retry and no money.
		log.Printf("atmos: mark payment %s as %s: %v", payment.ID, provider, err)
		s.store.FailPayment(payment.ID, "To'lov turi saqlanmadi: "+err.Error())
		writeError(w, http.StatusBadGateway, "gateway_unavailable",
			"To'lovni ochishda xatolik bo'ldi. Yangisini boshlang.")
		return false
	}
	payment.ExternalID = externalID
	payment.Provider = provider
	return true
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

// clientIP is the payer's own address, which MPS requires and scores.
//
// Reuses the callback's reader — the last X-Forwarded-For entry, being the one
// the proxy itself appended — for the same reason: the first entry is whatever
// the caller chose to write there. An unknown address is sent as a plain zero
// rather than as a plausible public one; inventing an address to improve the
// odds would be lying to a fraud check.
func clientIP(r *http.Request) string {
	if ip := callbackSourceIP(r); ip != "" {
		return ip
	}
	return "0.0.0.0"
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
