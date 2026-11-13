package port

import (
	"context"
	"time"

	"github.com/GnuBurn/sds_payment/internal/payment/core/domain"
)

// Outbound (driven) ports: what the core needs from infrastructure.

type IntentRepository interface {
	// Create stores a new intent. Returns domain.ErrDuplicateCode if another
	// live intent has the same code, or domain.ErrDuplicateBill if the bill
	// already has a live intent.
	Create(ctx context.Context, in *domain.Intent) error
	Get(ctx context.Context, id string) (*domain.Intent, error)
	// GetForUpdate loads the intent and locks it until the unit of work ends.
	GetForUpdate(ctx context.Context, id string) (*domain.Intent, error)
	// GetByCode returns the newest intent with this code.
	GetByCode(ctx context.Context, code string) (*domain.Intent, error)
	// FindOpenForBill returns the newest intent of a bill that is pending,
	// processing or paid, or domain.ErrBillNotFound.
	FindOpenForBill(ctx context.Context, merchantWalletID, billID string) (*domain.Intent, error)
	// ExpireStale marks pending intents whose expiry has passed as expired,
	// limited to those using code or belonging to the given bill, so their
	// code and bill slot can be reused.
	ExpireStale(ctx context.Context, now time.Time, code, merchantWalletID, billID string) error
	Update(ctx context.Context, in *domain.Intent) error
}

// Repositories are the transaction-scoped dependencies handed to a unit of work.
type Repositories struct {
	Intents IntentRepository
}

// UnitOfWork runs fn atomically: everything done through the given
// Repositories is committed together or rolled back together.
type UnitOfWork interface {
	Do(ctx context.Context, fn func(ctx context.Context, r Repositories) error) error
}

// WalletClient talks to wallet-service. Each call acts as UserID (sent as
// X-User-Id), so wallet-service's own ownership checks apply.
//
// Errors are domain errors. A definitive refusal (the transfer did not and
// will not happen) is one of ErrForbidden, ErrWalletNotFound,
// ErrWalletInactive, ErrInsufficientFunds, ErrCurrencyMismatch,
// ErrSameWallet or ErrPaymentRejected. Anything else, including
// ErrWalletUnavailable, means the outcome is unknown.
type WalletClient interface {
	GetWallet(ctx context.Context, userID, walletID string) (*Wallet, error)
	Transfer(ctx context.Context, req TransferRequest) (*TransferResult, error)
}

type Wallet struct {
	ID       string
	OwnerID  string
	Currency string
	Status   string
}

type TransferRequest struct {
	UserID         string
	FromWalletID   string
	ToWalletID     string
	Amount         int64
	Note           string
	IdempotencyKey string
}

type TransferResult struct {
	TransactionID string
	CreatedAt     time.Time
	BalanceAfter  int64 // sender's balance after the transfer
}
