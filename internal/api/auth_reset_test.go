package api

import (
	"testing"
	"time"

	"prostaff/internal/auth"
)

const testSecret = "test-secret"

func TestParseResetTokenRoundTrip(t *testing.T) {
	token, err := auth.Sign(testSecret, resetSubject("user-1"), resetTokenTTL)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	userID, ok := parseResetToken(testSecret, token)
	if !ok {
		t.Fatal("a freshly issued reset token should parse")
	}
	if userID != "user-1" {
		t.Fatalf("user id = %q, want user-1", userID)
	}
}

// A reset token authorises one thing. An ordinary access token must not be
// usable in its place, or a stolen session could silently change the password.
func TestParseResetTokenRejectsAccessToken(t *testing.T) {
	token, err := auth.Sign(testSecret, "user-1", accessTokenTTL)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, ok := parseResetToken(testSecret, token); ok {
		t.Fatal("an access token must not pass as a reset token")
	}
}

// The mirror of the case above: the authed middleware and the SSE stream both
// resolve a token's subject to a user row, and the prefix guarantees no lookup
// can ever succeed for a reset token.
func TestResetSubjectCannotMatchAUserID(t *testing.T) {
	sub := resetSubject("user-1")
	if sub == "user-1" {
		t.Fatal("the reset subject must differ from the bare user id")
	}
	if _, ok := parseResetToken(testSecret, "user-1"); ok {
		t.Fatal("a bare user id is not a token")
	}
}

func TestParseResetTokenRejectsExpiredWrongSecretAndGarbage(t *testing.T) {
	expired, err := auth.Sign(testSecret, resetSubject("user-1"), -time.Minute)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	valid, err := auth.Sign(testSecret, resetSubject("user-1"), resetTokenTTL)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	empty, err := auth.Sign(testSecret, resetSubjectPrefix, resetTokenTTL)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	cases := []struct {
		name   string
		secret string
		token  string
	}{
		{"expired", testSecret, expired},
		{"signed with another secret", "other-secret", valid},
		{"empty", testSecret, ""},
		{"garbage", testSecret, "not.a.token"},
		{"prefix with no user id", testSecret, empty},
	}
	for _, c := range cases {
		if _, ok := parseResetToken(c.secret, c.token); ok {
			t.Errorf("%s: expected the token to be rejected", c.name)
		}
	}
}
