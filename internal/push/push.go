// Package push sends Expo push notifications via the Expo Push API.
package push

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// expoURL is a var, not a const, so tests can point Send at a local server and
// assert on the payload that actually goes over the wire.
var expoURL = "https://exp.host/--/api/v2/push/send"

// Message is a single push notification payload.
type Message struct {
	To    string `json:"to"`
	Title string `json:"title"`
	Body  string `json:"body"`
	Sound string `json:"sound,omitempty"`
	Data  any    `json:"data,omitempty"`

	// Priority is "default", "normal" or "high". "high" wakes the device
	// immediately instead of being batched by the OS.
	Priority string `json:"priority,omitempty"`
	// ChannelID selects the Android notification channel (must exist on the
	// client). The "orders" channel is registered with MAX importance.
	ChannelID string `json:"channelId,omitempty"`
	// InterruptionLevel is iOS 15+: "passive", "active", "time-sensitive" or
	// "critical". Time-sensitive breaks through Focus modes.
	InterruptionLevel string `json:"interruptionLevel,omitempty"`
	// TTL caps how long the push service keeps retrying, in seconds. An invite
	// that arrives after its 2 minutes are up is just noise.
	TTL int `json:"ttl,omitempty"`
}

// Urgent returns a message configured to alert immediately even when the app is
// closed and the device is in a Focus mode — used for incoming order invites.
func Urgent(token, title, body string, data any, ttlSeconds int) Message {
	return Message{
		To:                token,
		Title:             title,
		Body:              body,
		Sound:             "default",
		Data:              data,
		Priority:          "high",
		ChannelID:         "orders",
		InterruptionLevel: "time-sensitive",
		TTL:               ttlSeconds,
	}
}

var client = &http.Client{Timeout: 10 * time.Second}

// Ticket is one entry of Expo's reply. Status is "ok" or "error"; on error the
// message says why ("DeviceNotRegistered" is the common one — the app was
// reinstalled and the stored token is dead).
type Ticket struct {
	Status  string `json:"status"`
	ID      string `json:"id,omitempty"`
	Message string `json:"message,omitempty"`
	Details any    `json:"details,omitempty"`
}

// ErrNoRecipients is returned by SendSync when every message had an empty
// token — most often a user who has never opened the app on a real build.
var ErrNoRecipients = errors.New("push: no valid push tokens")

// Send delivers in the background. Errors are logged, not returned, because
// push delivery is best-effort and must never block an HTTP handler.
func Send(messages ...Message) {
	go func() {
		if _, err := SendSync(messages...); err != nil && !errors.Is(err, ErrNoRecipients) {
			log.Printf("push: %v", err)
		}
	}()
}

// SendSync delivers and waits, returning Expo's per-message tickets. Used by
// the push self-test endpoint, where "did it actually go out?" is the whole
// question and a swallowed error is useless.
func SendSync(messages ...Message) ([]Ticket, error) {
	valid := make([]Message, 0, len(messages))
	for _, m := range messages {
		if m.To == "" {
			continue
		}
		if m.Sound == "" {
			m.Sound = "default"
		}
		valid = append(valid, m)
	}
	if len(valid) == 0 {
		return nil, ErrNoRecipients
	}

	body, err := json.Marshal(valid)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequest("POST", expoURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("expo returned %d: %s", resp.StatusCode, string(raw))
	}
	var envelope struct {
		Data []Ticket `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("decode reply: %w", err)
	}
	return envelope.Data, nil
}
