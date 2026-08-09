package fcm

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testAccount builds a throwaway service account key, so the tests exercise the
// real PEM parse and the real RS256 signature rather than a stub.
func testAccount(t *testing.T, tokenURI string) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	raw, err := json.Marshal(map[string]string{
		"type":         "service_account",
		"project_id":   "prostaff-cec9e",
		"private_key":  pemText,
		"client_email": "pusher@prostaff-cec9e.iam.gserviceaccount.com",
		"token_uri":    tokenURI,
	})
	if err != nil {
		t.Fatalf("marshal account: %v", err)
	}
	return string(raw)
}

// fcmStub answers the token exchange and records the message body FCM is given.
func fcmStub(t *testing.T, status int, reply string) (*Client, *map[string]any) {
	t.Helper()
	var got map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"access_token":"ya29.test","expires_in":3600}`)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "Bearer ya29.test" {
			t.Errorf("Authorization = %q, want the exchanged bearer token", auth)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("FCM received malformed JSON: %v (%s)", err, body)
		}
		w.WriteHeader(status)
		io.WriteString(w, reply)
	})
	srv := httptest.NewServer(mux)

	original := sendURL
	sendURL = srv.URL + "/v1/projects/%s/messages:send"
	t.Cleanup(func() {
		sendURL = original
		srv.Close()
	})

	client, err := New("", testAccount(t, srv.URL+"/token"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client, &got
}

// The `notification` block is the entire reason this package exists. Without
// it Android has to wake the app to draw anything, which is what made invites
// vanish the moment the app was closed.
func TestSendCarriesANotificationAndroidCanDrawItself(t *testing.T) {
	client, got := fcmStub(t, http.StatusOK, `{"name":"projects/x/messages/1"}`)

	err := client.Send(Message{
		Token:     "device-abc",
		Title:     "Yangi buyurtma!",
		Body:      "Xona tozalash",
		Data:      map[string]any{"type": "order.invite", "order_id": "o-1"},
		ChannelID: "orders",
		TTL:       120 * time.Second,
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	message, ok := (*got)["message"].(map[string]any)
	if !ok {
		t.Fatalf("payload = %#v, want a message object", *got)
	}
	notification, ok := message["notification"].(map[string]any)
	if !ok {
		t.Fatal("no notification block — the system will not render anything with the app closed")
	}
	if notification["title"] != "Yangi buyurtma!" || notification["body"] != "Xona tozalash" {
		t.Errorf("notification = %#v, want the title and body verbatim", notification)
	}
	if message["token"] != "device-abc" {
		t.Errorf("token = %v, want the device token", message["token"])
	}

	android, ok := message["android"].(map[string]any)
	if !ok {
		t.Fatal("no android block")
	}
	if android["priority"] != "high" {
		t.Errorf("priority = %v, want high — a batched invite lands after it expired", android["priority"])
	}
	if android["ttl"] != "120s" {
		t.Errorf("ttl = %v, want 120s — the search window", android["ttl"])
	}
	androidNotification, _ := android["notification"].(map[string]any)
	if androidNotification["channel_id"] != "orders" {
		t.Errorf("channel_id = %v, want orders — anything else is filed as silent",
			androidNotification["channel_id"])
	}
}

// FCM rejects a data map containing anything but strings, and the order id is
// the one thing the app routes on when it is tapped.
func TestDataValuesAreStringified(t *testing.T) {
	client, got := fcmStub(t, http.StatusOK, `{"name":"n"}`)

	if err := client.Send(Message{
		Token: "device-abc",
		Title: "t",
		Data:  map[string]any{"order_id": "o-9", "attempt": 2},
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	message := (*got)["message"].(map[string]any)
	data, ok := message["data"].(map[string]any)
	if !ok {
		t.Fatalf("data = %#v, want an object", message["data"])
	}
	if data["order_id"] != "o-9" {
		t.Errorf("order_id = %#v, want the string o-9", data["order_id"])
	}
	if data["attempt"] != "2" {
		t.Errorf("attempt = %#v, want the string 2 — FCM refuses a number here", data["attempt"])
	}
}

// A dead token should cost the device its row; anything else must not, or one
// bad afternoon at Google unsubscribes the whole user base.
func TestUnregisteredIsDistinguishedFromEverythingElse(t *testing.T) {
	client, _ := fcmStub(t, http.StatusNotFound,
		`{"error":{"status":"UNREGISTERED","message":"Requested entity was not found."}}`)
	if err := client.Send(Message{Token: "gone", Title: "t"}); err != ErrUnregistered {
		t.Fatalf("err = %v, want ErrUnregistered", err)
	}

	mismatched, _ := fcmStub(t, http.StatusForbidden,
		`{"error":{"status":"PERMISSION_DENIED","message":"SenderId mismatch"}}`)
	err := mismatched.Send(Message{Token: "fine", Title: "t"})
	if err == nil || err == ErrUnregistered {
		t.Fatalf("err = %v, want a plain error that keeps the token", err)
	}
	if !strings.Contains(err.Error(), "SenderId mismatch") {
		t.Errorf("err = %v, want Firebase's reason included", err)
	}
}

// The key routinely arrives through an env var with escaped newlines, which is
// not decodable PEM. Failing there would look exactly like "push is broken".
func TestPrivateKeyWithEscapedNewlinesIsAccepted(t *testing.T) {
	account := testAccount(t, "https://example.invalid/token")
	escaped := strings.ReplaceAll(account, `\n`, `\\n`)
	if _, err := New("", escaped); err != nil {
		t.Fatalf("New with escaped newlines: %v", err)
	}
}

func TestNoServiceAccountIsNotAnError(t *testing.T) {
	if _, err := New("", ""); err != ErrNotConfigured {
		t.Fatalf("err = %v, want ErrNotConfigured so the caller falls back to Expo", err)
	}
}
