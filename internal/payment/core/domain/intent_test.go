package domain

import (
	"strings"
	"testing"
	"time"
)

func TestNewIntentComputesTotal(t *testing.T) {
	now := time.Now()
	in, err := NewIntent(NewBill{
		BillID: " B-1 ", CreatedBy: "u", RestaurantName: "Noodle House", MerchantWalletID: "w",
		Items: []Item{{Name: " Pad Thai ", Qty: 2, UnitPrice: 6000}, {Name: "Soda", Qty: 2, UnitPrice: 3000}},
	}, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if in.Total != 18000 || in.BillID != "B-1" || in.Items[0].Name != "Pad Thai" {
		t.Fatalf("got %+v", in)
	}
	if in.Status != StatusPending || !in.ExpiresAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("got %+v", in)
	}
}

func TestEffectiveStatus(t *testing.T) {
	now := time.Now()
	in := &Intent{Status: StatusPending, ExpiresAt: now}
	if in.EffectiveStatus(now.Add(-time.Second)) != StatusPending || in.EffectiveStatus(now) != StatusExpired {
		t.Fatal("pending intent should expire at ExpiresAt")
	}
	in.Status = StatusProcessing
	if in.EffectiveStatus(now.Add(time.Hour)) != StatusProcessing {
		t.Fatal("processing intent must not expire before it is settled")
	}
}

func TestClaimRelease(t *testing.T) {
	now := time.Now()
	in := &Intent{Status: StatusPending, ExpiresAt: now.Add(time.Minute)}
	if err := in.Claim("u", "w", "k", now); err != nil {
		t.Fatal(err)
	}
	if err := in.Claim("u2", "w2", "k2", now); err != ErrBillBeingPaid {
		t.Fatalf("second claim: %v", err)
	}
	if !in.IsPaymentBy("u", "w", "k") || in.IsPaymentBy("u", "w", "other") {
		t.Fatal("IsPaymentBy")
	}
	in.Release(now)
	if in.Status != StatusPending || in.PayerUserID != "" {
		t.Fatalf("after release: %+v", in)
	}
	in.ExpiresAt = now
	if err := in.Claim("u", "w", "k", now); err != ErrBillExpired {
		t.Fatalf("claim expired: %v", err)
	}
}

func TestTransferNoteFitsWallet(t *testing.T) {
	in := &Intent{Code: "482913", RestaurantName: strings.Repeat("ร้าน", 40)}
	if n := len([]rune(in.TransferNote())); n > 80 {
		t.Fatalf("note has %d characters", n)
	}
}

func TestCodes(t *testing.T) {
	for range 1000 {
		if c := RandomCode(); !ValidCode(c) {
			t.Fatalf("RandomCode() = %q", c)
		}
	}
	for _, bad := range []string{"", "12345", "1234567", "12a456", "１２３４５６"} {
		if ValidCode(bad) {
			t.Errorf("ValidCode(%q) = true", bad)
		}
	}
}
