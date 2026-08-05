package store

import (
	"prostaff/internal/id"
	"prostaff/internal/models"
)

// GetWallet returns a user's wallet.
func (s *Store) GetWallet(userID string) (*models.Wallet, bool) {
	w := s.getWallet(userID)
	if w == nil {
		return nil, false
	}
	return w, true
}

// Topup records a payment. When credit is true (dev mode) the amount is applied
// to the balance immediately and the payment is marked successful.
func (s *Store) Topup(userID, provider string, amount int64, credit bool) (*models.Payment, *models.Wallet, error) {
	if amount <= 0 {
		return nil, nil, ErrBadRequest
	}
	if provider != "click" && provider != "payme" {
		return nil, nil, ErrBadRequest
	}
	w := s.getWallet(userID)
	if w == nil {
		return nil, nil, ErrNotFound
	}
	status := "pending"
	if credit {
		s.db.Exec("UPDATE wallets SET balance = balance + $1 WHERE user_id = $2", amount, userID)
		w.Balance += amount
		status = "success"
	}
	p := s.CreatePayment(userID, provider, amount)
	if status == "success" {
		s.db.Exec("UPDATE payments SET status='success' WHERE id=$1", p.ID)
		p.Status = "success"
	} else {
		// CreatePayment already sets "completed", override to pending.
		s.db.Exec("UPDATE payments SET status='pending' WHERE id=$1", p.ID)
		p.Status = "pending"
	}
	return p, w, nil
}

// CommissionPercent exposes the current commission percentage.
func (s *Store) CommissionPercent() int {
	return s.settings.CommissionPercent
}

// deductCommission deducts commission from worker's wallet.
func (s *Store) deductCommission(workerUserID string, amount int64) {
	s.db.Exec("UPDATE wallets SET balance = balance - $1 WHERE user_id = $2", amount, workerUserID)
}

// walletCanAccept checks if a worker has enough balance or credit to accept.
func (s *Store) walletCanAccept(workerUserID string) bool {
	w := s.getWallet(workerUserID)
	if w == nil {
		return false
	}
	return w.Balance >= w.MinBalance || w.CreditLimit > 0
}

// TopupWallet is used by Topup method.
func (s *Store) TopupWallet(userID string, amount int64) {
	s.db.Exec("UPDATE wallets SET balance = balance + $1 WHERE user_id = $2", amount, userID)
}

// SetWalletCreditLimit sets credit limit for a worker's wallet.
func (s *Store) SetWalletCreditLimit(userID string, limit int64) {
	s.db.Exec("UPDATE wallets SET credit_limit = $1 WHERE user_id = $2", limit, userID)
}

// ---- internal ---------------------------------------------------------------

// createWalletIfNotExists ensures a wallet row exists for the user.
func (s *Store) createWalletIfNotExists(userID string) {
	var count int
	s.db.QueryRow("SELECT COUNT(*) FROM wallets WHERE user_id=$1", userID).Scan(&count)
	if count == 0 {
		s.db.Exec("INSERT INTO wallets (user_id, min_balance) VALUES ($1, $2)", userID, s.settings.MinBalance)
	}
}

func init() {
	_ = id.New // ensure import
}
