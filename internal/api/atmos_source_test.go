package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func req(remote string, xff string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/payments/atmos/callback", nil)
	r.RemoteAddr = remote
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	return r
}

func TestCallbackSourceIP(t *testing.T) {
	cases := []struct {
		name   string
		remote string
		xff    string
		want   string
	}{
		{"no proxy", "92.63.207.79:51122", "", "92.63.207.79"},
		{"behind one proxy", "172.18.0.3:40000", "92.63.207.79", "92.63.207.79"},
		// The reason the last entry is the one read: a proxy appends what it
		// saw, so anything the caller invented is to its left. Taking the first
		// entry here would report the attacker's chosen address.
		{"spoofed prefix", "172.18.0.3:40000", "92.63.207.79, 203.0.113.7", "203.0.113.7"},
		{"spaces", "172.18.0.3:40000", "  92.63.207.79 ", "92.63.207.79"},
		{"garbage header falls back to peer", "92.63.207.79:5", "not-an-ip", "92.63.207.79"},
		{"remote without port", "92.63.207.79", "", "92.63.207.79"},
		{"ipv6 peer", "[2001:db8::1]:443", "", "2001:db8::1"},
		{"nothing usable", "garbage", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := callbackSourceIP(req(c.remote, c.xff)); got != c.want {
				t.Errorf("callbackSourceIP = %q, want %q", got, c.want)
			}
		})
	}
}

// An address that a caller put at the front of X-Forwarded-For must not open the
// allowlist. This is the whole point of reading the last entry, so it is worth
// asserting end to end rather than only through the helper.
func TestCallbackAllowlistIsNotSpoofable(t *testing.T) {
	s := &Server{atmosCallbackNets: parseCIDRs("92.63.207.0/24")}

	permitted := req("172.18.0.3:40000", "92.63.207.79")
	if !s.callbackAllowed(callbackSourceIP(permitted)) {
		t.Error("a genuine Atmos address behind a proxy must be allowed")
	}

	spoofed := req("172.18.0.3:40000", "92.63.207.79, 203.0.113.7")
	if s.callbackAllowed(callbackSourceIP(spoofed)) {
		t.Error("an address forged at the head of X-Forwarded-For must not be allowed")
	}
}

func TestCallbackAllowed(t *testing.T) {
	// Unconfigured means "no address check": the signature is the real gate, and
	// an allowlist nobody set must not quietly refuse the gateway.
	open := &Server{atmosCallbackNets: parseCIDRs("")}
	for _, ip := range []string{"203.0.113.7", "92.63.207.79", ""} {
		if !open.callbackAllowed(ip) {
			t.Errorf("with no allowlist configured, %q must be allowed", ip)
		}
	}

	s := &Server{atmosCallbackNets: parseCIDRs("92.63.207.0/24, 10.0.0.5")}
	allowed := []string{"92.63.207.1", "92.63.207.79", "92.63.207.255", "10.0.0.5"}
	for _, ip := range allowed {
		if !s.callbackAllowed(ip) {
			t.Errorf("%q must be allowed", ip)
		}
	}
	refused := []string{"92.63.208.1", "10.0.0.6", "203.0.113.7", "", "not-an-ip"}
	for _, ip := range refused {
		if s.callbackAllowed(ip) {
			t.Errorf("%q must be refused", ip)
		}
	}
}

// A list that is entirely unparseable must not silently become an allowlist of
// nothing, which would refuse every callback including the real ones.
func TestParseCIDRsIgnoresJunk(t *testing.T) {
	if got := parseCIDRs("nonsense, , 999.999.999.999/24"); len(got) != 0 {
		t.Errorf("parseCIDRs kept %d unusable entries", len(got))
	}
	if got := parseCIDRs("92.63.207.0/24"); len(got) != 1 {
		t.Fatalf("parseCIDRs = %d networks, want 1", len(got))
	}
}
