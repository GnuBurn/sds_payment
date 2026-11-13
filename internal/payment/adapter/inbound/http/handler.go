package http

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/GnuBurn/sds_payment/internal/payment/core/domain"
	"github.com/GnuBurn/sds_payment/internal/payment/core/port"
)

const (
	idempotencyHeader = "Idempotency-Key"
	// userIDHeader carries the authenticated user's ID. The API gateway
	// verifies the JWT, sets this header from its `sub` claim, and must strip
	// any value sent by the client.
	userIDHeader = "X-User-Id"
	maxBodyBytes = 1 << 20
)

type Handler struct {
	svc port.PaymentService
	log *slog.Logger
	now func() time.Time
}

// NewHandler returns the HTTP API.
//
// Routes under /internal/ are for pos-service only. The gateway must not
// route them from the browser (it maps /api/pay/* to /*, so it has to
// reject /api/pay/internal/*).
func NewHandler(svc port.PaymentService, log *slog.Logger) http.Handler {
	h := &Handler{svc: svc, log: log, now: func() time.Time { return time.Now().UTC() }}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", h.health)

	// pos-service (on behalf of the cashier)
	mux.HandleFunc("POST /internal/bills", h.createBill)
	mux.HandleFunc("GET /internal/bills/{id}", h.getBill)

	// customer, via gateway /api/pay/*
	mux.HandleFunc("GET /bills/{code}", h.getBillByCode)
	mux.HandleFunc("POST /bills/{code}", h.payBill)

	return recoverer(log, requestLogger(log, mux))
}

func (h *Handler) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) createBill(w http.ResponseWriter, r *http.Request) {
	var req createBillRequest
	if !h.decode(w, r, &req) {
		return
	}
	in, err := h.svc.CreateBill(r.Context(), port.CreateBillCmd{
		UserID:           userID(r),
		BillID:           req.BillID,
		RestaurantName:   req.RestaurantName,
		Table:            req.Table,
		MerchantWalletID: req.MerchantWalletID,
		Items:            req.Items,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toCashierBillResponse(in, h.now()))
}

func (h *Handler) getBill(w http.ResponseWriter, r *http.Request) {
	in, err := h.svc.GetBill(r.Context(), userID(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toCashierBillResponse(in, h.now()))
}

func (h *Handler) getBillByCode(w http.ResponseWriter, r *http.Request) {
	in, err := h.svc.GetBillByCode(r.Context(), userID(r), r.PathValue("code"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toBillResponse(in, h.now()))
}

func (h *Handler) payBill(w http.ResponseWriter, r *http.Request) {
	var req payBillRequest
	if !h.decode(w, r, &req) {
		return
	}
	receipt, err := h.svc.PayBill(r.Context(), port.PayBillCmd{
		UserID:         userID(r),
		Code:           r.PathValue("code"),
		WalletID:       req.WalletID,
		IdempotencyKey: r.Header.Get(idempotencyHeader),
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toReceiptResponse(receipt))
}

func userID(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get(userIDHeader))
}

func (h *Handler) decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "request body is required")
		} else {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		}
		return false
	}
	return true
}

// fail maps domain errors to HTTP status codes.
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusInternalServerError
	msg := err.Error()
	switch {
	case errors.Is(err, domain.ErrUnauthenticated):
		status = http.StatusUnauthorized
	case errors.Is(err, domain.ErrForbidden):
		status = http.StatusForbidden
	case errors.Is(err, domain.ErrBillNotFound):
		status = http.StatusNotFound
	case errors.Is(err, domain.ErrWalletNotFound):
		status = http.StatusNotFound
	case errors.Is(err, domain.ErrIdempotencyKeyReused):
		status = http.StatusConflict
	case errors.Is(err, domain.ErrBillAlreadyPaid),
		errors.Is(err, domain.ErrBillExpired),
		errors.Is(err, domain.ErrBillBeingPaid),
		errors.Is(err, domain.ErrInsufficientFunds),
		errors.Is(err, domain.ErrWalletInactive),
		errors.Is(err, domain.ErrCurrencyMismatch),
		errors.Is(err, domain.ErrPaymentRejected):
		status = http.StatusUnprocessableEntity
	case errors.Is(err, domain.ErrMissingBillID),
		errors.Is(err, domain.ErrMissingRestaurant),
		errors.Is(err, domain.ErrNoItems),
		errors.Is(err, domain.ErrInvalidItem),
		errors.Is(err, domain.ErrInvalidAmount),
		errors.Is(err, domain.ErrAmountOverflow),
		errors.Is(err, domain.ErrFieldTooLong),
		errors.Is(err, domain.ErrMissingWallet),
		errors.Is(err, domain.ErrSameWallet),
		errors.Is(err, domain.ErrMissingIdempotencyKey):
		status = http.StatusBadRequest
	case errors.Is(err, domain.ErrWalletUnavailable):
		status = http.StatusBadGateway
		msg = domain.ErrWalletUnavailable.Error()
	}

	if status >= http.StatusInternalServerError {
		h.log.ErrorContext(r.Context(), "request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		if status == http.StatusInternalServerError {
			msg = "internal server error"
		}
	}
	writeError(w, status, msg)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}
