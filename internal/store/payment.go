package store

import (
	"database/sql"
	"strconv"
	"time"

	"prostaff/internal/id"
	"prostaff/internal/models"
)

// The payment ledger.
//
// Money arriving is the one place in this application where being wrong twice
// is much worse than being wrong once: a gateway retries its callback, a payer
// reloads the invoice page, a network blip makes the same notification arrive
// three times. So settlement is a single guarded UPDATE — the row moves from
// pending to success exactly once, and the second caller is told "nothing to
// do" rather than credited again.
//
// Nothing here talks to Atmos. This file knows only that a payment was opened
// somewhere, and that something later claimed it was paid; the api layer is
// what checks that claim's signature before calling in.

const paymentCols = `id, user_id, provider, amount, status, created_at,
	COALESCE(purpose, 'wallet'), COALESCE(months, 0), COALESCE(external_id, ''), paid_at,
	COALESCE(error, '')`

// paymentColsP is paymentCols qualified for the admin join, where `users` and
// `companies` are also in scope and a bare `id` would be ambiguous.
//
// Written out rather than derived from the constant above by string surgery:
// the two are read side by side, and a list of column names is a thing to check
// with your eyes, not a thing to compute.
const paymentColsP = `p.id, p.user_id, p.provider, p.amount, p.status, p.created_at,
	COALESCE(p.purpose, 'wallet'), COALESCE(p.months, 0), COALESCE(p.external_id, ''), p.paid_at,
	COALESCE(p.error, '')`

func scanPayment(row interface{ Scan(dest ...any) error }) (*models.Payment, error) {
	var p models.Payment
	var paidAt sql.NullTime
	err := row.Scan(&p.ID, &p.UserID, &p.Provider, &p.Amount, &p.Status, &p.CreatedAt,
		&p.Purpose, &p.Months, &p.ExternalID, &paidAt, &p.Error)
	if err != nil {
		return nil, err
	}
	if paidAt.Valid {
		t := paidAt.Time
		p.PaidAt = &t
	}
	return &p, nil
}

// PaymentRequest is a payment about to be opened at the gateway.
type PaymentRequest struct {
	UserID   string
	Provider string
	Amount   int64
	Purpose  string
	// Months is only meaningful for a subscription payment, and is what will be
	// added to the firm when it settles.
	Months int
}

// OpenPayment records a payment as pending, before the payer has been sent
// anywhere.
//
// The row exists first on purpose: if the gateway call fails, or the process
// dies between opening the transaction and writing it down, the worst case is a
// pending row nobody ever pays. The other order — send them to pay, then write
// it down — loses money that has actually moved.
func (s *Store) OpenPayment(in PaymentRequest) (*models.Payment, error) {
	if in.Amount <= 0 {
		return nil, ErrBadRequest
	}
	if _, ok := s.GetUser(in.UserID); !ok {
		return nil, ErrNotFound
	}
	purpose := in.Purpose
	if purpose != models.PurposeSubscription {
		purpose = models.PurposeWallet
	}
	p := &models.Payment{
		ID:        id.New(),
		UserID:    in.UserID,
		Provider:  in.Provider,
		Amount:    in.Amount,
		Status:    models.PaymentPending,
		Purpose:   purpose,
		Months:    in.Months,
		CreatedAt: time.Now(),
	}
	if _, err := s.db.Exec(`INSERT INTO payments
		(id, user_id, provider, amount, status, created_at, purpose, months)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		p.ID, p.UserID, p.Provider, p.Amount, p.Status, p.CreatedAt, p.Purpose, p.Months); err != nil {
		return nil, ErrConflict
	}
	return p, nil
}

// AttachExternalID records the gateway's transaction id against a payment, so
// the callback that arrives under that id can find its way home.
func (s *Store) AttachExternalID(paymentID, externalID string) error {
	if externalID == "" {
		return ErrBadRequest
	}
	res, err := s.db.Exec("UPDATE payments SET external_id=$1 WHERE id=$2", externalID, paymentID)
	if err != nil {
		return ErrConflict
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetPayment returns one payment. Used by the app to ask "did my money land?"
// after coming back from the invoice page — the browser returning proves
// nothing, only the callback does.
func (s *Store) GetPayment(paymentID string) (*models.Payment, bool) {
	p, err := scanPayment(s.db.QueryRow("SELECT "+paymentCols+" FROM payments WHERE id=$1", paymentID))
	if err != nil {
		return nil, false
	}
	return p, true
}

// GetPaymentByExternalID finds the payment a gateway callback belongs to.
func (s *Store) GetPaymentByExternalID(externalID string) (*models.Payment, bool) {
	if externalID == "" {
		return nil, false
	}
	p, err := scanPayment(s.db.QueryRow(
		"SELECT "+paymentCols+" FROM payments WHERE external_id=$1", externalID))
	if err != nil {
		return nil, false
	}
	return p, true
}

// SettlePayment flips a pending payment to success and returns it — once.
//
// The guard is inside the statement, so two callbacks racing (a gateway retry
// arriving while the first is still being handled) cannot both come away
// holding a payment to credit. `ok` false means either there is no such payment
// or it was already settled; both are "do nothing", which is exactly right for
// a duplicate delivery.
func (s *Store) SettlePayment(externalID string) (*models.Payment, bool) {
	if externalID == "" {
		return nil, false
	}
	p, err := scanPayment(s.db.QueryRow(`UPDATE payments
		SET status=$1, paid_at=NOW()
		WHERE external_id=$2 AND status=$3
		RETURNING `+paymentCols, models.PaymentSuccess, externalID, models.PaymentPending))
	if err != nil {
		return nil, false
	}
	return p, true
}

// CancelPayment marks a payment as not going to happen. Only ever moves a
// pending row, so a cancellation that arrives after the money did is ignored.
func (s *Store) CancelPayment(externalID string) {
	s.db.Exec("UPDATE payments SET status=$1 WHERE external_id=$2 AND status=$3",
		models.PaymentCanceled, externalID, models.PaymentPending)
}

// maxPaymentError keeps a gateway's reply from filling a column. Long enough to
// diagnose, short enough that a page of provider HTML does not end up in the
// admin list.
const maxPaymentError = 400

// FailPayment marks a pending payment as never going to happen, and says why.
//
// Only a pending row moves: a failure arriving after the money did — a retry, a
// late timeout — must not undo a settled payment.
func (s *Store) FailPayment(paymentID, reason string) {
	s.db.Exec("UPDATE payments SET status=$1, error=$2 WHERE id=$3 AND status=$4",
		models.PaymentFailed, truncate(reason, maxPaymentError), paymentID, models.PaymentPending)
}

// NotePaymentError records something odd about a payment without changing what
// it is.
//
// For the cases where the money is not in doubt but something else is: a
// callback whose signature did not verify, an amount that disagreed with what
// was asked for, months that were paid for and could not be applied. The office
// needs to see all three, and none of them is a reason to move the status.
func (s *Store) NotePaymentError(externalID, reason string) {
	if externalID == "" {
		return
	}
	s.db.Exec("UPDATE payments SET error=$1 WHERE external_id=$2",
		truncate(reason, maxPaymentError), externalID)
}

// NotePaymentErrorByID is NotePaymentError for a payment addressed by our own
// id — used where the gateway's id was never obtained.
func (s *Store) NotePaymentErrorByID(paymentID, reason string) {
	s.db.Exec("UPDATE payments SET error=$1 WHERE id=$2",
		truncate(reason, maxPaymentError), paymentID)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// AdminPayment is a payment with the person behind it — the office's log reads
// names, not account ids.
type AdminPayment struct {
	models.Payment
	UserName    string `json:"user_name"`
	UserPhone   string `json:"user_phone"`
	CompanyName string `json:"company_name,omitempty"`
}

// ListAllPayments is every payment, newest first, optionally narrowed.
//
// `status` accepts the stored values plus "problem", which is the filter the
// office actually wants: everything that failed or settled with something
// wrong about it. Hunting for those by eye through a list of successes is how
// a firm's complaint goes unanswered for a week.
func (s *Store) ListAllPayments(status, purpose string, limit int) []AdminPayment {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	q := `SELECT ` + paymentColsP + `,
		COALESCE(u.name,''), COALESCE(u.phone,''), COALESCE(c.name,'')
		FROM payments p
		LEFT JOIN users u ON u.id = p.user_id
		LEFT JOIN companies c ON c.user_id = p.user_id
		WHERE 1=1`
	args := []any{}
	switch status {
	case "":
		// everything
	case "problem":
		q += ` AND (p.status = 'failed' OR COALESCE(p.error,'') <> '')`
	default:
		args = append(args, status)
		q += ` AND p.status = $1`
	}
	if purpose != "" {
		args = append(args, purpose)
		q += ` AND COALESCE(p.purpose,'wallet') = $` + itoa(len(args))
	}
	args = append(args, limit)
	q += ` ORDER BY p.created_at DESC LIMIT $` + itoa(len(args))

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return []AdminPayment{}
	}
	defer rows.Close()

	out := []AdminPayment{}
	for rows.Next() {
		var a AdminPayment
		var paidAt sql.NullTime
		if rows.Scan(&a.ID, &a.UserID, &a.Provider, &a.Amount, &a.Status, &a.CreatedAt,
			&a.Purpose, &a.Months, &a.ExternalID, &paidAt, &a.Error,
			&a.UserName, &a.UserPhone, &a.CompanyName) != nil {
			continue
		}
		if paidAt.Valid {
			t := paidAt.Time
			a.PaidAt = &t
		}
		out = append(out, a)
	}
	return out
}

// PaymentTotals is the health of the money coming in: what has settled, what is
// still in flight, and what went wrong.
type PaymentTotals struct {
	Settled  int64 `json:"settled"`
	Pending  int   `json:"pending"`
	Failed   int   `json:"failed"`
	Problems int   `json:"problems"`
}

// PaymentOverview counts the payment log.
func (s *Store) PaymentOverview() PaymentTotals {
	var t PaymentTotals
	s.db.QueryRow("SELECT COALESCE(SUM(amount),0) FROM payments WHERE status='success'").Scan(&t.Settled)
	s.db.QueryRow("SELECT COUNT(*) FROM payments WHERE status='pending'").Scan(&t.Pending)
	s.db.QueryRow("SELECT COUNT(*) FROM payments WHERE status='failed'").Scan(&t.Failed)
	// Anything carrying a note, whatever its status — a settled payment with a
	// mismatch on it is still something somebody has to look at.
	s.db.QueryRow("SELECT COUNT(*) FROM payments WHERE COALESCE(error,'') <> ''").Scan(&t.Problems)
	return t
}

func itoa(n int) string { return strconv.Itoa(n) }

// CreditWallet applies a settled wallet top-up.
func (s *Store) CreditWallet(userID string, amount int64) *models.Wallet {
	s.createWalletIfNotExists(userID)
	s.TopupWallet(userID, amount)
	w, _ := s.GetWallet(userID)
	return w
}

// ListUserPayments returns one account's payment history, newest first.
func (s *Store) ListUserPayments(userID string, limit int) []models.Payment {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.Query("SELECT "+paymentCols+
		" FROM payments WHERE user_id=$1 ORDER BY created_at DESC LIMIT $2", userID, limit)
	if err != nil {
		return []models.Payment{}
	}
	defer rows.Close()
	out := []models.Payment{}
	for rows.Next() {
		if p, err := scanPayment(rows); err == nil {
			out = append(out, *p)
		}
	}
	return out
}
