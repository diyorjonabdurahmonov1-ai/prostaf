package api

import (
	"testing"
	"time"
)

func newCooldown() *phoneCooldown {
	return &phoneCooldown{last: map[string]time.Time{}}
}

func TestCooldownBlocksARepeatSendToTheSamePhone(t *testing.T) {
	c := newCooldown()

	if _, ok := c.hit(scopeSignIn, "+998901234567"); !ok {
		t.Fatal("the first send should be allowed")
	}
	wait, ok := c.hit(scopeSignIn, "+998901234567")
	if ok {
		t.Fatal("a second send inside the window should be blocked")
	}
	if wait <= 0 || wait > int(otpResendWindow.Seconds())+1 {
		t.Fatalf("wait = %d s, want something inside the window", wait)
	}
}

// Signing up and resetting a password are throttled independently: a user who
// has just received a sign-up code must not be locked out of the reset flow.
func TestCooldownScopesAreIndependent(t *testing.T) {
	c := newCooldown()
	const phone = "+998901234567"

	if _, ok := c.hit(scopeSignIn, phone); !ok {
		t.Fatal("sign-in send should be allowed")
	}
	if _, ok := c.hit(scopeReset, phone); !ok {
		t.Fatal("a reset send must not be blocked by the sign-in cooldown")
	}
	if _, ok := c.hit(scopeReset, phone); ok {
		t.Fatal("a second reset send inside the window should still be blocked")
	}
}

func TestCooldownIsPerPhone(t *testing.T) {
	c := newCooldown()

	if _, ok := c.hit(scopeSignIn, "+998901234567"); !ok {
		t.Fatal("first phone should be allowed")
	}
	if _, ok := c.hit(scopeSignIn, "+998907654321"); !ok {
		t.Fatal("one phone's cooldown must not block another's")
	}
}

func TestCooldownExpiresAndForgetsTheEntry(t *testing.T) {
	original := otpResendWindow
	otpResendWindow = 10 * time.Millisecond
	defer func() { otpResendWindow = original }()

	c := newCooldown()
	const phone = "+998901234567"

	if _, ok := c.hit(scopeSignIn, phone); !ok {
		t.Fatal("the first send should be allowed")
	}
	time.Sleep(20 * time.Millisecond)
	if _, ok := c.hit(scopeSignIn, phone); !ok {
		t.Fatal("the window has passed, the send should be allowed again")
	}

	// Cooled-down entries are dropped, so the map tracks only live windows.
	time.Sleep(20 * time.Millisecond)
	c.hit(scopeSignIn, "+998907654321")
	if _, still := c.last[scopeSignIn+":"+phone]; still {
		t.Fatal("an expired entry should have been cleaned up")
	}
}
