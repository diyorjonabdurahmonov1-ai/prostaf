package store

import "testing"

// The document is the whole basis of the free trial: get its normalisation
// wrong and "AA 1234567" opens a second three-job trial next to "AA1234567".
func TestNormalizePassport(t *testing.T) {
	cases := map[string]string{
		"AA1234567":   "AA1234567",
		"aa1234567":   "AA1234567",
		"AA 1234567":  "AA1234567",
		"aa-1234 567": "AA1234567",
		"":            "",
	}
	for in, want := range cases {
		if got := NormalizePassport(in); got != want {
			t.Errorf("NormalizePassport(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidPassport(t *testing.T) {
	valid := []string{"AA1234567", "ZZ0000000"}
	for _, s := range valid {
		if !ValidPassport(s) {
			t.Errorf("ValidPassport(%q) = false, want true", s)
		}
	}
	invalid := []string{"", "AA123456", "AA12345678", "A11234567", "1A1234567", "AAA123456"}
	for _, s := range invalid {
		if ValidPassport(s) {
			t.Errorf("ValidPassport(%q) = true, want false", s)
		}
	}
}

func TestNormalizeAndValidateINN(t *testing.T) {
	if got := NormalizeINN("123 456 789"); got != "123456789" {
		t.Errorf("NormalizeINN = %q, want 123456789", got)
	}
	if !ValidINN("123456789") {
		t.Error("ValidINN(123456789) = false, want true")
	}
	for _, s := range []string{"", "12345678", "1234567890"} {
		if ValidINN(s) {
			t.Errorf("ValidINN(%q) = true, want false", s)
		}
	}
}

// A profile with no document on file must produce no ledger key at all —
// otherwise every such profile would share one "worker:" bucket, and the first
// of them to take three jobs would spend the trial for all the others.
func TestTrialKeysRequireADocument(t *testing.T) {
	if k := WorkerTrialKey(""); k != "" {
		t.Errorf("WorkerTrialKey(\"\") = %q, want empty", k)
	}
	if k := CompanyTrialKey(""); k != "" {
		t.Errorf("CompanyTrialKey(\"\") = %q, want empty", k)
	}
	if hasDocument("worker:") || hasDocument("company:") || hasDocument("") {
		t.Error("a bare prefix must not count as a document")
	}
	if !hasDocument(WorkerTrialKey("AA1234567")) {
		t.Error("a real passport key must count as a document")
	}
	// The two hats are counted apart: one person may hold both.
	if WorkerTrialKey("123456789") == CompanyTrialKey("123456789") {
		t.Error("worker and company trials must not share a key")
	}
}

func TestFormatSom(t *testing.T) {
	cases := map[int64]string{
		0:       "0",
		999:     "999",
		10000:   "10 000",
		300000:  "300 000",
		1234567: "1 234 567",
		-10000:  "-10 000",
	}
	for in, want := range cases {
		if got := formatSom(in); got != want {
			t.Errorf("formatSom(%d) = %q, want %q", in, got, want)
		}
	}
}
