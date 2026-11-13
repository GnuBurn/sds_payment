package domain

import (
	"crypto/rand"
	"fmt"
	"math"
	"math/big"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// Currency is the only currency bills are paid in. Amounts are int64 satang
// (10050 = 100.50 THB). Never use float64 for money.
const Currency = "THB"

const (
	CodeLength    = 6
	maxTextLength = 120
	maxItems      = 200
	maxNoteLength = 80 // wallet-service limit for transfer notes
)

type Status string

const (
	// StatusPending: waiting for a customer to pay.
	StatusPending Status = "pending"
	// StatusProcessing: a customer has started paying and the wallet
	// transfer is in flight (or its outcome is not known yet).
	StatusProcessing Status = "processing"
	StatusPaid       Status = "paid"
	StatusExpired    Status = "expired"
)

// Item is one line of a bill. Prices are satang.
type Item struct {
	Name      string `json:"name"`
	Qty       int64  `json:"qty"`
	UnitPrice int64  `json:"unit_price"`
}

// Intent is a request for payment of one POS bill. The customer finds it by
// its short Code and pays it from their wallet into MerchantWalletID.
//
// Lifecycle:
//
//	pending ──Claim──▶ processing ──MarkPaid──▶ paid
//	   ▲                    │
//	   └──────Release───────┘   (wallet refused the transfer)
//	pending ──(ExpiresAt passes / bill replaced)──▶ expired
type Intent struct {
	ID               string
	BillID           string // the POS bill/order reference, e.g. "B-1042"
	Code             string // 6 digits, unique among live intents
	CreatedBy        string // cashier user ID; owns MerchantWalletID
	RestaurantName   string
	Table            string
	MerchantWalletID string
	Items            []Item
	Total            int64
	Status           Status

	// Set when a customer claims the intent (processing) and kept once paid.
	PayerUserID       string
	PayerWalletID     string
	PayIdempotencyKey string

	// Set when paid.
	TransactionID string
	BalanceAfter  int64 // payer's wallet balance after paying
	PaidAt        time.Time

	ExpiresAt time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewBill holds the POS-supplied fields of a new intent.
type NewBill struct {
	BillID           string
	CreatedBy        string
	RestaurantName   string
	Table            string
	MerchantWalletID string
	Items            []Item
}

// NewIntent validates the bill and computes its total. ID and Code are set by
// the caller.
func NewIntent(b NewBill, now time.Time, ttl time.Duration) (*Intent, error) {
	b.BillID = strings.TrimSpace(b.BillID)
	b.RestaurantName = strings.TrimSpace(b.RestaurantName)
	b.Table = strings.TrimSpace(b.Table)
	b.MerchantWalletID = strings.TrimSpace(b.MerchantWalletID)
	switch {
	case strings.TrimSpace(b.CreatedBy) == "":
		return nil, ErrUnauthenticated
	case b.BillID == "":
		return nil, ErrMissingBillID
	case b.RestaurantName == "":
		return nil, ErrMissingRestaurant
	case b.MerchantWalletID == "":
		return nil, ErrMissingWallet
	case len(b.Items) == 0:
		return nil, ErrNoItems
	case len(b.Items) > maxItems:
		return nil, ErrFieldTooLong
	}
	for _, s := range []string{b.BillID, b.RestaurantName, b.Table} {
		if utf8.RuneCountInString(s) > maxTextLength {
			return nil, ErrFieldTooLong
		}
	}

	items := make([]Item, len(b.Items))
	var total int64
	for i, it := range b.Items {
		it.Name = strings.TrimSpace(it.Name)
		if it.Name == "" || it.Qty <= 0 || it.UnitPrice < 0 {
			return nil, ErrInvalidItem
		}
		if utf8.RuneCountInString(it.Name) > maxTextLength {
			return nil, ErrFieldTooLong
		}
		if it.UnitPrice > 0 && it.Qty > math.MaxInt64/it.UnitPrice {
			return nil, ErrAmountOverflow
		}
		line := it.Qty * it.UnitPrice
		if total > math.MaxInt64-line {
			return nil, ErrAmountOverflow
		}
		total += line
		items[i] = it
	}
	if total <= 0 {
		return nil, ErrInvalidAmount
	}

	return &Intent{
		BillID:           b.BillID,
		CreatedBy:        b.CreatedBy,
		RestaurantName:   b.RestaurantName,
		Table:            b.Table,
		MerchantWalletID: b.MerchantWalletID,
		Items:            items,
		Total:            total,
		Status:           StatusPending,
		ExpiresAt:        now.Add(ttl),
		CreatedAt:        now,
		UpdatedAt:        now,
	}, nil
}

// EffectiveStatus is the status as seen at time now: a pending intent whose
// ExpiresAt has passed is expired even if the row hasn't been updated yet.
// A processing intent never expires; its outcome must be settled first.
func (i *Intent) EffectiveStatus(now time.Time) Status {
	if i.Status == StatusPending && !now.Before(i.ExpiresAt) {
		return StatusExpired
	}
	return i.Status
}

// IsLive reports whether the intent can still be (or is being) paid.
func (i *Intent) IsLive(now time.Time) bool {
	s := i.EffectiveStatus(now)
	return s == StatusPending || s == StatusProcessing
}

// SameBill reports whether other describes the same bill contents, so
// re-creating it can return the existing code.
func (i *Intent) SameBill(other *Intent) bool {
	return i.Total == other.Total &&
		i.MerchantWalletID == other.MerchantWalletID &&
		i.RestaurantName == other.RestaurantName &&
		i.Table == other.Table &&
		slices.Equal(i.Items, other.Items)
}

// Claim marks the intent as being paid by a customer.
func (i *Intent) Claim(userID, walletID, idempotencyKey string, now time.Time) error {
	switch i.EffectiveStatus(now) {
	case StatusPending:
	case StatusExpired:
		return ErrBillExpired
	case StatusPaid:
		return ErrBillAlreadyPaid
	default:
		return ErrBillBeingPaid
	}
	i.Status = StatusProcessing
	i.PayerUserID = userID
	i.PayerWalletID = walletID
	i.PayIdempotencyKey = idempotencyKey
	i.UpdatedAt = now
	return nil
}

// Release puts a processing intent back to pending after the wallet refused
// the transfer, so the bill can be paid again.
func (i *Intent) Release(now time.Time) {
	i.Status = StatusPending
	i.PayerUserID, i.PayerWalletID, i.PayIdempotencyKey = "", "", ""
	i.UpdatedAt = now
}

func (i *Intent) MarkPaid(transactionID string, balanceAfter int64, paidAt, now time.Time) {
	i.Status = StatusPaid
	i.TransactionID = transactionID
	i.BalanceAfter = balanceAfter
	i.PaidAt = paidAt
	i.UpdatedAt = now
}

func (i *Intent) Expire(now time.Time) {
	i.Status = StatusExpired
	i.UpdatedAt = now
}

// IsPaymentBy reports whether a pay request matches the one that claimed or
// paid this intent.
func (i *Intent) IsPaymentBy(userID, walletID, idempotencyKey string) bool {
	return i.PayIdempotencyKey == idempotencyKey &&
		i.PayerUserID == userID &&
		i.PayerWalletID == walletID
}

// TransferNote is the note stored on the wallet transfer so the customer's
// history can show what was paid, e.g. "Bill 482913 · Noodle House".
func (i *Intent) TransferNote() string {
	note := fmt.Sprintf("Bill %s · %s", i.Code, i.RestaurantName)
	if utf8.RuneCountInString(note) > maxNoteLength {
		note = string([]rune(note)[:maxNoteLength])
	}
	return strings.TrimSpace(note)
}

// TransferKey is the idempotency key used for the wallet transfer. It is tied
// to the intent, not to the customer's request, so a bill can never be
// charged twice even if the customer retries with a new key.
func (i *Intent) TransferKey() string {
	return "bill:" + i.ID
}

// Receipt is the result of paying an intent.
type Receipt struct {
	TransactionID string
	PaidAt        time.Time
	Amount        int64
	BalanceAfter  int64
}

func (i *Intent) Receipt() *Receipt {
	return &Receipt{
		TransactionID: i.TransactionID,
		PaidAt:        i.PaidAt,
		Amount:        i.Total,
		BalanceAfter:  i.BalanceAfter,
	}
}

// ValidCode reports whether s looks like a bill code (6 ASCII digits).
func ValidCode(s string) bool {
	if len(s) != CodeLength {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// RandomCode returns a uniformly random 6-digit code such as "048213".
func RandomCode() string {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return fmt.Sprintf("%06d", n.Int64())
}
