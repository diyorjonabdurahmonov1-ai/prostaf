package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"prostaff/internal/auth"
	"prostaff/internal/models"
	"prostaff/internal/store"
)

const (
	accessTokenTTL  = 15 * time.Minute
	refreshTokenTTL = 30 * 24 * time.Hour
	// resetTokenTTL is how long a proven phone stays good for choosing a new
	// password — long enough to type one, short enough that a stolen handset
	// after the fact is useless.
	resetTokenTTL = 10 * time.Minute

	// resetSubjectPrefix marks a JWT as "may set a password, nothing else".
	// Every other consumer of a token — the authed middleware and the SSE
	// stream — resolves the subject to a user row, and no user id carries this
	// prefix, so a reset token is inert as a Bearer credential.
	resetSubjectPrefix = "pwreset:"

	// minPasswordLen mirrors the rule the mobile sign-up screen enforces.
	minPasswordLen = 6
	// maxPasswordLen is bcrypt's hard limit — it silently ignores anything past
	// 72 bytes, so reject instead of hashing a truncated password.
	maxPasswordLen = 72
)

// otpResendWindow is how long a phone must wait between OTP messages. Every
// SMS costs money, so an unthrottled endpoint is a way to spend the platform's
// balance. Overridden in tests.
var otpResendWindow = 60 * time.Second

// Cooldown scopes. Signing in and resetting a password are throttled
// independently, so someone who has just signed up isn't locked out of the
// reset flow for a minute by their own sign-up code.
const (
	scopeSignIn = "signin"
	scopeReset  = "reset"
)

// smsCooldown throttles the endpoints that send an SMS on demand.
var smsCooldown = &phoneCooldown{last: map[string]time.Time{}}

// phoneCooldown records the last send per scope and phone number.
type phoneCooldown struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// hit reserves a send slot for phone within scope. It returns the seconds left
// to wait when the caller is still inside the cooldown window.
func (c *phoneCooldown) hit(scope, phone string) (int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := scope + ":" + phone
	now := time.Now()
	if t, ok := c.last[key]; ok {
		if left := otpResendWindow - now.Sub(t); left > 0 {
			return int(left.Seconds()) + 1, false
		}
	}
	// Drop entries that have cooled down, so the map only ever holds the phones
	// currently inside a window.
	for k, t := range c.last {
		if now.Sub(t) > otpResendWindow {
			delete(c.last, k)
		}
	}
	c.last[key] = now
	return 0, true
}

// lockedOut answers a caller who has run out of attempts and reports whether it
// did, so a handler can start with `if lockedOut(...) { return }`.
//
// The message says how long the wait is rather than only that there is one: a
// locked-out person is nearly always somebody who mistyped their own password
// four times, and "try again later" is what makes them phone the office.
func lockedOut(w http.ResponseWriter, l *attemptLimiter, key string) bool {
	left, locked := l.locked(key)
	if !locked {
		return false
	}
	writeError(w, http.StatusTooManyRequests, "too_many_attempts",
		fmt.Sprintf("too many failed attempts — try again in %d minutes", int(left.Minutes())+1))
	return true
}

// ---- Handlers ---------------------------------------------------------------

// POST /auth/otp/request — body { phone } → { sent, dev_code? }
func (s *Server) handleOTPRequest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone string `json:"phone"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	phone := store.NormalizePhone(body.Phone)
	if !validPhone(phone) {
		writeError(w, http.StatusBadRequest, "bad_request", "a valid phone number is required")
		return
	}
	// This code only ever opens an account, so a number that already has one is
	// turned away here — before an SMS is paid for, and before the caller can
	// reach /auth/otp/verify and overwrite the existing name and password.
	// Someone who has forgotten their password goes through /auth/forgot-password.
	if _, taken := s.store.GetUserByPhone(phone); taken {
		writeError(w, http.StatusConflict, "phone_taken",
			"this phone already has an account — sign in or reset your password")
		return
	}
	if wait, ok := smsCooldown.hit(scopeSignIn, phone); !ok {
		writeError(w, http.StatusTooManyRequests, "too_many_requests",
			fmt.Sprintf("please wait %d seconds before requesting a new code", wait))
		return
	}

	code := otpCode()
	s.store.SetOTP(phone, code)

	resp := map[string]any{"sent": true, "expires_in": int(store.OTPTTL.Seconds())}
	if err := s.sendOTPCode(phone, code); err != nil {
		log.Printf("otp: sending to %s failed: %v", phone, err)
		// In dev the code comes back in the response, so a missing or broken SMS
		// provider must not block sign-in. In production it has to be an error —
		// otherwise the client waits for a message that will never arrive.
		if !s.devAllowed(r) {
			writeError(w, http.StatusBadGateway, "sms_failed", "could not send the verification code")
			return
		}
	}
	if s.devAllowed(r) {
		resp["dev_code"] = code
	}
	writeJSON(w, http.StatusOK, resp)
}

// POST /auth/otp/verify — body { phone, code, name?, password? } → AuthResponse.
//
// This is also the last step of sign-up: the mobile app collects the name and
// password first, then sends them here once the phone is proven.
func (s *Server) handleOTPVerify(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone    string `json:"phone"`
		Code     string `json:"code"`
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	phone := store.NormalizePhone(body.Phone)
	code := strings.TrimSpace(body.Code)
	if !validPhone(phone) || code == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "phone and code are required")
		return
	}
	if body.Password != "" && !validPassword(w, body.Password) {
		return
	}
	// Wrong codes are counted. A six-digit code that lives two minutes is a
	// small enough space that an unlimited guesser gets in eventually, and
	// nothing here was counting — see ratelimit.go.
	if lockedOut(w, otpAttempts, phone) {
		return
	}
	// A password here means "finish signing up". Refuse before the code is spent
	// if the number was claimed in the meantime, so an existing account can never
	// have its name and password replaced through this route.
	if body.Password != "" {
		if _, taken := s.store.GetUserByPhone(phone); taken {
			writeError(w, http.StatusConflict, "phone_taken",
				"this phone already has an account — sign in or reset your password")
			return
		}
	}
	if !s.store.VerifyOTP(phone, code) {
		otpAttempts.fail(phone)
		writeError(w, http.StatusUnauthorized, "invalid_code", "the code is wrong, already used, or has expired")
		return
	}
	otpAttempts.pass(phone)

	u, _ := s.store.GetOrCreateUserByPhone(phone)
	if u == nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not create the account")
		return
	}
	if !u.Active {
		writeError(w, http.StatusForbidden, "forbidden", "this account is blocked")
		return
	}

	if name := strings.TrimSpace(body.Name); name != "" {
		s.store.SetUserName(u.ID, name)
	}
	// Proving the phone is enough to set a password — it is the same authority
	// the reset flow relies on.
	if body.Password != "" {
		hash, err := hashPassword(body.Password)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "could not set the password")
			return
		}
		s.store.SetPasswordHash(u.ID, hash)
	}

	s.issueSession(w, u.ID)
}

// POST /auth/login — body { phone, password } → AuthResponse.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone    string `json:"phone"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	phone := store.NormalizePhone(body.Phone)
	// Counted per number, and the count is what makes bcrypt's cost a defence
	// rather than a liability: every guess is deliberately expensive to check,
	// so an unlimited guesser is also a way to spend the server's CPU.
	if lockedOut(w, loginAttempts, phone) {
		return
	}
	u, ok := s.store.GetUserByPhone(phone)
	// One message for "no such phone" and "wrong password" alike, so the endpoint
	// can't be used to find out which numbers are registered.
	if !ok || !passwordMatches(s.store.GetPasswordHash(u.ID), body.Password) {
		loginAttempts.fail(phone)
		writeError(w, http.StatusUnauthorized, "unauthorized", "phone or password is incorrect")
		return
	}
	loginAttempts.pass(phone)
	if !u.Active {
		writeError(w, http.StatusForbidden, "forbidden", "this account is blocked")
		return
	}
	s.issueSession(w, u.ID)
}

// POST /auth/admin/login — body { email, password } → AuthResponse.
func (s *Server) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	email := strings.TrimSpace(body.Email)
	// The admin account is the one worth guessing, and its address is not a
	// secret. Same counter as the phone login, keyed by email.
	if lockedOut(w, loginAttempts, email) {
		return
	}
	u, ok := s.store.GetUserByEmail(email)
	if !ok || !u.HasRole(models.RoleAdmin) {
		loginAttempts.fail(email)
		writeError(w, http.StatusUnauthorized, "unauthorized", "email or password is incorrect")
		return
	}

	// The seeded admin has no password hash yet — it falls back to the
	// ADMIN_PASSWORD env value, and the hash takes over once one is set.
	hash := s.store.GetPasswordHash(u.ID)
	authorized := passwordMatches(hash, body.Password)
	if hash == "" {
		authorized = email == s.cfg.AdminEmail && body.Password != "" &&
			subtle.ConstantTimeCompare([]byte(body.Password), []byte(s.cfg.AdminPassword)) == 1
	}
	if !authorized {
		loginAttempts.fail(email)
		writeError(w, http.StatusUnauthorized, "unauthorized", "email or password is incorrect")
		return
	}
	loginAttempts.pass(email)
	if !u.Active {
		writeError(w, http.StatusForbidden, "forbidden", "this account is blocked")
		return
	}
	s.issueSession(w, u.ID)
}

// POST /auth/forgot-password — body { phone } → { sent, dev_code? }.
//
// Step 1 of 3. Sends the same kind of code sign-in uses; the caller proves the
// phone at /auth/forgot-password/verify and chooses the new password at
// /auth/reset-password. A password is never generated for the user or carried
// over SMS.
//
// The reply is identical whether or not the number is registered, so the
// endpoint can't be used to find out which phones have accounts.
func (s *Server) handleForgotPassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone string `json:"phone"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	phone := store.NormalizePhone(body.Phone)
	if !validPhone(phone) {
		writeError(w, http.StatusBadRequest, "bad_request", "a valid phone number is required")
		return
	}

	resp := map[string]any{"sent": true, "expires_in": int(store.OTPTTL.Seconds())}
	u, ok := s.store.GetUserByPhone(phone)
	if !ok || !u.Active {
		// Nothing is sent and nothing is said — an unregistered number must look
		// exactly like a registered one from here.
		writeJSON(w, http.StatusOK, resp)
		return
	}
	if wait, ok := smsCooldown.hit(scopeReset, phone); !ok {
		writeError(w, http.StatusTooManyRequests, "too_many_requests",
			fmt.Sprintf("please wait %d seconds before trying again", wait))
		return
	}

	code := otpCode()
	s.store.SetOTP(phone, code)

	if err := s.sendOTPCode(phone, code); err != nil {
		log.Printf("forgot-password: sending to %s failed: %v", phone, err)
		if !s.devAllowed(r) {
			writeError(w, http.StatusBadGateway, "sms_failed", "could not send the verification code")
			return
		}
	}
	if s.devAllowed(r) {
		resp["dev_code"] = code
	}
	writeJSON(w, http.StatusOK, resp)
}

// POST /auth/forgot-password/verify — body { phone, code } → { reset_token, expires_in }.
//
// Step 2 of 3. Spends the code and hands back a token that authorises exactly
// one thing: choosing a new password. Keeping this separate from
// /auth/otp/verify is what lets the caller confirm the code first and pick the
// password afterwards, without being signed in half-way through.
func (s *Server) handleForgotPasswordVerify(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone string `json:"phone"`
		Code  string `json:"code"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	phone := store.NormalizePhone(body.Phone)
	code := strings.TrimSpace(body.Code)
	if !validPhone(phone) || code == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "phone and code are required")
		return
	}

	// Counted like every other code: this one is worth more, because what it
	// buys is a token that sets a password on somebody else's account.
	if lockedOut(w, otpAttempts, phone) {
		return
	}
	u, ok := s.store.GetUserByPhone(phone)
	if !ok || !u.Active || !s.store.VerifyOTP(phone, code) {
		otpAttempts.fail(phone)
		writeError(w, http.StatusUnauthorized, "invalid_code", "the code is wrong, already used, or has expired")
		return
	}
	otpAttempts.pass(phone)

	token, err := auth.Sign(s.cfg.JWTSecret, resetSubject(u.ID), resetTokenTTL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not issue a reset token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"reset_token": token,
		"expires_in":  int(resetTokenTTL.Seconds()),
	})
}

// POST /auth/reset-password — body { reset_token, password } → { ok: true }.
//
// Step 3 of 3.
func (s *Server) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ResetToken string `json:"reset_token"`
		Password   string `json:"password"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	userID, ok := parseResetToken(s.cfg.JWTSecret, body.ResetToken)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "the reset link has expired — request a new code")
		return
	}
	if !validPassword(w, body.Password) {
		return
	}
	u, found := s.store.GetUser(userID)
	if !found || !u.Active {
		writeError(w, http.StatusUnauthorized, "unauthorized", "unknown or inactive user")
		return
	}

	hash, err := hashPassword(body.Password)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not set the password")
		return
	}
	s.store.SetPasswordHash(u.ID, hash)
	// Every session that predates the reset was built on the old password.
	s.store.RevokeAllRefreshTokens(u.ID)

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// POST /auth/refresh — body { refresh_token } → { token, refresh_token, expires_in }.
//
// The refresh token is rotated: the presented one is revoked and a fresh pair
// is issued, so a leaked token is usable at most once.
func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RefreshToken string `json:"refresh_token"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	token := strings.TrimSpace(body.RefreshToken)
	userID, ok := s.store.ValidateRefreshToken(token)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "invalid refresh token")
		return
	}
	u, ok := s.store.GetUser(userID)
	if !ok || !u.Active {
		s.store.RevokeRefreshToken(token)
		writeError(w, http.StatusUnauthorized, "unauthorized", "unknown or inactive user")
		return
	}

	access, refresh, err := s.newSession(userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not issue a token")
		return
	}
	s.store.RevokeRefreshToken(token)

	writeJSON(w, http.StatusOK, map[string]any{
		"token":         access,
		"refresh_token": refresh,
		"expires_in":    int(accessTokenTTL.Seconds()),
	})
}

// POST /auth/logout — body { refresh_token } → { ok: true }.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RefreshToken string `json:"refresh_token"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if token := strings.TrimSpace(body.RefreshToken); token != "" {
		s.store.RevokeRefreshToken(token)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- Session ----------------------------------------------------------------

// newSession mints an access token and a stored refresh token for a user.
func (s *Server) newSession(userID string) (access, refresh string, err error) {
	access, err = auth.Sign(s.cfg.JWTSecret, userID, accessTokenTTL)
	if err != nil {
		return "", "", err
	}
	refresh, err = generateRefreshToken()
	if err != nil {
		return "", "", err
	}
	s.store.StoreRefreshToken(refresh, userID)
	return access, refresh, nil
}

// issueSession writes the AuthResponse every successful sign-in returns.
func (s *Server) issueSession(w http.ResponseWriter, userID string) {
	access, refresh, err := s.newSession(userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not issue a token")
		return
	}
	view, ok := s.store.UserView(userID)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal", "could not load the account")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token":         access,
		"refresh_token": refresh,
		"expires_in":    int(accessTokenTTL.Seconds()),
		"user":          view,
	})
}

// ---- Credentials ------------------------------------------------------------

// generateRefreshToken creates a cryptographically random opaque token.
func generateRefreshToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// otpCode returns a random 4-digit numeric code.
func otpCode() string {
	n, err := rand.Int(rand.Reader, big.NewInt(10000))
	if err != nil {
		return "0000"
	}
	return fmt.Sprintf("%04d", n.Int64())
}

// resetSubject builds the JWT subject a password-reset token carries.
func resetSubject(userID string) string { return resetSubjectPrefix + userID }

// parseResetToken validates a reset token and returns the user it belongs to.
// A plain access token is rejected here, just as a reset token is rejected
// everywhere an access token is expected.
func parseResetToken(secret, token string) (string, bool) {
	sub, err := auth.Parse(secret, strings.TrimSpace(token))
	if err != nil {
		return "", false
	}
	userID, ok := strings.CutPrefix(sub, resetSubjectPrefix)
	if !ok || userID == "" {
		return "", false
	}
	return userID, true
}

func hashPassword(password string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(b), err
}

func passwordMatches(hash, password string) bool {
	if hash == "" || password == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// validPhone accepts only a canonical Uzbek mobile number (+998XXXXXXXXX), the
// shape store.NormalizePhone produces for a well-formed input.
func validPhone(phone string) bool {
	if len(phone) != 13 || !strings.HasPrefix(phone, "+998") {
		return false
	}
	for _, r := range phone[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// validPassword writes a 400 and returns false when the password is unusable.
func validPassword(w http.ResponseWriter, password string) bool {
	switch {
	case len(password) < minPasswordLen:
		writeError(w, http.StatusBadRequest, "bad_request",
			fmt.Sprintf("password must be at least %d characters", minPasswordLen))
		return false
	case len(password) > maxPasswordLen:
		writeError(w, http.StatusBadRequest, "bad_request",
			fmt.Sprintf("password must be at most %d characters", maxPasswordLen))
		return false
	}
	return true
}
