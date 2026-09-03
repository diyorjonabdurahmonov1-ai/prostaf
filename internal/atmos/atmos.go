// Package atmos talks to the Atmos merchant API — the one place in this
// backend that knows anything about how money actually arrives.
//
// The flow is the hosted one, deliberately: the server opens a transaction, the
// app sends the person to Atmos's own invoice page, and Atmos calls back when
// the money has moved. No card number ever touches this application, which is
// both the safer arrangement and the one that needs no PCI conversation.
//
// # What is settled here and what is not
//
// A payment is *never* credited on the strength of the app coming back from the
// browser — a person can close the page, come back, and swear they paid. The
// only thing that credits a wallet is the signed callback in
// api/payments.go, checked against the merchant key and applied exactly once.
// Everything in this file is the outbound half: get a token, open a
// transaction, build the URL to send somebody to.
//
// # Checked against docs.atmos.uz
//
// This is the flow the documentation calls "To'lov sahifasi", which it
// recommends for mobile apps: call /merchant/pay/create for a draft
// transaction, then send the payer to the hosted page with storeId,
// transactionId and an optional redirectLink. These match the published spec:
//
//   - POST {base}/token, Basic consumer-key:consumer-secret, client_credentials
//   - POST {base}/merchant/pay/create with amount in tiyin, account, store_id
//   - the page at {checkout}?storeId=…&transactionId=…&redirectLink=…
//     — test-checkout.pays.uz in the sandbox, checkout.pays.uz live
//   - sign = hash of store_id+transaction_id+invoice+amount+api_key, joined
//     with no separators, in that order
//
// Two things the documentation does not settle, both configurable rather than
// guessed at here:
//
//   - It names the concatenation but not the hash. MD5 is what Atmos
//     integrations use and what Signature computes; if callbacks are rejected
//     with everything else correct, this is the thing to ask the cabinet about.
//   - `terminal_id` is only sent by accounts that were issued one.
//
// Getting one of these wrong fails loudly at the first real payment rather than
// silently mis-crediting anybody: a create call that is refused returns an
// error, and a callback whose signature does not match is rejected.
package atmos

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Config is everything the merchant account is identified by.
type Config struct {
	// BaseURL is the merchant API root, e.g. https://partner.atmos.uz.
	BaseURL string
	// CheckoutURL is the hosted invoice page the payer is sent to.
	CheckoutURL string
	// ConsumerKey / ConsumerSecret authenticate this server to Atmos.
	ConsumerKey    string
	ConsumerSecret string
	// StoreID is the shop this backend sells for.
	StoreID string
	// TerminalID is only used by accounts that were issued one.
	TerminalID string
	// APIKey is the secret the callback signature is hashed with. It is not the
	// consumer secret — a different value, from the merchant cabinet.
	APIKey string
	// RedirectURL is where Atmos sends the payer's browser once the invoice page
	// is done. A deep link back into the app; empty leaves them on Atmos's page.
	RedirectURL string

	// MPSAPIKey turns on Visa and Mastercard — see mps.go. It is a third
	// secret, separate from both ConsumerSecret and APIKey, issued when the MPS
	// module is applied for. Empty means the account has merchant-API access
	// only, which is the ordinary case: local cards work and international ones
	// are refused with a sentence saying why, rather than with "Karta topilmadi"
	// from a processing the card was never in.
	MPSAPIKey string
	// MPSStoreID is only for accounts issued a distinct store id for MPS.
	// Empty reuses StoreID, which is how Atmos sets most accounts up.
	MPSStoreID string
}

// Enabled reports whether enough is configured to take money *and account for
// it*. With it false the server runs exactly as it did before — useful in
// development, where DEV_MODE credits top-ups directly.
//
// APIKey is in this list even though nothing outbound needs it, and that is the
// point. Without it every callback fails its signature check, so a server
// configured with everything *except* the merchant key is the one arrangement
// that is worse than having no gateway at all: it opens real transactions, it
// sends real people to a real card form, and then it credits nobody, for ever,
// silently. Refusing to call that "enabled" turns a catastrophe into an
// obvious, harmless "payments are not set up yet".
func (c Config) Enabled() bool {
	return c.BaseURL != "" && c.ConsumerKey != "" && c.ConsumerSecret != "" &&
		c.StoreID != "" && c.APIKey != ""
}

// Client is a configured Atmos merchant client. Safe for concurrent use: the
// only mutable state is the cached bearer token.
type Client struct {
	cfg  Config
	http *http.Client

	// tokenMu is held across the whole fetch, not just the read, so a burst of
	// concurrent payments collapses into one token request instead of one per
	// in-flight payment.
	tokenMu   sync.Mutex
	token     string
	tokenTill time.Time
}

// New builds a client. A slow or unreachable Atmos must not hang a request
// goroutine, hence the timeout.
func New(cfg Config) *Client {
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	cfg.CheckoutURL = strings.TrimSpace(cfg.CheckoutURL)
	return &Client{cfg: cfg, http: &http.Client{Timeout: 20 * time.Second}}
}

// Enabled reports whether this client can reach Atmos.
func (c *Client) Enabled() bool { return c.cfg.Enabled() }

// ---- Auth -------------------------------------------------------------------

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	TokenType   string `json:"token_type"`
}

// accessToken returns a valid bearer token, fetching a new one when the cached
// one is gone or nearly gone.
//
// The minute of slack is not paranoia: a token that expires between this check
// and Atmos reading it fails a payment that had already been shown to somebody
// as "opening…".
func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	if c.token != "" && time.Now().Add(time.Minute).Before(c.tokenTill) {
		return c.token, nil
	}

	form := url.Values{"grant_type": {"client_credentials"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.cfg.BaseURL+"/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	basic := base64.StdEncoding.EncodeToString([]byte(c.cfg.ConsumerKey + ":" + c.cfg.ConsumerSecret))
	req.Header.Set("Authorization", "Basic "+basic)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	res, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return "", fmt.Errorf("atmos: token: http %d: %s", res.StatusCode, snippet(body))
	}
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil || tr.AccessToken == "" {
		return "", fmt.Errorf("atmos: token: unexpected response: %s", snippet(body))
	}
	ttl := time.Duration(tr.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = time.Hour
	}
	c.token, c.tokenTill = tr.AccessToken, time.Now().Add(ttl)
	return c.token, nil
}

// ---- Transactions -----------------------------------------------------------

// Transaction is an opened, unpaid invoice.
type Transaction struct {
	// ID is Atmos's own transaction id. It is the key the callback arrives
	// under, so it is what the payment row is matched on.
	ID string
	// PayURL is where the payer has to be sent.
	PayURL string
}

type createResponse struct {
	TransactionID    json.Number `json:"transaction_id"`
	StoreTransaction *struct {
		TransactionID json.Number `json:"trans_id"`
	} `json:"store_transaction"`
	Result struct {
		Code        string `json:"code"`
		Description string `json:"description"`
	} `json:"result"`
}

// CreateTransaction opens an invoice for `amountSom` and returns it with the
// URL the payer must be sent to.
//
// `account` is the merchant's own reference for what is being paid for — this
// backend passes the payment row id, so a callback can be traced back to a
// person and a purpose even if Atmos's id were somehow lost.
func (c *Client) CreateTransaction(ctx context.Context, amountSom int64, account string) (*Transaction, error) {
	if !c.Enabled() {
		return nil, fmt.Errorf("atmos: not configured")
	}
	if amountSom <= 0 {
		return nil, fmt.Errorf("atmos: amount must be positive")
	}
	// Atmos counts in tiyin; the rest of this application counts in whole so'm.
	payload := map[string]any{
		"amount":   amountSom * 100,
		"account":  account,
		"store_id": c.storeID(),
		"lang":     "uz",
	}
	if c.cfg.TerminalID != "" {
		payload["terminal_id"] = c.cfg.TerminalID
	}

	raw, err := c.post(ctx, "/merchant/pay/create", payload)
	if err != nil {
		return nil, err
	}

	var cr createResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return nil, fmt.Errorf("atmos: create: unexpected response: %s", snippet(raw))
	}
	id := cr.TransactionID.String()
	if id == "" || id == "0" {
		if cr.StoreTransaction != nil {
			id = cr.StoreTransaction.TransactionID.String()
		}
	}
	if id == "" || id == "0" {
		return nil, fmt.Errorf("atmos: create: no transaction id (code %s: %s)",
			cr.Result.Code, cr.Result.Description)
	}
	return &Transaction{ID: id, PayURL: c.InvoiceURL(id)}, nil
}

// InvoiceURL builds the hosted checkout link for a transaction.
func (c *Client) InvoiceURL(transactionID string) string {
	base := c.cfg.CheckoutURL
	if base == "" {
		return ""
	}
	q := url.Values{
		"storeId":       {c.cfg.StoreID},
		"transactionId": {transactionID},
	}
	if c.cfg.RedirectURL != "" {
		q.Set("redirectLink", c.cfg.RedirectURL)
	}
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep + q.Encode()
}

// ---- Direct card payment ----------------------------------------------------

// APIError is Atmos refusing something, as opposed to the network failing to
// reach it. The two need telling apart at the call site: a refusal has a reason
// worth showing the payer ("card expired", "wrong code"), while a transport
// failure has nothing to say to them and everything to say to the log.
type APIError struct {
	Op          string
	Code        string
	Description string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("atmos: %s refused (code %s): %s", e.Op, e.Code, e.Description)
}

// Message is what a payer can usefully be shown.
//
// The code is consulted first — see messages.go. Atmos's own descriptions are
// Russian, and its commonest one ("Карта не найдена") tells a payer their card
// is missing when what is actually wrong is a mistyped digit or expiry, so for
// the codes that a cardholder can act on there is a better sentence to give
// them.
//
// Anything not in that table keeps Atmos's wording. An unanticipated refusal is
// more use in the gateway's own words, however awkward, than flattened into
// "something went wrong": the payer can read it out, and support can look it up.
func (e *APIError) Message() string {
	if m := payerMessage(e.Code); m != "" {
		return m
	}
	if d := strings.TrimSpace(e.Description); d != "" {
		return d
	}
	return "To'lov tizimi so'rovni rad etdi."
}

// apiResult is the status envelope every merchant/pay/* response carries.
type apiResult struct {
	Result struct {
		Code        string `json:"code"`
		Description string `json:"description"`
	} `json:"result"`
}

// ok reports success. Atmos answers HTTP 200 to refusals as well as successes
// and puts the verdict in the body, so the status line alone is not evidence
// that anything happened.
func (r apiResult) ok() bool { return strings.EqualFold(r.Result.Code, "OK") }

// PreApply hands Atmos the card and asks it to text the cardholder a code.
//
// Nothing is debited here. The card number is passed through and not retained:
// it is a parameter on the way to Atmos, never a field on a struct this server
// keeps, and never anything the logs see.
//
// `expiry` is Atmos's YYmm — use NormalizeExpiry on whatever the app collected.
func (c *Client) PreApply(ctx context.Context, transactionID, cardNumber, expiry string) error {
	if !c.Enabled() {
		return fmt.Errorf("atmos: not configured")
	}
	if transactionID == "" || cardNumber == "" || expiry == "" {
		return fmt.Errorf("atmos: pre-apply: transaction, card and expiry are all required")
	}
	raw, err := c.post(ctx, "/merchant/pay/pre-apply", map[string]any{
		"card_number":    cardNumber,
		"expiry":         expiry,
		"store_id":       c.storeID(),
		"transaction_id": numeric(transactionID),
	})
	if err != nil {
		return err
	}
	var res apiResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("atmos: pre-apply: unexpected response: %s", snippet(raw))
	}
	if !res.ok() {
		return &APIError{Op: "pre-apply", Code: res.Result.Code, Description: res.Result.Description}
	}
	return nil
}

// Payment is a transaction Atmos has confirmed and debited.
type Payment struct {
	// TransactionID is the id the callback for this payment will arrive under,
	// so it stays the key everything is matched on.
	TransactionID string
	// AmountTiyin is what Atmos says it actually took, which is the figure to
	// check against what was asked for.
	AmountTiyin int64
	// Confirmed is Atmos's own verdict. Checked as well as the result code,
	// because "no errors" and "the money moved" are not the same claim.
	Confirmed bool
	// OFDURL is the fiscal receipt, when the account is set up to issue one.
	OFDURL string
}

type applyResponse struct {
	apiResult
	StoreTransaction *struct {
		TransID   json.Number `json:"trans_id"`
		Amount    json.Number `json:"amount"`
		Confirmed bool        `json:"confirmed"`
	} `json:"store_transaction"`
	OFDURL string `json:"ofd_url"`
}

// Apply confirms the transaction with the code the cardholder was sent, and is
// the call that actually moves the money.
//
// A transaction that comes back unconfirmed is an error even when the result
// code says OK: crediting a wallet on the strength of a reply that never
// claimed the card was debited is the one mistake this whole file exists to
// avoid.
func (c *Client) Apply(ctx context.Context, transactionID, otp string) (*Payment, error) {
	if !c.Enabled() {
		return nil, fmt.Errorf("atmos: not configured")
	}
	if transactionID == "" || otp == "" {
		return nil, fmt.Errorf("atmos: apply: transaction and otp are both required")
	}
	raw, err := c.post(ctx, "/merchant/pay/apply", map[string]any{
		"transaction_id": numeric(transactionID),
		// Sent as a string, never through numeric(). An SMS code is a sequence
		// of six digits, not a quantity: "012345" as a JSON number is 12345,
		// which is a different code and one Atmos will refuse. That refusal
		// reads as "Noto'g'ri SMS kod" to a payer who typed theirs correctly,
		// and it lands on roughly one code in ten.
		"otp":      otp,
		"store_id": c.storeID(),
	})
	if err != nil {
		return nil, err
	}
	var res applyResponse
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("atmos: apply: unexpected response: %s", snippet(raw))
	}
	if !res.ok() {
		return nil, &APIError{Op: "apply", Code: res.Result.Code, Description: res.Result.Description}
	}
	if res.StoreTransaction == nil || !res.StoreTransaction.Confirmed {
		return nil, &APIError{
			Op:          "apply",
			Code:        res.Result.Code,
			Description: "To'lov tasdiqlanmadi. Qaytadan urinib ko'ring.",
		}
	}
	amount, _ := res.StoreTransaction.Amount.Int64()
	id := res.StoreTransaction.TransID.String()
	if id == "" || id == "0" {
		id = transactionID
	}
	return &Payment{
		TransactionID: id,
		AmountTiyin:   amount,
		Confirmed:     true,
		OFDURL:        res.OFDURL,
	}, nil
}

// RetryOTP asks Atmos to text the code again.
//
// Only meaningful for a transaction pre-applied with a card number: one
// pre-applied with a saved card token was never sent a code to begin with.
func (c *Client) RetryOTP(ctx context.Context, transactionID string) error {
	if !c.Enabled() {
		return fmt.Errorf("atmos: not configured")
	}
	if transactionID == "" {
		return fmt.Errorf("atmos: retry-otp: transaction is required")
	}
	raw, err := c.post(ctx, "/merchant/pay/retry-otp", map[string]any{
		"store_id":       c.storeID(),
		"transaction_id": numeric(transactionID),
	})
	if err != nil {
		return err
	}
	var res apiResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("atmos: retry-otp: unexpected response: %s", snippet(raw))
	}
	if !res.ok() {
		return &APIError{Op: "retry-otp", Code: res.Result.Code, Description: res.Result.Description}
	}
	return nil
}

// NormalizeExpiry turns what a card form collects into the YYmm Atmos wants.
//
// Accepts "MM/YY", "MM/YYYY", "MMYY" and YYmm itself, because a card prints
// MM/YY and that is what people type, while the API wants the halves the other
// way round. Getting this backwards is a payment refused for a card that was
// perfectly good, so it is decided here once rather than at each caller.
func NormalizeExpiry(s string) (string, bool) {
	digits := make([]rune, 0, 6)
	for _, r := range s {
		if r >= '0' && r <= '9' {
			digits = append(digits, r)
		}
	}
	switch len(digits) {
	case 4:
		// Ambiguous on its face: "0128" is January 2028 read as MMYY and August
		// 2001 read as YYmm. Only one of the two halves can be a month, and a
		// card that expired in 2001 is not the one somebody is paying with, so
		// a valid leading month wins.
		mm, yy := string(digits[:2]), string(digits[2:])
		if isMonth(mm) {
			return yy + mm, true
		}
		if isMonth(yy) {
			return mm + yy, true
		}
		return "", false
	case 6:
		// MM/YYYY — drop the century.
		mm, yyyy := string(digits[:2]), string(digits[2:])
		if !isMonth(mm) {
			return "", false
		}
		return yyyy[2:] + mm, true
	default:
		return "", false
	}
}

func isMonth(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n >= 1 && n <= 12
}

// ---- Transport --------------------------------------------------------------

// post sends an authenticated JSON request and returns the raw body.
//
// Every merchant call is this same shape, and the bodies carry card data on the
// way out and cardholder detail on the way back, so there is exactly one place
// that touches them — one place to be careful about, and one place that can
// never accidentally grow a log line.
func (c *Client) post(ctx context.Context, path string, payload any) ([]byte, error) {
	return c.postWithHeaders(ctx, path, payload, nil)
}

// postWithHeaders is post() for a module that authenticates with more than the
// bearer token — MPS, which wants its own `apikey` alongside it. Everything
// else about the call is identical, retry on 401 included, so the two share a
// body rather than a resemblance.
func (c *Client) postWithHeaders(ctx context.Context, path string, payload any, extra map[string]string) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	raw, status, err := c.postOnce(ctx, path, body, extra)
	if err != nil {
		return nil, err
	}
	// One retry on 401, with the cached token thrown away first.
	//
	// A token is cached until the expiry Atmos stated, but the gateway is
	// entitled to end one early — a new token issued for the same account, a
	// restart on their side — and when it does, every call from here fails
	// until the stated hour is up. That is a whole afternoon of "to'lov
	// tizimiga ulanib bo'lmadi" from a server that only had to ask for a new
	// token. Retried exactly once: a 401 that survives a fresh token is a
	// credentials problem, and repeating it would only bury the reason.
	if status == http.StatusUnauthorized {
		c.forgetToken()
		raw, status, err = c.postOnce(ctx, path, body, extra)
		if err != nil {
			return nil, err
		}
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("atmos: %s: http %d: %s", strings.TrimPrefix(path, "/"),
			status, snippet(raw))
	}
	return raw, nil
}

// postOnce is a single attempt: the body is already marshalled, so a retry
// re-sends the identical bytes rather than re-encoding a map whose key order
// might differ.
func (c *Client) postOnce(ctx context.Context, path string, body []byte, extra map[string]string) ([]byte, int, error) {
	token, err := c.accessToken(ctx)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.cfg.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range extra {
		req.Header.Set(k, v)
	}

	res, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	return raw, res.StatusCode, nil
}

// forgetToken drops the cached bearer so the next call fetches a fresh one.
func (c *Client) forgetToken() {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	c.token, c.tokenTill = "", time.Time{}
}

// storeID renders the configured store the way the documented examples do — as
// a number — while still working for an account whose id is not one.
func (c *Client) storeID() any { return numeric(c.cfg.StoreID) }

// numeric sends a digit string as a JSON number, and anything else as a string.
//
// The documentation's examples are all numbers (`"store_id": 11100`), and the
// gateway has been seen to accept strings too, but only the documented shape is
// promised — so send that whenever the value allows it.
func numeric(s string) any {
	s = strings.TrimSpace(s)
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	return s
}

// ---- Callback ---------------------------------------------------------------

// Callback is what Atmos posts once money has moved.
//
// Every field is held as text because that is what the signature is computed
// over — see Signature. It is *not* a claim that Atmos sends them as JSON
// strings; see UnmarshalJSON for why that distinction matters.
type Callback struct {
	StoreID       string `json:"store_id"`
	TransactionID string `json:"transaction_id"`
	Invoice       string `json:"invoice"`
	// Amount is in tiyin, as Atmos counts.
	Amount string `json:"amount"`
	Sign   string `json:"sign"`
}

// UnmarshalJSON reads a callback whose numeric fields may arrive as JSON
// numbers rather than as strings.
//
// This is the difference between money landing and money vanishing. Atmos posts
// `{"store_id":11100,"transaction_id":257022,"amount":100000,…}` — bare
// numbers — and the plain string fields above cannot hold them: encoding/json
// fails the whole document with "cannot unmarshal number into Go value of type
// string". The callback is then read as malformed, answered "Bad request", and
// the payment stays pending for ever while the card has already been debited.
// Nothing else in the system notices, because from its side no callback ever
// arrived.
//
// The number's own digits are kept verbatim rather than parsed and reprinted:
// the signature is a hash of the exact text Atmos concatenated, so a value that
// came back through an int64 — losing a leading zero, or rendering 1e5 — would
// verify against nothing.
func (cb *Callback) UnmarshalJSON(data []byte) error {
	// A distinct type, or this method would call itself.
	type raw struct {
		StoreID       json.RawMessage `json:"store_id"`
		TransactionID json.RawMessage `json:"transaction_id"`
		Invoice       json.RawMessage `json:"invoice"`
		Amount        json.RawMessage `json:"amount"`
		Sign          json.RawMessage `json:"sign"`
	}
	var in raw
	if err := json.Unmarshal(data, &in); err != nil {
		return err
	}
	cb.StoreID = jsonText(in.StoreID)
	cb.TransactionID = jsonText(in.TransactionID)
	cb.Invoice = jsonText(in.Invoice)
	cb.Amount = jsonText(in.Amount)
	cb.Sign = jsonText(in.Sign)
	return nil
}

// jsonText renders one JSON value as the text it was signed as: a string
// unquoted, a number left exactly as its digits were written, anything absent
// as empty.
func jsonText(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	if s[0] == '"' {
		var out string
		if err := json.Unmarshal(raw, &out); err != nil {
			return ""
		}
		return strings.TrimSpace(out)
	}
	return s
}

// Signature is the hash Atmos signs a callback with:
//
//	md5(store_id + transaction_id + invoice + amount + api_key)
//
// Verified rather than trusted, because this endpoint is necessarily public:
// without the check, anybody who guessed a transaction id could credit a
// wallet by posting to it.
func (c *Client) Signature(cb Callback) string {
	sum := md5.Sum([]byte(cb.StoreID + cb.TransactionID + cb.Invoice + cb.Amount + c.cfg.APIKey))
	return hex.EncodeToString(sum[:])
}

// VerifyCallback reports whether a callback really came from Atmos, and for
// this shop.
//
// A merchant key that is not configured makes every callback invalid rather
// than every callback valid — the failure mode of a missing secret must be
// "nothing is credited", never "anything is".
func (c *Client) VerifyCallback(cb Callback) bool {
	if c.cfg.APIKey == "" || cb.Sign == "" {
		return false
	}
	if c.cfg.StoreID != "" && cb.StoreID != "" && cb.StoreID != c.cfg.StoreID {
		return false
	}
	return strings.EqualFold(cb.Sign, c.Signature(cb))
}

// AmountSom converts a callback's tiyin figure back to whole so'm, so it can be
// compared against what the payment row says was asked for. Reports false when
// the field is not a number or is not a whole number of so'm.
func (cb Callback) AmountSom() (int64, bool) {
	tiyin, err := strconv.ParseInt(strings.TrimSpace(cb.Amount), 10, 64)
	if err != nil || tiyin <= 0 || tiyin%100 != 0 {
		return 0, false
	}
	return tiyin / 100, true
}

// snippet keeps a provider's error out of the logs at full length while still
// saying enough to diagnose it.
func snippet(b []byte) string {
	const max = 300
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
