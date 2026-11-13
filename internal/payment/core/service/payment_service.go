package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/GnuBurn/sds_payment/internal/payment/core/domain"
	"github.com/GnuBurn/sds_payment/internal/payment/core/port"
)

const (
	DefaultBillTTL = 15 * time.Minute
	// DefaultSettleAfter is how long an intent may sit in "processing" before
	// a read (e.g. the cashier polling) tries to settle it.
	DefaultSettleAfter = 10 * time.Second

	maxCreateAttempts = 8 // code collisions are rare; this is plenty
	maxPayAttempts    = 3
)

type Deps struct {
	UnitOfWork port.UnitOfWork
	Intents    port.IntentRepository // non-transactional reads
	Wallet     port.WalletClient
	NewID      func() string
	NewCode    func() string
	Now        func() time.Time
	// BillTTL is how long a customer has to pay a new bill.
	BillTTL     time.Duration
	SettleAfter time.Duration
}

type PaymentService struct {
	d Deps
}

var _ port.PaymentService = (*PaymentService)(nil)

func New(d Deps) *PaymentService {
	if d.Now == nil {
		// Postgres stores microseconds; truncating keeps responses consistent.
		d.Now = func() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }
	}
	if d.NewCode == nil {
		d.NewCode = domain.RandomCode
	}
	if d.BillTTL <= 0 {
		d.BillTTL = DefaultBillTTL
	}
	if d.SettleAfter <= 0 {
		d.SettleAfter = DefaultSettleAfter
	}
	return &PaymentService{d: d}
}

// --- cashier / pos-service ---

func (s *PaymentService) CreateBill(ctx context.Context, cmd port.CreateBillCmd) (*domain.Intent, error) {
	if strings.TrimSpace(cmd.UserID) == "" {
		return nil, domain.ErrUnauthenticated
	}
	in, err := domain.NewIntent(domain.NewBill{
		BillID:           cmd.BillID,
		CreatedBy:        cmd.UserID,
		RestaurantName:   cmd.RestaurantName,
		Table:            cmd.Table,
		MerchantWalletID: cmd.MerchantWalletID,
		Items:            cmd.Items,
	}, s.d.Now(), s.d.BillTTL)
	if err != nil {
		return nil, err
	}

	// The shop got this ID from GET /wallets/me; asking wallet-service as
	// the cashier proves the wallet is theirs (403 otherwise).
	w, err := s.d.Wallet.GetWallet(ctx, cmd.UserID, in.MerchantWalletID)
	if err != nil {
		return nil, err
	}
	switch {
	case w.OwnerID != cmd.UserID:
		return nil, domain.ErrForbidden
	case w.Status != "active":
		return nil, domain.ErrWalletInactive
	case w.Currency != domain.Currency:
		return nil, domain.ErrCurrencyMismatch
	}

	for range maxCreateAttempts {
		now := s.d.Now()
		candidate := *in
		candidate.ID = s.d.NewID()
		candidate.Code = s.d.NewCode()
		candidate.CreatedAt, candidate.UpdatedAt = now, now
		candidate.ExpiresAt = now.Add(s.d.BillTTL)

		var result *domain.Intent
		err := s.d.UnitOfWork.Do(ctx, func(ctx context.Context, r port.Repositories) error {
			if err := r.Intents.ExpireStale(ctx, now, candidate.Code, candidate.MerchantWalletID, candidate.BillID); err != nil {
				return err
			}
			open, err := r.Intents.FindOpenForBill(ctx, candidate.MerchantWalletID, candidate.BillID)
			switch {
			case err == nil:
				if open, err = r.Intents.GetForUpdate(ctx, open.ID); err != nil {
					return err
				}
				switch open.EffectiveStatus(now) {
				case domain.StatusPaid:
					return domain.ErrBillAlreadyPaid
				case domain.StatusProcessing:
					return domain.ErrBillBeingPaid
				case domain.StatusPending:
					if open.SameBill(&candidate) {
						result = open // retry of the same request: keep the code
						return nil
					}
					// The order changed: the old code stops working.
					open.Expire(now)
					if err := r.Intents.Update(ctx, open); err != nil {
						return err
					}
				}
			case !errors.Is(err, domain.ErrBillNotFound):
				return err
			}
			if err := r.Intents.Create(ctx, &candidate); err != nil {
				return err
			}
			result = &candidate
			return nil
		})
		// Another live bill drew the same code, or a concurrent request
		// created this bill first: try again (the latter then finds it).
		if errors.Is(err, domain.ErrDuplicateCode) || errors.Is(err, domain.ErrDuplicateBill) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return result, nil
	}
	return nil, fmt.Errorf("could not allocate a bill code after %d attempts", maxCreateAttempts)
}

func (s *PaymentService) GetBill(ctx context.Context, userID, id string) (*domain.Intent, error) {
	if strings.TrimSpace(userID) == "" {
		return nil, domain.ErrUnauthenticated
	}
	in, err := s.d.Intents.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if in.CreatedBy != userID {
		return nil, domain.ErrForbidden
	}
	return s.refresh(ctx, in)
}

// --- customer ---

func (s *PaymentService) GetBillByCode(ctx context.Context, userID, code string) (*domain.Intent, error) {
	if strings.TrimSpace(userID) == "" {
		return nil, domain.ErrUnauthenticated
	}
	code = strings.TrimSpace(code)
	if !domain.ValidCode(code) {
		return nil, domain.ErrBillNotFound
	}
	in, err := s.d.Intents.GetByCode(ctx, code)
	if err != nil {
		return nil, err
	}
	return s.refresh(ctx, in)
}

// PayBill moves the bill total from the customer's wallet to the
// restaurant's and marks the intent paid.
//
// It is safe to retry and safe against concurrent payers:
//   - Claiming (pending -> processing) is done under a row lock and
//     committed before wallet-service is called, so only one customer's
//     attempt is in flight and a crash leaves a durable record of it.
//   - The wallet transfer uses an idempotency key derived from the intent
//     (domain.Intent.TransferKey), so however often it is retried, the bill
//     is charged at most once.
//   - If wallet-service refuses (e.g. insufficient funds), the intent goes
//     back to pending. If the outcome is unknown, it stays processing and
//     the next request settles it by replaying the same transfer.
func (s *PaymentService) PayBill(ctx context.Context, cmd port.PayBillCmd) (*domain.Receipt, error) {
	cmd.UserID = strings.TrimSpace(cmd.UserID)
	cmd.WalletID = strings.TrimSpace(cmd.WalletID)
	cmd.IdempotencyKey = strings.TrimSpace(cmd.IdempotencyKey)
	cmd.Code = strings.TrimSpace(cmd.Code)
	switch {
	case cmd.UserID == "":
		return nil, domain.ErrUnauthenticated
	case cmd.IdempotencyKey == "":
		return nil, domain.ErrMissingIdempotencyKey
	case cmd.WalletID == "":
		return nil, domain.ErrMissingWallet
	case !domain.ValidCode(cmd.Code):
		return nil, domain.ErrBillNotFound
	}

	found, err := s.d.Intents.GetByCode(ctx, cmd.Code)
	if err != nil {
		return nil, err
	}

	for range maxPayAttempts {
		var in *domain.Intent
		err := s.d.UnitOfWork.Do(ctx, func(ctx context.Context, r port.Repositories) error {
			cur, err := r.Intents.GetForUpdate(ctx, found.ID)
			if err != nil {
				return err
			}
			in = cur
			if cur.EffectiveStatus(s.d.Now()) != domain.StatusPending {
				return nil
			}
			if err := cur.Claim(cmd.UserID, cmd.WalletID, cmd.IdempotencyKey, s.d.Now()); err != nil {
				return err
			}
			return r.Intents.Update(ctx, cur)
		})
		if err != nil {
			return nil, err
		}

		mine := in.IsPaymentBy(cmd.UserID, cmd.WalletID, cmd.IdempotencyKey)
		if !mine && in.PayIdempotencyKey == cmd.IdempotencyKey {
			return nil, domain.ErrIdempotencyKeyReused
		}

		switch in.EffectiveStatus(s.d.Now()) {
		case domain.StatusPaid:
			if mine {
				return in.Receipt(), nil // replay of a successful request
			}
			return nil, domain.ErrBillAlreadyPaid
		case domain.StatusExpired:
			return nil, domain.ErrBillExpired
		case domain.StatusProcessing:
			err := s.settle(ctx, in.ID)
			if mine {
				if err != nil {
					return nil, err
				}
				continue // re-read: now paid
			}
			// Someone else's attempt. If it was refused the bill is pending
			// again and we try to claim it; if its outcome is still
			// unknown we can't go ahead.
			if err != nil && !isRefusal(err) {
				return nil, domain.ErrBillBeingPaid
			}
		}
	}
	return nil, domain.ErrBillBeingPaid
}

// settle finishes a processing intent by (re)playing its wallet transfer.
// It returns nil once the intent is no longer processing, the wallet's
// refusal (after putting the intent back to pending), or an error if the
// outcome is still unknown (the intent stays processing).
func (s *PaymentService) settle(ctx context.Context, id string) error {
	// Finish even if the caller hangs up: leaving the intent processing
	// is safe but makes the customer wait.
	ctx = context.WithoutCancel(ctx)

	var refusal error
	err := s.d.UnitOfWork.Do(ctx, func(ctx context.Context, r port.Repositories) error {
		refusal = nil
		in, err := r.Intents.GetForUpdate(ctx, id)
		if err != nil {
			return err
		}
		if in.Status != domain.StatusProcessing {
			return nil // settled by someone else meanwhile
		}
		res, err := s.d.Wallet.Transfer(ctx, port.TransferRequest{
			UserID:         in.PayerUserID,
			FromWalletID:   in.PayerWalletID,
			ToWalletID:     in.MerchantWalletID,
			Amount:         in.Total,
			Note:           in.TransferNote(),
			IdempotencyKey: in.TransferKey(),
		})
		switch {
		case err == nil:
			in.MarkPaid(res.TransactionID, res.BalanceAfter, res.CreatedAt, s.d.Now())
		case isRefusal(err):
			refusal = err
			in.Release(s.d.Now())
		default:
			return err // unknown outcome: roll back, stay processing
		}
		return r.Intents.Update(ctx, in)
	})
	if err != nil {
		return err
	}
	return refusal
}

// refresh settles an intent that has been processing for a while, so a
// customer who gave up mid-payment doesn't leave the bill stuck.
func (s *PaymentService) refresh(ctx context.Context, in *domain.Intent) (*domain.Intent, error) {
	if in.Status != domain.StatusProcessing || s.d.Now().Sub(in.UpdatedAt) < s.d.SettleAfter {
		return in, nil
	}
	if err := s.settle(ctx, in.ID); err != nil && !isRefusal(err) {
		return in, nil // still unknown; report it as processing
	}
	return s.d.Intents.Get(ctx, in.ID)
}

// isRefusal reports whether err is a definitive "no" from wallet-service:
// the transfer did not happen, so the intent can safely go back to pending.
func isRefusal(err error) bool {
	for _, target := range []error{
		domain.ErrForbidden,
		domain.ErrWalletNotFound,
		domain.ErrWalletInactive,
		domain.ErrInsufficientFunds,
		domain.ErrCurrencyMismatch,
		domain.ErrSameWallet,
		domain.ErrPaymentRejected,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}
