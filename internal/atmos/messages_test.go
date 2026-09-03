package atmos

import "testing"

// The message a payer actually sees for the commonest refusal there is.
//
// Two things are being asserted: that it is Uzbek rather than the Russian Atmos
// sends, and that it names the two things the payer can check. "Karta topilmadi"
// on its own reads as a verdict on the card; it is almost always a mistyped
// digit or expiry.
func TestCardNotFoundIsActionable(t *testing.T) {
	err := &APIError{Op: "pre-apply", Code: "STPIMS-ERR-004", Description: "Карта не найдена"}
	msg := err.Message()
	if msg == err.Description {
		t.Fatal("passed Atmos's Russian description straight through")
	}
	for _, want := range []string{"raqami", "muddat"} {
		if !contains(msg, want) {
			t.Errorf("message %q does not mention %q", msg, want)
		}
	}
}

// MPS reports a small integer status with a trace id appended. Those numbers
// are not STPIMS codes, and reading one as the other would tell a Visa payer
// to ring their bank about SMS notifications (STPIMS-ERR-005).
func TestMPSStatusCodesAreNotReadAsSTPIMS(t *testing.T) {
	for _, code := range []string{
		"5 trace=6602952bd44ca285f56c38255f2c1e15",
		"5",
		"0",
		"1 trace=004",
		"",
		"Approved",
	} {
		if got := payerMessage(code); got != "" {
			t.Errorf("payerMessage(%q) = %q, want no match", code, got)
		}
	}
	// And an MPS refusal keeps its own wording rather than borrowing one.
	err := &APIError{Op: "mps create", Code: "5 trace=abc004", Description: "Declined by issuer"}
	if err.Message() != "Declined by issuer" {
		t.Errorf("MPS message = %q, want the gateway's own text", err.Message())
	}
}

func TestNormalizeCode(t *testing.T) {
	cases := map[string]string{
		"STPIMS-ERR-004": "004",
		"STPIMS-ERR-4":   "004",
		"STPIMS-ERR-098": "098",
		"STPIMS-ERR-117": "117",
		"057":            "057",
		"OK":             "",
		"5":              "",
		"":               "",
	}
	for in, want := range cases {
		if got := normalizeCode(in); got != want {
			t.Errorf("normalizeCode(%q) = %q, want %q", in, got, want)
		}
	}
}

// A code nobody anticipated keeps Atmos's own description — better read out to
// support than flattened into "something went wrong".
func TestUnknownCodeKeepsTheGatewaysWording(t *testing.T) {
	err := &APIError{Op: "apply", Code: "STPIMS-ERR-145", Description: "Что-то новое"}
	if got := err.Message(); got != "Что-то новое" {
		t.Errorf("Message() = %q, want the description passed through", got)
	}
	// With nothing to pass through, something is still said.
	bare := &APIError{Op: "apply", Code: "STPIMS-ERR-145"}
	if bare.Message() == "" {
		t.Error("Message() was empty")
	}
}

// The codes that mean this server or the merchant account is misconfigured say
// so, rather than showing the payer a sentence about partners and terminals
// that reads as their own fault.
func TestOperatorFacingCodesSayItIsNotThePayersFault(t *testing.T) {
	for _, code := range []string{"STPIMS-ERR-003", "STPIMS-ERR-037", "STPIMS-ERR-044"} {
		err := &APIError{Op: "pre-apply", Code: code, Description: "Неправильный партнёр"}
		if got := err.Message(); got != operatorMessage {
			t.Errorf("Message() for %s = %q, want the operator message", code, got)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
