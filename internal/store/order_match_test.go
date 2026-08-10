package store

import (
	"testing"

	"prostaff/internal/models"
)

// Tashkent, and a point far enough out to be past the 15 km match radius.
var (
	nearLat, nearLng = 41.3111, 69.2797
	farLat, farLng   = 41.3111, 69.7000 // ~35 km east
)

func broadcastOrder() *models.Order {
	return &models.Order{
		CustomerID: "customer-1",
		CategoryID: "cat-clean",
		Gender:     "any",
		Lat:        nearLat,
		Lng:        nearLng,
	}
}

func worker(id string) *models.WorkerProfile {
	return &models.WorkerProfile{
		ID:          id,
		UserID:      "user-" + id,
		Gender:      "male",
		CategoryIDs: []string{"cat-clean"},
		Lat:         nearLat,
		Lng:         nearLng,
	}
}

// The ordinary order: nobody is named, so matchmaking decides who is rung.
func TestMatchesOrderBroadcast(t *testing.T) {
	cases := []struct {
		name  string
		mutex func(*models.Order, *models.WorkerProfile)
		want  bool
	}{
		{"a matching worker nearby", func(*models.Order, *models.WorkerProfile) {}, true},
		{
			"the customer is not called to their own job",
			func(o *models.Order, wp *models.WorkerProfile) { wp.UserID = o.CustomerID },
			false,
		},
		{
			"a worker who does not do this category",
			func(_ *models.Order, wp *models.WorkerProfile) { wp.CategoryIDs = []string{"cat-garden"} },
			false,
		},
		{
			"the wrong gender for an order that asked",
			func(o *models.Order, wp *models.WorkerProfile) { o.Gender = "female" },
			false,
		},
		{
			"the right gender for an order that asked",
			func(o *models.Order, wp *models.WorkerProfile) { o.Gender, wp.Gender = "female", "female" },
			true,
		},
		{
			"too far away",
			func(_ *models.Order, wp *models.WorkerProfile) { wp.Lat, wp.Lng = farLat, farLng },
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o, wp := broadcastOrder(), worker("w1")
			tc.mutex(o, wp)
			if got := matchesOrder(o, wp); got != tc.want {
				t.Errorf("matchesOrder = %v, want %v", got, tc.want)
			}
		})
	}
}

// The order that names its workers. This is the whole point of the feature: the
// customer looked at these people and chose them, so the matchmaking filters
// step aside — and every worker they did *not* choose is excluded no matter how
// well they would otherwise match.
func TestMatchesOrderTargeted(t *testing.T) {
	t.Run("a chosen worker is rung", func(t *testing.T) {
		o, wp := broadcastOrder(), worker("w1")
		o.TargetWorkerIDs = []string{"w1", "w2"}
		if !matchesOrder(o, wp) {
			t.Error("matchesOrder = false, want true for a chosen worker")
		}
	})

	t.Run("a perfect match nobody chose is not rung", func(t *testing.T) {
		o, wp := broadcastOrder(), worker("w9")
		o.TargetWorkerIDs = []string{"w1", "w2"}
		if matchesOrder(o, wp) {
			t.Error("matchesOrder = true, want false — this worker was not chosen")
		}
	})

	// Each of these would have excluded the worker on a broadcast. Being named
	// overrides all three, because the customer answered the question the filter
	// exists to guess at.
	t.Run("category, gender and radius do not veto a choice", func(t *testing.T) {
		o, wp := broadcastOrder(), worker("w1")
		o.TargetWorkerIDs = []string{"w1"}
		o.Gender = "female" // worker is male
		wp.CategoryIDs = []string{"cat-something-else"}
		wp.Lat, wp.Lng = farLat, farLng
		if !matchesOrder(o, wp) {
			t.Error("matchesOrder = false, want true — an explicit choice outranks the filters")
		}
	})

	// The one rule that survives being named: an order still cannot ring the
	// account that placed it.
	t.Run("the customer still cannot ring themselves", func(t *testing.T) {
		o, wp := broadcastOrder(), worker("w1")
		o.TargetWorkerIDs = []string{"w1"}
		wp.UserID = o.CustomerID
		if matchesOrder(o, wp) {
			t.Error("matchesOrder = true, want false — a customer is not their own worker")
		}
	})
}

// A worker is offered one order out of a batch, not all of them.
//
// This is the rule that keeps "I need five helpers" from putting five cards and
// five pushes on every nearby phone. `batchEngagedWorkers` needs a database, so
// what is tested here is the filter it feeds — that a worker a sibling order is
// already ringing is skipped, and that nothing changes for an ordinary order
// with no batch at all.
func TestBatchEngagedWorkersAreSkipped(t *testing.T) {
	// The predicate as candidatesFor applies it: eligible for the order, and not
	// already spoken for by a sibling.
	eligible := func(o *models.Order, wp *models.WorkerProfile, engaged map[string]bool) bool {
		return matchesOrder(o, wp) && !engaged[wp.ID]
	}

	t.Run("a worker a sibling order is ringing is skipped", func(t *testing.T) {
		o, wp := broadcastOrder(), worker("w1")
		o.BatchID = "batch-1"
		engaged := map[string]bool{"w1": true}
		if eligible(o, wp, engaged) {
			t.Error("eligible = true, want false — a sibling order already has them")
		}
	})

	t.Run("a worker nobody has yet is offered the job", func(t *testing.T) {
		o, wp := broadcastOrder(), worker("w2")
		o.BatchID = "batch-1"
		engaged := map[string]bool{"w1": true}
		if !eligible(o, wp, engaged) {
			t.Error("eligible = false, want true — this worker is free")
		}
	})

	// An order placed on its own has no siblings, so nothing is ever withheld
	// from it: the ordinary broadcast still rings everybody at once.
	t.Run("a lone order still reaches everybody", func(t *testing.T) {
		o, wp := broadcastOrder(), worker("w1")
		if !eligible(o, wp, map[string]bool{}) {
			t.Error("eligible = false, want true — a single order has no batch")
		}
	})
}

// When a decline is the customer's answer.
//
// This is what decides whether they are told "rad etildi" now or left watching a
// countdown for two minutes and then told nobody was free. Getting it wrong in
// the generous direction is worse: ending a broadcast on one refusal would kill
// searches that somebody was about to accept.
func TestAllTargetsRejected(t *testing.T) {
	s := &Store{}
	cases := []struct {
		name     string
		targets  []string
		rejected []string
		want     bool
	}{
		{
			// A broadcast never runs out: the dispatch loop keeps pulling in
			// workers who come online inside the window.
			name:     "a broadcast is never exhausted",
			targets:  nil,
			rejected: []string{"w1", "w2", "w3"},
			want:     false,
		},
		{
			name:     "the only worker asked said no",
			targets:  []string{"w1"},
			rejected: []string{"w1"},
			want:     true,
		},
		{
			name:     "one of three has declined",
			targets:  []string{"w1", "w2", "w3"},
			rejected: []string{"w2"},
			want:     false,
		},
		{
			name:     "the last of three declines",
			targets:  []string{"w1", "w2", "w3"},
			rejected: []string{"w2", "w3", "w1"},
			want:     true,
		},
		{
			name:     "nobody has answered yet",
			targets:  []string{"w1", "w2"},
			rejected: nil,
			want:     false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := &models.Order{TargetWorkerIDs: tc.targets, RejectedWorkerIDs: tc.rejected}
			if got := s.allTargetsRejected(o); got != tc.want {
				t.Errorf("allTargetsRejected = %v, want %v", got, tc.want)
			}
		})
	}
}
