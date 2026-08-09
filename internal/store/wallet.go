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

// TopupDev credits a wallet without a gateway, for local development.
//
// This is the whole of what the old Topup did in production too: it invented a
// payment, marked it pending, and handed back a made-up checkout URL. Real
// money now goes through OpenPayment → the gateway → SettlePayment, so this is
// explicitly the development path and named as one — a function called `Topup`
// that quietly credits an account is a thing somebody eventually calls by
// accident.
func (s *Store) TopupDev(userID string, amount int64) (*models.Payment, *models.Wallet, error) {
	if amount <= 0 {
		return nil, nil, ErrBadRequest
	}
	if s.getWallet(userID) == nil {
		return nil, nil, ErrNotFound
	}
	p := s.CreatePayment(userID, "dev", amount, models.PurposeWallet)
	return p, s.CreditWallet(userID, amount), nil
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

// canWorkerAccept is the whole question of whether a worker may take a job:
// either they still have one of their free trial jobs, or their wallet can carry
// the commission.
//
// It reads the counter off the profile rather than the ledger, because the
// profile was scanned with it — the dispatch path asks this about every online
// worker in the city for every order, and a query per worker there is a query
// per worker per twelve seconds.
func (s *Store) canWorkerAccept(wp *models.WorkerProfile) bool {
	if wp == nil {
		return false
	}
	if wp.FreeOrdersLeft() > 0 {
		return true
	}
	return s.walletCanAccept(wp.UserID)
}

// chargeWorkerAcceptance settles a job the worker has just won and returns the
// commission actually taken.
//
// The free trial is spent before money is: that is what makes it a trial. The
// consume is atomic, so if it fails — the last free job went to another order a
// millisecond earlier — this falls through to the wallet rather than handing out
// a fourth free job.
func (s *Store) chargeWorkerAcceptance(wp *models.WorkerProfile, commission int64) int64 {
	if s.consumeFreeOrder(WorkerTrialKey(wp.PassportSeries)) {
		return 0
	}
	s.deductCommission(wp.UserID, commission)
	return commission
}

// chargeCompanyAcceptance settles a job a firm has just accepted.
//
// A firm holding a subscription pays nothing per job — it has already paid for
// the month this job arrived in — so nothing is charged and no trial is spent.
// Only a firm still on its free jobs burns one here.
func (s *Store) chargeCompanyAcceptance(cp *models.CompanyProfile) {
	if cp.SubscriptionActive {
		return
	}
	s.consumeFreeOrder(CompanyTrialKey(cp.INN))
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
