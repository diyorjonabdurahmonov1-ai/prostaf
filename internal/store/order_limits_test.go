package store

import (
	"math"
	"strings"
	"testing"
)

func TestValidateOrderInput(t *testing.T) {
	ok := OrderInput{Price: 150000, Description: "bog' ishlari", Phone: "+998901234567", Lat: 41.3, Lng: 69.2}
	if err := validateOrderInput(ok); err != nil {
		t.Fatalf("a normal order was refused: %v", err)
	}
	bad := map[string]func(*OrderInput){
		"free":             func(o *OrderInput) { o.Price = 0 },
		"absurd price":     func(o *OrderInput) { o.Price = math.MaxInt64 },
		"long description": func(o *OrderInput) { o.Description = strings.Repeat("a", maxOrderDescription+1) },
		"long phone":       func(o *OrderInput) { o.Phone = strings.Repeat("9", 40) },
		"latitude":         func(o *OrderInput) { o.Lat = 91 },
		"longitude":        func(o *OrderInput) { o.Lng = -181 },
		"nan":              func(o *OrderInput) { o.Lat = math.NaN() },
		"long batch id":    func(o *OrderInput) { o.BatchID = strings.Repeat("b", 100) },
		"huge batch":       func(o *OrderInput) { o.BatchSize = 500 },
	}
	for name, mutate := range bad {
		in := ok
		mutate(&in)
		if validateOrderInput(in) == nil {
			t.Errorf("%s: should have been refused", name)
		}
	}
}

func TestCleanRatingComment(t *testing.T) {
	if got := cleanRatingComment("  zo'r  "); got != "zo'r" {
		t.Errorf("got %q", got)
	}
	long := strings.Repeat("я", maxRatingComment+50)
	if n := len([]rune(cleanRatingComment(long))); n != maxRatingComment {
		t.Errorf("kept %d characters", n)
	}
}
