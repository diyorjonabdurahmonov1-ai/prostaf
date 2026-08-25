package api

import (
	"sync"
	"time"
)

// Throttles, and the shape of them is decided by where this runs.
//
// The obvious control — a request cap per IP address — is the wrong one here.
// Uzbek mobile carriers put whole cities behind a handful of NAT addresses, so
// counting requests per address is not a safety net: it is a way to lock a
// thousand subscribers out because one of them was refreshing. Raw flood
// protection belongs in front of the process, in Caddy or the host firewall,
// where dropping a packet is cheap and the decision is not being made about a
// network pretending to be one caller.
//
// That leaves the three things a proxy cannot judge:
//
//   - Guessing. A six-digit code that lives two minutes is a million tries, and
//     nothing counted them: a script could spend the whole window guessing one
//     number's code and expect to be right roughly one time in eight. Counted
//     per phone — the thing a guesser has to hold still — so it does not matter
//     how many addresses they come from.
//   - Spending. Every code sent costs money. smsCooldown is per phone, so a
//     list of numbers walks straight past it; this caps what the platform will
//     send in an hour whoever is asking, so the worst case is a bill with a
//     ceiling rather than a bill.
//   - One account hammering. A logged-in caller is a known user id, not an
//     address, so this can be tight without a shared carrier NAT ever noticing.
//
// All three are counters in memory. That is right for one process and wrong for
// several — a second API container would hand an attacker a second allowance —
// and it is a deliberate trade: the alternative is running Redis on a 4 GB box
// for numbers that reset every hour.

const (
	// otpMaxAttempts is how many wrong codes a phone may offer before the
	// window is shut on it. Five is enough for a misread SMS and nowhere near
	// enough to search a six-digit space.
	otpMaxAttempts = 5
	// loginMaxAttempts is the same for a password. Higher, because a person
	// genuinely does try several of their own passwords before remembering
	// which one they used here.
	loginMaxAttempts = 10
	// authLockout is how long a key that ran out of attempts must wait. Long
	// enough to make a search hopeless — five tries a quarter of an hour is
	// tens of thousands of years of six-digit codes — and short enough that a
	// locked-out user can be told to try again shortly.
	authLockout = 15 * time.Minute
)

// attemptLimiter counts failures against a key and refuses further tries once
// there have been too many of them.
//
// Only failures are counted, and a success clears the count: someone who signs
// in correctly every day never approaches the limit, and someone who does not
// is exactly who this is for.
type attemptLimiter struct {
	mu    sync.Mutex
	tries map[string]*attemptRecord
	max   int
}

type attemptRecord struct {
	n int
	// last is when the most recent failure happened. The count is not a rolling
	// window: it survives until authLockout of quiet has passed, so a guesser
	// pacing themselves at four tries per window still runs out.
	last time.Time
}

func newAttemptLimiter(max int) *attemptLimiter {
	return &attemptLimiter{tries: map[string]*attemptRecord{}, max: max}
}

// locked reports how long the key must wait, and whether it must wait at all.
func (l *attemptLimiter) locked(key string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	rec, ok := l.tries[key]
	if !ok {
		return 0, false
	}
	idle := time.Since(rec.last)
	if left := authLockout - idle; rec.n >= l.max && left > 0 {
		return left, true
	}
	if idle >= authLockout {
		delete(l.tries, key)
	}
	return 0, false
}

// fail records one wrong answer for the key.
func (l *attemptLimiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	rec, ok := l.tries[key]
	if !ok || now.Sub(rec.last) >= authLockout {
		rec = &attemptRecord{}
		l.tries[key] = rec
	}
	rec.n++
	rec.last = now

	// The map only ever holds keys inside a lockout window. Without this it is
	// a list of every phone number that has ever mistyped anything, which grows
	// for as long as the process runs — a memory leak an attacker with a
	// phone-number generator gets to drive.
	for k, r := range l.tries {
		if now.Sub(r.last) >= authLockout {
			delete(l.tries, k)
		}
	}
}

// pass clears the key's record: whoever it is has just proved who they are.
func (l *attemptLimiter) pass(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.tries, key)
}

// otpAttempts and loginAttempts count wrong codes and wrong passwords. Keyed by
// phone number, or by email for the admin login — never by address.
var (
	otpAttempts   = newAttemptLimiter(otpMaxAttempts)
	loginAttempts = newAttemptLimiter(loginMaxAttempts)
)

// smsBudget is a ceiling on how many messages the platform will send in a
// window, whoever is asking for them.
//
// The per-phone cooldown stops one number being used to send a hundred
// messages. It does nothing about a hundred numbers being used to send one
// each, which costs the same and is the version somebody with a generated list
// actually does. This is the number that turns "an attacker can spend the SMS
// balance" into "an attacker can spend an hour of it".
//
// A nil budget is unlimited, which is what a Server assembled by hand in a test
// gets.
type smsBudget struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	sent   int
	resets time.Time
}

func newSMSBudget(max int, window time.Duration) *smsBudget {
	return &smsBudget{max: max, window: window}
}

// take reserves one message, reporting whether there was one left to reserve.
func (b *smsBudget) take() bool {
	if b == nil || b.max <= 0 {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	if now.After(b.resets) {
		b.sent = 0
		b.resets = now.Add(b.window)
	}
	if b.sent >= b.max {
		return false
	}
	b.sent++
	return true
}

// userLimiter is a token bucket per authenticated user: a steady rate with room
// for a burst on top of it.
//
// The burst is what makes this safe to switch on at all. A phone coming back
// from a tunnel does not make one request, it makes a screenful — the socket
// reconnects and every mounted screen refetches at once — and a limiter with no
// slack would answer that entirely reasonable behaviour with errors. So the
// bucket starts full, refills at the steady rate, and only a caller who keeps
// asking past both is refused.
//
// A nil limiter allows everything: RATE_LIMIT_PER_MIN=0 is the switch for
// turning this off in an emergency without a rebuild.
type userLimiter struct {
	mu      sync.Mutex
	buckets map[string]*tokenBucket
	perSec  float64
	burst   float64
}

type tokenBucket struct {
	tokens float64
	last   time.Time
}

func newUserLimiter(perMin int) *userLimiter {
	if perMin <= 0 {
		return nil
	}
	return &userLimiter{
		buckets: map[string]*tokenBucket{},
		perSec:  float64(perMin) / 60,
		// A burst of one minute's allowance. Anything smaller and a reconnect
		// storm from a single phone looks like an attack.
		burst: float64(perMin),
	}
}

// userLimiterSweepAt is how many buckets may accumulate before the idle ones
// are dropped. Ten thousand of them is a few hundred kilobytes, so this is
// housekeeping rather than a limit on how many people may use the platform.
const userLimiterSweepAt = 10000

// allow spends one token for the key.
func (l *userLimiter) allow(key string) bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	b, ok := l.buckets[key]
	if !ok {
		// Buckets are dropped once they have refilled, so the map holds the
		// users who have made a request recently rather than everyone who ever
		// has. Swept here because there is no other moment this code runs.
		if len(l.buckets) > userLimiterSweepAt {
			l.sweep(now)
		}
		b = &tokenBucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * l.perSec
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (l *userLimiter) sweep(now time.Time) {
	for k, b := range l.buckets {
		// Full again, so deleting it and letting the next request start a fresh
		// one is the same answer with less memory.
		if b.tokens+now.Sub(b.last).Seconds()*l.perSec >= l.burst {
			delete(l.buckets, k)
		}
	}
}
