package api

import (
	"testing"

	"prostaff/internal/models"
)

// The customer's number is the one thing on an order that cannot be taken back
// once it has been shown, so the rule is worth pinning down: a provider gets it
// when they hold the job, and not one moment earlier.
//
// A firm used to be exempt by accident. It owns the order row from the instant
// the customer picks it — there is no invite step the way there is for a worker
// — so every endpoint that asked "is this your order?" answered yes and handed
// over the phone, on a job the firm had not agreed to and could decline a second
// later.
func TestCompanySeesTheCustomerPhoneOnlyOnceItHoldsTheJob(t *testing.T) {
	const phone = "+998991093878"

	hidden := []string{
		models.StatusPending,
		models.StatusMatching,
		models.StatusInvited,
		models.StatusCancelled,
		models.StatusExpired,
	}
	for _, status := range hidden {
		got := maskForCompany(models.Order{Status: status, CustomerPhone: phone})
		if got.CustomerPhone != "" {
			t.Errorf("status %q leaked the customer phone before the firm accepted", status)
		}
	}

	shown := []string{models.StatusAccepted, models.StatusInProgress, models.StatusCompleted}
	for _, status := range shown {
		got := maskForCompany(models.Order{Status: status, CustomerPhone: phone})
		if got.CustomerPhone != phone {
			t.Errorf("status %q withheld the phone from the firm doing the job", status)
		}
	}
}

// Masking must not quietly edit the caller's order — the same value is written
// to the database, published on the socket and rated later.
func TestMaskingLeavesTheOriginalOrderAlone(t *testing.T) {
	order := models.Order{Status: models.StatusPending, CustomerPhone: "+998901112233"}
	_ = maskForCompany(order)
	if order.CustomerPhone == "" {
		t.Fatal("maskForCompany mutated its argument")
	}

	worker := models.Order{WorkerID: "u-1", CustomerPhone: "+998901112233"}
	_ = maskForWorker(worker, "u-2")
	if worker.CustomerPhone == "" {
		t.Fatal("maskForWorker mutated its argument")
	}
}

// The worker rule, unchanged, next to the firm's — they are the same promise and
// should fail together if either drifts.
func TestWorkerSeesTheCustomerPhoneOnlyWhenTheJobIsTheirs(t *testing.T) {
	const phone = "+998991093878"

	invited := maskForWorker(models.Order{CustomerPhone: phone}, "u-1")
	if invited.CustomerPhone != "" {
		t.Error("an invited worker was shown the customer phone")
	}

	holder := maskForWorker(models.Order{WorkerID: "u-1", CustomerPhone: phone}, "u-1")
	if holder.CustomerPhone != phone {
		t.Error("the worker doing the job was denied the customer phone")
	}
}
