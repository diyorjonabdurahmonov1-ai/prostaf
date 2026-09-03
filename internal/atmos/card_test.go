package atmos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// The card half of the integration, which is the half that moves money without
// anybody ever visiting a page at the gateway.
//
// These are worth writing down because the real thing cannot be rehearsed: a
// wrong expiry format or a result code read as success would each surface as
// somebody's card being refused, or worse, a wallet credited for a debit that
// never happened.

// fakeCardAtmos answers the three merchant/pay calls the card flow makes.
type fakeCardAtmos struct {
	server *httptest.Server

	mu     sync.Mutex
	bodies map[string]map[string]any

	preApply string
	apply    string
	retry    string
}

func newFakeCardAtmos(t *testing.T) *fakeCardAtmos {
	t.Helper()
	f := &fakeCardAtmos{
		bodies:   map[string]map[string]any{},
		preApply: `{"transaction_id":202072,"result":{"code":"OK","description":"no errors"}}`,
		apply: `{"result":{"code":"OK","description":"no errors"},
			"store_transaction":{"trans_id":202072,"amount":5000000,"confirmed":true},
			"ofd_url":"https://ofd2.atmos.uz/api/ofd/abc"}`,
		retry: `{"result":{"code":"OK","description":"no errors"},"transaction_id":202072}`,
	}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			fmt.Fprint(w, `{"access_token":"tok-1","expires_in":3600,"token_type":"Bearer"}`)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		body := map[string]any{}
		_ = json.Unmarshal(raw, &body)
		f.mu.Lock()
		f.bodies[r.URL.Path] = body
		f.mu.Unlock()

		switch r.URL.Path {
		case "/merchant/pay/pre-apply":
			fmt.Fprint(w, f.preApply)
		case "/merchant/pay/apply":
			fmt.Fprint(w, f.apply)
		case "/merchant/pay/retry-otp":
			fmt.Fprint(w, f.retry)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeCardAtmos) client() *Client {
	return New(Config{
		BaseURL:        f.server.URL,
		ConsumerKey:    "key",
		ConsumerSecret: "secret",
		StoreID:        "11100",
		APIKey:         "merchant-api-key",
	})
}

func (f *fakeCardAtmos) body(path string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[path]
}

// The documented request shape, field by field. store_id and transaction_id
// leave as numbers because that is what every example in the documentation
// shows, and the expiry leaves as YYmm however the app spelled it.
func TestPreApplySendsDocumentedShape(t *testing.T) {
	f := newFakeCardAtmos(t)

	if err := f.client().PreApply(context.Background(), "257000", "8600490000000000", "2801"); err != nil {
		t.Fatalf("PreApply: %v", err)
	}

	body := f.body("/merchant/pay/pre-apply")
	if got := body["card_number"]; got != "8600490000000000" {
		t.Errorf("card_number = %v", got)
	}
	if got := body["expiry"]; got != "2801" {
		t.Errorf("expiry = %v, want 2801", got)
	}
	if got, ok := body["store_id"].(float64); !ok || got != 11100 {
		t.Errorf("store_id = %#v, want the number 11100", body["store_id"])
	}
	if got, ok := body["transaction_id"].(float64); !ok || got != 257000 {
		t.Errorf("transaction_id = %#v, want the number 257000", body["transaction_id"])
	}
}

// Atmos answers HTTP 200 to refusals too, so the result code is the only thing
// that says whether anything happened. Reading a refusal as success is how a
// wallet gets credited for money that never moved.
func TestPreApplyRefusalIsAnError(t *testing.T) {
	f := newFakeCardAtmos(t)
	f.preApply = `{"result":{"code":"STPIMS-ERR-092","description":"Karta muddati tugagan"}}`

	err := f.client().PreApply(context.Background(), "257000", "8600490000000000", "2801")
	if err == nil {
		t.Fatal("PreApply: want an error for a refused card, got nil")
	}
	var refusal *APIError
	if !errors.As(err, &refusal) {
		t.Fatalf("PreApply: error is %T, want *APIError", err)
	}
	// The payer sees Atmos's own sentence rather than one of ours: it says more,
	// and it is written for cardholders.
	if refusal.Message() != "Karta muddati tugagan" {
		t.Errorf("Message() = %q", refusal.Message())
	}
}

func TestApplyReturnsConfirmedPayment(t *testing.T) {
	f := newFakeCardAtmos(t)

	paid, err := f.client().Apply(context.Background(), "257000", "111111")
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !paid.Confirmed {
		t.Error("Confirmed = false, want true")
	}
	if paid.AmountTiyin != 5_000_000 {
		t.Errorf("AmountTiyin = %d, want 5000000", paid.AmountTiyin)
	}
	if paid.TransactionID != "202072" {
		t.Errorf("TransactionID = %q, want 202072", paid.TransactionID)
	}
	if body := f.body("/merchant/pay/apply"); body["otp"] != float64(111111) {
		t.Errorf("otp = %#v, want the number 111111", body["otp"])
	}
}

// "code: OK" and "the card was debited" are different claims, and only the
// second one may credit a wallet. A reply that says the first without the
// second has to fail, or a declined payment tops somebody up for free.
func TestApplyUnconfirmedIsAnError(t *testing.T) {
	f := newFakeCardAtmos(t)
	f.apply = `{"result":{"code":"OK","description":"no errors"},
		"store_transaction":{"trans_id":202072,"amount":5000000,"confirmed":false}}`

	if _, err := f.client().Apply(context.Background(), "257000", "111111"); err == nil {
		t.Fatal("Apply: want an error when the transaction is not confirmed, got nil")
	}
}

func TestApplyWrongCodeIsRefusal(t *testing.T) {
	f := newFakeCardAtmos(t)
	f.apply = `{"result":{"code":"STPIMS-ERR-014","description":"Noto'g'ri SMS kod"}}`

	_, err := f.client().Apply(context.Background(), "257000", "000000")
	var refusal *APIError
	if !errors.As(err, &refusal) {
		t.Fatalf("Apply: error is %v (%T), want *APIError", err, err)
	}
	if !strings.Contains(refusal.Message(), "SMS") {
		t.Errorf("Message() = %q, want Atmos's own wording", refusal.Message())
	}
}

func TestRetryOTP(t *testing.T) {
	f := newFakeCardAtmos(t)

	if err := f.client().RetryOTP(context.Background(), "257000"); err != nil {
		t.Fatalf("RetryOTP: %v", err)
	}
	if got := f.body("/merchant/pay/retry-otp")["transaction_id"]; got != float64(257000) {
		t.Errorf("transaction_id = %#v", got)
	}
}

// A card prints MM/YY; Atmos wants YYmm. Every one of these is something a
// person really types, and getting the halves the wrong way round refuses a
// card that was perfectly good.
func TestNormalizeExpiry(t *testing.T) {
	for _, tc := range []struct {
		in    string
		want  string
		valid bool
	}{
		{"01/28", "2801", true},
		{"0128", "2801", true},
		{"01 / 28", "2801", true},
		{"12/29", "2912", true},
		{"01/2028", "2801", true},
		{"2801", "2801", true}, // already YYmm: 28 is no month, 01 is
		{"1229", "2912", true}, // 12 is a month, so MM/YY wins
		{"13/28", "", false},   // no such month either way round
		{"128", "", false},
		{"", "", false},
	} {
		got, ok := NormalizeExpiry(tc.in)
		if ok != tc.valid || got != tc.want {
			t.Errorf("NormalizeExpiry(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.valid)
		}
	}
}
