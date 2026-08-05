// Package auth implements a minimal HS256 JWT (sign + verify) using only the
// standard library.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// ErrInvalidToken is returned when a token fails structural, signature or
// expiry validation.
var ErrInvalidToken = errors.New("invalid token")

type header struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

type claims struct {
	Sub string `json:"sub"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
}

var b64 = base64.RawURLEncoding

// Sign creates a signed JWT for the given subject that expires after ttl.
func Sign(secret, subject string, ttl time.Duration) (string, error) {
	now := time.Now()
	h, err := json.Marshal(header{Alg: "HS256", Typ: "JWT"})
	if err != nil {
		return "", err
	}
	c, err := json.Marshal(claims{
		Sub: subject,
		Iat: now.Unix(),
		Exp: now.Add(ttl).Unix(),
	})
	if err != nil {
		return "", err
	}
	signingInput := b64.EncodeToString(h) + "." + b64.EncodeToString(c)
	sig := sign(secret, signingInput)
	return signingInput + "." + sig, nil
}

// Parse verifies the token signature and expiry and returns its subject.
func Parse(secret, token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", ErrInvalidToken
	}
	signingInput := parts[0] + "." + parts[1]
	expected := sign(secret, signingInput)
	// Constant-time comparison of the signatures.
	if !hmac.Equal([]byte(expected), []byte(parts[2])) {
		return "", ErrInvalidToken
	}
	payload, err := b64.DecodeString(parts[1])
	if err != nil {
		return "", ErrInvalidToken
	}
	var c claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return "", ErrInvalidToken
	}
	if c.Exp != 0 && time.Now().Unix() > c.Exp {
		return "", ErrInvalidToken
	}
	if c.Sub == "" {
		return "", ErrInvalidToken
	}
	return c.Sub, nil
}

func sign(secret, input string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(input))
	return b64.EncodeToString(mac.Sum(nil))
}
