package atmos

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
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

// The shape a real callback arrives in.
//
// Atmos posts its ids and amounts as bare JSON numbers, and a struct of plain
// strings cannot hold them: encoding/json fails the whole document, the
// callback is read as malformed, and a card that was already debited credits
// nobody. This is the decode that has to work before anything else in the
// callback path is worth testing.
func TestCallbackDecodesNumericFields(t *testing.T) {
	body := `{"store_id":11100,"transaction_id":257022,
		"invoice":"b649db3d-5185-4569-a15e-07da3659d11d",
		"amount":100000,"sign":"e2c3f0e6"}`

	var cb Callback
	if err := json.Unmarshal([]byte(body), &cb); err != nil {
		t.Fatalf("decoding a callback with numeric fields: %v", err)
	}
	for _, tc := range []struct{ field, got, want string }{
		{"store_id", cb.StoreID, "11100"},
		{"transaction_id", cb.TransactionID, "257022"},
		{"invoice", cb.Invoice, "b649db3d-5185-4569-a15e-07da3659d11d"},
		{"amount", cb.Amount, "100000"},
		{"sign", cb.Sign, "e2c3f0e6"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.field, tc.got, tc.want)
		}
	}
}

// Strings still decode, because which of the two an account sends is the
// merchant cabinet's setting rather than ours to depend on.
func TestCallbackDecodesStringFields(t *testing.T) {
	body := `{"store_id":"11100","transaction_id":"257022","invoice":"pay-1",
		"amount":"100000","sign":"abc"}`

	var cb Callback
	if err := json.Unmarshal([]byte(body), &cb); err != nil {
		t.Fatalf("decoding a callback with string fields: %v", err)
	}
	if cb.TransactionID != "257022" || cb.Amount != "100000" {
		t.Errorf("decoded %+v", cb)
	}
}

// A numeric callback must verify against the same signature a string one does:
// the hash is over the digits, and a value that went through an int64 on the
// way in would be hashing something Atmos never wrote.
func TestVerifyCallbackFromNumericJSON(t *testing.T) {
	c := testClient()
	want := c.Signature(Callback{
		StoreID: "1234", TransactionID: "987654", Invoice: "pay-1", Amount: "5000000",
	})
	body := fmt.Sprintf(`{"store_id":1234,"transaction_id":987654,"invoice":"pay-1",
		"amount":5000000,"sign":%q}`, want)

	var cb Callback
	if err := json.Unmarshal([]byte(body), &cb); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !c.VerifyCallback(cb) {
		t.Error("a correctly signed numeric callback must verify")
	}
}

// The documented formula signs over `account`, which is the field Atmos
// actually posts; a callback carrying the merchant reference under that name
// must verify, and one that omits it entirely must not be signed as if the
// reference were empty.
func TestVerifyCallbackSignsOverAccount(t *testing.T) {
	c := &Client{cfg: Config{StoreID: "11100", APIKey: "secret"}}
	var cb Callback
	if err := json.Unmarshal([]byte(`{"store_id":"11100","transaction_id":"12769588","amount":"50000","account":"2"}`), &cb); err != nil {
		t.Fatal(err)
	}
	want := md5.Sum([]byte("11100" + "12769588" + "2" + "50000" + "secret"))
	cb.Sign = hex.EncodeToString(want[:])
	if !c.VerifyCallback(cb) {
		t.Fatalf("callback with account did not verify; got signature %s", c.Signature(cb))
	}
	// The older shape put the same value in `invoice`.
	old := Callback{StoreID: "11100", TransactionID: "12769588", Invoice: "2", Amount: "50000", Sign: cb.Sign}
	if !c.VerifyCallback(old) {
		t.Error("callback with invoice did not verify")
	}
}
