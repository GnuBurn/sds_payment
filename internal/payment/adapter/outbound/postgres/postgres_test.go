package postgres_test

// Integration tests against a real Postgres. They are skipped unless
// TEST_DATABASE_URL is set, e.g.
//
//	make db
//	TEST_DATABASE_URL="postgres://sds:sds@localhost:5434/sds?sslmode=disable" go test ./...
//
// Each test runs in its own freshly migrated schema, which is dropped afterwards.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GnuBurn/sds_payment/internal/payment/adapter/outbound/postgres"
	"github.com/GnuBurn/sds_payment/internal/payment/core/domain"
	"github.com/GnuBurn/sds_payment/internal/payment/core/port"
	"github.com/GnuBurn/sds_payment/internal/payment/core/service"
)

var schemaSeq atomic.Int64

func newTestDB(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()

	// Migrate creates the schema itself, as it does in production.
	schema := fmt.Sprintf("test_payment_%d_%d", time.Now().UnixNano(), schemaSeq.Add(1))
	cfg, err := postgres.PoolConfig(url, schema)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
		pool.Close()
	})

	if err := postgres.Migrate(ctx, pool, schema); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool, schema
}

func newID() string { return uuid.Must(uuid.NewV7()).String() }

// stubWallet is a minimal wallet-service: every wallet belongs to the user
// named after it and transfers are idempotent by key.
type stubWallet struct {
	mu        sync.Mutex
	done      map[string]*port.TransferResult
	transfers atomic.Int64
	balance   map[string]int64
}

func newStubWallet() *stubWallet {
	return &stubWallet{done: map[string]*port.TransferResult{}, balance: map[string]int64{}}
}

func (s *stubWallet) GetWallet(_ context.Context, userID, walletID string) (*port.Wallet, error) {
	if walletID != "w_"+userID {
		return nil, domain.ErrForbidden
	}
	return &port.Wallet{ID: walletID, OwnerID: userID, Currency: "THB", Status: "active"}, nil
}

func (s *stubWallet) Transfer(_ context.Context, req port.TransferRequest) (*port.TransferResult, error) {
	time.Sleep(5 * time.Millisecond) // widen race windows
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.done[req.IdempotencyKey]; ok {
		return r, nil
	}
	s.transfers.Add(1)
	s.balance[req.ToWalletID] += req.Amount
	r := &port.TransferResult{TransactionID: newID(), CreatedAt: time.Now().UTC().Truncate(time.Microsecond), BalanceAfter: 1}
	s.done[req.IdempotencyKey] = r
	return r, nil
}

func newService(pool *pgxpool.Pool, w port.WalletClient) *service.PaymentService {
	return service.New(service.Deps{
		UnitOfWork: postgres.NewUnitOfWork(pool),
		Intents:    postgres.NewIntentRepository(pool),
		Wallet:     w,
		NewID:      newID,
	})
}

func createCmd(billID string) port.CreateBillCmd {
	return port.CreateBillCmd{
		UserID: "shop", BillID: billID, RestaurantName: "Noodle House", Table: "5", MerchantWalletID: "w_shop",
		Items: []domain.Item{{Name: "Pad Thai", Qty: 2, UnitPrice: 6000}, {Name: "ต้มยำ", Qty: 1, UnitPrice: 7500}},
	}
}

func TestMigrateIsIdempotentAndUsesOwnSchema(t *testing.T) {
	pool, schema := newTestDB(t)
	ctx := context.Background()
	if err := postgres.Migrate(ctx, pool, schema); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.tables WHERE table_schema = $1
		   AND table_name IN ('payment_intents', 'schema_migrations')`, schema).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("found %d tables in schema %s, want 2", n, schema)
	}
}

func TestCreateAndPayRoundTrip(t *testing.T) {
	pool, _ := newTestDB(t)
	ctx := context.Background()
	w := newStubWallet()
	svc := newService(pool, w)

	in, err := svc.CreateBill(ctx, createCmd("B-1"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.GetBillByCode(ctx, "alice", in.Code)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != in.ID || got.Total != 19500 || len(got.Items) != 2 || got.Items[1].Name != "ต้มยำ" ||
		got.Status != domain.StatusPending || !got.ExpiresAt.Equal(in.ExpiresAt.Truncate(time.Microsecond)) {
		t.Fatalf("got %+v", got)
	}

	again, err := svc.CreateBill(ctx, createCmd("B-1"))
	if err != nil || again.ID != in.ID {
		t.Fatalf("re-create: %v, %v", again, err)
	}

	r, err := svc.PayBill(ctx, port.PayBillCmd{UserID: "alice", Code: in.Code, WalletID: "w_alice", IdempotencyKey: "k1"})
	if err != nil {
		t.Fatal(err)
	}
	paid, err := svc.GetBill(ctx, "shop", in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if paid.Status != domain.StatusPaid || paid.TransactionID != r.TransactionID || !paid.PaidAt.Equal(r.PaidAt) ||
		paid.PayerUserID != "alice" || paid.PayIdempotencyKey != "k1" {
		t.Fatalf("paid = %+v", paid)
	}
	r2, err := svc.PayBill(ctx, port.PayBillCmd{UserID: "alice", Code: in.Code, WalletID: "w_alice", IdempotencyKey: "k1"})
	if err != nil || r2.TransactionID != r.TransactionID {
		t.Fatalf("replay = %+v, %v", r2, err)
	}
}

func TestLiveCodeAndBillAreUnique(t *testing.T) {
	pool, _ := newTestDB(t)
	ctx := context.Background()
	repo := postgres.NewIntentRepository(pool)
	now := time.Now().UTC()

	mk := func(code, billID string) *domain.Intent {
		in, err := domain.NewIntent(domain.NewBill{
			BillID: billID, CreatedBy: "shop", RestaurantName: "R", MerchantWalletID: "w_shop",
			Items: []domain.Item{{Name: "x", Qty: 1, UnitPrice: 100}},
		}, now, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		in.ID, in.Code = newID(), code
		return in
	}

	a := mk("111111", "B-1")
	if err := repo.Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := repo.Create(ctx, mk("111111", "B-2")); !errors.Is(err, domain.ErrDuplicateCode) {
		t.Fatalf("same code: %v", err)
	}
	if err := repo.Create(ctx, mk("222222", "B-1")); !errors.Is(err, domain.ErrDuplicateBill) {
		t.Fatalf("same bill: %v", err)
	}

	// Once expired, both the code and the bill slot are free again.
	if err := repo.ExpireStale(ctx, now.Add(2*time.Minute), "111111", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := repo.Create(ctx, mk("111111", "B-1")); err != nil {
		t.Fatalf("reuse after expiry: %v", err)
	}
	old, _ := repo.Get(ctx, a.ID)
	if old.Status != domain.StatusExpired {
		t.Fatalf("old status = %s", old.Status)
	}

	if _, err := repo.Get(ctx, "not-a-uuid"); !errors.Is(err, domain.ErrBillNotFound) {
		t.Fatalf("bad uuid: %v", err)
	}
}

// Many customers on several "replicas" (separate service instances sharing
// the database) race to pay the same bill: exactly one succeeds and the
// wallet is charged once.
func TestConcurrentPayersChargeOnce(t *testing.T) {
	pool, _ := newTestDB(t)
	ctx := context.Background()
	w := newStubWallet()
	replicas := []*service.PaymentService{newService(pool, w), newService(pool, w)}

	in, err := replicas[0].CreateBill(ctx, createCmd("B-1"))
	if err != nil {
		t.Fatal(err)
	}

	const n = 20
	var wg sync.WaitGroup
	var ok, alreadyPaid atomic.Int64
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			user := fmt.Sprintf("u%d", i)
			_, err := replicas[i%2].PayBill(ctx, port.PayBillCmd{
				UserID: user, Code: in.Code, WalletID: "w_" + user, IdempotencyKey: newID(),
			})
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, domain.ErrBillAlreadyPaid):
				alreadyPaid.Add(1)
			default:
				t.Errorf("unexpected: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 1 || alreadyPaid.Load() != n-1 || w.transfers.Load() != 1 || w.balance["w_shop"] != 19500 {
		t.Fatalf("ok = %d, already paid = %d, transfers = %d, shop = %d",
			ok.Load(), alreadyPaid.Load(), w.transfers.Load(), w.balance["w_shop"])
	}
}

// Concurrent creates of the same bill on two replicas yield one intent.
func TestConcurrentCreateSameBill(t *testing.T) {
	pool, _ := newTestDB(t)
	ctx := context.Background()
	svc := newService(pool, newStubWallet())

	var wg sync.WaitGroup
	ids := make([]string, 10)
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			in, err := svc.CreateBill(ctx, createCmd("B-1"))
			if err != nil {
				t.Errorf("create: %v", err)
				return
			}
			ids[i] = in.ID
		}()
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("got different intents: %v", ids)
		}
	}
}
