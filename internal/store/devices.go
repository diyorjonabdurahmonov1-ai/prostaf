package store

import (
	"database/sql"
	"time"
)

// One account, one phone at a time.
//
// The app sends a stable id for the install (header X-Device-Id). The account's
// lock names the phone that currently holds it and when that phone was last
// heard from. A second phone signing in while the first is still in use is
// refused; one that has been quiet long enough simply takes over, and the old
// phone is signed out the next time it speaks.
//
// A build that sends no id is not held to any of this (old installs keep
// working), and neither are the office's admin accounts, which are used from
// several devices on purpose.

const (
	// deviceActiveFor: the app pings about once a minute while it is open, and
	// every request counts. Three minutes of silence means the app was closed.
	deviceActiveFor = 3 * time.Minute
	// deviceOnlineGrace: a worker or firm that is *online* is expected to be
	// reachable even with the app out of sight, so the lock is held a good deal
	// longer for them — but not for ever, or a dead phone would lock the account.
	deviceOnlineGrace = 20 * time.Minute
)

// deviceMayClaim decides whether a phone may take the account's lock.
// It is the whole rule, kept free of the database so it can be tested.
func deviceMayClaim(sameDevice, takeover, online bool, idle time.Duration) bool {
	if sameDevice || takeover {
		return true
	}
	if idle < deviceActiveFor {
		return false
	}
	if online && idle < deviceOnlineGrace {
		return false
	}
	return true
}

// ClaimDevice is a sign-in from this phone. `takeover` is for sign-ins that
// prove the person holds the phone number (an SMS code), which may always take
// the account over. It reports whether the phone now holds the lock.
func (s *Store) ClaimDevice(userID, deviceID string, takeover bool) bool {
	if deviceID == "" {
		return true
	}
	tx, err := s.db.Begin()
	if err != nil {
		return true // never lock somebody out because of our own failure
	}
	defer tx.Rollback()

	tx.Exec(`INSERT INTO device_locks (user_id, device_id, last_seen) VALUES ($1, $2, NOW())
		ON CONFLICT (user_id) DO NOTHING`, userID, deviceID)
	var holder string
	var idleSec float64
	if err := tx.QueryRow(`SELECT device_id, EXTRACT(EPOCH FROM (NOW() - last_seen))
		FROM device_locks WHERE user_id=$1 FOR UPDATE`, userID).Scan(&holder, &idleSec); err != nil {
		return true
	}
	if holder != deviceID {
		online := s.providerOnline(userID)
		if !deviceMayClaim(false, takeover, online, time.Duration(idleSec*float64(time.Second))) {
			return false
		}
	}
	tx.Exec(`UPDATE device_locks SET device_id=$2, last_seen=NOW() WHERE user_id=$1`, userID, deviceID)
	return tx.Commit() == nil
}

// TouchDevice records that this phone is in use and reports whether it still
// holds the account. False means another phone took over: the caller signs this
// one out.
func (s *Store) TouchDevice(userID, deviceID string) bool {
	if deviceID == "" {
		return true
	}
	var holder string
	var idleSec float64
	err := s.db.QueryRow(`SELECT device_id, EXTRACT(EPOCH FROM (NOW() - last_seen))
		FROM device_locks WHERE user_id=$1`, userID).Scan(&holder, &idleSec)
	switch {
	case err == sql.ErrNoRows:
		// Signed in before this existed: the first phone to speak holds it.
		s.db.Exec(`INSERT INTO device_locks (user_id, device_id, last_seen) VALUES ($1, $2, NOW())
			ON CONFLICT (user_id) DO NOTHING`, userID, deviceID)
		return s.deviceHolder(userID) == deviceID
	case err != nil:
		return true
	case holder != deviceID:
		return false
	}
	if idleSec > 20 { // one write every twenty seconds is plenty
		s.db.Exec(`UPDATE device_locks SET last_seen=NOW() WHERE user_id=$1 AND device_id=$2`, userID, deviceID)
	}
	return true
}

func (s *Store) deviceHolder(userID string) string {
	var holder string
	s.db.QueryRow(`SELECT device_id FROM device_locks WHERE user_id=$1`, userID).Scan(&holder)
	return holder
}

// ReleaseDevice is a sign-out: the account is free for any phone straight away.
func (s *Store) ReleaseDevice(userID, deviceID string) {
	if deviceID == "" {
		return
	}
	s.db.Exec(`DELETE FROM device_locks WHERE user_id=$1 AND device_id=$2`, userID, deviceID)
}

// ReleaseAllDevices frees the account whatever phone held it — after a password
// reset, which an SMS code vouched for.
func (s *Store) ReleaseAllDevices(userID string) {
	s.db.Exec(`DELETE FROM device_locks WHERE user_id=$1`, userID)
}

func (s *Store) providerOnline(userID string) bool {
	var on bool
	s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM workers WHERE user_id=$1 AND online)
		OR EXISTS(SELECT 1 FROM companies WHERE user_id=$1 AND online)`, userID).Scan(&on)
	return on
}
