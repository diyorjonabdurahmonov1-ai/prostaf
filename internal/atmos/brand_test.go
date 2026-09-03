package atmos

import "testing"

// The case this whole file exists for: the local BINs sit inside the
// international ranges, so a generic "5 means Mastercard" test would route
// ordinary UzCard and Humo cards to the Visa acquirer, which refuses them.
//
// 5614688715378807 is not a hypothetical — it is Atmos's own Humo sandbox card,
// and it is the one that was confirmed working against the merchant API. If
// this test ever goes red, real Humo cards are being sent to the wrong gateway.
func TestDetectBrandLocalBeatsInternationalRanges(t *testing.T) {
	for _, pan := range []string{
		"8600492986215602", // UzCard
		"5440000000000000", // UzCard, inside Mastercard's 51-55
		"6262000000000000", // UzCard
		"6440000000000000", // UzCard
		"9860090101014364", // Humo
		"5614688715378807", // Humo, the sandbox card that works
	} {
		if got := DetectBrand(pan); got != BrandLocal {
			t.Errorf("DetectBrand(%s) = %s, want %s", pan, got, BrandLocal)
		}
		if DetectBrand(pan).UsesMPS() {
			t.Errorf("DetectBrand(%s) routed a local card to MPS", pan)
		}
	}
}

func TestDetectBrandInternational(t *testing.T) {
	cases := map[string]Brand{
		"4278310021875430": BrandVisa,       // Atmos's own MPS example
		"4111111111111111": BrandVisa,
		"5100000000000000": BrandMastercard, // bottom of 51-55
		"5599999999999999": BrandMastercard, // top of 51-55
		"2221000000000000": BrandMastercard, // bottom of the 2017 block
		"2720999999999999": BrandMastercard, // top of it
	}
	for pan, want := range cases {
		if got := DetectBrand(pan); got != want {
			t.Errorf("DetectBrand(%s) = %s, want %s", pan, got, want)
		}
		if !DetectBrand(pan).UsesMPS() {
			t.Errorf("DetectBrand(%s) did not route an international card to MPS", pan)
		}
	}
}

// Just outside Mastercard's ranges, and short input. Unknown rather than a
// guess: half a number identifies nothing, and the payer is still typing.
func TestDetectBrandUnknown(t *testing.T) {
	for _, pan := range []string{
		"2220000000000000", // one below the 2221 block
		"2721000000000000", // one above it
		"3782822463100",    // Amex — neither gateway takes it
		"8600",             // too short to identify
		"",
	} {
		if got := DetectBrand(pan); got != BrandUnknown {
			t.Errorf("DetectBrand(%q) = %s, want %s", pan, got, BrandUnknown)
		}
	}
}

// The two gateways read the same four digits in opposite orders, and neither
// rejects the other's reading — an expiry sent the wrong way round is a payment
// refused for a card that was perfectly good.
func TestExpiryMMYYIsTheReverseOfNormalizeExpiry(t *testing.T) {
	cases := map[string]struct{ yymm, mmyy string }{
		"03/28":   {"2803", "0328"},
		"0328":    {"2803", "0328"},
		"03/2028": {"2803", "0328"},
		"08/25":   {"2508", "0825"},
	}
	for in, want := range cases {
		yymm, ok := NormalizeExpiry(in)
		if !ok || yymm != want.yymm {
			t.Errorf("NormalizeExpiry(%s) = %q, %v; want %q", in, yymm, ok, want.yymm)
		}
		mmyy, ok := ExpiryMMYY(in)
		if !ok || mmyy != want.mmyy {
			t.Errorf("ExpiryMMYY(%s) = %q, %v; want %q", in, mmyy, ok, want.mmyy)
		}
	}
	if _, ok := ExpiryMMYY("nonsense"); ok {
		t.Error("ExpiryMMYY accepted a non-expiry")
	}
}

// MPS stays off unless its own key is set, even on an account whose merchant
// API is fully configured. The failure mode of a missing MPS key must be "Visa
// is politely refused", never "Visa is attempted against an endpoint that will
// 401".
func TestMPSEnabledNeedsItsOwnKey(t *testing.T) {
	base := Config{
		BaseURL: "https://apigw.atmos.uz", ConsumerKey: "k", ConsumerSecret: "s",
		StoreID: "11100", APIKey: "callback-key",
	}
	if c := New(base); c.MPSEnabled() {
		t.Error("MPS reported enabled with no MPS key")
	}
	withMPS := base
	withMPS.MPSAPIKey = "mps-key"
	if c := New(withMPS); !c.MPSEnabled() {
		t.Error("MPS reported disabled with an MPS key set")
	}
	// And an MPS key on an otherwise unconfigured account buys nothing: there
	// is no token to authenticate the call with.
	if c := New(Config{MPSAPIKey: "mps-key"}); c.MPSEnabled() {
		t.Error("MPS reported enabled on an account with no merchant credentials")
	}
}

// The MPS store id falls back to the merchant one, which is how Atmos sets most
// accounts up — a wrong store id here is a refusal with no useful message.
func TestMPSStoreIDFallsBackToStoreID(t *testing.T) {
	c := New(Config{StoreID: "11100", MPSAPIKey: "k"})
	if got := c.mpsStoreID(); got != int64(11100) {
		t.Errorf("mpsStoreID() = %v, want 11100", got)
	}
	c = New(Config{StoreID: "11100", MPSStoreID: "3", MPSAPIKey: "k"})
	if got := c.mpsStoreID(); got != int64(3) {
		t.Errorf("mpsStoreID() = %v, want 3", got)
	}
}
