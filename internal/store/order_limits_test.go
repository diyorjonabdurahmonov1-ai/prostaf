package store

import (
	"math"
	"strings"
	"testing"

	"prostaff/internal/models"
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

func TestMatchesOrderRespectsWorkerMinimum(t *testing.T) {
	wp := &models.WorkerProfile{ID: "w", UserID: "u", PriceMin: 150000, CategoryIDs: []string{"c"}, Lat: 41, Lng: 69}
	o := &models.Order{CustomerID: "x", CategoryID: "c", Gender: "any", Lat: 41, Lng: 69, Price: 149000}
	if matchesOrderWithin(o, wp, 50) {
		t.Fatal("order below the minimum must not match")
	}
	o.Price = 150000
	if !matchesOrderWithin(o, wp, 50) {
		t.Fatal("order at the minimum must match")
	}
}

func TestCommissionForFlatFeeOrPercent(t *testing.T) {
	s := &Store{}
	s.settings.CommissionPercent = 10
	s.settings.CommissionFixed = 10000
	if got := s.commissionFor(150000); got != 10000 {
		t.Fatalf("flat fee: got %d", got)
	}
	if got := s.commissionFor(50000); got != 10000 {
		t.Fatalf("flat fee does not depend on price: got %d", got)
	}
	s.settings.CommissionFixed = 0
	if got := s.commissionFor(150000); got != 15000 {
		t.Fatalf("percent fallback: got %d", got)
	}
}
