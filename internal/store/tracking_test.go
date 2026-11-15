package store

import "testing"

func TestValidPoint(t *testing.T) {
	cases := []struct {
		lat, lng float64
		want     bool
	}{
		{41.31, 69.28, true},
		{0, 0, false},
		{91, 10, false},
		{10, 181, false},
		{-33.9, 151.2, true},
	}
	for _, c := range cases {
		if got := validPoint(c.lat, c.lng); got != c.want {
			t.Errorf("validPoint(%v,%v)=%v want %v", c.lat, c.lng, got, c.want)
		}
	}
}
