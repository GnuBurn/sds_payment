package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GnuBurn/sds_payment/internal/payment/core/port"
)

const maxTxAttempts = 3

type UnitOfWork struct {
	pool *pgxpool.Pool
}

var _ port.UnitOfWork = (*UnitOfWork)(nil)

func NewUnitOfWork(pool *pgxpool.Pool) *UnitOfWork {
	return &UnitOfWork{pool: pool}
}

// Do runs fn in a database transaction, retrying on deadlocks and
// serialization failures.
func (u *UnitOfWork) Do(ctx context.Context, fn func(ctx context.Context, r port.Repositories) error) error {
	var err error
	for range maxTxAttempts {
		err = u.once(ctx, fn)
		if code, _ := pgCode(err); code != codeSerializationFailure && code != codeDeadlockDetected {
			return err
		}
	}
	return err
}

func (u *UnitOfWork) once(ctx context.Context, fn func(ctx context.Context, r port.Repositories) error) error {
	tx, err := u.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	repos := port.Repositories{
		Intents: NewIntentRepository(tx),
	}
	if err := fn(ctx, repos); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		return err
	}
	return nil
}
