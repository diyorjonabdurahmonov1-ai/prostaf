package store

import (
	"strings"

	"prostaff/internal/models"
)

// The free trial: three jobs a provider may take before the money rules apply.
//
// Somebody the office signed up this morning has an empty wallet, and until this
// existed that meant the account could not be tested at all — a worker was not
// even *rung* by the dispatcher, because candidatesFor drops anybody who cannot
// pay the commission. Three jobs on the house fixes that, but only if they can
// be given away exactly once per person.
//
// So the counter is keyed on the identity document — a worker's passport series,
// a firm's INN — and lives in its own table rather than on the profile row. The
// profile is the thing an admin can delete and re-create; the document is not.
// Delete a worker and open them again under a different phone number and the
// ledger still says three of three used.

// Trial key prefixes. A person who is both a worker and a firm gets one trial
// per hat: the two are separate businesses with separate paperwork.
const (
	trialWorker  = "worker:"
	trialCompany = "company:"
)

// NormalizePassport brings an operator-typed passport series to the one shape
// the unique index sees: no spaces, upper case (AA1234567).
func NormalizePassport(raw string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(raw) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ValidPassport reports whether a normalized series looks like an Uzbek
// passport: two letters and seven digits.
//
// Loose on purpose about *which* letters — a series is not a checksum and the
// office knows the document it is holding better than this function does. It
// only refuses what cannot be a passport at all, so a typo that would silently
// open a second free trial under a near-miss of somebody else's number is
// caught at the form rather than three jobs later.
func ValidPassport(series string) bool {
	if len(series) != 9 {
		return false
	}
	for i, r := range series {
		if i < 2 {
			if r < 'A' || r > 'Z' {
				return false
			}
			continue
		}
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// NormalizeINN keeps the digits of a tax number and nothing else.
func NormalizeINN(raw string) string {
	var b strings.Builder
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ValidINN reports whether a normalized INN is the nine digits Uzbek tax
// numbers carry.
func ValidINN(inn string) bool {
	return len(inn) == 9
}

// FreeOrderLimit is how many trial jobs a provider gets, as the admin has set
// it.
func (s *Store) FreeOrderLimit() int {
	if s.settings.FreeOrderLimit < 0 {
		return 0
	}
	return s.settings.FreeOrderLimit
}

// freeOrdersUsed reads the ledger. A document with no row has used none, which
// is also what an empty document reports — a legacy profile with no passport on
// file has no trial to spend, because there is nothing to stop it being spent
// twice.
func (s *Store) freeOrdersUsed(key string) int {
	if !hasDocument(key) {
		return 0
	}
	var used int
	s.db.QueryRow("SELECT used FROM free_trials WHERE key=$1", key).Scan(&used)
	return used
}

// freeOrdersLeft is how many trial jobs the document still has. Zero for a
// provider with no document on file.
func (s *Store) freeOrdersLeft(key string) int {
	if !hasDocument(key) {
		return 0
	}
	if n := s.FreeOrderLimit() - s.freeOrdersUsed(key); n > 0 {
		return n
	}
	return 0
}

// consumeFreeOrder spends one trial job and reports whether it got one.
//
// A single statement, and the `used < limit` guard is inside it: two accepts
// racing on the same document cannot both come away thinking they took the last
// free job. The insert-then-update shape means a document with no row yet is
// handled by the same call.
func (s *Store) consumeFreeOrder(key string) bool {
	limit := s.FreeOrderLimit()
	if !hasDocument(key) || limit <= 0 {
		return false
	}
	var used int
	err := s.db.QueryRow(`INSERT INTO free_trials (key, used) VALUES ($1, 1)
		ON CONFLICT (key) DO UPDATE SET used = free_trials.used + 1
		WHERE free_trials.used < $2
		RETURNING used`, key, limit).Scan(&used)
	return err == nil
}

// SetFreeOrdersUsed overwrites the counter for a document, so the office can
// undo a mistake (a trial spent on a test order, a worker whose paperwork was
// entered twice) without touching the database by hand.
func (s *Store) SetFreeOrdersUsed(key string, used int) {
	if !hasDocument(key) || used < 0 {
		return
	}
	s.db.Exec(`INSERT INTO free_trials (key, used) VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET used = $2`, key, used)
}

// WorkerTrialKey / CompanyTrialKey are the ledger keys for a provider, built
// from the document on their profile. Empty when there is none.
func WorkerTrialKey(passportSeries string) string {
	if passportSeries == "" {
		return ""
	}
	return trialWorker + passportSeries
}

func CompanyTrialKey(inn string) string {
	if inn == "" {
		return ""
	}
	return trialCompany + inn
}

// hasDocument reports whether a key names an actual document rather than a bare
// prefix — which is what a profile with no passport or INN produces.
func hasDocument(key string) bool {
	switch {
	case key == "":
		return false
	case key == trialWorker, key == trialCompany:
		return false
	}
	return true
}

// passportTaken / innTaken report whether the document already belongs to some
// other profile. `exceptUserID` is the profile being edited, so re-saving a
// worker's own passport is not a clash with themselves.
//
// The unique index is the real guard — these two only exist so the operator gets
// "this passport is already registered" instead of a failed insert.
func (s *Store) passportTaken(series, exceptUserID string) bool {
	return s.documentTaken("workers", "passport_series", series, exceptUserID)
}

func (s *Store) innTaken(inn, exceptUserID string) bool {
	return s.documentTaken("companies", "inn", inn, exceptUserID)
}

func (s *Store) documentTaken(table, column, value, exceptUserID string) bool {
	if value == "" {
		return false
	}
	var n int
	// table/column are compile-time constants from the two callers above, never
	// anything a request supplies.
	s.db.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE "+column+"=$1 AND user_id <> $2",
		value, exceptUserID).Scan(&n)
	return n > 0
}

// TrialKeyForUser resolves whichever document a user has for the given role, so
// admin routes can address the ledger by account id.
func (s *Store) TrialKeyForUser(userID, role string) string {
	switch role {
	case models.RoleCompany:
		if cp, ok := s.getCompanyByUser(userID); ok {
			return CompanyTrialKey(cp.INN)
		}
	case models.RoleWorker:
		if wp, ok := s.GetWorkerByUser(userID); ok {
			return WorkerTrialKey(wp.PassportSeries)
		}
	}
	return ""
}
