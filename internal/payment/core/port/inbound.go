package port

import (
	"context"

	"github.com/GnuBurn/sds_payment/internal/payment/core/domain"
)

// PaymentService is the inbound (driving) port: the use cases exposed to
// adapters such as the HTTP handler.
type PaymentService interface {
	// CreateBill is called by pos-service on behalf of a cashier. It returns
	// the intent with its 6-digit code. Calling it again for the same open
	// bill with the same contents returns the same intent; with different
	// contents it replaces the old code.
	CreateBill(ctx context.Context, cmd CreateBillCmd) (*domain.Intent, error)
	// GetBill returns an intent by ID for the cashier who created it.
	GetBill(ctx context.Context, userID, id string) (*domain.Intent, error)
	// GetBillByCode is used by the customer after typing the code.
	GetBillByCode(ctx context.Context, userID, code string) (*domain.Intent, error)
	// PayBill pays the bill from the customer's wallet into the restaurant's.
	PayBill(ctx context.Context, cmd PayBillCmd) (*domain.Receipt, error)
}

type CreateBillCmd struct {
	UserID           string // cashier; must own MerchantWalletID
	BillID           string
	RestaurantName   string
	Table            string
	MerchantWalletID string
	Items            []domain.Item
}

type PayBillCmd struct {
	UserID         string // customer; must own WalletID
	Code           string
	WalletID       string
	IdempotencyKey string
}
