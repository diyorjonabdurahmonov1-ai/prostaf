package push

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// expoStub stands in for exp.host and records the batch it was given.
func expoStub(t *testing.T, reply string, status int) (*httptest.Server, *[]Message) {
	t.Helper()
	var got []Message
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("expo received malformed JSON: %v (%s)", err, body)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, reply)
	}))
	original := expoURL
	expoURL = srv.URL
	t.Cleanup(func() {
		expoURL = original
		srv.Close()
	})
	return srv, &got
}

// An order invite has to break through a locked screen and a Focus mode, so the
// flags that make that happen are part of the contract, not decoration.
func TestUrgentCarriesTheWakeTheDeviceFlags(t *testing.T) {
	_, got := expoStub(t, `{"data":[{"status":"ok","id":"XXX"}]}`, http.StatusOK)

	tickets, err := SendSync(Urgent("ExponentPushToken[abc]", "Yangi buyurtma!", "Xona tozalash",
		map[string]any{"type": "order.invited", "order_id": "o-1"}, 120))
	if err != nil {
		t.Fatalf("SendSync: %v", err)
	}
	if len(tickets) != 1 || tickets[0].Status != "ok" {
		t.Fatalf("tickets = %+v, want one ok ticket", tickets)
	}

	if len(*got) != 1 {
		t.Fatalf("expo got %d messages, want 1", len(*got))
	}
	m := (*got)[0]
	if m.Priority != "high" {
		t.Errorf("Priority = %q, want high — a batched invite arrives after it expired", m.Priority)
	}
	if m.ChannelID != "orders" {
		t.Errorf("ChannelID = %q, want orders — the MAX-importance Android channel", m.ChannelID)
	}
	if m.InterruptionLevel != "time-sensitive" {
		t.Errorf("InterruptionLevel = %q, want time-sensitive", m.InterruptionLevel)
	}
	if m.Sound != "default" {
		t.Errorf("Sound = %q, want default", m.Sound)
	}
	if m.TTL != 120 {
		t.Errorf("TTL = %d, want 120 — the invite window", m.TTL)
	}
}

// The data payload is what the app routes on (`type` starting with "order."
// wakes the realtime poll), so it must survive the round trip intact.
func TestDataPayloadReachesExpoUnchanged(t *testing.T) {
	_, got := expoStub(t, `{"data":[{"status":"ok"}]}`, http.StatusOK)

	if _, err := SendSync(Message{
		To:    "ExponentPushToken[abc]",
		Title: "t",
		Data:  map[string]any{"type": "order.accepted", "order_id": "o-9"},
	}); err != nil {
		t.Fatalf("SendSync: %v", err)
	}

	data, ok := (*got)[0].Data.(map[string]any)
	if !ok {
		t.Fatalf("Data = %#v, want an object", (*got)[0].Data)
	}
	if data["type"] != "order.accepted" || data["order_id"] != "o-9" {
		t.Errorf("Data = %#v, want type/order_id preserved", data)
	}
}

// A user who has never opened a real build has no token. Posting an empty "to"
// makes Expo reject the whole batch, so those messages are dropped first.
func TestEmptyTokensAreDroppedBeforeTheRequest(t *testing.T) {
	_, got := expoStub(t, `{"data":[{"status":"ok"}]}`, http.StatusOK)

	if _, err := SendSync(
		Message{To: "", Title: "no token"},
		Message{To: "ExponentPushToken[real]", Title: "has token"},
	); err != nil {
		t.Fatalf("SendSync: %v", err)
	}
	if len(*got) != 1 || (*got)[0].To != "ExponentPushToken[real]" {
		t.Fatalf("expo got %+v, want only the message with a token", *got)
	}
}

// Every token empty means there is nobody to reach — a distinct outcome from a
// failed request, so the self-test endpoint can say which one happened.
func TestSendSyncReportsWhenNobodyHasAToken(t *testing.T) {
	_, got := expoStub(t, `{"data":[]}`, http.StatusOK)

	_, err := SendSync(Message{To: "", Title: "nobody"})
	if !errors.Is(err, ErrNoRecipients) {
		t.Fatalf("err = %v, want ErrNoRecipients", err)
	}
	if len(*got) != 0 {
		t.Errorf("expo was called with %+v, want no request at all", *got)
	}
}

// Expo answers 200 with per-message errors ("DeviceNotRegistered" after a
// reinstall). Those must reach the caller instead of looking like success.
func TestPerMessageErrorsSurfaceInTickets(t *testing.T) {
	expoStub(t, `{"data":[{"status":"error","message":"DeviceNotRegistered"}]}`, http.StatusOK)

	tickets, err := SendSync(Message{To: "ExponentPushToken[stale]", Title: "t"})
	if err != nil {
		t.Fatalf("SendSync: %v", err)
	}
	if len(tickets) != 1 || tickets[0].Status != "error" {
		t.Fatalf("tickets = %+v, want one error ticket", tickets)
	}
	if tickets[0].Message != "DeviceNotRegistered" {
		t.Errorf("Message = %q, want DeviceNotRegistered", tickets[0].Message)
	}
}

func TestHTTPErrorIsReturnedWithExpoBody(t *testing.T) {
	expoStub(t, `{"errors":[{"code":"PUSH_TOO_MANY_EXPERIENCE_IDS"}]}`, http.StatusBadRequest)

	_, err := SendSync(Message{To: "ExponentPushToken[abc]", Title: "t"})
	if err == nil {
		t.Fatal("SendSync returned nil, want an error for a 400")
	}
	if !strings.Contains(err.Error(), "PUSH_TOO_MANY_EXPERIENCE_IDS") {
		t.Errorf("err = %v, want Expo's reason included", err)
	}
}
