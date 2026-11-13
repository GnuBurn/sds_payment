// Package walletclient is the outbound adapter for wallet-service's REST API.
package walletclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/GnuBurn/sds_payment/internal/payment/core/domain"
	"github.com/GnuBurn/sds_payment/internal/payment/core/port"
)

const (
	userIDHeader      = "X-User-Id"
	idempotencyHeader = "Idempotency-Key"
	DefaultTimeout    = 5 * time.Second
	maxResponseBytes  = 1 << 20
)

type Client struct {
	base *url.URL
	http *http.Client
}

var _ port.WalletClient = (*Client)(nil)

// New returns a client for wallet-service at baseURL, e.g.
// "http://wallet-service:8080".
func New(baseURL string, httpClient *http.Client) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("invalid wallet service URL %q", baseURL)
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: DefaultTimeout}
	}
	return &Client{base: u, http: httpClient}, nil
}

type walletBody struct {
	ID       string `json:"id"`
	OwnerID  string `json:"owner_id"`
	Currency string `json:"currency"`
	Status   string `json:"status"`
}

type transferBody struct {
	FromWalletID string `json:"from_wallet_id"`
	ToWalletID   string `json:"to_wallet_id"`
	Amount       int64  `json:"amount"`
	Note         string `json:"note,omitempty"`
}

type transactionBody struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	Entries   []struct {
		WalletID     string `json:"wallet_id"`
		BalanceAfter int64  `json:"balance_after"`
	} `json:"entries"`
}

type errorBody struct {
	Error string `json:"error"`
}

func (c *Client) GetWallet(ctx context.Context, userID, walletID string) (*port.Wallet, error) {
	var w walletBody
	if err := c.do(ctx, http.MethodGet, "/wallets/"+url.PathEscape(walletID), userID, "", nil, &w); err != nil {
		return nil, err
	}
	return &port.Wallet{ID: w.ID, OwnerID: w.OwnerID, Currency: w.Currency, Status: w.Status}, nil
}

func (c *Client) Transfer(ctx context.Context, req port.TransferRequest) (*port.TransferResult, error) {
	var tx transactionBody
	err := c.do(ctx, http.MethodPost, "/transfers", req.UserID, req.IdempotencyKey, transferBody{
		FromWalletID: req.FromWalletID,
		ToWalletID:   req.ToWalletID,
		Amount:       req.Amount,
		Note:         req.Note,
	}, &tx)
	if err != nil {
		return nil, err
	}
	res := &port.TransferResult{TransactionID: tx.ID, CreatedAt: tx.CreatedAt}
	found := false
	for _, e := range tx.Entries {
		if e.WalletID == req.FromWalletID {
			res.BalanceAfter, found = e.BalanceAfter, true
			break
		}
	}
	if tx.ID == "" || !found {
		return nil, fmt.Errorf("%w: transfer response has no sender entry", domain.ErrWalletUnavailable)
	}
	return res, nil
}

func (c *Client) do(ctx context.Context, method, path, userID, idemKey string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base.String()+path, body)
	if err != nil {
		return err
	}
	req.Header.Set(userIDHeader, userID)
	if idemKey != "" {
		req.Header.Set(idempotencyHeader, idemKey)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", domain.ErrWalletUnavailable, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("%w: %v", domain.ErrWalletUnavailable, err)
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("%w: bad response: %v", domain.ErrWalletUnavailable, err)
		}
		return nil
	}
	var e errorBody
	_ = json.Unmarshal(data, &e)
	return mapError(resp.StatusCode, e.Error)
}

// mapError turns a wallet-service error response into a domain error.
// 4xx answers other than 409 are definitive refusals: wallet-service rolled
// back and the transfer did not happen. Everything else is "unknown".
func mapError(status int, msg string) error {
	m := strings.ToLower(msg)
	switch status {
	case http.StatusForbidden:
		return domain.ErrForbidden
	case http.StatusNotFound:
		return domain.ErrWalletNotFound
	case http.StatusUnauthorized:
		return domain.ErrUnauthenticated
	case http.StatusUnprocessableEntity:
		switch {
		case strings.Contains(m, "insufficient"):
			return domain.ErrInsufficientFunds
		case strings.Contains(m, "not active"):
			return domain.ErrWalletInactive
		case strings.Contains(m, "currenc"):
			return domain.ErrCurrencyMismatch
		}
		return fmt.Errorf("%w: %s", domain.ErrPaymentRejected, msg)
	case http.StatusBadRequest:
		if strings.Contains(m, "same wallet") {
			return domain.ErrSameWallet
		}
		return fmt.Errorf("%w: %s", domain.ErrPaymentRejected, msg)
	}
	// 409 (idempotency key reused with a different body) can't happen by
	// design, so treat it like a 5xx: outcome unknown, log and retry later.
	return fmt.Errorf("%w: status %d: %s", domain.ErrWalletUnavailable, status, msg)
}
