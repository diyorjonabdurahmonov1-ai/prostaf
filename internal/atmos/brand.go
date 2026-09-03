package atmos

import "strings"

// Which processing a card belongs to, decided from its first digits.
//
// # Why this has to be decided at all
//
// Atmos is two gateways behind one hostname, and they do not overlap. The
// merchant API (/merchant/pay/*) settles UzCard and Humo, and knows nothing
// else: hand it a Visa number and it answers "Karta topilmadi" — the card is
// not missing, it was never in that processing to begin with. Visa and
// Mastercard go through MPS (/mps/*) instead, which is a separate module with
// its own key, its own three calls, and a CVC2 the local cards have no use for.
//
// Sending a card down the wrong one of those is not a recoverable mistake the
// payer can fix by retrying, so the number decides the route before anything is
// sent anywhere.
//
// # Why the order of the checks is not arbitrary
//
// The local BINs are checked first because they sit *inside* the international
// ranges. UzCard issues 5440 and Humo issues 5614, both of which begin "54"
// and "56" — squarely within Mastercard's 51-55 and its co-badged neighbours.
// A generic "5 means Mastercard" test would therefore route a perfectly
// ordinary Humo card to the Visa/Mastercard acquirer, which would refuse it.
// Testing the four-digit local prefixes first is what keeps that from
// happening, and is why they are listed as prefixes rather than as ranges.
type Brand string

const (
	// BrandLocal is UzCard or Humo — the merchant API, confirmed by SMS code.
	BrandLocal Brand = "local"
	// BrandVisa and BrandMastercard both go through MPS, which wants a CVC2 and
	// finishes with a 3DS page rather than a code. They are told apart only so
	// the app can show the right logo and the right wording.
	BrandVisa       Brand = "visa"
	BrandMastercard Brand = "mastercard"
	// BrandUnknown is a number that belongs to neither. Refused up front: it
	// would be refused by whichever gateway it was guessed into anyway, and
	// saying so here costs the payer one round trip instead of two.
	BrandUnknown Brand = "unknown"
)

// UsesMPS reports whether this brand is settled through the MPS module rather
// than the merchant API — which is also exactly the set that needs a CVC2.
func (b Brand) UsesMPS() bool { return b == BrandVisa || b == BrandMastercard }

// localPrefixes are the UzCard and Humo BINs, longest-lived first. Kept as a
// list of prefixes rather than numeric ranges precisely because they must beat
// the international ranges they overlap — see the type comment.
var localPrefixes = []string{
	"8600", // UzCard
	"5440", // UzCard, inside Mastercard's 51-55
	"6262", // UzCard
	"6440", // UzCard
	"9860", // Humo
	"5614", // Humo, inside Mastercard's 56 co-badge range
}

// DetectBrand reads the processing off a card number.
//
// `pan` is expected to be digits only — pass it through the same stripping the
// handler does. A number too short to identify is BrandUnknown rather than a
// guess: half a Visa number and half a Humo number look identical for the first
// digit, and the payer is still typing.
func DetectBrand(pan string) Brand {
	pan = strings.TrimSpace(pan)
	if len(pan) < 6 {
		return BrandUnknown
	}
	for _, p := range localPrefixes {
		if strings.HasPrefix(pan, p) {
			return BrandLocal
		}
	}
	if pan[0] == '4' {
		return BrandVisa
	}
	// Mastercard's two ranges: the classic 51-55, and the 2221-2720 block it
	// added in 2017. Both are checked numerically because neither is a tidy
	// prefix — 2720 is in and 2721 is not.
	if n, ok := prefixInt(pan, 2); ok && n >= 51 && n <= 55 {
		return BrandMastercard
	}
	if n, ok := prefixInt(pan, 4); ok && n >= 2221 && n <= 2720 {
		return BrandMastercard
	}
	return BrandUnknown
}

// prefixInt reads the first n digits as a number, reporting false if they are
// not all digits.
func prefixInt(s string, n int) (int, bool) {
	if len(s) < n {
		return 0, false
	}
	out := 0
	for _, r := range s[:n] {
		if r < '0' || r > '9' {
			return 0, false
		}
		out = out*10 + int(r-'0')
	}
	return out, true
}

// ExpiryMMYY renders an expiry the way MPS wants it, which is the other way
// round from the merchant API.
//
// The merchant API takes YYmm and MPS takes MMYY, from the same four digits and
// with no way to tell a wrong one from a right one by looking — "0825" is
// August 2025 to one and May 2008 to the other. Both are therefore derived here
// from NormalizeExpiry's single reading of what the payer typed, rather than
// each parsing the field for itself.
func ExpiryMMYY(input string) (string, bool) {
	yymm, ok := NormalizeExpiry(input)
	if !ok {
		return "", false
	}
	return yymm[2:] + yymm[:2], true
}
