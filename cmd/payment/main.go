// Command payment runs the payment microservice.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	httpadapter "github.com/GnuBurn/sds_payment/internal/payment/adapter/inbound/http"
	"github.com/GnuBurn/sds_payment/internal/payment/adapter/outbound/postgres"
	"github.com/GnuBurn/sds_payment/internal/payment/adapter/outbound/walletclient"
	"github.com/GnuBurn/sds_payment/internal/payment/core/service"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "payment")
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	addr := getenv("HTTP_ADDR", ":8080")
	dbURL := getenv("DATABASE_URL", "postgres://sds:sds@localhost:5432/sds?sslmode=disable")
	schema := getenv("DB_SCHEMA", "payment")
	walletURL := getenv("WALLET_URL", "http://localhost:8080")
	billTTL, err := time.ParseDuration(getenv("BILL_TTL", service.DefaultBillTTL.String()))
	if err != nil {
		return err
	}

	// --- outbound adapters ---
	cfg, err := postgres.PoolConfig(dbURL, schema)
	if err != nil {
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return err
	}
	if err := postgres.Migrate(ctx, pool, schema); err != nil {
		return err
	}

	wallet, err := walletclient.New(walletURL, nil)
	if err != nil {
		return err
	}

	// --- core ---
	svc := service.New(service.Deps{
		UnitOfWork: postgres.NewUnitOfWork(pool),
		Intents:    postgres.NewIntentRepository(pool),
		Wallet:     wallet,
		NewID:      func() string { return uuid.Must(uuid.NewV7()).String() },
		BillTTL:    billTTL,
	})

	// --- inbound adapter ---
	srv := &http.Server{
		Addr:              addr,
		Handler:           httpadapter.NewHandler(svc, log),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("http server listening", "addr", addr, "wallet_url", walletURL, "schema", schema)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
