package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// eskizTemplateOTP is the only text this backend sends. Sign-in, sign-up and
// password reset all go through the same code, so one approved template covers
// everything.
//
// This string must stay character-for-character identical to the template
// approved in the my.eskiz.uz dashboard — Eskiz matches every outgoing message
// against the approved list and rejects anything that differs, down to a stray
// space. Change it here only after the new wording has cleared moderation.
const eskizTemplateOTP = "PROSTAFF ilovasiga kirish tasdiqlash kodingiz: {kod}. Kodni hech kimga bermang."

// eskizTokenMu serialises every read and write of the cached bearer token on
// s.cfg.EskizToken, and is held across the login/refresh round-trip on purpose:
// that collapses a burst of concurrent senders into one login instead of one
// login per in-flight request, which Eskiz rate-limits.
var eskizTokenMu sync.Mutex

// eskizHTTPClient is used for all Eskiz calls so a slow or unreachable Eskiz
// endpoint can't hang a request goroutine forever.
var eskizHTTPClient = &http.Client{Timeout: 15 * time.Second}

// eskizOffOnce keeps the "SMS is switched off" notice to a single log line.
var eskizOffOnce sync.Once

func (s *Server) sendOTPCode(phone, code string) error {
	return s.sendEskizSMS(phone, eskizTemplateOTP, map[string]string{"kod": code})
}

// renderTemplate substitutes {name} placeholders with their values, matching the
// placeholder syntax the approved dashboard template uses.
//
// Eskiz has no server-side template variables on /message/sms/send — it sends
// the `message` field verbatim — so the text has to leave here fully rendered.
// The braces mark the one slot moderation allows to vary; everything around them
// is compared literally.
func renderTemplate(tpl string, vars map[string]string) string {
	if len(vars) == 0 {
		return tpl
	}
	pairs := make([]string, 0, len(vars)*2)
	for k, v := range vars {
		pairs = append(pairs, "{"+k+"}", v)
	}
	return strings.NewReplacer(pairs...).Replace(tpl)
}

// sendEskizSMS renders and sends a single SMS. It uses the cached bearer token,
// and if that token has expired (401/403) it re-authenticates once and retries
// exactly once more.
//
// With ESKIZ_BASE_URL unset it is a no-op, which is how local development runs
// without a provider account.
func (s *Server) sendEskizSMS(phone, template string, vars map[string]string) error {
	baseURL := strings.TrimRight(strings.TrimSpace(s.cfg.EskizBaseURL), "/")
	if baseURL == "" {
		eskizOffOnce.Do(func() {
			log.Printf("eskiz: ESKIZ_BASE_URL is empty — SMS delivery is disabled")
		})
		return nil
	}

	to, ok := normalizeEskizPhone(phone)
	if !ok {
		return fmt.Errorf("eskiz: %q is not a valid Uzbek mobile number", phone)
	}
	message := renderTemplate(template, vars)

	token, err := s.eskizAccessToken()
	if err != nil {
		return fmt.Errorf("eskiz: get access token: %w", err)
	}

	status, body, err := s.eskizPostSMS(baseURL, token, to, message)
	if err != nil {
		return fmt.Errorf("eskiz: sms request: %w", err)
	}

	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		newToken, authErr := s.eskizReauthenticate(token)
		if authErr != nil {
			return fmt.Errorf("eskiz: reauthenticate: %w", authErr)
		}
		status, body, err = s.eskizPostSMS(baseURL, newToken, to, message)
		if err != nil {
			return fmt.Errorf("eskiz: sms request after reauth: %w", err)
		}
	}
	return eskizSendResult(status, body)
}

// eskizPostSMS performs one POST /api/message/sms/send and returns the status
// and raw body. Form encoding matches what Eskiz documents for this endpoint.
func (s *Server) eskizPostSMS(baseURL, token, phone, message string) (int, []byte, error) {
	form := url.Values{}
	form.Set("mobile_phone", phone)
	form.Set("message", message)
	if from := strings.TrimSpace(s.cfg.EskizSender); from != "" {
		form.Set("from", from)
	}

	req, err := http.NewRequest(http.MethodPost, baseURL+"/api/message/sms/send", strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	res, err := eskizHTTPClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return res.StatusCode, nil, fmt.Errorf("read response: %w", err)
	}
	return res.StatusCode, body, nil
}

// eskizSendResult turns a send reply into an error or nil. A 2xx alone is not
// proof of delivery: Eskiz reports some rejections as {"status":"error"} inside
// an otherwise successful response.
func eskizSendResult(status int, body []byte) error {
	var reply struct {
		ID      any    `json:"id"`
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &reply)
	detail := strings.TrimSpace(reply.Message)
	if detail == "" {
		detail = strings.TrimSpace(string(body))
	}

	if status < 200 || status >= 300 {
		return fmt.Errorf("eskiz: status %d: %s", status, detail)
	}
	if strings.EqualFold(reply.Status, "error") {
		return fmt.Errorf("eskiz: rejected: %s", detail)
	}
	log.Printf("eskiz: sms queued id=%v status=%s", reply.ID, reply.Status)
	return nil
}

// eskizAccessToken returns the cached bearer token, logging in when there is
// none. Callers that hit an expired token go through eskizReauthenticate.
func (s *Server) eskizAccessToken() (string, error) {
	eskizTokenMu.Lock()
	defer eskizTokenMu.Unlock()

	if cached := strings.TrimSpace(s.cfg.EskizToken); cached != "" {
		return cached, nil
	}
	return s.eskizLoginLocked()
}

// eskizReauthenticate replaces a token the API has just rejected. It tries a
// lightweight refresh first and falls back to a full login when the old token
// is too stale to refresh.
func (s *Server) eskizReauthenticate(rejected string) (string, error) {
	eskizTokenMu.Lock()
	defer eskizTokenMu.Unlock()

	// Another request may have replaced the token while we waited for the lock;
	// reuse theirs rather than logging in a second time.
	if cur := strings.TrimSpace(s.cfg.EskizToken); cur != "" && cur != rejected {
		return cur, nil
	}
	if token, err := s.eskizRefreshTokenLocked(rejected); err == nil && token != "" {
		return token, nil
	}
	return s.eskizLoginLocked()
}

// eskizLoginLocked authenticates with ESKIZ_EMAIL/ESKIZ_PASSWORD and caches the
// returned token. The caller must hold eskizTokenMu.
func (s *Server) eskizLoginLocked() (string, error) {
	email := strings.TrimSpace(s.cfg.EskizEmail)
	password := strings.TrimSpace(s.cfg.EskizPassword)
	if email == "" || password == "" {
		return "", fmt.Errorf("ESKIZ_EMAIL/ESKIZ_PASSWORD not configured")
	}

	baseURL := strings.TrimRight(strings.TrimSpace(s.cfg.EskizBaseURL), "/")
	form := url.Values{}
	form.Set("email", email)
	form.Set("password", password)

	req, err := http.NewRequest(http.MethodPost, baseURL+"/api/auth/login", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	token, err := eskizTokenCall(req, "login")
	if err != nil {
		return "", err
	}
	s.cfg.EskizToken = token
	log.Printf("eskiz: logged in as %s, token cached", email)
	return token, nil
}

// eskizRefreshTokenLocked exchanges an existing token for a fresh one without a
// full re-login. The caller must hold eskizTokenMu.
func (s *Server) eskizRefreshTokenLocked(oldToken string) (string, error) {
	if strings.TrimSpace(oldToken) == "" {
		return "", fmt.Errorf("eskiz refresh: no token to refresh")
	}

	baseURL := strings.TrimRight(strings.TrimSpace(s.cfg.EskizBaseURL), "/")
	req, err := http.NewRequest(http.MethodPatch, baseURL+"/api/auth/refresh", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+oldToken)

	token, err := eskizTokenCall(req, "refresh")
	if err != nil {
		return "", err
	}
	s.cfg.EskizToken = token
	return token, nil
}

// eskizTokenCall runs a login or refresh request and pulls the bearer token out
// of the reply. Eskiz nests it under `data.token`; a flat `token` is accepted
// too because their older responses use that shape.
func eskizTokenCall(req *http.Request, op string) (string, error) {
	res, err := eskizHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("eskiz %s request: %w", op, err)
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return "", fmt.Errorf("eskiz %s read body: %w", op, err)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return "", fmt.Errorf("eskiz %s status %d: %s", op, res.StatusCode, strings.TrimSpace(string(body)))
	}

	var data struct {
		Message string `json:"message"`
		Data    struct {
			Token string `json:"token"`
		} `json:"data"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return "", fmt.Errorf("eskiz %s decode: %w", op, err)
	}

	token := strings.TrimSpace(data.Data.Token)
	if token == "" {
		token = strings.TrimSpace(data.Token)
	}
	if token == "" {
		return "", fmt.Errorf("eskiz %s response missing token", op)
	}
	return token, nil
}

// normalizeEskizPhone converts a stored number to the bare 998XXXXXXXXX form
// Eskiz expects, e.g. "+998 90 123-45-67" and "901234567" both become
// "998901234567". Anything that is not an Uzbek mobile number is rejected
// rather than passed on, so a bad number surfaces here instead of as an opaque
// provider error.
func normalizeEskizPhone(phone string) (string, bool) {
	var digits strings.Builder
	for _, r := range phone {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	p := digits.String()

	switch {
	case len(p) == 9:
		// local subscriber number, e.g. 901234567
		return "998" + p, true
	case len(p) == 12 && strings.HasPrefix(p, "998"):
		return p, true
	default:
		return "", false
	}
}
