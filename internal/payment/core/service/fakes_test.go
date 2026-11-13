package service

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/GnuBurn/sds_payment/internal/payment/core/domain"
	"github.com/GnuBurn/sds_payment/internal/payment/core/port"
)

// memStore is an in-memory IntentRepository + UnitOfWork. The unit of work
// works on a copy and only swaps it in on success, which gives the same
// all-or-nothing behaviour as a DB transaction.
type memStore struct {
	mu      sync.Mutex
	intents map[string]domain.Intent
}

func newMemStore() *memStore {
	return &memStore{intents: map[string]domain.Intent{}}
}

func (m *memStore) Do(ctx context.Context, fn func(context.Context, port.Repositories) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	work := &memRepo{intents: map[string]domain.Intent{}}
	for k, v := range m.intents {
		work.intents[k] = v
	}
	if err := fn(ctx, port.Repositories{Intents: work}); err != nil {
		return err
	}
	m.intents = work.intents
	return nil
}

// reader serves non-transactional reads. It looks up m.intents on every
// call because Do swaps the map on commit.
type reader struct{ m *memStore }

func (r reader) repo() *memRepo { return &memRepo{intents: r.m.intents, mu: &r.m.mu} }

func (r reader) Create(ctx context.Context, in *domain.Intent) error { return r.repo().Create(ctx, in) }
func (r reader) Get(ctx context.Context, id string) (*domain.Intent, error) {
	return r.repo().Get(ctx, id)
}
func (r reader) GetForUpdate(ctx context.Context, id string) (*domain.Intent, error) {
	return r.repo().Get(ctx, id)
}
func (r reader) GetByCode(ctx context.Context, code string) (*domain.Intent, error) {
	return r.repo().GetByCode(ctx, code)
}
func (r reader) FindOpenForBill(ctx context.Context, w, b string) (*domain.Intent, error) {
	return r.repo().FindOpenForBill(ctx, w, b)
}
func (r reader) ExpireStale(ctx context.Context, now time.Time, c, w, b string) error {
	return r.repo().ExpireStale(ctx, now, c, w, b)
}
func (r reader) Update(ctx context.Context, in *domain.Intent) error { return r.repo().Update(ctx, in) }

type memRepo struct {
	intents map[string]domain.Intent
	mu      *sync.Mutex // set for non-transactional reads only
}

func (r *memRepo) lock() func() {
	if r.mu == nil {
		return func() {}
	}
	r.mu.Lock()
	return r.mu.Unlock
}

func copyIntent(in domain.Intent) *domain.Intent {
	in.Items = slices.Clone(in.Items)
	return &in
}

func live(s domain.Status) bool { return s == domain.StatusPending || s == domain.StatusProcessing }

func (r *memRepo) Create(_ context.Context, in *domain.Intent) error {
	defer r.lock()()
	for _, other := range r.intents {
		if !live(other.Status) {
			continue
		}
		if other.Code == in.Code {
			return domain.ErrDuplicateCode
		}
		if other.MerchantWalletID == in.MerchantWalletID && other.BillID == in.BillID {
			return domain.ErrDuplicateBill
		}
	}
	r.intents[in.ID] = *copyIntent(*in)
	return nil
}

func (r *memRepo) Get(_ context.Context, id string) (*domain.Intent, error) {
	defer r.lock()()
	in, ok := r.intents[id]
	if !ok {
		return nil, domain.ErrBillNotFound
	}
	return copyIntent(in), nil
}

func (r *memRepo) GetForUpdate(ctx context.Context, id string) (*domain.Intent, error) {
	return r.Get(ctx, id)
}

func (r *memRepo) newest(match func(domain.Intent) bool) (*domain.Intent, error) {
	defer r.lock()()
	var best *domain.Intent
	for _, in := range r.intents {
		if match(in) && (best == nil || in.CreatedAt.After(best.CreatedAt) ||
			(in.CreatedAt.Equal(best.CreatedAt) && in.ID > best.ID)) {
			best = copyIntent(in)
		}
	}
	if best == nil {
		return nil, domain.ErrBillNotFound
	}
	return best, nil
}

func (r *memRepo) GetByCode(_ context.Context, code string) (*domain.Intent, error) {
	return r.newest(func(in domain.Intent) bool { return in.Code == code })
}

func (r *memRepo) FindOpenForBill(_ context.Context, walletID, billID string) (*domain.Intent, error) {
	return r.newest(func(in domain.Intent) bool {
		return in.MerchantWalletID == walletID && in.BillID == billID && in.Status != domain.StatusExpired
	})
}

func (r *memRepo) ExpireStale(_ context.Context, now time.Time, code, walletID, billID string) error {
	defer r.lock()()
	for id, in := range r.intents {
		if in.Status == domain.StatusPending && !in.ExpiresAt.After(now) &&
			(in.Code == code || (in.MerchantWalletID == walletID && in.BillID == billID)) {
			in.Status = domain.StatusExpired
			r.intents[id] = in
		}
	}
	return nil
}

func (r *memRepo) Update(_ context.Context, in *domain.Intent) error {
	defer r.lock()()
	if _, ok := r.intents[in.ID]; !ok {
		return domain.ErrBillNotFound
	}
	r.intents[in.ID] = *copyIntent(*in)
	return nil
}

// fakeWallet mimics wallet-service: ownership checks, balances and
// idempotent transfers keyed by Idempotency-Key.
type fakeWallet struct {
	mu        sync.Mutex
	wallets   map[string]*fakeWalletRow
	done      map[string]port.TransferRequest // by idempotency key
	results   map[string]*port.TransferResult
	transfers int // transfers actually applied
	calls     int
	// failNext makes the next n Transfer calls return err. If applyFirst is
	// set, the transfer is applied before failing (response lost).
	failNext   int
	failErr    error
	applyFirst bool
	seq        int
}

type fakeWalletRow struct {
	owner, currency, status string
	balance                 int64
}

func newFakeWallet() *fakeWallet {
	return &fakeWallet{
		wallets: map[string]*fakeWalletRow{},
		done:    map[string]port.TransferRequest{},
		results: map[string]*port.TransferResult{},
	}
}

func (f *fakeWallet) add(id, owner string, balance int64) {
	f.wallets[id] = &fakeWalletRow{owner: owner, currency: "THB", status: "active", balance: balance}
}

func (f *fakeWallet) balance(id string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.wallets[id].balance
}

func (f *fakeWallet) GetWallet(_ context.Context, userID, walletID string) (*port.Wallet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w, ok := f.wallets[walletID]
	if !ok {
		return nil, domain.ErrWalletNotFound
	}
	if w.owner != userID {
		return nil, domain.ErrForbidden
	}
	return &port.Wallet{ID: walletID, OwnerID: w.owner, Currency: w.currency, Status: w.status}, nil
}

func (f *fakeWallet) Transfer(_ context.Context, req port.TransferRequest) (*port.TransferResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.failNext > 0 && !f.applyFirst {
		f.failNext--
		return nil, f.failErr
	}
	res, err := f.apply(req)
	if err == nil && f.failNext > 0 {
		f.failNext--
		return nil, f.failErr
	}
	return res, err
}

func (f *fakeWallet) apply(req port.TransferRequest) (*port.TransferResult, error) {
	if prev, ok := f.done[req.IdempotencyKey]; ok {
		if prev != req {
			return nil, domain.ErrWalletUnavailable // 409 from wallet-service
		}
		return f.results[req.IdempotencyKey], nil
	}
	from, ok := f.wallets[req.FromWalletID]
	if !ok {
		return nil, domain.ErrWalletNotFound
	}
	if from.owner != req.UserID {
		return nil, domain.ErrForbidden
	}
	to, ok := f.wallets[req.ToWalletID]
	if !ok {
		return nil, domain.ErrWalletNotFound
	}
	if req.FromWalletID == req.ToWalletID {
		return nil, domain.ErrSameWallet
	}
	if from.balance < req.Amount {
		return nil, domain.ErrInsufficientFunds
	}
	from.balance -= req.Amount
	to.balance += req.Amount
	f.seq++
	f.transfers++
	res := &port.TransferResult{
		TransactionID: "tx-" + string(rune('0'+f.seq)),
		CreatedAt:     time.Date(2026, 11, 12, 12, 43, 8, 0, time.UTC),
		BalanceAfter:  from.balance,
	}
	f.done[req.IdempotencyKey] = req
	f.results[req.IdempotencyKey] = res
	return res, nil
}
