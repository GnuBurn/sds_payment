package http

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GnuBurn/sds_payment/internal/payment/core/domain"
	"github.com/GnuBurn/sds_payment/internal/payment/core/port"
)

// stubService records the last command and returns canned results.
type stubService struct {
	intent  *domain.Intent
	receipt *domain.Receipt
	err     error

	createCmd port.CreateBillCmd
	payCmd    port.PayBillCmd
	userID    string
	arg       string
}

func (s *stubService) CreateBill(_ context.Context, cmd port.CreateBillCmd) (*domain.Intent, error) {
	s.createCmd = cmd
	return s.intent, s.err
}

func (s *stubService) GetBill(_ context.Context, userID, id string) (*domain.Intent, error) {
	s.userID, s.arg = userID, id
	return s.intent, s.err
}

func (s *stubService) GetBillByCode(_ context.Context, userID, code string) (*domain.Intent, error) {
	s.userID, s.arg = userID, code
	return s.intent, s.err
}

func (s *stubService) PayBill(_ context.Context, cmd port.PayBillCmd) (*domain.Receipt, error) {
	s.payCmd = cmd
	return s.receipt, s.err
}

func sampleIntent(status domain.Status) *domain.Intent {
	created := time.Date(2026, 11, 12, 12, 41, 0, 0, time.UTC)
	return &domain.Intent{
		ID: "in-1", BillID: "B-1042", Code: "482913", CreatedBy: "u_shop",
		RestaurantName: "Noodle House", Table: "5", MerchantWalletID: "w_shop",
		Items:       []domain.Item{{Name: "Pad Thai", Qty: 2, UnitPrice: 6000}},
		Total:       12000,
		Status:      status,
		PayerUserID: "u_alice", PayerWalletID: "w_alice", TransactionID: "tx-1",
		PaidAt:    created.Add(2 * time.Minute),
		ExpiresAt: time.Now().Add(time.Hour),
		CreatedAt: created,
	}
}

func serve(t *testing.T, svc port.PaymentService, method, path, body string, headers map[string]string) (int, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	NewHandler(svc, slog.New(slog.NewTextHandler(io.Discard, nil))).ServeHTTP(rec, req)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON: %q", rec.Body.String())
	}
	return rec.Code, out
}

func TestGetBillByCodeCustomerView(t *testing.T) {
	svc := &stubService{intent: sampleIntent(domain.StatusProcessing)}
	status, body := serve(t, svc, "GET", "/bills/482913", "", map[string]string{"X-User-Id": "u_alice"})
	if status != 200 {
		t.Fatalf("status = %d, body = %v", status, body)
	}
	if svc.userID != "u_alice" || svc.arg != "482913" {
		t.Fatalf("service got %q, %q", svc.userID, svc.arg)
	}
	if body["bill_id"] != "B-1042" || body["restaurant_name"] != "Noodle House" || body["table"] != "5" ||
		body["total"] != float64(12000) || body["created_at"] != "2026-11-12T12:41:00Z" {
		t.Fatalf("body = %v", body)
	}
	// The customer app only knows pending | paid | expired.
	if body["status"] != "pending" {
		t.Fatalf("status = %v, want pending", body["status"])
	}
	for _, hidden := range []string{"id", "wallet_id", "transaction_id"} {
		if _, ok := body[hidden]; ok {
			t.Errorf("customer view leaks %q", hidden)
		}
	}
	items := body["items"].([]any)
	if item := items[0].(map[string]any); item["name"] != "Pad Thai" || item["qty"] != float64(2) || item["unit_price"] != float64(6000) {
		t.Fatalf("items = %v", items)
	}
}

func TestCashierView(t *testing.T) {
	svc := &stubService{intent: sampleIntent(domain.StatusPaid)}
	status, body := serve(t, svc, "GET", "/internal/bills/in-1", "", map[string]string{"X-User-Id": "u_shop"})
	if status != 200 || svc.arg != "in-1" {
		t.Fatalf("status = %d, arg = %q", status, svc.arg)
	}
	if body["id"] != "in-1" || body["status"] != "paid" || body["transaction_id"] != "tx-1" ||
		body["wallet_id"] != "w_shop" || body["paid_at"] != "2026-11-12T12:43:00Z" {
		t.Fatalf("body = %v", body)
	}
}

func TestCreateBill(t *testing.T) {
	svc := &stubService{intent: sampleIntent(domain.StatusPending)}
	status, body := serve(t, svc, "POST", "/internal/bills",
		`{"bill_id":"B-1042","restaurant_name":"Noodle House","table":"5","wallet_id":"w_shop",
		  "items":[{"name":"Pad Thai","qty":2,"unit_price":6000}]}`,
		map[string]string{"X-User-Id": "u_shop"})
	if status != 200 || body["code"] != "482913" || body["id"] != "in-1" {
		t.Fatalf("status = %d, body = %v", status, body)
	}
	want := port.CreateBillCmd{
		UserID: "u_shop", BillID: "B-1042", RestaurantName: "Noodle House", Table: "5", MerchantWalletID: "w_shop",
		Items: []domain.Item{{Name: "Pad Thai", Qty: 2, UnitPrice: 6000}},
	}
	got := svc.createCmd
	if got.UserID != want.UserID || got.BillID != want.BillID || got.MerchantWalletID != want.MerchantWalletID ||
		len(got.Items) != 1 || got.Items[0] != want.Items[0] {
		t.Fatalf("cmd = %+v", got)
	}
}

func TestPayBill(t *testing.T) {
	svc := &stubService{receipt: &domain.Receipt{
		TransactionID: "tx-1", PaidAt: time.Date(2026, 11, 12, 12, 43, 8, 0, time.UTC), Amount: 25500, BalanceAfter: 98550,
	}}
	status, body := serve(t, svc, "POST", "/bills/482913", `{"wallet_id":"w_alice"}`,
		map[string]string{"X-User-Id": "u_alice", "Idempotency-Key": "k1"})
	if status != 200 {
		t.Fatalf("status = %d, body = %v", status, body)
	}
	if svc.payCmd != (port.PayBillCmd{UserID: "u_alice", Code: "482913", WalletID: "w_alice", IdempotencyKey: "k1"}) {
		t.Fatalf("cmd = %+v", svc.payCmd)
	}
	if body["transaction_id"] != "tx-1" || body["paid_at"] != "2026-11-12T12:43:08Z" ||
		body["amount"] != float64(25500) || body["balance_after"] != float64(98550) {
		t.Fatalf("body = %v", body)
	}
}

func TestBadBodies(t *testing.T) {
	svc := &stubService{}
	for _, body := range []string{"", "{", `{"wallet_id":"w","note":"x"}`} {
		status, out := serve(t, svc, "POST", "/bills/482913", body, map[string]string{"X-User-Id": "u"})
		if status != 400 {
			t.Errorf("body %q: status = %d, %v", body, status, out)
		}
	}
}

func TestErrorStatus(t *testing.T) {
	tests := []struct {
		err     error
		status  int
		keyword string
	}{
		{domain.ErrUnauthenticated, 401, ""},
		{domain.ErrForbidden, 403, ""},
		{domain.ErrBillNotFound, 404, "bill"},
		{domain.ErrWalletNotFound, 404, "wallet"},
		{domain.ErrIdempotencyKeyReused, 409, "different"},
		{domain.ErrInsufficientFunds, 422, "insufficient"},
		{domain.ErrBillAlreadyPaid, 422, "paid"},
		{domain.ErrBillBeingPaid, 422, "paid"},
		{domain.ErrBillExpired, 422, "expired"},
		{domain.ErrWalletInactive, 422, "inactive"},
		{domain.ErrCurrencyMismatch, 422, "currency"},
		{domain.ErrMissingIdempotencyKey, 400, "idempotency"},
		{domain.ErrInvalidAmount, 400, "amount"},
		{domain.ErrSameWallet, 400, "same wallet"},
		{errors.Join(domain.ErrWalletUnavailable, errors.New("dial tcp: refused")), 502, "unavailable"},
		{errors.New("db exploded"), 500, "internal server error"},
	}
	for _, tt := range tests {
		t.Run(tt.err.Error(), func(t *testing.T) {
			status, body := serve(t, &stubService{err: tt.err}, "GET", "/bills/482913", "", nil)
			if status != tt.status {
				t.Fatalf("status = %d, want %d", status, tt.status)
			}
			msg, _ := body["error"].(string)
			if !strings.Contains(msg, tt.keyword) {
				t.Fatalf("error %q lacks keyword %q", msg, tt.keyword)
			}
			if strings.Contains(msg, "dial tcp") || strings.Contains(msg, "exploded") {
				t.Fatalf("internal detail leaked: %q", msg)
			}
		})
	}
}
