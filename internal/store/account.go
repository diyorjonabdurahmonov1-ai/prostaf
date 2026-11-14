package store

import (
	"log"

	"prostaff/internal/models"
)

// DeletedName is what an erased account is called wherever its name would
// still be read: on the other side's old orders, in a list of reviews.
const DeletedName = "O'chirilgan foydalanuvchi"

// DeleteAccount erases a person's account, as the app stores require anyone
// who can create one to be able to do.
//
// What is erased: the name, phone number, e-mail, photo, password and every
// session; the worker's or firm's profile; every notification; every photo the
// person uploaded; the customer's name and phone on their old orders and the
// worker's phone on theirs; the text of reviews they wrote.
//
// What is kept, without anything that names the person: the user row itself,
// renamed and switched off (orders and payments point at it, and the books have
// to balance), the order history, the payment and wallet records, and the
// star ratings other people gave. The trial ledger is kept too — it is keyed on
// the passport and exists so that deleting an account and registering again
// does not hand out the free jobs a second time.
//
// A person with a job in progress cannot be deleted: the other side is waiting
// on them. That is ErrActiveOrders, and it ends when the job does.
func (s *Store) DeleteAccount(userID string) error {
	u, ok := s.GetUser(userID)
	if !ok {
		return ErrNotFound
	}
	if u.HasRole(models.RoleAdmin) {
		return ErrAdminAccount
	}

	var active int
	if err := s.db.QueryRow(`
		SELECT COUNT(*) FROM orders WHERE
		  (customer_id = $1 AND status IN ('pending','matching','invited','accepted','in_progress'))
		  OR (worker_id = $1 AND status IN ('accepted','in_progress'))
		  OR (company_id IN (SELECT id FROM companies WHERE user_id = $1)
		      AND status IN ('pending','accepted','in_progress'))`, userID).Scan(&active); err != nil {
		return err
	}
	if active > 0 {
		return ErrActiveOrders
	}

	// The paths of the photos, read before their rows go, so the files can be
	// removed from the disk once the database no longer names them.
	var paths []string
	if rows, err := s.db.Query("SELECT COALESCE(path, '') FROM files WHERE user_id = $1", userID); err == nil {
		for rows.Next() {
			var p string
			if rows.Scan(&p) == nil && p != "" {
				paths = append(paths, p)
			}
		}
		rows.Close()
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	steps := []struct {
		sql  string
		args []any
	}{
		{"DELETE FROM refresh_tokens WHERE user_id = $1", []any{userID}},
		{"DELETE FROM notifications WHERE user_id = $1", []any{userID}},
		{"DELETE FROM otps WHERE phone = $1", []any{u.Phone}},
		{"DELETE FROM workers WHERE user_id = $1", []any{userID}},
		{"DELETE FROM companies WHERE user_id = $1", []any{userID}},
		{"DELETE FROM files WHERE user_id = $1", []any{userID}},
		{"UPDATE orders SET customer_phone = '', customer_name = '' WHERE customer_id = $1", []any{userID}},
		{"UPDATE orders SET worker_phone = '' WHERE worker_id = $1", []any{userID}},
		{"UPDATE ratings SET comment = '' WHERE from_user_id = $1", []any{userID}},
		// Renamed, switched off and given a phone number nobody can dial, which
		// also frees the real one: the person may register again.
		{`UPDATE users SET name = $2, phone = 'deleted-' || id, gmail = '', avatar_url = '',
		    push_token = '', fcm_token = '', push_channel = '', password_hash = '',
		    roles = '["customer"]', active = FALSE WHERE id = $1`, []any{userID, DeletedName}},
	}
	for _, step := range steps {
		if _, err := tx.Exec(step.sql, step.args...); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	for _, p := range paths {
		if err := s.blobs.Remove(p); err != nil {
			log.Printf("account deletion: leaving %s on disk: %v", p, err)
		}
	}
	return nil
}
