package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/GnuBurn/sds_payment/internal/payment/core/domain"
	"github.com/GnuBurn/sds_payment/internal/payment/core/port"
)

const (
	cashier     = "u_cashier"
	alice       = "u_alice"
	bob         = "u_bob"
	shopWallet  = "w_shop"
	aliceWallet = "w_alice"
	bobWallet   = "w_bob"
)

type env struct {
	svc    *PaymentService
	store  *memStore
	wallet *fakeWallet
	now    time.Time
	mu     sync.Mutex
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{store: newMemStore(), wallet: newFakeWallet(), now: time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)}
	e.wallet.add(shopWallet, cashier, 0)
	e.wallet.add(aliceWallet, alice, 100_000)
	e.wallet.add(bobWallet, bob, 100_000)
	ids, codes := 0, 0
	e.svc = New(Deps{
		UnitOfWork: e.store,
		Intents:    reader{e.store},
		Wallet:     e.wallet,
		NewID: func() string {
			e.mu.Lock()
			defer e.mu.Unlock()
			ids++
			return fmt.Sprintf("in-%03d", ids)
		},
		NewCode: func() string {
			e.mu.Lock()
			defer e.mu.Unlock()
			codes++
			return fmt.Sprintf("%06d", 100000+codes)
		},
		Now: func() time.Time {
			e.mu.Lock()
			defer e.mu.Unlock()
			return e.now
		},
		BillTTL: 15 * time.Minute,
	})
	return e
}

func (e *env) advance(d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.now = e.now.Add(d)
}

var ctx = context.Background()

func billCmd(billID string, items ...domain.Item) port.CreateBillCmd {
	if len(items) == 0 {
		items = []domain.Item{{Name: "Pad Thai", Qty: 2, UnitPrice: 6000}, {Name: "Lime Soda", Qty: 1, UnitPrice: 3000}}
	}
	return port.CreateBillCmd{
		UserID: cashier, BillID: billID, RestaurantName: "Noodle House", Table: "5",
		MerchantWalletID: shopWallet, Items: items,
	}
}

func (e *env) createBill(t *testing.T, billID string) *domain.Intent {
	t.Helper()
	in, err := e.svc.CreateBill(ctx, billCmd(billID))
	if err != nil {
		t.Fatalf("CreateBill: %v", err)
	}
	return in
}

func pay(code, user, wallet, key string) port.PayBillCmd {
	return port.PayBillCmd{UserID: user, Code: code, WalletID: wallet, IdempotencyKey: key}
}

func TestCreateBill(t *testing.T) {
	e := newEnv(t)
	in := e.createBill(t, "B-1")
	if in.Total != 15000 || in.Status != domain.StatusPending || !domain.ValidCode(in.Code) {
		t.Fatalf("got %+v", in)
	}
	if want := e.now.Add(15 * time.Minute); !in.ExpiresAt.Equal(want) {
		t.Fatalf("expires_at = %v, want %v", in.ExpiresAt, want)
	}

	// Retrying the same request returns the same code.
	again := e.createBill(t, "B-1")
	if again.ID != in.ID || again.Code != in.Code {
		t.Fatalf("retry created a new intent: %s/%s vs %s/%s", again.ID, again.Code, in.ID, in.Code)
	}

	// A changed order replaces the code; the old one stops working.
	changed, err := e.svc.CreateBill(ctx, billCmd("B-1", domain.Item{Name: "Pad Thai", Qty: 3, UnitPrice: 6000}))
	if err != nil {
		t.Fatal(err)
	}
	if changed.ID == in.ID || changed.Total != 18000 {
		t.Fatalf("changed = %+v", changed)
	}
	old, _ := e.svc.GetBillByCode(ctx, alice, in.Code)
	if old.Status != domain.StatusExpired {
		t.Fatalf("old status = %s, want expired", old.Status)
	}
	if _, err := e.svc.PayBill(ctx, pay(in.Code, alice, aliceWallet, "k1")); !errors.Is(err, domain.ErrBillExpired) {
		t.Fatalf("paying replaced code: err = %v", err)
	}
}

func TestCreateBillValidation(t *testing.T) {
	e := newEnv(t)
	e.wallet.add("w_frozen", cashier, 0)
	e.wallet.wallets["w_frozen"].status = "frozen"

	tests := []struct {
		name string
		edit func(*port.CreateBillCmd)
		want error
	}{
		{"no user", func(c *port.CreateBillCmd) { c.UserID = "" }, domain.ErrUnauthenticated},
		{"no bill id", func(c *port.CreateBillCmd) { c.BillID = " " }, domain.ErrMissingBillID},
		{"no restaurant", func(c *port.CreateBillCmd) { c.RestaurantName = "" }, domain.ErrMissingRestaurant},
		{"no wallet", func(c *port.CreateBillCmd) { c.MerchantWalletID = "" }, domain.ErrMissingWallet},
		{"no items", func(c *port.CreateBillCmd) { c.Items = nil }, domain.ErrNoItems},
		{"zero qty", func(c *port.CreateBillCmd) { c.Items = []domain.Item{{Name: "x", Qty: 0, UnitPrice: 1}} }, domain.ErrInvalidItem},
		{"negative price", func(c *port.CreateBillCmd) { c.Items = []domain.Item{{Name: "x", Qty: 1, UnitPrice: -1}} }, domain.ErrInvalidItem},
		{"free bill", func(c *port.CreateBillCmd) { c.Items = []domain.Item{{Name: "water", Qty: 1, UnitPrice: 0}} }, domain.ErrInvalidAmount},
		{"overflow", func(c *port.CreateBillCmd) {
			c.Items = []domain.Item{{Name: "x", Qty: 1 << 40, UnitPrice: 1 << 40}}
		}, domain.ErrAmountOverflow},
		{"someone else's wallet", func(c *port.CreateBillCmd) { c.MerchantWalletID = aliceWallet }, domain.ErrForbidden},
		{"unknown wallet", func(c *port.CreateBillCmd) { c.MerchantWalletID = "w_nope" }, domain.ErrWalletNotFound},
		{"frozen wallet", func(c *port.CreateBillCmd) { c.MerchantWalletID = "w_frozen" }, domain.ErrWalletInactive},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := billCmd("B-1")
			tt.edit(&cmd)
			if _, err := e.svc.CreateBill(ctx, cmd); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestCreateBillSkipsCodesInUse(t *testing.T) {
	e := newEnv(t)
	codes := []string{"111111", "111111", "222222"}
	e.svc.d.NewCode = func() string { c := codes[0]; codes = codes[1:]; return c }
	a := e.createBill(t, "B-1")
	b := e.createBill(t, "B-2")
	if a.Code != "111111" || b.Code != "222222" {
		t.Fatalf("codes = %s, %s", a.Code, b.Code)
	}
}

func TestCreateBillReusesExpiredCode(t *testing.T) {
	e := newEnv(t)
	e.svc.d.NewCode = func() string { return "333333" }
	a := e.createBill(t, "B-1")
	e.advance(16 * time.Minute)
	b := e.createBill(t, "B-2")
	if b.Code != a.Code || b.ID == a.ID {
		t.Fatalf("a = %s/%s, b = %s/%s", a.ID, a.Code, b.ID, b.Code)
	}
	got, err := e.svc.GetBillByCode(ctx, alice, "333333")
	if err != nil || got.ID != b.ID {
		t.Fatalf("lookup returned %v, %v; want newest bill", got, err)
	}
}

func TestPayBill(t *testing.T) {
	e := newEnv(t)
	in := e.createBill(t, "B-1")

	r, err := e.svc.PayBill(ctx, pay(in.Code, alice, aliceWallet, "k1"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Amount != 15000 || r.BalanceAfter != 85000 || r.TransactionID == "" {
		t.Fatalf("receipt = %+v", r)
	}
	if e.wallet.balance(shopWallet) != 15000 || e.wallet.balance(aliceWallet) != 85000 {
		t.Fatal("money did not move")
	}

	got, err := e.svc.GetBill(ctx, cashier, in.ID)
	if err != nil || got.Status != domain.StatusPaid || got.TransactionID != r.TransactionID {
		t.Fatalf("cashier sees %+v, %v", got, err)
	}

	// The wallet transfer carries the bill so history can show it.
	req := e.wallet.done[in.TransferKey()]
	if req.Note != "Bill "+in.Code+" · Noodle House" || req.UserID != alice {
		t.Fatalf("transfer = %+v", req)
	}

	// Same request again: same receipt, no new transfer.
	r2, err := e.svc.PayBill(ctx, pay(in.Code, alice, aliceWallet, "k1"))
	if err != nil || *r2 != *r {
		t.Fatalf("replay = %+v, %v", r2, err)
	}
	// Same key, different body.
	if _, err := e.svc.PayBill(ctx, pay(in.Code, alice, "w_other", "k1")); !errors.Is(err, domain.ErrIdempotencyKeyReused) {
		t.Fatalf("reused key: err = %v", err)
	}
	// New key, or someone else.
	if _, err := e.svc.PayBill(ctx, pay(in.Code, alice, aliceWallet, "k2")); !errors.Is(err, domain.ErrBillAlreadyPaid) {
		t.Fatalf("pay twice: err = %v", err)
	}
	if _, err := e.svc.PayBill(ctx, pay(in.Code, bob, bobWallet, "kb")); !errors.Is(err, domain.ErrBillAlreadyPaid) {
		t.Fatalf("bob: err = %v", err)
	}
	if e.wallet.transfers != 1 {
		t.Fatalf("transfers = %d, want 1", e.wallet.transfers)
	}

	// The paid bill can't get a new code.
	if _, err := e.svc.CreateBill(ctx, billCmd("B-1")); !errors.Is(err, domain.ErrBillAlreadyPaid) {
		t.Fatalf("re-create paid bill: err = %v", err)
	}
}

func TestPayBillRefusedThenRetried(t *testing.T) {
	e := newEnv(t)
	in := e.createBill(t, "B-1")
	e.wallet.wallets[aliceWallet].balance = 100

	if _, err := e.svc.PayBill(ctx, pay(in.Code, alice, aliceWallet, "k1")); !errors.Is(err, domain.ErrInsufficientFunds) {
		t.Fatalf("err = %v", err)
	}
	got, _ := e.svc.GetBillByCode(ctx, alice, in.Code)
	if got.Status != domain.StatusPending {
		t.Fatalf("status after refusal = %s, want pending", got.Status)
	}

	// Paying from someone else's wallet is refused by wallet-service.
	if _, err := e.svc.PayBill(ctx, pay(in.Code, alice, bobWallet, "k2")); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("err = %v", err)
	}

	// After topping up, the same key works.
	e.wallet.wallets[aliceWallet].balance = 100_000
	if _, err := e.svc.PayBill(ctx, pay(in.Code, alice, aliceWallet, "k1")); err != nil {
		t.Fatal(err)
	}
}

func TestPayBillExpired(t *testing.T) {
	e := newEnv(t)
	in := e.createBill(t, "B-1")
	e.advance(15 * time.Minute)
	if _, err := e.svc.PayBill(ctx, pay(in.Code, alice, aliceWallet, "k1")); !errors.Is(err, domain.ErrBillExpired) {
		t.Fatalf("err = %v", err)
	}
	if e.wallet.calls != 0 {
		t.Fatal("wallet was called for an expired bill")
	}
}

func TestPayBillValidation(t *testing.T) {
	e := newEnv(t)
	in := e.createBill(t, "B-1")
	tests := []struct {
		name string
		cmd  port.PayBillCmd
		want error
	}{
		{"no user", pay(in.Code, "", aliceWallet, "k"), domain.ErrUnauthenticated},
		{"no key", pay(in.Code, alice, aliceWallet, ""), domain.ErrMissingIdempotencyKey},
		{"no wallet", pay(in.Code, alice, "", "k"), domain.ErrMissingWallet},
		{"bad code", pay("12ab56", alice, aliceWallet, "k"), domain.ErrBillNotFound},
		{"unknown code", pay("999999", alice, aliceWallet, "k"), domain.ErrBillNotFound},
		{"pay own shop", pay(in.Code, cashier, shopWallet, "k"), domain.ErrSameWallet},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := e.svc.PayBill(ctx, tt.cmd); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

// The wallet applied the transfer but the response was lost. The intent stays
// processing; the retry replays the same transfer and charges only once,
// even after the bill's expiry time has passed.
func TestPayBillUnknownOutcomeIsSettledOnRetry(t *testing.T) {
	e := newEnv(t)
	in := e.createBill(t, "B-1")
	e.wallet.failNext, e.wallet.failErr, e.wallet.applyFirst = 1, domain.ErrWalletUnavailable, true

	if _, err := e.svc.PayBill(ctx, pay(in.Code, alice, aliceWallet, "k1")); !errors.Is(err, domain.ErrWalletUnavailable) {
		t.Fatalf("err = %v", err)
	}
	cashierView, _ := e.svc.GetBill(ctx, cashier, in.ID)
	if cashierView.Status != domain.StatusProcessing {
		t.Fatalf("status = %s, want processing", cashierView.Status)
	}
	// Someone else can't take over an in-flight payment while wallet is down.
	e.wallet.failNext, e.wallet.applyFirst = 1, false
	if _, err := e.svc.PayBill(ctx, pay(in.Code, bob, bobWallet, "kb")); !errors.Is(err, domain.ErrBillBeingPaid) {
		t.Fatalf("bob: err = %v", err)
	}

	e.advance(time.Hour)
	r, err := e.svc.PayBill(ctx, pay(in.Code, alice, aliceWallet, "k1"))
	if err != nil {
		t.Fatal(err)
	}
	if e.wallet.transfers != 1 || e.wallet.balance(aliceWallet) != 85000 || r.BalanceAfter != 85000 {
		t.Fatalf("transfers = %d, alice = %d", e.wallet.transfers, e.wallet.balance(aliceWallet))
	}
}

// A customer who gave up mid-payment doesn't leave the bill stuck: the
// cashier's polling settles it.
func TestCashierPollSettlesStuckPayment(t *testing.T) {
	e := newEnv(t)
	in := e.createBill(t, "B-1")
	e.wallet.failNext, e.wallet.failErr, e.wallet.applyFirst = 1, domain.ErrWalletUnavailable, true
	_, _ = e.svc.PayBill(ctx, pay(in.Code, alice, aliceWallet, "k1"))

	got, _ := e.svc.GetBill(ctx, cashier, in.ID)
	if got.Status != domain.StatusProcessing {
		t.Fatalf("status = %s, want processing (too soon to settle)", got.Status)
	}
	e.advance(DefaultSettleAfter)
	got, err := e.svc.GetBill(ctx, cashier, in.ID)
	if err != nil || got.Status != domain.StatusPaid {
		t.Fatalf("status = %v, %v; want paid", got, err)
	}
	if e.wallet.transfers != 1 {
		t.Fatalf("transfers = %d", e.wallet.transfers)
	}
}

// Someone else's in-flight attempt that turns out refused frees the bill.
func TestPayBillTakesOverRefusedAttempt(t *testing.T) {
	e := newEnv(t)
	in := e.createBill(t, "B-1")
	e.wallet.failNext, e.wallet.failErr = 1, domain.ErrWalletUnavailable
	e.wallet.wallets[aliceWallet].balance = 0
	_, _ = e.svc.PayBill(ctx, pay(in.Code, alice, aliceWallet, "k1")) // stuck processing

	r, err := e.svc.PayBill(ctx, pay(in.Code, bob, bobWallet, "kb"))
	if err != nil {
		t.Fatal(err)
	}
	if r.BalanceAfter != 85000 || e.wallet.balance(shopWallet) != 15000 {
		t.Fatalf("receipt = %+v", r)
	}
}

func TestPayBillConcurrentPayersChargeOnce(t *testing.T) {
	e := newEnv(t)
	in := e.createBill(t, "B-1")
	users := []struct{ user, wallet string }{{alice, aliceWallet}, {bob, bobWallet}}

	var wg sync.WaitGroup
	errs := make([]error, 10)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			u := users[i%2]
			_, errs[i] = e.svc.PayBill(ctx, pay(in.Code, u.user, u.wallet, fmt.Sprintf("k%d", i)))
		}()
	}
	wg.Wait()

	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, domain.ErrBillAlreadyPaid):
		default:
			t.Errorf("unexpected err: %v", err)
		}
	}
	if ok != 1 || e.wallet.transfers != 1 || e.wallet.balance(shopWallet) != 15000 {
		t.Fatalf("ok = %d, transfers = %d, shop = %d", ok, e.wallet.transfers, e.wallet.balance(shopWallet))
	}
}

func TestGetBillOwnership(t *testing.T) {
	e := newEnv(t)
	in := e.createBill(t, "B-1")
	if _, err := e.svc.GetBill(ctx, alice, in.ID); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("err = %v", err)
	}
	if _, err := e.svc.GetBill(ctx, "", in.ID); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("err = %v", err)
	}
	if _, err := e.svc.GetBillByCode(ctx, "", in.Code); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("err = %v", err)
	}
}
