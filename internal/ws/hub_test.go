package ws

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// dial starts a hub behind a test server and connects one client as userID.
func dial(t *testing.T, h *Hub, userID string) *websocket.Conn {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.Serve(w, r, userID)
	}))
	t.Cleanup(srv.Close)

	c, _, err := websocket.DefaultDialer.Dial(strings.Replace(srv.URL, "http", "ws", 1), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })

	// The connection is registered inside Serve, on the server's goroutine.
	waitFor(t, func() bool { return h.IsConnected(userID) }, "connection to register")
	return c
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func readEvent(t *testing.T, c *websocket.Conn) Event {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var e Event
	if err := json.Unmarshal(data, &e); err != nil {
		t.Fatalf("unmarshal %s: %v", data, err)
	}
	return e
}

// The whole point: a worker who is connected is reached without asking.
func TestPublishReachesTheUsersOwnConnection(t *testing.T) {
	h := NewHub()
	c := dial(t, h, "worker-1")

	h.Publish("worker-1", Event{Type: "order.invite", Data: map[string]any{"id": "o-1"}})

	e := readEvent(t, c)
	if e.Type != "order.invite" {
		t.Fatalf("type = %q, want order.invite", e.Type)
	}
	data, _ := e.Data.(map[string]any)
	if data["id"] != "o-1" {
		t.Errorf("data = %#v, want the order id to survive", e.Data)
	}
}

// One account, two devices — a phone and a tablet both have to ring.
func TestEveryConnectionOfAUserIsReached(t *testing.T) {
	h := NewHub()
	phone := dial(t, h, "worker-1")
	tablet := dial(t, h, "worker-1")

	h.Publish("worker-1", Event{Type: "order.invite"})

	for name, c := range map[string]*websocket.Conn{"phone": phone, "tablet": tablet} {
		if e := readEvent(t, c); e.Type != "order.invite" {
			t.Errorf("%s got %q, want order.invite", name, e.Type)
		}
	}
}

// A message for somebody else must never leak — invites carry customer details.
func TestPublishDoesNotReachOtherUsers(t *testing.T) {
	h := NewHub()
	mine := dial(t, h, "worker-1")
	theirs := dial(t, h, "worker-2")

	h.Publish("worker-1", Event{Type: "order.invite"})
	readEvent(t, mine)

	_ = theirs.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	if _, _, err := theirs.ReadMessage(); err == nil {
		t.Fatal("worker-2 received a message addressed to worker-1")
	}
}

// Dispatch rings a whole wave at once; the payload is built a single time.
func TestPublishManyReachesEveryRecipient(t *testing.T) {
	h := NewHub()
	a := dial(t, h, "w-a")
	b := dial(t, h, "w-b")
	dial(t, h, "w-c") // connected but not in the wave

	h.PublishMany([]string{"w-a", "w-b"}, Event{Type: "order.invite"})

	if e := readEvent(t, a); e.Type != "order.invite" {
		t.Errorf("w-a got %q", e.Type)
	}
	if e := readEvent(t, b); e.Type != "order.invite" {
		t.Errorf("w-b got %q", e.Type)
	}
}

// Presence is what lets dispatch pick recipients without touching the database.
func TestSnapshotReturnsOnlyAvailableWorkers(t *testing.T) {
	h := NewHub()
	dial(t, h, "on-duty")
	dial(t, h, "off-duty")

	h.SetPresence("on-duty", Presence{Lat: 41.31, Lng: 69.24, Online: true, CategoryIDs: []string{"c1"}})
	h.SetPresence("off-duty", Presence{Lat: 41.31, Lng: 69.24, Online: false})

	snap := h.Snapshot()
	if _, ok := snap["on-duty"]; !ok {
		t.Error("an available worker is missing from the snapshot")
	}
	if _, ok := snap["off-duty"]; ok {
		t.Error("a worker with the toggle off must not be offered work")
	}
	if got := snap["on-duty"].CategoryIDs; len(got) != 1 || got[0] != "c1" {
		t.Errorf("categories = %v, want [c1]", got)
	}
}

// The snapshot must be a copy: matching walks it without holding the lock, and
// a later disconnect must not mutate a walk already in progress.
func TestSnapshotIsACopy(t *testing.T) {
	h := NewHub()
	dial(t, h, "w-1")
	h.SetPresence("w-1", Presence{Online: true, Lat: 1})

	snap := h.Snapshot()
	snap["w-1"] = Presence{Online: true, Lat: 999}

	if again := h.Snapshot(); again["w-1"].Lat != 1 {
		t.Errorf("hub state changed through the snapshot: lat = %v", again["w-1"].Lat)
	}
}

// Presence belongs to a live socket. Left behind, it would have dispatch offer
// work to a phone that stopped listening.
func TestPresenceIsDroppedWhenTheLastConnectionGoes(t *testing.T) {
	h := NewHub()
	c := dial(t, h, "w-1")
	h.SetPresence("w-1", Presence{Online: true})
	if len(h.Snapshot()) != 1 {
		t.Fatal("worker was not available to begin with")
	}

	c.Close()

	waitFor(t, func() bool { return !h.IsConnected("w-1") }, "the connection to be dropped")
	if len(h.Snapshot()) != 0 {
		t.Error("a disconnected worker is still being offered work")
	}
}

// Going off duty keeps the socket — the worker still wants updates on the job
// they are already doing.
func TestClearPresenceKeepsTheConnection(t *testing.T) {
	h := NewHub()
	c := dial(t, h, "w-1")
	h.SetPresence("w-1", Presence{Online: true})

	h.ClearPresence("w-1")

	if len(h.Snapshot()) != 0 {
		t.Error("worker still offered work after going off duty")
	}
	if !h.IsConnected("w-1") {
		t.Fatal("connection was dropped when it should have stayed open")
	}
	h.Publish("w-1", Event{Type: "order.status"})
	if e := readEvent(t, c); e.Type != "order.status" {
		t.Errorf("off-duty worker stopped receiving updates: %q", e.Type)
	}
}

// Presence for somebody who never connected would grow the map forever.
func TestPresenceIsIgnoredForUnconnectedUsers(t *testing.T) {
	h := NewHub()
	h.SetPresence("ghost", Presence{Online: true})
	if len(h.Snapshot()) != 0 {
		t.Error("presence was stored for a user with no connection")
	}
}

func TestCountsReportsConnectionsUsersAndAvailable(t *testing.T) {
	h := NewHub()
	dial(t, h, "w-1")
	dial(t, h, "w-1") // same account, second device
	dial(t, h, "w-2")
	h.SetPresence("w-1", Presence{Online: true})
	h.SetPresence("w-2", Presence{Online: false})

	conns, users, available := h.Counts()
	if conns != 3 || users != 2 || available != 1 {
		t.Errorf("counts = (%d,%d,%d), want (3,2,1)", conns, users, available)
	}
}

// Publishing to somebody who is not connected is a no-op, not a panic: most
// users are offline most of the time and are reached by push instead.
func TestPublishToAnAbsentUserIsSafe(t *testing.T) {
	h := NewHub()
	h.Publish("nobody", Event{Type: "order.invite"})
	h.PublishMany([]string{"nobody", "also-nobody"}, Event{Type: "order.invite"})
}
