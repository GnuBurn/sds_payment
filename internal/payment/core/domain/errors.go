package domain

import "errors"

// Domain errors. Adapters translate these into transport-specific responses
// (e.g. HTTP status codes); the core never knows about HTTP.
//
// The customer app picks its message from the status code plus keywords in
// the error text (amount, same wallet, idempotency, bill, paid, expired,
// insufficient, inactive, currency), so keep those words when editing.
var (
	ErrUnauthenticated = errors.New("caller identity is required")
	ErrForbidden       = errors.New("not allowed to use this wallet or bill")

	ErrBillNotFound      = errors.New("bill not found")
	ErrBillAlreadyPaid   = errors.New("bill already paid")
	ErrBillExpired       = errors.New("bill expired")
	ErrBillBeingPaid     = errors.New("bill is being paid, try again shortly")
	ErrMissingBillID     = errors.New("bill_id is required")
	ErrMissingRestaurant = errors.New("restaurant_name is required")
	ErrNoItems           = errors.New("items are required")
	ErrInvalidItem       = errors.New("each item needs a name, qty > 0 and unit_price >= 0")
	ErrInvalidAmount     = errors.New("amount must be greater than zero")
	ErrAmountOverflow    = errors.New("amount is too large")
	ErrFieldTooLong      = errors.New("text field is too long")

	ErrMissingWallet     = errors.New("wallet_id is required")
	ErrWalletNotFound    = errors.New("wallet not found")
	ErrWalletInactive    = errors.New("wallet is inactive")
	ErrCurrencyMismatch  = errors.New("wallet currency must be THB")
	ErrSameWallet        = errors.New("cannot pay a bill into the same wallet")
	ErrInsufficientFunds = errors.New("insufficient funds")
	// ErrPaymentRejected is any other definitive refusal from wallet-service.
	ErrPaymentRejected = errors.New("payment was rejected by the wallet")
	// ErrWalletUnavailable means wallet-service could not be reached or
	// answered unexpectedly. The outcome of a transfer is unknown.
	ErrWalletUnavailable = errors.New("wallet service unavailable")

	ErrMissingIdempotencyKey = errors.New("idempotency key is required")
	ErrIdempotencyKeyReused  = errors.New("idempotency key was already used for a different request")

	// Repository-level conflicts, handled inside the service.
	ErrDuplicateCode = errors.New("bill code already in use")
	ErrDuplicateBill = errors.New("bill already has an open payment")
)
