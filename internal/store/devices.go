package store

import (
	"database/sql"
	"time"
)

// A few phones per account — one for most people, four for a firm.
//
// The app sends a stable id for the install (header X-Device-Id). The account
// keeps a row per phone with when it was last heard from. A phone that is not
// on the list may join only while there is room: a worker or customer has one
// place, a firm (a team, with several people on the road) has four. A phone
// that has been quiet long enough gives up its place, and one that has been
// pushed out is signed out the next time it speaks.
//
// A build that sends no id is not held to any of this (old installs keep
// working), and neither are the office's admin accounts, which are used from
// several devices on purpose.

const (
	// deviceActiveFor: the app pings about once a minute while it is open, and
	// every request counts. Three minutes of silence means the app was closed.
	deviceActiveFor = 3 * time.Minute
	// deviceOnlineGrace: a worker or firm that is *online* is expected to be
	// reachable even with the app out of sight, so a place is held a good deal
	// longer for them — but not for ever, or a dead phone would hold it for good.
	deviceOnlineGrace = 20 * time.Minute

	// How many phones one account may have in use at once.
	phonesPerAccount = 1
	phonesPerFirm    = 4
)

// deviceStillHolds reports whether a phone last heard from `idle` ago still
// holds its place. Free of the database so the rule can be tested.
func deviceStillHolds(online bool, idle time.Duration) bool {
	if idle < deviceActiveFor {
		return true
	}
	return online && idle < deviceOnlineGrace
}

// DeviceLimit is how many phones this account may use at once.
func (s *Store) DeviceLimit(userID string) int {
	if _, ok := s.getCompanyByUser(userID); ok {
		return phonesPerFirm
	}
	return phonesPerAccount
}

type deviceRow struct {
	id   string
	idle time.Duration
}

func (s *Store) deviceRows(q interface {
	Query(string, ...any) (*sql.Rows, error)
}, userID string) []deviceRow {
	rows, err := q.Query(`SELECT device_id, EXTRACT(EPOCH FROM (NOW() - last_seen))
		FROM device_locks WHERE user_id=$1 ORDER BY last_seen DESC`, userID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []deviceRow
	for rows.Next() {
		var id string
		var sec float64
		if rows.Scan(&id, &sec) == nil {
			out = append(out, deviceRow{id, time.Duration(sec * float64(time.Second))})
		}
	}
	return out
}

// ClaimDevice is a sign-in from this phone. `takeover` is for sign-ins that
// prove the person holds the phone number (an SMS code): when the account is
// full they push out the phone that has been quiet longest. It reports whether
// the phone now has a place.
func (s *Store) ClaimDevice(userID, deviceID string, takeover bool) bool {
	if deviceID == "" {
		return true
	}
	tx, err := s.db.Begin()
	if err != nil {
		return true // never lock somebody out because of our own failure
	}
	defer tx.Rollback()

	// Serialise claims for one account.
	tx.Exec(`SELECT pg_advisory_xact_lock(hashtext($1))`, userID)

	online := s.providerOnline(userID)
	rows := s.deviceRows(tx, userID)
	holding := 0
	for _, r := range rows {
		if r.id == deviceID {
			tx.Exec(`UPDATE device_locks SET last_seen=NOW() WHERE user_id=$1 AND device_id=$2`, userID, deviceID)
			return tx.Commit() == nil
		}
		if deviceStillHolds(online, r.idle) {
			holding++
		}
	}
	if holding >= s.DeviceLimit(userID) {
		if !takeover {
			return false
		}
		// Push out the longest-quiet phones until there is room.
		for i := len(rows) - 1; i >= 0 && holding >= s.DeviceLimit(userID); i-- {
			tx.Exec(`DELETE FROM device_locks WHERE user_id=$1 AND device_id=$2`, userID, rows[i].id)
			if deviceStillHolds(online, rows[i].idle) {
				holding--
			}
		}
	}
	// Places given up by quiet phones are cleared out, so the list stays short.
	for _, r := range rows {
		if !deviceStillHolds(online, r.idle) {
			tx.Exec(`DELETE FROM device_locks WHERE user_id=$1 AND device_id=$2`, userID, r.id)
		}
	}
	tx.Exec(`INSERT INTO device_locks (user_id, device_id, last_seen) VALUES ($1, $2, NOW())
		ON CONFLICT (user_id, device_id) DO UPDATE SET last_seen=NOW()`, userID, deviceID)
	return tx.Commit() == nil
}

// TouchDevice records that this phone is in use and reports whether it still
// has its place. False means other phones filled the account: the caller signs
// this one out.
func (s *Store) TouchDevice(userID, deviceID string) bool {
	if deviceID == "" {
		return true
	}
	var idleSec float64
	err := s.db.QueryRow(`SELECT EXTRACT(EPOCH FROM (NOW() - last_seen))
		FROM device_locks WHERE user_id=$1 AND device_id=$2`, userID, deviceID).Scan(&idleSec)
	switch {
	case err == sql.ErrNoRows:
		// Not on the list: signed in before this existed, or pushed out. It gets
		// a place only if there is room, same as a new sign-in.
		return s.ClaimDevice(userID, deviceID, false)
	case err != nil:
		return true
	}
	if idleSec > 20 { // one write every twenty seconds is plenty
		s.db.Exec(`UPDATE device_locks SET last_seen=NOW() WHERE user_id=$1 AND device_id=$2`, userID, deviceID)
	}
	return true
}

// ReleaseDevice is a sign-out: this phone's place is free straight away.
func (s *Store) ReleaseDevice(userID, deviceID string) {
	if deviceID == "" {
		return
	}
	s.db.Exec(`DELETE FROM device_locks WHERE user_id=$1 AND device_id=$2`, userID, deviceID)
}

// ReleaseAllDevices frees the account whatever phones held it — after a
// password reset, which an SMS code vouched for.
func (s *Store) ReleaseAllDevices(userID string) {
	s.db.Exec(`DELETE FROM device_locks WHERE user_id=$1`, userID)
}

func (s *Store) providerOnline(userID string) bool {
	var on bool
	s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM workers WHERE user_id=$1 AND online)
		OR EXISTS(SELECT 1 FROM companies WHERE user_id=$1 AND online)`, userID).Scan(&on)
	return on
}

// HasActiveDevice reports whether any phone still holds a place on the account.
func (s *Store) HasActiveDevice(userID string) bool {
	online := s.providerOnline(userID)
	for _, r := range s.deviceRows(s.db, userID) {
		if deviceStillHolds(online, r.idle) {
			return true
		}
	}
	return false
}

// SignedOut is what leaving the app for good means for a provider: offline, at
// once, and no more rings on a phone that is no longer signed in. Orders used to
// keep arriving for a worker who had pressed "exit", because the server never
// heard that they had gone.
func (s *Store) SignedOut(userID string) {
	if _, ok := s.GetWorkerByUser(userID); ok {
		s.SetWorkerStatus(userID, false, nil, nil)
	}
	if _, ok := s.getCompanyByUser(userID); ok {
		s.SetCompanyStatus(userID, false)
	}
	s.db.Exec("UPDATE users SET push_token='', fcm_token='' WHERE id=$1", userID)
}
