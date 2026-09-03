// MPS is Atmos's Visa and Mastercard acquiring, and it is a different gateway
// from the one in atmos.go in every way that matters.
//
// # Why it is a separate file and not a few more methods
//
//   - Different base path (/mps), different key (an `apikey` header alongside
//     the bearer, issued separately from the consumer credentials), and a
//     different store id for accounts that were given one.
//   - Different envelope. The merchant API answers
//     {"result":{"code":"OK",…}}; MPS answers {"payload":{…},"status":{"code":0,
//     "message":"Success"}}. Nothing that reads one can read the other.
//   - Different confirmation. A local card is confirmed by an SMS code the payer
//     types. A Visa card is confirmed by 3DS — the payer is sent to their own
//     bank's page, comes back, and only then is the transaction applied. There
//     is no code to ask for and asking for one would strand them.
//
// # The three calls, in order
//
//	pre-create  opens a draft for an amount            -> transaction id
//	create      hands over the card and the CVC2       -> 3DS redirect_uri
//	apply       confirms after 3DS; moves the money    -> final status
//
// # What is kept
//
// The same as in atmos.go: nothing. The PAN and the CVC2 are parameters on the
// way to Atmos and are never fields on anything this server holds, never
// logged, and never written down. `post` in atmos.go is still the only place a
// request body is built, so it is still the only place that could leak one.
package atmos

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// MPSEnabled reports whether Visa and Mastercard can be taken.
//
// Separate from Enabled(): an account can perfectly well have the merchant API
// and not MPS, which is the common case — MPS is applied for on its own. With
// this false the local cards keep working exactly as before and an
// international card is refused with a sentence saying so, which beats sending
// it to an endpoint that will answer with a trace id.
func (c *Client) MPSEnabled() bool {
	return c.Enabled() && c.cfg.MPSAPIKey != ""
}

// mpsStoreID is the shop MPS knows this merchant as. Accounts issued a separate
// MPS store id set one; the rest reuse the merchant store, which is what Atmos
// configures by default.
func (c *Client) mpsStoreID() any {
	if s := strings.TrimSpace(c.cfg.MPSStoreID); s != "" {
		return numeric(s)
	}
	return c.storeID()
}

// mpsEnvelope is the status wrapper every /mps response carries.
//
// Note that success is code 0 and not "OK": reusing apiResult here would read
// every successful MPS call as a refusal, and — worse — every refusal as a
// success, since a missing string field and a zero int are indistinguishable
// after unmarshalling.
type mpsEnvelope struct {
	Status struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		TraceID string `json:"trace_id"`
	} `json:"status"`
}

func (e mpsEnvelope) ok() bool { return e.Status.Code == 0 }

// err turns a refusal into the same APIError the merchant API produces, so the
// handler has one thing to check and the payer gets Atmos's own wording either
// way. The trace id rides along in the code because it is the only handle
// support has on a specific failed call.
func (e mpsEnvelope) err(op string) *APIError {
	code := fmt.Sprintf("%d", e.Status.Code)
	if e.Status.TraceID != "" {
		code += " trace=" + e.Status.TraceID
	}
	return &APIError{Op: op, Code: code, Description: e.Status.Message}
}

// MPSTransaction is a Visa or Mastercard payment that has been authorised but
// not yet applied.
type MPSTransaction struct {
	// ID is the MPS transaction id, and the id `MPSApply` must be given. It is
	// not the merchant API's transaction id and the two never mix.
	ID string
	// RedirectURI is the payer's own bank's 3DS page. When it is set the payer
	// has to go there and come back before the transaction can be applied;
	// applying early fails, because the bank has not authenticated anybody yet.
	RedirectURI string
	// MaskedPAN and CardType are what came back about the card — shown to the
	// payer so they can see which card they are about to be charged on.
	MaskedPAN string
	CardType  string
}

type mpsCreateResponse struct {
	mpsEnvelope
	Payload struct {
		ID          json.Number `json:"id"`
		Status      int         `json:"status"`
		RedirectURI string      `json:"redirect_uri"`
		MaskedPAN   string      `json:"masked_pan"`
		ResultCode  string      `json:"result_code"`
		StatusPS    string      `json:"status_ps"`
		Card        *struct {
			CardType string `json:"card_type"`
		} `json:"card"`
	} `json:"payload"`
}

// MPSPreCreate opens a draft transaction for an amount, and is the MPS
// equivalent of CreateTransaction.
//
// `account` is this backend's own reference — the payment row id, as elsewhere
// — and `extID` is a unique id for the request itself, which Atmos uses to
// recognise a retry rather than opening a second transaction for it. The
// payment row id serves for both: it is unique per payment and it is the thing
// a retry of the same payment should collapse onto.
func (c *Client) MPSPreCreate(ctx context.Context, amountSom int64, account string) (string, error) {
	if !c.MPSEnabled() {
		return "", fmt.Errorf("atmos: mps not configured")
	}
	if amountSom <= 0 {
		return "", fmt.Errorf("atmos: amount must be positive")
	}
	raw, err := c.postMPS(ctx, "/mps/pay/transaction/pre-create", map[string]any{
		// Tiyin, as everywhere Atmos counts.
		"amount":   amountSom * 100,
		"ext_id":   account,
		"store_id": c.mpsStoreID(),
		"account":  account,
		// Required by the spec and meaningfully empty: this backend issues no
		// invoice numbers of its own, and fiscalisation is the account's
		// setting rather than something decided per payment.
		"invoice_id": account,
		"ofd_items":  []any{},
	})
	if err != nil {
		return "", err
	}
	var res mpsCreateResponse
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("atmos: mps pre-create: unexpected response: %s", snippet(raw))
	}
	if !res.ok() {
		return "", res.err("mps pre-create")
	}
	id := res.Payload.ID.String()
	if id == "" || id == "0" {
		return "", fmt.Errorf("atmos: mps pre-create: no transaction id")
	}
	return id, nil
}

// MPSCreate hands the card over and authorises the amount.
//
// Nothing is captured here. What comes back is a 3DS page the payer has to
// clear at their own bank; the money moves in MPSApply, after they have.
//
// `clientIP` is the payer's address and is mandatory — MPS scores it, and a
// blank or private one raises the odds of a refusal that has nothing to do with
// the card. `cardName` is a label rather than the cardholder's name; the real
// name comes back masked in the response.
func (c *Client) MPSCreate(ctx context.Context, transactionID, pan, expiryMMYY, cvc2, cardName, clientIP, extID string) (*MPSTransaction, error) {
	if !c.MPSEnabled() {
		return nil, fmt.Errorf("atmos: mps not configured")
	}
	if transactionID == "" || pan == "" || expiryMMYY == "" || cvc2 == "" {
		return nil, fmt.Errorf("atmos: mps create: transaction, card, expiry and cvc2 are all required")
	}
	if cardName == "" {
		cardName = "CARD"
	}
	raw, err := c.postMPS(ctx, "/mps/pay/transaction/create", map[string]any{
		"transaction_id": numeric(transactionID),
		"pan":            pan,
		"expiry":         expiryMMYY,
		// Sent as a string for the same reason the SMS code is: "075" as a JSON
		// number is 75, which is a different security code and one the bank
		// will refuse. Leading zeros are common and the failure looks exactly
		// like a mistyped card.
		"cvc2":           cvc2,
		"card_name":      cardName,
		"client_ip_addr": clientIP,
		"ext_id":         extID,
	})
	if err != nil {
		return nil, err
	}
	var res mpsCreateResponse
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("atmos: mps create: unexpected response: %s", snippet(raw))
	}
	if !res.ok() {
		return nil, res.err("mps create")
	}
	// A create that came back Success but Declined is a refusal by the card's
	// own bank rather than by Atmos, and the envelope does not carry it — only
	// result_code does. Left unchecked it would read as an authorised payment
	// that merely has no 3DS page, which is indistinguishable from a card
	// exempt from 3DS.
	if rc := strings.ToUpper(strings.TrimSpace(res.Payload.ResultCode)); rc != "" &&
		rc != "APPROVED" && rc != "PENDING" && rc != "CREATED" {
		return nil, &APIError{
			Op:          "mps create",
			Code:        res.Payload.ResultCode,
			Description: declineMessage(res.Payload.ResultCode, res.Payload.StatusPS),
		}
	}
	out := &MPSTransaction{
		ID:          res.Payload.ID.String(),
		RedirectURI: res.Payload.RedirectURI,
		MaskedPAN:   res.Payload.MaskedPAN,
	}
	if out.ID == "" || out.ID == "0" {
		out.ID = transactionID
	}
	if res.Payload.Card != nil {
		out.CardType = res.Payload.Card.CardType
	}
	return out, nil
}

type mpsApplyResponse struct {
	mpsEnvelope
	Payload struct {
		ID         json.Number `json:"id"`
		Status     int         `json:"status"`
		StatusPS   string      `json:"status_ps"`
		Status3DS  string      `json:"status_3ds"`
		ResultCode string      `json:"result_code"`
		Amount     json.Number `json:"amount"`
		MaskedPAN  string      `json:"masked_pan"`
	} `json:"payload"`
}

// MPSApply confirms an authorised transaction and is the call that moves the
// money.
//
// Called only after the payer has cleared 3DS. Like Apply, a transaction that
// comes back with anything other than an approval is an error even when the
// envelope says Success — "no error" and "the money moved" are different
// claims, and crediting a wallet on the weaker one is the mistake both of these
// files exist to avoid.
func (c *Client) MPSApply(ctx context.Context, transactionID string) (*Payment, error) {
	if !c.MPSEnabled() {
		return nil, fmt.Errorf("atmos: mps not configured")
	}
	if transactionID == "" {
		return nil, fmt.Errorf("atmos: mps apply: transaction is required")
	}
	raw, err := c.postMPS(ctx, "/mps/pay/transaction/apply", map[string]any{
		"transaction_id": numeric(transactionID),
	})
	if err != nil {
		return nil, err
	}
	var res mpsApplyResponse
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("atmos: mps apply: unexpected response: %s", snippet(raw))
	}
	if !res.ok() {
		return nil, res.err("mps apply")
	}
	rc := strings.ToUpper(strings.TrimSpace(res.Payload.ResultCode))
	if rc != "APPROVED" && rc != "OK" && rc != "SUCCESS" {
		return nil, &APIError{
			Op:          "mps apply",
			Code:        res.Payload.ResultCode,
			Description: declineMessage(res.Payload.ResultCode, res.Payload.Status3DS),
		}
	}
	amount, _ := res.Payload.Amount.Int64()
	id := res.Payload.ID.String()
	if id == "" || id == "0" {
		id = transactionID
	}
	return &Payment{
		TransactionID: id,
		AmountTiyin:   amount,
		Confirmed:     true,
	}, nil
}

// declineMessage is what a payer can be told about a refusal by their own bank.
//
// MPS returns the scheme's own codes, which are English and terse
// ("DECLINED"), so unlike the merchant API's descriptions they are not
// something to hand a cardholder untranslated. The handful worth naming are
// named; anything else gets a sentence that is honest about not knowing, plus
// the code, which is what support will ask for.
func declineMessage(resultCode, state string) string {
	switch strings.ToUpper(strings.TrimSpace(resultCode)) {
	case "DECLINED":
		return "Bank to'lovni rad etdi. Kartangizni yoki bankingizni tekshiring."
	case "INSUFFICIENT_FUNDS":
		return "Kartada mablag' yetarli emas."
	case "EXPIRED", "EXPIRED_CARD":
		return "Kartaning amal qilish muddati tugagan."
	case "FAILED", "REVERSED":
		return "To'lov amalga oshmadi. Qaytadan urinib ko'ring."
	}
	if strings.EqualFold(strings.TrimSpace(state), "FAILED") ||
		strings.EqualFold(strings.TrimSpace(state), "DECLINED") {
		return "Bank kartani tasdiqlamadi. Qaytadan urinib ko'ring."
	}
	if resultCode != "" {
		return "To'lov amalga oshmadi (" + resultCode + ")."
	}
	return "To'lov amalga oshmadi. Qaytadan urinib ko'ring."
}

// postMPS is post() with the extra header MPS requires.
//
// MPS authenticates twice over: the same bearer token as everything else, plus
// an `apikey` header naming the MPS contract specifically. Sending only the
// bearer gets a 401 that looks exactly like an expired token, so the retry in
// post() would fire, fetch a perfectly good token, and fail again identically —
// which is why the header is set here rather than left to the caller.
func (c *Client) postMPS(ctx context.Context, path string, payload any) ([]byte, error) {
	return c.postWithHeaders(ctx, path, payload, map[string]string{"apikey": c.cfg.MPSAPIKey})
}
