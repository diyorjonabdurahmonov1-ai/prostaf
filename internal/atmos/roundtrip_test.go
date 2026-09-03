package atmos

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// What a real Atmos would see and say back.
//
// The point of standing one up in a test is that the outbound half of this
// integration cannot otherwise be exercised without a merchant account: whether
// the token request is authenticated the way Atmos expects, whether the amount
// leaves here in tiyin, and whether a transaction id is found in the reply are
// all questions whose real answer would otherwise arrive as somebody's failed
// first payment.
type fakeAtmos struct {
	server *httptest.Server

	mu       sync.Mutex
	tokens   atomic.Int32
	lastAuth string
	lastBody map[string]any
}

func newFakeAtmos(t *testing.T, createBody string) *fakeAtmos {
	t.Helper()
	f := &fakeAtmos{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			f.tokens.Add(1)
			if err := r.ParseForm(); err != nil {
				t.Errorf("token: parse form: %v", err)
			}
			if got := r.Form.Get("grant_type"); got != "client_credentials" {
				t.Errorf("token: grant_type = %q, want client_credentials", got)
			}
			want := "Basic " + base64.StdEncoding.EncodeToString([]byte("key:secret"))
			if got := r.Header.Get("Authorization"); got != want {
				t.Errorf("token: Authorization = %q, want %q", got, want)
			}
			fmt.Fprint(w, `{"access_token":"tok-1","expires_in":3600,"token_type":"Bearer"}`)

		case "/merchant/pay/create":
			raw, _ := io.ReadAll(r.Body)
			f.mu.Lock()
			f.lastAuth = r.Header.Get("Authorization")
			f.lastBody = map[string]any{}
			_ = json.Unmarshal(raw, &f.lastBody)
			f.mu.Unlock()
			fmt.Fprint(w, createBody)

		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeAtmos) client(cfg func(*Config)) *Client {
	c := Config{
		BaseURL:        f.server.URL,
		CheckoutURL:    "https://checkout.example/invoice/get",
		ConsumerKey:    "key",
		ConsumerSecret: "secret",
		StoreID:        "1234",
		APIKey:         "merchant-api-key",
		RedirectURL:    "ishtopish://wallet",
	}
	if cfg != nil {
		cfg(&c)
	}
	return New(c)
}

func (f *fakeAtmos) body() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastBody
}

// The whole outbound half, in the order it really happens: authenticate, open a
// transaction for a sum in so'm, get back somewhere to send the payer.
func TestCreateTransactionRoundTrip(t *testing.T) {
	f := newFakeAtmos(t, `{"transaction_id":987654,"result":{"code":"OK","description":"success"}}`)
	c := f.client(nil)

	tx, err := c.CreateTransaction(context.Background(), 50_000, "payment-abc")
	if err != nil {
		t.Fatalf("CreateTransaction: %v", err)
	}
	if tx.ID != "987654" {
		t.Errorf("transaction id = %q, want 987654", tx.ID)
	}
	want := "https://checkout.example/invoice/get?redirectLink=ishtopish%3A%2F%2Fwallet&storeId=1234&transactionId=987654"
	if tx.PayURL != want {
		t.Errorf("PayURL =\n  %s\nwant\n  %s", tx.PayURL, want)
	}

	body := f.body()
	// Atmos counts in tiyin and this application counts in so'm. Getting the
	// factor wrong is the one mistake here that would not look like a mistake —
	// it would charge a hundred times too little, or too much.
	if got := body["amount"]; got != float64(5_000_000) {
		t.Errorf("amount = %v, want 5000000 tiyin for 50000 so'm", got)
	}
	// The payment row id travels as `account`, which is what lets a callback be
	// traced back to a person even if Atmos's own id went missing.
	if got := body["account"]; got != "payment-abc" {
		t.Errorf("account = %v, want the payment id", got)
	}
	// A number, not a string: every example in the documentation writes it that
	// way, and it is the shape the live gateway was checked against.
	if got, ok := body["store_id"].(float64); !ok || got != 1234 {
		t.Errorf("store_id = %#v, want the number 1234", body["store_id"])
	}
	// Accounts without a terminal must not send an empty one: a blank field is
	// not the same as an absent field to a payment gateway.
	if _, present := body["terminal_id"]; present {
		t.Error("terminal_id must be absent when the account was not issued one")
	}
	f.mu.Lock()
	auth := f.lastAuth
	f.mu.Unlock()
	if auth != "Bearer tok-1" {
		t.Errorf("create Authorization = %q, want the bearer token from /token", auth)
	}
}

func TestCreateTransactionSendsTerminalWhenConfigured(t *testing.T) {
	f := newFakeAtmos(t, `{"transaction_id":11,"result":{"code":"OK"}}`)
	c := f.client(func(cfg *Config) { cfg.TerminalID = "term-9" })

	if _, err := c.CreateTransaction(context.Background(), 10_000, "p1"); err != nil {
		t.Fatalf("CreateTransaction: %v", err)
	}
	if got := f.body()["terminal_id"]; got != "term-9" {
		t.Errorf("terminal_id = %v, want term-9", got)
	}
}

// Some merchant accounts answer with the id nested under store_transaction
// instead of at the top level. Both are the same transaction; reading only one
// shape would fail every payment on half the accounts.
func TestCreateTransactionReadsNestedTransactionID(t *testing.T) {
	f := newFakeAtmos(t, `{"transaction_id":0,"store_transaction":{"trans_id":"55501"},"result":{"code":"OK"}}`)
	c := f.client(nil)

	tx, err := c.CreateTransaction(context.Background(), 10_000, "p1")
	if err != nil {
		t.Fatalf("CreateTransaction: %v", err)
	}
	if tx.ID != "55501" {
		t.Errorf("transaction id = %q, want 55501", tx.ID)
	}
}

// A reply with no usable id has to be an error, not a Transaction with an empty
// id: the id is what a callback is matched on, so an empty one is a payment
// that could never be credited however much money moved.
func TestCreateTransactionRefusesReplyWithoutID(t *testing.T) {
	f := newFakeAtmos(t, `{"transaction_id":0,"result":{"code":"STPIMS-ERR-389","description":"store not found"}}`)
	c := f.client(nil)

	_, err := c.CreateTransaction(context.Background(), 10_000, "p1")
	if err == nil {
		t.Fatal("expected an error when the reply carries no transaction id")
	}
	// The provider's own words have to survive into the error, because they are
	// the only thing that says which of a dozen settings is wrong.
	if !strings.Contains(err.Error(), "store not found") {
		t.Errorf("error dropped the provider's reason: %v", err)
	}
}

// A burst of payers must collapse into one token request, not one each: Atmos
// rate-limits that endpoint, and the whole point of caching the token is that
// a busy minute does not lock everybody out of paying.
func TestTokenFetchedOncePerBurst(t *testing.T) {
	f := newFakeAtmos(t, `{"transaction_id":1,"result":{"code":"OK"}}`)
	c := f.client(nil)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := c.CreateTransaction(context.Background(), 10_000, fmt.Sprintf("p%d", i)); err != nil {
				t.Errorf("CreateTransaction: %v", err)
			}
		}(i)
	}
	wg.Wait()

	if got := f.tokens.Load(); got != 1 {
		t.Errorf("token requests = %d, want 1", got)
	}
}

// A refused amount must never reach the gateway. Nothing downstream treats a
// zero-so'm payment as meaningful, and an invoice for nothing is a support call.
func TestCreateTransactionRefusesNonPositiveAmount(t *testing.T) {
	f := newFakeAtmos(t, `{"transaction_id":1}`)
	c := f.client(nil)

	for _, amount := range []int64{0, -1} {
		if _, err := c.CreateTransaction(context.Background(), amount, "p1"); err == nil {
			t.Errorf("CreateTransaction(%d) must fail", amount)
		}
	}
	if got := f.tokens.Load(); got != 0 {
		t.Errorf("a refused amount still asked for a token (%d requests)", got)
	}
}
