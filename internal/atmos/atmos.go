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
// # Verify against the merchant docs before going live
//
// Atmos publishes its paths, field names and signature recipe per merchant
// account, and they are all configurable (ATMOS_* in .env) rather than compiled
// in for exactly that reason. The defaults below are the common shape of the
// API; the four things to confirm against your own documentation are:
//
//   - the token, create and checkout paths (ATMOS_BASE_URL, ATMOS_CHECKOUT_URL)
//   - that amounts are sent in tiyin (this package converts from so'm)
//   - the callback field names and the order they are hashed in (Signature)
//   - whether your account uses `terminal_id` in addition to `store_id`
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
}

// Enabled reports whether enough is configured to talk to Atmos at all. With it
// false the server runs exactly as it did before — useful in development, where
// DEV_MODE credits top-ups directly.
func (c Config) Enabled() bool {
	return c.BaseURL != "" && c.ConsumerKey != "" && c.ConsumerSecret != "" && c.StoreID != ""
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
	TransactionID json.Number `json:"transaction_id"`
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
	token, err := c.accessToken(ctx)
	if err != nil {
		return nil, err
	}

	// Atmos counts in tiyin; the rest of this application counts in whole so'm.
	payload := map[string]any{
		"amount":   amountSom * 100,
		"account":  account,
		"store_id": c.cfg.StoreID,
		"lang":     "uz",
	}
	if c.cfg.TerminalID != "" {
		payload["terminal_id"] = c.cfg.TerminalID
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.cfg.BaseURL+"/merchant/pay/create", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("atmos: create: http %d: %s", res.StatusCode, snippet(raw))
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

// ---- Callback ---------------------------------------------------------------

// Callback is what Atmos posts once money has moved.
type Callback struct {
	StoreID       string `json:"store_id"`
	TransactionID string `json:"transaction_id"`
	Invoice       string `json:"invoice"`
	// Amount is in tiyin, as Atmos counts.
	Amount string `json:"amount"`
	Sign   string `json:"sign"`
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
