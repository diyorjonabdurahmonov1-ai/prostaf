// Package fcm talks to Firebase Cloud Messaging's HTTP v1 API directly.
//
// Why not just use Expo, which is already wired up?
//
// Expo's push service hands Android a *data-only* FCM message. A data message
// is not a notification: nothing is shown until the app's own process starts,
// receives it, and builds one — that is what `expo-notifications` does in its
// FirebaseMessagingService. While the app is running that works perfectly, and
// it is exactly why "the notification arrives, but only when the app is open"
// is the shape this failure takes. Once the process is gone — swiped out of
// recents, or put to sleep by Samsung's battery manager, which does it after a
// few days of the app going unused — Android has nobody to hand the message to
// and simply drops it. Telegram does not have this problem because it also
// sends a `notification` block, which the system's own FCM receiver renders
// with the app entirely uninvolved.
//
// So this sends a real `notification` payload. Android draws it whether or not
// the app exists at that moment, on the "orders" channel, at max priority.
// `data` still rides along for the app to route on when it *is* running.
//
// Auth is the OAuth2 service-account flow: a self-signed JWT exchanged for a
// bearer token, cached until it expires. That is the whole of it — no SDK, and
// no new module dependency.
package fcm

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Endpoints are vars so tests can point them at a local server.
var (
	tokenURL = "https://oauth2.googleapis.com/token"
	sendURL  = "https://fcm.googleapis.com/v1/projects/%s/messages:send"
)

const scope = "https://www.googleapis.com/auth/firebase.messaging"

// serviceAccount is the subset of Firebase's key file that matters.
type serviceAccount struct {
	Type        string `json:"type"`
	ProjectID   string `json:"project_id"`
	PrivateKey  string `json:"private_key"`
	ClientEmail string `json:"client_email"`
	TokenURI    string `json:"token_uri"`
}

// Client sends to one Firebase project.
type Client struct {
	account serviceAccount
	key     *rsa.PrivateKey
	http    *http.Client

	mu          sync.Mutex
	accessToken string
	expiresAt   time.Time
}

// ErrNotConfigured is returned by New when no service account was supplied.
// Callers treat it as "fall back to Expo", not as a failure.
var ErrNotConfigured = errors.New("fcm: no service account configured")

// New builds a client from a service account key, given either as a path to the
// JSON file or as the JSON itself (handy on a host where secrets are env vars
// and there is no filesystem to put a key on). Both empty means not configured.
func New(path, inline string) (*Client, error) {
	raw := []byte(strings.TrimSpace(inline))
	if len(raw) == 0 {
		if strings.TrimSpace(path) == "" {
			return nil, ErrNotConfigured
		}
		var err error
		raw, err = os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("fcm: read %s: %w", path, err)
		}
	}

	var account serviceAccount
	if err := json.Unmarshal(raw, &account); err != nil {
		return nil, fmt.Errorf("fcm: parse service account: %w", err)
	}
	if account.ProjectID == "" || account.ClientEmail == "" || account.PrivateKey == "" {
		return nil, errors.New("fcm: service account is missing project_id, client_email or private_key")
	}

	key, err := parsePrivateKey(account.PrivateKey)
	if err != nil {
		return nil, err
	}
	if account.TokenURI == "" {
		account.TokenURI = tokenURL
	}
	return &Client{
		account: account,
		key:     key,
		http:    &http.Client{Timeout: 15 * time.Second},
	}, nil
}

// ProjectID is the Firebase project this client sends to. Worth logging at
// startup: the commonest misconfiguration is a key for a different project than
// the one google-services.json shipped in the app.
func (c *Client) ProjectID() string { return c.account.ProjectID }

func parsePrivateKey(pemText string) (*rsa.PrivateKey, error) {
	// Env-var copies of the key routinely arrive with literal "\n" instead of
	// newlines; PEM cannot be decoded that way and the error is unhelpful.
	pemText = strings.ReplaceAll(pemText, `\n`, "\n")
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return nil, errors.New("fcm: private_key is not valid PEM")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("fcm: parse private key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("fcm: private key is not RSA")
	}
	return key, nil
}

/* ------------------------------------------------------------------ *
 * Access token
 * ------------------------------------------------------------------ */

// accessTokenFor returns a cached bearer token, minting a new one when the old
// one is inside a minute of expiring.
func (c *Client) accessTokenFor() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.accessToken != "" && time.Until(c.expiresAt) > time.Minute {
		return c.accessToken, nil
	}

	assertion, err := c.signJWT()
	if err != nil {
		return "", err
	}
	form := strings.NewReader(
		"grant_type=urn:ietf:params:oauth:grant-type:jwt-bearer&assertion=" + assertion)
	req, err := http.NewRequest("POST", c.account.TokenURI, form)
	if err != nil {
		return "", fmt.Errorf("fcm: token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("fcm: token exchange: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("fcm: token exchange returned %d: %s", resp.StatusCode, body)
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("fcm: decode token: %w", err)
	}
	if out.AccessToken == "" {
		return "", errors.New("fcm: token exchange returned no access_token")
	}
	c.accessToken = out.AccessToken
	c.expiresAt = time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)
	return c.accessToken, nil
}

func (c *Client) signJWT() (string, error) {
	now := time.Now()
	header := base64url(`{"alg":"RS256","typ":"JWT"}`)
	claims, err := json.Marshal(map[string]any{
		"iss":   c.account.ClientEmail,
		"scope": scope,
		"aud":   c.account.TokenURI,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	})
	if err != nil {
		return "", fmt.Errorf("fcm: claims: %w", err)
	}
	signingInput := header + "." + base64url(string(claims))
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, c.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("fcm: sign: %w", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func base64url(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}

/* ------------------------------------------------------------------ *
 * Sending
 * ------------------------------------------------------------------ */

// Message is one push, in the terms this app thinks in.
type Message struct {
	Token string
	Title string
	Body  string
	// Data rides alongside the notification. Values must be strings — FCM
	// rejects a data map with any other type — so Send stringifies them.
	Data map[string]any
	// ChannelID is the Android channel the notification is posted on. The app
	// registers "orders" at MAX importance.
	ChannelID string
	// TTL caps how long FCM keeps retrying. An invite that lands after its two
	// minutes are up is noise; zero means FCM's default (four weeks).
	TTL time.Duration
}

// ErrUnregistered means the token belongs to an app instance that no longer
// exists — uninstalled, or reinstalled with a new token. The row should be
// cleared rather than retried.
var ErrUnregistered = errors.New("fcm: token is not registered")

// Send delivers one message and waits for FCM's answer.
func (c *Client) Send(m Message) error {
	if m.Token == "" {
		return errors.New("fcm: empty token")
	}
	access, err := c.accessTokenFor()
	if err != nil {
		return err
	}

	data := make(map[string]string, len(m.Data))
	for k, v := range m.Data {
		data[k] = fmt.Sprint(v)
	}
	channel := m.ChannelID
	if channel == "" {
		channel = "orders"
	}

	android := map[string]any{
		"priority": "high",
		"notification": map[string]any{
			// The channel has to exist on the device or Android files the
			// notification under "Miscellaneous" at default importance — which
			// is silent, and looks exactly like not being delivered at all.
			"channel_id":              channel,
			"sound":                   "default",
			"default_vibrate_timings": true,
			"notification_priority":   "PRIORITY_MAX",
			"visibility":              "PUBLIC",
		},
	}
	if m.TTL > 0 {
		android["ttl"] = fmt.Sprintf("%ds", int(m.TTL.Seconds()))
	}

	payload := map[string]any{
		"message": map[string]any{
			"token": m.Token,
			// The `notification` block is the whole point: the system renders
			// this one, so it appears with the app closed. `data` is for the
			// app to route on when it happens to be running.
			"notification": map[string]any{"title": m.Title, "body": m.Body},
			"data":         data,
			"android":      android,
			"apns": map[string]any{
				"headers": map[string]any{"apns-priority": "10"},
				"payload": map[string]any{
					"aps": map[string]any{
						"sound":              "default",
						"interruption-level": "time-sensitive",
					},
				},
			},
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("fcm: marshal: %w", err)
	}
	req, err := http.NewRequest("POST", fmt.Sprintf(sendURL, c.account.ProjectID), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("fcm: request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("fcm: send: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 400 {
		return nil
	}

	// FCM reports a dead token as 404 UNREGISTERED, and a token that belongs to
	// another Firebase project as 403 SENDER_ID_MISMATCH. Only the first is the
	// device's fault, and only the first should cost it its row.
	var errBody struct {
		Error struct {
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &errBody)
	if resp.StatusCode == http.StatusNotFound || errBody.Error.Status == "UNREGISTERED" {
		return ErrUnregistered
	}
	if errBody.Error.Message != "" {
		return fmt.Errorf("fcm: %d %s: %s", resp.StatusCode, errBody.Error.Status, errBody.Error.Message)
	}
	return fmt.Errorf("fcm: %d: %s", resp.StatusCode, raw)
}
