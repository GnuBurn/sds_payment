package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/GnuBurn/sds_payment/internal/payment/core/domain"
	"github.com/GnuBurn/sds_payment/internal/payment/core/port"
)

type IntentRepository struct {
	q querier
}

var _ port.IntentRepository = (*IntentRepository)(nil)

func NewIntentRepository(q querier) *IntentRepository {
	return &IntentRepository{q: q}
}

const intentColumns = `id, bill_id, code, created_by, restaurant_name, table_label, merchant_wallet_id,
	items, total, status, payer_user_id, payer_wallet_id, pay_idempotency_key,
	transaction_id, balance_after, paid_at, expires_at, created_at, updated_at`

func (r *IntentRepository) Create(ctx context.Context, in *domain.Intent) error {
	items, err := json.Marshal(in.Items)
	if err != nil {
		return err
	}
	_, err = r.q.Exec(ctx,
		`INSERT INTO payment_intents (`+intentColumns+`)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)`,
		append([]any{in.ID, in.BillID, in.Code, in.CreatedBy, in.RestaurantName, in.Table,
			in.MerchantWalletID, items, in.Total, string(in.Status)}, mutableArgs(in)...)...,
	)
	if code, constraint := pgCode(err); code == codeUniqueViolation {
		switch constraint {
		case "payment_intents_live_code_key":
			return domain.ErrDuplicateCode
		case "payment_intents_live_bill_key":
			return domain.ErrDuplicateBill
		}
	}
	return err
}

// mutableArgs are the columns Update writes, in intentColumns order after status.
func mutableArgs(in *domain.Intent) []any {
	var balanceAfter *int64
	var paidAt *time.Time
	if in.Status == domain.StatusPaid {
		balanceAfter, paidAt = &in.BalanceAfter, &in.PaidAt
	}
	return []any{
		nullable(in.PayerUserID), nullable(in.PayerWalletID), nullable(in.PayIdempotencyKey),
		nullable(in.TransactionID), balanceAfter, paidAt,
		in.ExpiresAt, in.CreatedAt, in.UpdatedAt,
	}
}

func (r *IntentRepository) Update(ctx context.Context, in *domain.Intent) error {
	tag, err := r.q.Exec(ctx,
		`UPDATE payment_intents
		    SET status = $2, payer_user_id = $3, payer_wallet_id = $4, pay_idempotency_key = $5,
		        transaction_id = $6, balance_after = $7, paid_at = $8,
		        expires_at = $9, created_at = $10, updated_at = $11
		  WHERE id = $1`,
		append([]any{in.ID, string(in.Status)}, mutableArgs(in)...)...,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrBillNotFound
	}
	return nil
}

func (r *IntentRepository) Get(ctx context.Context, id string) (*domain.Intent, error) {
	return r.one(ctx, `SELECT `+intentColumns+` FROM payment_intents WHERE id = $1`, id)
}

func (r *IntentRepository) GetForUpdate(ctx context.Context, id string) (*domain.Intent, error) {
	return r.one(ctx, `SELECT `+intentColumns+` FROM payment_intents WHERE id = $1 FOR UPDATE`, id)
}

func (r *IntentRepository) GetByCode(ctx context.Context, code string) (*domain.Intent, error) {
	return r.one(ctx, `SELECT `+intentColumns+` FROM payment_intents
		WHERE code = $1 ORDER BY created_at DESC, id DESC LIMIT 1`, code)
}

func (r *IntentRepository) FindOpenForBill(ctx context.Context, merchantWalletID, billID string) (*domain.Intent, error) {
	return r.one(ctx, `SELECT `+intentColumns+` FROM payment_intents
		WHERE merchant_wallet_id = $1 AND bill_id = $2 AND status IN ('pending', 'processing', 'paid')
		ORDER BY created_at DESC, id DESC LIMIT 1`, merchantWalletID, billID)
}

func (r *IntentRepository) ExpireStale(ctx context.Context, now time.Time, code, merchantWalletID, billID string) error {
	_, err := r.q.Exec(ctx,
		`UPDATE payment_intents SET status = 'expired', updated_at = $1
		  WHERE status = 'pending' AND expires_at <= $1
		    AND (code = $2 OR (merchant_wallet_id = $3 AND bill_id = $4))`,
		now, code, merchantWalletID, billID)
	return err
}

func (r *IntentRepository) one(ctx context.Context, sql string, args ...any) (*domain.Intent, error) {
	in, err := scanIntent(r.q.QueryRow(ctx, sql, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrBillNotFound
	}
	if code, _ := pgCode(err); code == codeInvalidText {
		return nil, domain.ErrBillNotFound // not a valid UUID, so it can't exist
	}
	return in, err
}

func scanIntent(row pgx.Row) (*domain.Intent, error) {
	var (
		in                     domain.Intent
		status                 string
		items                  []byte
		payerUser, payerWallet *string
		payKey, txID           *string
		balanceAfter           *int64
		paidAt                 *time.Time
	)
	if err := row.Scan(&in.ID, &in.BillID, &in.Code, &in.CreatedBy, &in.RestaurantName, &in.Table,
		&in.MerchantWalletID, &items, &in.Total, &status, &payerUser, &payerWallet, &payKey,
		&txID, &balanceAfter, &paidAt, &in.ExpiresAt, &in.CreatedAt, &in.UpdatedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(items, &in.Items); err != nil {
		return nil, err
	}
	in.Status = domain.Status(status)
	in.PayerUserID = deref(payerUser)
	in.PayerWalletID = deref(payerWallet)
	in.PayIdempotencyKey = deref(payKey)
	in.TransactionID = deref(txID)
	if balanceAfter != nil {
		in.BalanceAfter = *balanceAfter
	}
	if paidAt != nil {
		in.PaidAt = paidAt.UTC()
	}
	in.ExpiresAt = in.ExpiresAt.UTC()
	in.CreatedAt = in.CreatedAt.UTC()
	in.UpdatedAt = in.UpdatedAt.UTC()
	return &in, nil
}
