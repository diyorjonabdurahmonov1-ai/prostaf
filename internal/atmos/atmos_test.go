package atmos

import (
	"context"
	"strings"
	"testing"
)

func testClient() *Client {
	return New(Config{
		BaseURL:        "https://partner.example",
		CheckoutURL:    "https://checkout.example/invoice/get",
		ConsumerKey:    "key",
		ConsumerSecret: "secret",
		StoreID:        "1234",
		APIKey:         "merchant-api-key",
		RedirectURL:    "ishtopish://wallet",
	})
}

func validCallback(c *Client) Callback {
	cb := Callback{
		StoreID:       "1234",
		TransactionID: "987654",
		Invoice:       "pay-1",
		Amount:        "5000000",
	}
	cb.Sign = c.Signature(cb)
	return cb
}

// The signature is the only thing standing between a public endpoint and
// anybody's wallet, so every way of getting it wrong has to fail closed.
func TestVerifyCallback(t *testing.T) {
	c := testClient()
	if !c.VerifyCallback(validCallback(c)) {
		t.Fatal("a correctly signed callback must verify")
	}

	tampered := []struct {
		name  string
		apply func(*Callback)
	}{
		{"amount raised", func(cb *Callback) { cb.Amount = "9900000" }},
		{"different transaction", func(cb *Callback) { cb.TransactionID = "111" }},
		{"different invoice", func(cb *Callback) { cb.Invoice = "pay-2" }},
		{"another shop", func(cb *Callback) { cb.StoreID = "4321" }},
		{"no signature", func(cb *Callback) { cb.Sign = "" }},
		{"junk signature", func(cb *Callback) { cb.Sign = "deadbeef" }},
	}
	for _, tc := range tampered {
		cb := validCallback(c)
		tc.apply(&cb)
		if c.VerifyCallback(cb) {
			t.Errorf("%s: must not verify", tc.name)
		}
	}

	// Upper-case hex is the same hash — gateways are not consistent about it,
	// and a rejection over letter case would be a payment lost to nothing.
	upper := validCallback(c)
	upper.Sign = strings.ToUpper(upper.Sign)
	if !c.VerifyCallback(upper) {
		t.Error("an upper-case hex signature must still verify")
	}
}

// A merchant key that was never configured must make every callback invalid,
// never every callback valid: the failure mode of a missing secret has to be
// "nothing is credited".
func TestVerifyCallbackWithoutAPIKey(t *testing.T) {
	c := New(Config{StoreID: "1234"})
	cb := Callback{StoreID: "1234", TransactionID: "1", Invoice: "p", Amount: "100"}
	cb.Sign = c.Signature(cb)
	if c.VerifyCallback(cb) {
		t.Fatal("an unconfigured merchant key must reject every callback")
	}
}

func TestAmountSom(t *testing.T) {
	cases := []struct {
		in    string
		want  int64
		valid bool
	}{
		{"5000000", 50000, true},
		{" 1000000 ", 10000, true},
		{"1000050", 0, false}, // not a whole so'm
		{"0", 0, false},
		{"-100", 0, false},
		{"abc", 0, false},
		{"", 0, false},
	}
	for _, tc := range cases {
		got, ok := Callback{Amount: tc.in}.AmountSom()
		if ok != tc.valid || got != tc.want {
			t.Errorf("AmountSom(%q) = (%d, %v), want (%d, %v)", tc.in, got, ok, tc.want, tc.valid)
		}
	}
}

func TestInvoiceURL(t *testing.T) {
	c := testClient()
	got := c.InvoiceURL("987654")
	want := "https://checkout.example/invoice/get?redirectLink=ishtopish%3A%2F%2Fwallet&storeId=1234&transactionId=987654"
	if got != want {
		t.Errorf("InvoiceURL =\n  %s\nwant\n  %s", got, want)
	}
}

// An unconfigured gateway must announce itself as off rather than half-work:
// that is what keeps development running with no merchant account.
func TestEnabled(t *testing.T) {
	if New(Config{}).Enabled() {
		t.Error("an empty config must not report itself enabled")
	}
	if !testClient().Enabled() {
		t.Error("a full config must report itself enabled")
	}
	if _, err := New(Config{}).CreateTransaction(context.Background(), 10000, "p1"); err == nil {
		t.Error("CreateTransaction on a disabled client must fail")
	}
}

// The configuration that must never count as working: everything needed to
// take money, and nothing needed to recognise the confirmation that it moved.
//
// It is worth a test of its own because it is the shape a half-finished
// merchant onboarding naturally produces — the store id and API credentials
// arrive first, and the callback key is issued separately, later.
func TestEnabledRequiresCallbackKey(t *testing.T) {
	cfg := Config{
		BaseURL:        "https://apigw.atmos.uz",
		ConsumerKey:    "key",
		ConsumerSecret: "secret",
		StoreID:        "11100",
	}
	if cfg.Enabled() {
		t.Error("a gateway with no callback key must not report itself enabled: " +
			"it would charge cards and credit nobody")
	}
	cfg.APIKey = "merchant-api-key"
	if !cfg.Enabled() {
		t.Error("with the callback key present the gateway must be enabled")
	}
}
