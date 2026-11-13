package walletclient

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GnuBurn/sds_payment/internal/payment/core/domain"
	"github.com/GnuBurn/sds_payment/internal/payment/core/port"
)

func TestTransfer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/transfers" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("X-User-Id") != "u_alice" || r.Header.Get("Idempotency-Key") != "bill:1" {
			t.Errorf("headers = %v", r.Header)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["from_wallet_id"] != "wa" || body["to_wallet_id"] != "ws" || body["amount"] != float64(25500) || body["note"] != "Bill 1" {
			t.Errorf("body = %v", body)
		}
		_, _ = w.Write([]byte(`{"id":"tx1","created_at":"2026-11-12T12:43:08Z","entries":[
			{"wallet_id":"wa","balance_after":98550},{"wallet_id":"ws","balance_after":25500}]}`))
	}))
	defer srv.Close()

	c, err := New(srv.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Transfer(t.Context(), port.TransferRequest{
		UserID: "u_alice", FromWalletID: "wa", ToWalletID: "ws", Amount: 25500, Note: "Bill 1", IdempotencyKey: "bill:1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.TransactionID != "tx1" || res.BalanceAfter != 98550 || res.CreatedAt.IsZero() {
		t.Fatalf("res = %+v", res)
	}
}

func TestGetWallet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/wallets/ws" || r.Header.Get("X-User-Id") != "u_shop" {
			t.Errorf("%s %v", r.URL.Path, r.Header)
		}
		_, _ = w.Write([]byte(`{"id":"ws","owner_id":"u_shop","currency":"THB","balance":0,"status":"active"}`))
	}))
	defer srv.Close()
	c, _ := New(srv.URL, nil)
	w, err := c.GetWallet(t.Context(), "u_shop", "ws")
	if err != nil || *w != (port.Wallet{ID: "ws", OwnerID: "u_shop", Currency: "THB", Status: "active"}) {
		t.Fatalf("w = %+v, err = %v", w, err)
	}
}

func TestErrorMapping(t *testing.T) {
	tests := []struct {
		status int
		msg    string
		want   error
	}{
		{403, "wallet does not belong to the caller", domain.ErrForbidden},
		{404, "wallet not found", domain.ErrWalletNotFound},
		{422, "insufficient funds", domain.ErrInsufficientFunds},
		{422, "wallet is not active", domain.ErrWalletInactive},
		{422, "wallets have different currencies", domain.ErrCurrencyMismatch},
		{422, "balance overflow", domain.ErrPaymentRejected},
		{400, "cannot transfer to the same wallet", domain.ErrSameWallet},
		{400, "note must be at most 80 characters", domain.ErrPaymentRejected},
		{409, "idempotency key was already used for a different request", domain.ErrWalletUnavailable},
		{500, "internal server error", domain.ErrWalletUnavailable},
		{502, "", domain.ErrWalletUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.msg, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": tt.msg})
			}))
			defer srv.Close()
			c, _ := New(srv.URL, nil)
			_, err := c.Transfer(t.Context(), port.TransferRequest{FromWalletID: "a"})
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	c, _ := New(srv.URL, nil)
	if _, err := c.Transfer(t.Context(), port.TransferRequest{}); !errors.Is(err, domain.ErrWalletUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if _, err := New("not a url", nil); err == nil {
		t.Fatal("expected error for bad URL")
	}
}
