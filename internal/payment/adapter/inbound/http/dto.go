package http

import (
	"time"

	"github.com/GnuBurn/sds_payment/internal/payment/core/domain"
)

// Request/response bodies. Amounts are integers in satang
// (e.g. 10050 = 100.50 THB).

type createBillRequest struct {
	BillID           string        `json:"bill_id"`
	RestaurantName   string        `json:"restaurant_name"`
	Table            string        `json:"table"`
	MerchantWalletID string        `json:"wallet_id"`
	Items            []domain.Item `json:"items"`
}

type payBillRequest struct {
	WalletID string `json:"wallet_id"`
}

// billResponse is what the customer sees after typing a code (#9 in
// customer-api.md). It doesn't reveal the restaurant's wallet or who paid.
type billResponse struct {
	BillID         string        `json:"bill_id"`
	Code           string        `json:"code"`
	RestaurantName string        `json:"restaurant_name"`
	Table          string        `json:"table"`
	Items          []domain.Item `json:"items"`
	Total          int64         `json:"total"`
	Status         string        `json:"status"`
	ExpiresAt      time.Time     `json:"expires_at"`
	CreatedAt      time.Time     `json:"created_at"`
}

// cashierBillResponse is what pos-service sees: everything the customer sees
// plus the intent ID to poll, the real status and payment details.
type cashierBillResponse struct {
	ID string `json:"id"`
	billResponse
	WalletID      string     `json:"wallet_id"`
	TransactionID string     `json:"transaction_id,omitempty"`
	PaidAt        *time.Time `json:"paid_at,omitempty"`
}

type receiptResponse struct {
	TransactionID string    `json:"transaction_id"`
	PaidAt        time.Time `json:"paid_at"`
	Amount        int64     `json:"amount"`
	BalanceAfter  int64     `json:"balance_after"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func toBillResponse(in *domain.Intent, now time.Time) billResponse {
	status := in.EffectiveStatus(now)
	// The customer app knows pending | paid | expired. A payment in flight
	// is still "pending" to them; paying again just waits for it.
	if status == domain.StatusProcessing {
		status = domain.StatusPending
	}
	return billResponse{
		BillID:         in.BillID,
		Code:           in.Code,
		RestaurantName: in.RestaurantName,
		Table:          in.Table,
		Items:          in.Items,
		Total:          in.Total,
		Status:         string(status),
		ExpiresAt:      in.ExpiresAt.UTC(),
		CreatedAt:      in.CreatedAt.UTC(),
	}
}

func toCashierBillResponse(in *domain.Intent, now time.Time) cashierBillResponse {
	out := cashierBillResponse{
		ID:            in.ID,
		billResponse:  toBillResponse(in, now),
		WalletID:      in.MerchantWalletID,
		TransactionID: in.TransactionID,
	}
	out.Status = string(in.EffectiveStatus(now))
	if in.Status == domain.StatusPaid {
		paidAt := in.PaidAt.UTC()
		out.PaidAt = &paidAt
	}
	return out
}

func toReceiptResponse(r *domain.Receipt) receiptResponse {
	return receiptResponse{
		TransactionID: r.TransactionID,
		PaidAt:        r.PaidAt.UTC(),
		Amount:        r.Amount,
		BalanceAfter:  r.BalanceAfter,
	}
}
