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
	"strings"
	"time"
)

// expoURL is a var, not a const, so tests can point Send at a local server and
// assert on the payload that actually goes over the wire.
var expoURL = "https://exp.host/--/api/v2/push/send"

// receiptsURL is derived from expoURL so a test that redirects one redirects
// both. Expo serves both under the same host.
func receiptsURL() string {
	return strings.TrimSuffix(expoURL, "/send") + "/getReceipts"
}

// receiptDelay is how long Send waits before asking Expo what became of a
// message. Expo needs a moment to hand the push to FCM/APNs and hear back;
// asking immediately reliably returns nothing at all.
var receiptDelay = 15 * time.Second

// OnInvalidToken is called with a token Expo has declared dead — the app was
// uninstalled, or reinstalled and issued a new one. Wired at startup to clear
// the row, because a dead token is not merely useless: every later send still
// pays a round trip for it, and it hides the fact that the user has no working
// device from anyone reading the database.
var OnInvalidToken func(token string)

func invalidate(token string) {
	if OnInvalidToken != nil && token != "" {
		OnInvalidToken(token)
	}
}

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

// Receipt is Expo's verdict on a message it already accepted, fetched by
// ticket id a few seconds later.
//
// A ticket says "Expo took it"; only the receipt says whether FCM or APNs did.
// Every configuration failure that makes push look dead on a real build lands
// here and nowhere else: MismatchSenderId (the FCM key on the Expo project is
// not the one that signed google-services.json), InvalidCredentials (no FCM V1
// service account uploaded at all), DeviceNotRegistered (the app is gone).
type Receipt struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
	Details any    `json:"details,omitempty"`
}

// Code returns Expo's machine-readable error code, e.g. "DeviceNotRegistered".
func (r Receipt) Code() string { return errorCode(r.Details) }

// Code returns Expo's machine-readable error code for a rejected ticket.
func (t Ticket) Code() string { return errorCode(t.Details) }

// errorCode digs the `error` field out of Expo's loosely typed `details`.
func errorCode(details any) string {
	m, ok := details.(map[string]any)
	if !ok {
		return ""
	}
	code, _ := m["error"].(string)
	return code
}

// ErrNoRecipients is returned by SendSync when every message had an empty
// token — most often a user who has never opened the app on a real build.
var ErrNoRecipients = errors.New("push: no valid push tokens")

// Send delivers in the background. Errors are logged, not returned, because
// push delivery is best-effort and must never block an HTTP handler.
//
// It then follows the delivery through to its receipt. Without that half, the
// commonest way for push to fail on a release build — Expo accepts every
// message and FCM refuses every one of them — produced not a single line of
// log anywhere, and looked from the server exactly like success.
func Send(messages ...Message) {
	go func() {
		sent, tickets, err := deliver(messages)
		if err != nil {
			if !errors.Is(err, ErrNoRecipients) {
				log.Printf("push: %v", err)
			}
			return
		}
		pending := make(map[string]string, len(tickets)) // receipt id -> token
		for i, ticket := range tickets {
			if i >= len(sent) {
				break
			}
			token := sent[i].To
			if ticket.Status == "ok" {
				if ticket.ID != "" {
					pending[ticket.ID] = token
				}
				continue
			}
			log.Printf("push: expo rejected %s: %s (%s)", mask(token), ticket.Message, ticket.Code())
			if ticket.Code() == "DeviceNotRegistered" {
				invalidate(token)
			}
		}
		if len(pending) == 0 {
			return
		}

		time.Sleep(receiptDelay)
		ids := make([]string, 0, len(pending))
		for id := range pending {
			ids = append(ids, id)
		}
		receipts, err := GetReceipts(ids)
		if err != nil {
			log.Printf("push: receipts: %v", err)
			return
		}
		for id, receipt := range receipts {
			if receipt.Status == "ok" {
				continue
			}
			token := pending[id]
			log.Printf("push: undelivered to %s: %s (%s)", mask(token), receipt.Message, receipt.Code())
			if receipt.Code() == "DeviceNotRegistered" {
				invalidate(token)
			}
		}
	}()
}

// GetReceipts asks Expo what happened to messages it accepted earlier, keyed by
// ticket id. Ids Expo has no answer for yet are simply absent from the map.
func GetReceipts(ticketIDs []string) (map[string]Receipt, error) {
	if len(ticketIDs) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(map[string]any{"ids": ticketIDs})
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequest("POST", receiptsURL(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("expo returned %d: %s", resp.StatusCode, string(raw))
	}
	var envelope struct {
		Data map[string]Receipt `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("decode reply: %w", err)
	}
	return envelope.Data, nil
}

// mask keeps a token recognisable in a log without printing a credential.
func mask(token string) string {
	if len(token) <= 12 {
		return token
	}
	return token[:9] + "…" + token[len(token)-5:]
}

// SendSync delivers and waits, returning Expo's per-message tickets. Used by
// the push self-test endpoint, where "did it actually go out?" is the whole
// question and a swallowed error is useless.
func SendSync(messages ...Message) ([]Ticket, error) {
	_, tickets, err := deliver(messages)
	return tickets, err
}

// deliver posts the batch and returns the messages that were actually sent
// alongside Expo's tickets, index for index — the pairing Send needs to know
// which token a rejection belongs to.
func deliver(messages []Message) ([]Message, []Ticket, error) {
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
		return nil, nil, ErrNoRecipients
	}

	body, err := json.Marshal(valid)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequest("POST", expoURL, bytes.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("send: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, nil, fmt.Errorf("expo returned %d: %s", resp.StatusCode, string(raw))
	}
	var envelope struct {
		Data []Ticket `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, nil, fmt.Errorf("decode reply: %w", err)
	}
	return valid, envelope.Data, nil
}
