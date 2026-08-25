package api

import (
	"testing"
	"time"
)

const testPhone = "+998901234567"

// The point of the counter: a six-digit code cannot be searched.
func TestAttemptLimiterLocksOutAfterTooManyFailures(t *testing.T) {
	l := newAttemptLimiter(3)

	for i := 0; i < 3; i++ {
		if _, locked := l.locked(testPhone); locked {
			t.Fatalf("locked out after %d of 3 failures", i)
		}
		l.fail(testPhone)
	}
	left, locked := l.locked(testPhone)
	if !locked {
		t.Fatal("the fourth try should have been refused")
	}
	if left <= 0 || left > authLockout {
		t.Fatalf("wait = %s, want something inside the lockout window", left)
	}
}

// A right answer clears the count, so somebody who mistypes twice and then gets
// it right is not carrying two failures around for the next quarter of an hour.
func TestAttemptLimiterSuccessClearsTheCount(t *testing.T) {
	l := newAttemptLimiter(3)

	l.fail(testPhone)
	l.fail(testPhone)
	l.pass(testPhone)

	for i := 0; i < 3; i++ {
		if _, locked := l.locked(testPhone); locked {
			t.Fatalf("failure %d was still counted after a success", i)
		}
		l.fail(testPhone)
	}
	if _, locked := l.locked(testPhone); !locked {
		t.Fatal("three fresh failures should still lock out")
	}
}

// One number's failures must not lock anybody else out — the counter is keyed
// by the thing being guessed, not by the caller.
func TestAttemptLimiterKeysAreIndependent(t *testing.T) {
	l := newAttemptLimiter(1)

	l.fail(testPhone)
	if _, locked := l.locked(testPhone); !locked {
		t.Fatal("the guessed number should be locked")
	}
	if _, locked := l.locked("+998907654321"); locked {
		t.Fatal("another number must not be locked by it")
	}
}

// The map holds the keys inside a window, not every key ever seen: an attacker
// with a phone-number generator must not be able to grow it without limit.
func TestAttemptLimiterForgetsColdKeys(t *testing.T) {
	l := newAttemptLimiter(3)

	l.fail("+998900000001")
	l.mu.Lock()
	l.tries["+998900000001"].last = time.Now().Add(-2 * authLockout)
	l.mu.Unlock()

	l.fail("+998900000002")

	l.mu.Lock()
	defer l.mu.Unlock()
	if _, still := l.tries["+998900000001"]; still {
		t.Fatalf("a key idle for %s should have been dropped", 2*authLockout)
	}
}

// The cooldown is per phone, so a list of numbers walks past it. This is the
// cap that a list cannot walk past.
func TestSMSBudgetCapsTheWindow(t *testing.T) {
	b := newSMSBudget(2, time.Hour)

	if !b.take() || !b.take() {
		t.Fatal("the first two sends should be allowed")
	}
	if b.take() {
		t.Fatal("the third send should have been refused")
	}
}

func TestSMSBudgetRefillsWhenTheWindowTurns(t *testing.T) {
	b := newSMSBudget(1, time.Hour)

	if !b.take() {
		t.Fatal("the first send should be allowed")
	}
	b.mu.Lock()
	b.resets = time.Now().Add(-time.Second)
	b.mu.Unlock()

	if !b.take() {
		t.Fatal("a new window should start with a full budget")
	}
}

// A Server built by hand — in a test, or before the config is read — has no
// budget, and that must mean "send" rather than "refuse everything".
func TestNilSMSBudgetIsUnlimited(t *testing.T) {
	var b *smsBudget
	for i := 0; i < 100; i++ {
		if !b.take() {
			t.Fatal("a nil budget must not refuse anything")
		}
	}
}

// The burst is the part that matters: a phone coming back from a tunnel
// refetches every mounted screen at once and must not be answered with errors.
func TestUserLimiterAllowsTheBurstThenRefuses(t *testing.T) {
	l := newUserLimiter(60)

	for i := 0; i < 60; i++ {
		if !l.allow("user-1") {
			t.Fatalf("request %d of a 60-request burst was refused", i+1)
		}
	}
	if l.allow("user-1") {
		t.Fatal("the request past the burst should have been refused")
	}
	if !l.allow("user-2") {
		t.Fatal("another user must have their own allowance")
	}
}

func TestUserLimiterRefills(t *testing.T) {
	l := newUserLimiter(60) // one token a second

	for i := 0; i < 60; i++ {
		l.allow("user-1")
	}
	l.mu.Lock()
	l.buckets["user-1"].last = time.Now().Add(-5 * time.Second)
	l.mu.Unlock()

	for i := 0; i < 5; i++ {
		if !l.allow("user-1") {
			t.Fatalf("five seconds should have refilled five tokens, got %d", i)
		}
	}
	if l.allow("user-1") {
		t.Fatal("a sixth token should not have appeared")
	}
}

// RATE_LIMIT_PER_MIN=0 is the emergency switch, and it has to be a real one.
func TestUserLimiterOffIsUnlimited(t *testing.T) {
	l := newUserLimiter(0)
	if l != nil {
		t.Fatal("a rate of zero should build no limiter at all")
	}
	for i := 0; i < 1000; i++ {
		if !l.allow("user-1") {
			t.Fatal("a nil limiter must not refuse anything")
		}
	}
}

// Buckets that have refilled are dropped rather than kept for every user who
// has ever made a request.
func TestUserLimiterSweepsFullBuckets(t *testing.T) {
	l := newUserLimiter(60)

	l.allow("idle-user")
	l.mu.Lock()
	l.buckets["idle-user"].last = time.Now().Add(-time.Hour)
	l.sweep(time.Now())
	_, still := l.buckets["idle-user"]
	l.mu.Unlock()

	if still {
		t.Fatal("a bucket that has refilled should have been swept")
	}
}
