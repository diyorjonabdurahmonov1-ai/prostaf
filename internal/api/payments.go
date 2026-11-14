package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"prostaff/internal/atmos"
	"prostaff/internal/models"
	"prostaff/internal/sse"
	"prostaff/internal/store"
)

// Money coming in.
//
// Two things buy something here: a worker topping up their wallet, and a firm
// buying months of subscription. Both take the same road — open a pending
// payment, send the payer to Atmos's hosted invoice page, wait for a signed
// callback — and differ only in what settling one does.
//
// # The rule this file exists to enforce
//
// Nothing is credited because the app said so. The app coming back from the
// browser means the person closed a page, which they can do having paid,
// having failed to pay, or having changed their mind; only the callback, with a
// signature this server can check, moves anybody's balance. That is why
// `handleAtmosCallback` is public and unauthenticated but verified, and why
// settlement goes through a single guarded UPDATE that can fire once.

// POST /wallet/topup — body { amount }.
//
// Returns the payment and the URL to send the payer to. In development, with no
// gateway configured, it credits directly instead, which is how the app is
// worked on without a merchant account.
func (s *Server) handleTopup(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	var body struct {
		Amount int64 `json:"amount"`
		// Provider is accepted and ignored: older builds of the app send
		// "click"/"payme". There is one gateway now, and refusing the field
		// would break those installs for no gain.
		Provider string `json:"provider"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Amount <= 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "To'ldirish summasini kiriting.")
		return
	}

	if !s.atmos.Enabled() {
		s.devTopup(w, r, u.ID, body.Amount)
		return
	}
	payment, err := s.openGatewayPayment(r, store.PaymentRequest{
		UserID:   u.ID,
		Provider: "atmos",
		Amount:   body.Amount,
		Purpose:  models.PurposeWallet,
	})
	if err != nil {
		writePaymentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"payment": payment,
		// The app collects the card itself and drives /card and /otp from here.
		// See payments_card.go for why the hosted page is no longer used.
		"card_required": true,
		// Still sent for installs built before the card flow existed. Those
		// builds have no other path, and the field costs nothing to keep.
		"payment_url": payment.PayURL,
	})
}

// GET /company/subscription/plans — the tariffs on offer.
//
// The firm's screen reads these on every open rather than remembering them: the
// prices are the office's to change, and a stale card would mean a firm
// agreeing to one number and being charged another.
func (s *Server) handleSubscriptionPlans(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"plans": s.store.ListSubscriptionPlans(true),
		// "Can a firm buy a month right now", not "is Atmos configured". Those
		// were the same question until the development path below existed; with
		// no gateway and DEV_MODE on, a plan is bought instantly and the flag
		// saying otherwise would grey out a button that works.
		"payments_enabled": s.atmos.Enabled() || s.devAllowed(r),
	})
}

// POST /company/subscription/checkout — body { plan_id } (or { months } for
// older builds of the app).
//
// The price is looked up from the plan, never taken from the request: a client
// that could name its own price could buy a year for a so'm.
func (s *Server) handleCompanySubscriptionCheckout(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	cp, ok := s.store.CompanyByUser(u.ID)
	if !ok {
		writeError(w, http.StatusForbidden, "forbidden", "Sizda firma profili yo'q.")
		return
	}
	var body struct {
		PlanID string `json:"plan_id"`
		// Months keeps an app built before tariffs existed working: it is matched
		// against a plan of that length, never priced on its own.
		Months int `json:"months"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	plan := s.resolvePlan(body.PlanID, body.Months)
	if plan == nil {
		writeError(w, http.StatusBadRequest, "plan_not_found",
			"Tarif topilmadi. Ro'yxatdan tarifni tanlang.")
		return
	}
	amount := plan.Price
	if amount <= 0 {
		writeError(w, http.StatusConflict, "price_not_set",
			"Obuna narxi belgilanmagan. Operatorga murojaat qiling.")
		return
	}
	// No gateway yet: sell the months the way a top-up credits a wallet.
	//
	// This used to be a flat 503, which made a subscription the one thing in the
	// app that could not be exercised at all before Atmos was wired up — a firm
	// out of free jobs was simply stuck, on every environment. A top-up has
	// always had this path; there was no reason the other purchase did not.
	if !s.atmos.Enabled() {
		s.devSubscribe(w, r, u.ID, plan)
		return
	}

	payment, err := s.openGatewayPayment(r, store.PaymentRequest{
		UserID:   u.ID,
		Provider: "atmos",
		Amount:   amount,
		Purpose:  models.PurposeSubscription,
		Months:   plan.Months,
	})
	if err != nil {
		writePaymentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"payment":       payment,
		"card_required": true,
		"payment_url":   payment.PayURL,
		"company":       cp,
	})
}

// resolvePlan finds the tariff a checkout is for.
//
// `months` is the compatibility path: an app built before tariffs existed sends
// a length, and the nearest active plan of exactly that length is what it meant.
// It is never priced arithmetically — a term nobody put on a card is a term
// nobody set a price for.
func (s *Server) resolvePlan(planID string, months int) *models.SubscriptionPlan {
	if planID != "" {
		if plan, ok := s.store.GetSubscriptionPlan(planID); ok && plan.Active {
			return plan
		}
		return nil
	}
	if months <= 0 {
		return nil
	}
	for _, plan := range s.store.ListSubscriptionPlans(true) {
		if plan.Months == months {
			return &plan
		}
	}
	return nil
}

// openGatewayPayment writes the pending row, then opens the transaction.
//
// That order is deliberate. If the gateway call fails the worst case is a
// pending row nobody ever pays, which costs nothing; the other order can lose
// money that has actually moved, because a callback for a transaction this
// server never wrote down has nowhere to land.
func (s *Server) openGatewayPayment(r *http.Request, req store.PaymentRequest) (*models.Payment, error) {
	payment, err := s.store.OpenPayment(req)
	if err != nil {
		return nil, err
	}
	tx, err := s.atmos.CreateTransaction(r.Context(), payment.Amount, payment.ID)
	if err != nil {
		// Recorded on the row rather than deleted, and rather than left pending:
		// a transaction that never opened is a thing the office has to be able
		// to see when the person it happened to rings up about it.
		log.Printf("atmos: create transaction for payment %s: %v", payment.ID, err)
		s.store.FailPayment(payment.ID, "To'lov tizimida tranzaksiya ochilmadi: "+err.Error())
		return nil, errGatewayUnavailable
	}
	if err := s.store.AttachExternalID(payment.ID, tx.ID); err != nil {
		log.Printf("atmos: attach transaction %s to payment %s: %v", tx.ID, payment.ID, err)
		s.store.FailPayment(payment.ID, "Tranzaksiya raqami saqlanmadi: "+err.Error())
		return nil, errGatewayUnavailable
	}
	payment.ExternalID = tx.ID
	payment.PayURL = tx.PayURL
	return payment, nil
}

// devTopup is the no-gateway path: credit it and move on.
func (s *Server) devTopup(w http.ResponseWriter, r *http.Request, userID string, amount int64) {
	if !s.devAllowed(r) {
		writeError(w, http.StatusServiceUnavailable, "payments_unavailable",
			"To'lov tizimi sozlanmagan. Operatorga murojaat qiling.")
		return
	}
	payment, wallet, err := s.store.TopupDev(userID, amount)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "payment": payment, "wallet": wallet})
}

// devSubscribe sells a firm its months with no gateway in the loop — devTopup
// for the other kind of purchase.
//
// It deliberately goes the long way round: open a pending payment, settle it,
// then hand it to `applyPayment`. That is the identical sequence a real Atmos
// callback runs, so the months, the finance totals, the `subscription.changed`
// event and the "To'lov qabul qilindi" notification all come out of one code
// path. Crediting the firm inline instead would work today and quietly drift
// from the real thing the moment either side is touched.
func (s *Server) devSubscribe(w http.ResponseWriter, r *http.Request, userID string, plan *models.SubscriptionPlan) {
	if !s.devAllowed(r) {
		writeError(w, http.StatusServiceUnavailable, "payments_unavailable",
			"To'lov tizimi sozlanmagan. Operatorga murojaat qiling.")
		return
	}
	payment, err := s.store.OpenPayment(store.PaymentRequest{
		UserID:   userID,
		Provider: "dev",
		Amount:   plan.Price,
		Purpose:  models.PurposeSubscription,
		Months:   plan.Months,
	})
	if err != nil {
		writePaymentError(w, err)
		return
	}
	paid, ok := s.store.SettlePaymentByID(payment.ID)
	if !ok {
		writeError(w, http.StatusConflict, "payment_failed",
			"To'lovni yakunlab bo'lmadi. Qayta urinib ko'ring.")
		return
	}
	s.applyPayment(paid)

	// Re-read: applyPayment is what moved the date, and the firm's screen redraws
	// from whatever comes back here.
	cp, _ := s.store.CompanyByUser(userID)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"payment": paid,
		"company": cp,
	})
}

// GET /payments/{id} — "did my money land?"
//
// The app asks this after the invoice page closes, because the browser coming
// back is not evidence of anything. Scoped to the caller's own payments: a
// payment id is not a secret worth relying on.
func (s *Server) handleGetPayment(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	p, ok := s.store.GetPayment(r.PathValue("id"))
	if !ok || (p.UserID != u.ID && !u.HasRole(models.RoleAdmin)) {
		writeError(w, http.StatusNotFound, "not_found", "payment not found")
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// GET /payments — the caller's own payment history.
func (s *Server) handleListPayments(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	writeJSON(w, http.StatusOK, s.store.ListUserPayments(u.ID, 50))
}

// POST /payments/atmos/callback — the only thing that moves money.
//
// Public by necessity: Atmos has no credentials of ours to present. What makes
// it safe is the signature, which is checked before anything is looked up, let
// alone credited. An unsigned or mis-signed callback is refused outright.
//
// Atmos retries a callback it did not get a clean answer to, so this has to be
// safe to receive twice: settlement is a single guarded UPDATE and a repeat
// finds nothing pending, at which point the honest answer is still "OK" — the
// payment *is* settled, which is what the gateway is asking about.
func (s *Server) handleAtmosCallback(w http.ResponseWriter, r *http.Request) {
	// Logged on every callback, not only rejected ones, because it is the answer
	// to the question ATMOS_CALLBACK_IPS asks: you cannot set an allowlist
	// correctly until you have seen what address callbacks actually arrive from
	// on this deployment — which depends on what sits in front of the process.
	src := callbackSourceIP(r)
	if !s.callbackAllowed(src) {
		log.Printf("atmos: refused callback from %q (not in ATMOS_CALLBACK_IPS)", src)
		writeJSON(w, http.StatusOK, map[string]any{"status": 0, "message": "Forbidden"})
		return
	}
	cb, ok := readAtmosCallback(r)
	if !ok {
		log.Printf("atmos: unreadable callback from %q", src)
		writeJSON(w, http.StatusOK, map[string]any{"status": 0, "message": "Bad request"})
		return
	}
	log.Printf("atmos: callback from %q for transaction %q account %q amount %q",
		src, cb.TransactionID, cb.Ref(), cb.Amount)
	if !s.atmos.VerifyCallback(cb) {
		log.Printf("atmos: rejected callback for transaction %q: bad signature", cb.TransactionID)
		// Written onto the payment so the office can see why somebody's money
		// never arrived. Nothing but a fixed sentence is stored, and the status
		// does not move — an unverified caller must not be able to change what a
		// payment *is*, only to leave a mark that somebody tried.
		s.store.NotePaymentError(cb.TransactionID,
			"To'lov tasdig'i qabul qilinmadi: imzo mos kelmadi.")
		// Deliberately terse to the caller: an attacker probing this endpoint
		// learns nothing about which of store id, signature or transaction was
		// wrong.
		writeJSON(w, http.StatusOK, map[string]any{"status": 0, "message": "Invalid signature"})
		return
	}

	payment, settled := s.settleForCallback(cb)
	if !settled {
		// Nothing moved, and which of the three reasons it was decides what
		// Atmos is told — because this answer is not a receipt, it is an
		// instruction. Atmos debits the card *after* reading `status: 1` here,
		// so saying 1 about a payment this server cannot credit is asking for
		// somebody's money on behalf of nobody.
		existing, known := s.paymentForCallback(cb)
		switch {
		case known && existing.Status == models.PaymentSuccess:
			// A retry of one already handled. Atmos repeats a callback it did
			// not get a clean answer to, and the honest answer is still yes:
			// the payment is settled, which is what it is asking about.
			writeJSON(w, http.StatusOK, map[string]any{"status": 1, "message": "Muvaffaqiyatli"})
		case known:
			// Known, but dead — failed to open, or canceled. Confirming it would
			// charge a card for a purchase this server has already written off.
			log.Printf("atmos: refusing callback for %s payment %s (transaction %q)",
				existing.Status, existing.ID, cb.TransactionID)
			writeJSON(w, http.StatusOK, map[string]any{
				"status": 0, "message": "To'lov bekor qilingan yoki yakunlanmagan."})
		default:
			// A transaction this server never opened. The documented answer is a
			// refusal, and it is the right one twice over: nothing here could be
			// credited, so a yes would take money that lands nowhere.
			log.Printf("atmos: callback for unknown transaction %q (account %q)",
				cb.TransactionID, cb.Ref())
			writeJSON(w, http.StatusOK, map[string]any{
				"status": 0, "message": "Invoys tizimda mavjud emas."})
		}
		return
	}

	// The amount is checked after settling rather than before, so a mismatch
	// cannot be used to keep a payment permanently pending by replaying it. A
	// figure that does not match what was asked for is applied at the amount
	// this server opened the payment for — never at the caller's number — and
	// logged loudly, because it means the two systems disagree about money.
	if som, ok := cb.AmountSom(); !ok || som != payment.Amount {
		log.Printf("atmos: WARNING amount mismatch on transaction %s: callback %q, expected %d so'm",
			cb.TransactionID, cb.Amount, payment.Amount)
		// By row id, not transaction id: the callback's transaction id is not
		// one this server stored, so a note keyed on it would go nowhere.
		s.store.NotePaymentErrorByID(payment.ID, fmt.Sprintf(
			"Summa mos kelmadi: to'lov tizimi %q tiyin dedi, kutilgani %d so'm. Hisobga kutilgan summa yozildi.",
			cb.Amount, payment.Amount))
	}

	s.applyPayment(payment)
	writeJSON(w, http.StatusOK, map[string]any{"status": 1, "message": "Muvaffaqiyatli"})
}

// Which payment a callback is about, and why the transaction id is not enough.
//
// A callback does not arrive under the id this server opened the transaction
// with. /merchant/pay/create answers with transaction_id 2036254324, and the
// callback for that very payment carries transaction_id 146535697 — an id from
// Atmos's billing side that the merchant API never shows and nothing here could
// have stored.
//
// Looking up by that number alone finds nothing, and the cost of finding
// nothing is not a missed receipt. The callback is a pre-debit check: Atmos
// debits only after reading `status: 1`, so a "transaction unknown" refusal
// makes it abandon the payment, and the payer — who typed a correct SMS code —
// is shown STPIMS-ERR-093, "error in the supplier's billing system". Every 093
// in the log has one of these refusals logged a second or two before it.
//
// So the `account` field is the fallback, which is the whole reason this
// backend sends its own payment row id in it. It is safe to trust: the
// signature Atmos signs covers account alongside store id, transaction id and
// amount, and it is verified before either of these runs.
func (s *Server) paymentForCallback(cb atmos.Callback) (*models.Payment, bool) {
	if p, ok := s.store.GetPaymentByExternalID(cb.TransactionID); ok {
		return p, true
	}
	return s.store.GetPayment(cb.Ref())
}

// settleForCallback is paymentForCallback plus the settle, kept as one step so
// the guard that makes a repeated callback harmless stays inside a single
// statement — see Store.SettlePayment.
func (s *Server) settleForCallback(cb atmos.Callback) (*models.Payment, bool) {
	if p, ok := s.store.SettlePayment(cb.TransactionID); ok {
		return p, true
	}
	return s.store.SettlePaymentByID(cb.Ref())
}

// readAtmosCallback parses a callback sent either as JSON or as an HTML form.
//
// Which of the two a merchant account uses is a per-account setting, and an
// integration that only understood one would fail in the most expensive way
// possible: the money moves, the callback is rejected as malformed, and nobody
// is credited. Accepting both costs a content-type check.
func readAtmosCallback(r *http.Request) (atmos.Callback, bool) {
	var cb atmos.Callback
	if strings.Contains(r.Header.Get("Content-Type"), "form-urlencoded") {
		if err := r.ParseForm(); err != nil {
			return cb, false
		}
		cb = atmos.Callback{
			StoreID:       r.PostFormValue("store_id"),
			TransactionID: r.PostFormValue("transaction_id"),
			Account:       r.PostFormValue("account"),
			Invoice:       r.PostFormValue("invoice"),
			Amount:        r.PostFormValue("amount"),
			Sign:          r.PostFormValue("sign"),
		}
		return cb, cb.TransactionID != ""
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&cb); err != nil {
		return cb, false
	}
	return cb, cb.TransactionID != ""
}

// applyPayment gives the payer what they bought, and tells them so.
func (s *Server) applyPayment(p *models.Payment) {
	switch p.Purpose {
	case models.PurposeSubscription:
		// AlreadyPaid: the payment row above *is* the money. Booking another
		// would count these months twice in the finance overview.
		sub, cp, err := s.store.ExtendCompanySubscription(p.UserID, store.SubscriptionExtension{
			Months:      p.Months,
			AlreadyPaid: true,
		})
		if err != nil {
			// The money is real and already recorded; the months are not. Loud in
			// the log *and* on the row, because it needs a person: the office can
			// extend by hand from the admin panel, and this is the only place
			// that says which firm is owed what.
			log.Printf("atmos: PAID BUT NOT APPLIED — payment %s (%d months, %d so'm) for user %s: %v",
				p.ID, p.Months, p.Amount, p.UserID, err)
			s.store.NotePaymentErrorByID(p.ID, fmt.Sprintf(
				"To'lov qabul qilindi, lekin obuna uzaytirilmadi (%d oy): %v. Qo'lda uzaytiring.",
				p.Months, err))
			return
		}
		until := ""
		if cp != nil && cp.SubscriptionUntil != nil {
			until = cp.SubscriptionUntil.Format("02.01.2006")
		}
		if cp != nil {
			s.publish(cp.UserID, sse.Event{
				Type: "subscription.changed",
				Data: map[string]any{"company": cp},
			})
		}
		s.notify(p.UserID, "subscription.extended",
			"To'lov qabul qilindi",
			fmt.Sprintf("Obunangiz %d oyga uzaytirildi. Amal qilish muddati: %s.", sub.Months, until),
			map[string]any{"months": sub.Months, "until": until, "amount": p.Amount})

	default:
		wallet := s.store.CreditWallet(p.UserID, p.Amount)
		s.publish(p.UserID, sse.Event{
			Type: "wallet.changed",
			Data: map[string]any{"wallet": wallet},
		})
		s.notify(p.UserID, "wallet.topup",
			"Hisobingiz to'ldirildi",
			fmt.Sprintf("%s so'm hisobingizga qo'shildi.", store.FormatSom(p.Amount)),
			map[string]any{"amount": p.Amount, "payment_id": p.ID})
	}
}
