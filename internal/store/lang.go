package store

import "sync"

// Notification language per account: uz (the default), ru or en.

var (
	langMu    sync.RWMutex
	langCache = map[string]string{}
)

func validLang(l string) bool { return l == "uz" || l == "ru" || l == "en" }

// SetUserLang records the language an account's app is using. Called on every
// authenticated request, so an unchanged value never touches the database.
func (s *Store) SetUserLang(userID, lang string) {
	if userID == "" || !validLang(lang) {
		return
	}
	langMu.RLock()
	cur, ok := langCache[userID]
	langMu.RUnlock()
	if ok && cur == lang {
		return
	}
	if _, err := s.db.Exec(
		`INSERT INTO user_langs (user_id, lang) VALUES ($1, $2)
		 ON CONFLICT (user_id) DO UPDATE SET lang = $2`, userID, lang); err != nil {
		return
	}
	langMu.Lock()
	langCache[userID] = lang
	langMu.Unlock()
}

// UserLang is the account's language, "uz" when it has never said.
func (s *Store) UserLang(userID string) string {
	langMu.RLock()
	l, ok := langCache[userID]
	langMu.RUnlock()
	if ok {
		return l
	}
	l = "uz"
	var stored string
	if s.db.QueryRow("SELECT lang FROM user_langs WHERE user_id=$1", userID).Scan(&stored) == nil && validLang(stored) {
		l = stored
	}
	langMu.Lock()
	langCache[userID] = l
	langMu.Unlock()
	return l
}
