package store

import (
	"testing"
	"time"
)

func TestDeviceMayClaim(t *testing.T) {
	min := time.Minute
	cases := []struct {
		name                   string
		same, takeover, online bool
		idle                   time.Duration
		want                   bool
	}{
		{"same phone", true, false, true, 0, true},
		{"other phone, in use", false, false, false, 30 * time.Second, false},
		{"other phone, app closed", false, false, false, 5 * min, true},
		{"other phone, online and recent", false, false, true, 10 * min, false},
		{"other phone, online but long silent", false, false, true, 25 * min, true},
		{"sms takeover beats everything", false, true, true, 0, true},
	}
	for _, c := range cases {
		if got := deviceMayClaim(c.same, c.takeover, c.online, c.idle); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}
