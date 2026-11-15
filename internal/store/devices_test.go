package store

import (
	"testing"
	"time"
)

func TestDeviceStillHolds(t *testing.T) {
	min := time.Minute
	cases := []struct {
		name   string
		online bool
		idle   time.Duration
		want   bool
	}{
		{"in use", false, 30 * time.Second, true},
		{"app closed", false, 5 * min, false},
		{"online and recent", true, 10 * min, true},
		{"online but long silent", true, 25 * min, false},
	}
	for _, c := range cases {
		if got := deviceStillHolds(c.online, c.idle); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}
