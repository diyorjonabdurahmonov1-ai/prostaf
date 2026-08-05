package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"prostaff/internal/config"
)

func TestEskizAccessTokenParsesNestedLoginResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if got := r.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
			t.Errorf("expected form-encoded auth request, got %q", got)
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		if got := r.Form.Get("email"); got != "mail@example.com" {
			t.Errorf("email mismatch: %q", got)
		}
		if got := r.Form.Get("password"); got != "secret" {
			t.Errorf("password mismatch: %q", got)
		}

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"message":"token_generated","data":{"token":"new-eskiz-token"},"token_type":"bearer"}`)
	}))
	defer server.Close()

	s := &Server{cfg: config.Config{
		EskizBaseURL:  server.URL,
		EskizEmail:    "mail@example.com",
		EskizPassword: "secret",
	}}

	token, err := s.eskizAccessToken()
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if token != "new-eskiz-token" {
		t.Fatalf("expected nested token, got %q", token)
	}
}

// A burst of senders with no cached token must produce exactly one login, not
// one per caller — Eskiz rate-limits that endpoint.
func TestEskizAccessTokenLogsInOnceUnderConcurrency(t *testing.T) {
	var logins atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		logins.Add(1)
		fmt.Fprint(w, `{"data":{"token":"tok"}}`)
	}))
	defer server.Close()

	s := &Server{cfg: config.Config{
		EskizBaseURL:  server.URL,
		EskizEmail:    "mail@example.com",
		EskizPassword: "secret",
	}}

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.eskizAccessToken(); err != nil {
				t.Errorf("access token: %v", err)
			}
		}()
	}
	wg.Wait()

	if n := logins.Load(); n != 1 {
		t.Fatalf("expected exactly 1 login, got %d", n)
	}
}

// The OTP code has to reach Eskiz already substituted into the message: the
// send endpoint has no server-side template variables.
func TestSendOTPCodeRendersMessage(t *testing.T) {
	var got struct {
		phone, message, from, authz string
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/auth/login" {
			fmt.Fprint(w, `{"data":{"token":"tok"}}`)
			return
		}
		if r.URL.Path != "/api/message/sms/send" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		got.phone = r.Form.Get("mobile_phone")
		got.message = r.Form.Get("message")
		got.from = r.Form.Get("from")
		got.authz = r.Header.Get("Authorization")
		fmt.Fprint(w, `{"id":"123","status":"waiting","message":"Waiting for SMS provider"}`)
	}))
	defer server.Close()

	s := &Server{cfg: config.Config{
		EskizBaseURL:  server.URL,
		EskizEmail:    "mail@example.com",
		EskizPassword: "secret",
		EskizSender:   "4546",
	}}

	if err := s.sendOTPCode("+998901234567", "4821"); err != nil {
		t.Fatalf("sendOTPCode: %v", err)
	}
	if got.phone != "998901234567" {
		t.Errorf("phone = %q, want 998901234567", got.phone)
	}
	if strings.ContainsAny(got.message, "{}") {
		t.Errorf("message still has an unsubstituted placeholder: %q", got.message)
	}
	if !strings.Contains(got.message, "4821") {
		t.Errorf("message is missing the code: %q", got.message)
	}
	// Moderation compares the whole string, so the wording around the code has to
	// survive rendering untouched.
	want := strings.ReplaceAll(eskizTemplateOTP, "{kod}", "4821")
	if got.message != want {
		t.Errorf("message = %q, want %q", got.message, want)
	}
	if got.from != "4546" {
		t.Errorf("from = %q, want 4546", got.from)
	}
	if got.authz != "Bearer tok" {
		t.Errorf("authorization = %q, want %q", got.authz, "Bearer tok")
	}
}

// A rejected token must be swapped once and the message retried, not dropped.
func TestSendEskizSMSRetriesAfterExpiredToken(t *testing.T) {
	var sends atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/refresh":
			fmt.Fprint(w, `{"data":{"token":"fresh"}}`)
		case "/api/message/sms/send":
			if sends.Add(1) == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, `{"message":"Unauthenticated."}`)
				return
			}
			if got := r.Header.Get("Authorization"); got != "Bearer fresh" {
				t.Errorf("retry used %q, want the refreshed token", got)
			}
			fmt.Fprint(w, `{"id":"1","status":"waiting"}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	s := &Server{cfg: config.Config{
		EskizBaseURL: server.URL,
		EskizToken:   "stale",
		EskizSender:  "4546",
	}}

	if err := s.sendOTPCode("+998901234567", "1111"); err != nil {
		t.Fatalf("expected the retry to succeed, got %v", err)
	}
	if n := sends.Load(); n != 2 {
		t.Fatalf("expected 2 send attempts, got %d", n)
	}
	if s.cfg.EskizToken != "fresh" {
		t.Fatalf("cached token = %q, want the refreshed one", s.cfg.EskizToken)
	}
}

// Eskiz reports some rejections as a 200 carrying {"status":"error"}.
func TestSendEskizSMSFailsOnErrorStatusInBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"status":"error","message":"Nickname not found"}`)
	}))
	defer server.Close()

	s := &Server{cfg: config.Config{EskizBaseURL: server.URL, EskizToken: "tok"}}

	err := s.sendOTPCode("+998901234567", "1111")
	if err == nil {
		t.Fatal("expected an error for a rejected message, got nil")
	}
	if !strings.Contains(err.Error(), "Nickname not found") {
		t.Fatalf("error should carry the provider reason, got %v", err)
	}
}

// With no base URL configured sending is a no-op, which is how local
// development runs without a provider account.
func TestSendEskizSMSNoopWhenUnconfigured(t *testing.T) {
	s := &Server{cfg: config.Config{}}
	if err := s.sendOTPCode("+998901234567", "1111"); err != nil {
		t.Fatalf("expected a no-op, got %v", err)
	}
}

func TestNormalizeEskizPhone(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"+998 90 123-45-67", "998901234567", true},
		{"998901234567", "998901234567", true},
		{"901234567", "998901234567", true},
		{"", "", false},
		{"12345", "", false},
		{"+79161234567", "", false},
		{"9989012345678", "", false},
	}
	for _, c := range cases {
		got, ok := normalizeEskizPhone(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("normalizeEskizPhone(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestSendEskizSMSRejectsInvalidPhone(t *testing.T) {
	s := &Server{cfg: config.Config{EskizBaseURL: "http://example.invalid", EskizToken: "tok"}}
	if err := s.sendOTPCode("12345", "1111"); err == nil {
		t.Fatal("expected an invalid number to be rejected before the request")
	}
}
