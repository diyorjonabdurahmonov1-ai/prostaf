package store

import (
	"testing"

	"prostaff/internal/models"
)

// The card's arithmetic. A tariff shows three numbers — the price, what that
// works out to per month, and how much less it is than the old price — and two
// of them are derived, so getting the derivation wrong misprices every card on
// the screen at once.
func TestPlanDerive(t *testing.T) {
	cases := []struct {
		name             string
		plan             models.SubscriptionPlan
		wantPerMonth     int64
		wantDiscountPerc int
	}{
		{
			name:         "one month, no discount",
			plan:         models.SubscriptionPlan{Months: 1, Price: 100_000},
			wantPerMonth: 100_000,
		},
		{
			name:             "three months at 200k against a 300k list price",
			plan:             models.SubscriptionPlan{Months: 3, Price: 200_000, OldPrice: 300_000},
			wantPerMonth:     66_666,
			wantDiscountPerc: 33,
		},
		{
			name:             "a year at half price",
			plan:             models.SubscriptionPlan{Months: 12, Price: 600_000, OldPrice: 1_200_000},
			wantPerMonth:     50_000,
			wantDiscountPerc: 50,
		},
		{
			// An old price below the real one is somebody's typo, not a discount
			// of minus ten percent.
			name:         "old price lower than the price shows no discount",
			plan:         models.SubscriptionPlan{Months: 1, Price: 100_000, OldPrice: 90_000},
			wantPerMonth: 100_000,
		},
		{
			name:         "months of zero cannot divide",
			plan:         models.SubscriptionPlan{Months: 0, Price: 100_000},
			wantPerMonth: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.plan
			p.Derive()
			if p.PerMonth != tc.wantPerMonth {
				t.Errorf("PerMonth = %d, want %d", p.PerMonth, tc.wantPerMonth)
			}
			if p.DiscountPercent != tc.wantDiscountPerc {
				t.Errorf("DiscountPercent = %d, want %d", p.DiscountPercent, tc.wantDiscountPerc)
			}
		})
	}
}

// Seeded tariffs are printed on a card, so they have to be numbers a person
// would write down.
func TestRoundToThousand(t *testing.T) {
	cases := map[int64]int64{
		0:       0,
		-5:      0,
		254_736: 255_000,
		100_000: 100_000,
		499:     0,
		500:     1_000,
	}
	for in, want := range cases {
		if got := roundToThousand(in); got != want {
			t.Errorf("roundToThousand(%d) = %d, want %d", in, got, want)
		}
	}
}
