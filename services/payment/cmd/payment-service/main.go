// Command payment-service starts the Payment Execution HTTP service:
// chi router, Postgres-backed store, a configurable railclient.Client
// (sandbox or ACH), and an HTTP-backed AccountAPI typed client, wired
// together here and nowhere else in this codebase.
//
// The database connection this process uses MUST authenticate as the
// payment_app role the migration creates (see migrations/0001_init.up.sql),
// never as a superuser/owner role.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/accountclient"
	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/api"
	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/auth"
	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/railclient"
	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/rails/ach"
	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/rails/sandbox"
	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/service"
	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/store/postgres"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("payment-service: %v", err)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	databaseURL := os.Getenv("PAYMENT_SERVICE_DATABASE_URL")
	if databaseURL == "" {
		return fmt.Errorf("PAYMENT_SERVICE_DATABASE_URL is required (must authenticate as the payment_app role -- see package doc comment)")
	}
	addr := os.Getenv("PAYMENT_SERVICE_ADDR")
	if addr == "" {
		addr = ":8084"
	}

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("connecting to database: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("pinging database: %w", err)
	}

	jwksURL := os.Getenv("PAYMENT_SERVICE_JWKS_URL")
	if jwksURL == "" {
		return fmt.Errorf("PAYMENT_SERVICE_JWKS_URL is required")
	}
	validator := auth.NewJWKSValidator(jwksURL, os.Getenv("PAYMENT_SERVICE_TOKEN_ISSUER"), os.Getenv("PAYMENT_SERVICE_TOKEN_AUDIENCE"))

	accountBaseURL := os.Getenv("PAYMENT_SERVICE_ACCOUNT_BASE_URL")
	if accountBaseURL == "" {
		return fmt.Errorf("PAYMENT_SERVICE_ACCOUNT_BASE_URL is required (AccountAPI's base URL -- this service's typed client is the only way it talks to LAS)")
	}
	var accountTokens accountclient.TokenSource
	if tokenURL := os.Getenv("PAYMENT_SERVICE_ACCOUNT_TOKEN_URL"); tokenURL != "" {
		accountTokens = accountclient.NewClientCredentialsTokenSource(tokenURL, os.Getenv("PAYMENT_SERVICE_ACCOUNT_CLIENT_ID"), os.Getenv("PAYMENT_SERVICE_ACCOUNT_CLIENT_SECRET"), os.Getenv("PAYMENT_SERVICE_ACCOUNT_SCOPE"))
	}
	accountClient := accountclient.NewHTTPClient(accountBaseURL, accountTokens)

	rail, err := buildRail()
	if err != nil {
		return err
	}

	st := postgres.New(pool)
	svc := service.New(st, rail, accountClient)
	srv := api.NewServer(svc, st, validator)

	reconciliationInterval := 30 * time.Second
	if v := os.Getenv("PAYMENT_SERVICE_RECONCILIATION_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			reconciliationInterval = d
		}
	}
	go runReconciliationSweep(ctx, svc, reconciliationInterval)

	inboundPollInterval := 30 * time.Second
	if v := os.Getenv("PAYMENT_SERVICE_INBOUND_POLL_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			inboundPollInterval = d
		}
	}
	railName := os.Getenv("PAYMENT_SERVICE_RAIL")
	if railName == "" {
		railName = "sandbox"
	}
	go runInboundSweep(ctx, svc, railName, inboundPollInterval)

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("payment-service: listening on %s (rail=%s)", addr, railName)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	}
}

// buildRail selects the concrete railclient.Client this process uses.
// PAYMENT_SERVICE_RAIL=ach selects internal/rails/ach — but see
// PR_DESCRIPTION.md: its PayoutAccountResolver here is an EMPTY
// InMemoryPayoutDirectory, since a real PartyAPI-backed payout-account
// lookup is a deferred integration this pass does not build (every
// initiateDisbursement call would fail RAIL_REJECTED until one is wired
// in and populated). Any other value (including unset) selects the
// deterministic sandbox rail, matching this role's ground rule that a
// no-real-rail-dependency stub must exist and be maintained.
func buildRail() (railclient.Client, error) {
	switch os.Getenv("PAYMENT_SERVICE_RAIL") {
	case "ach":
		cfg := ach.Config{
			OriginRoutingNumber:      os.Getenv("PAYMENT_SERVICE_ACH_ORIGIN_ROUTING_NUMBER"),
			OriginName:               os.Getenv("PAYMENT_SERVICE_ACH_ORIGIN_NAME"),
			CompanyID:                os.Getenv("PAYMENT_SERVICE_ACH_COMPANY_ID"),
			CompanyName:              os.Getenv("PAYMENT_SERVICE_ACH_COMPANY_NAME"),
			DestinationRoutingNumber: os.Getenv("PAYMENT_SERVICE_ACH_DESTINATION_ROUTING_NUMBER"),
			DestinationName:          os.Getenv("PAYMENT_SERVICE_ACH_DESTINATION_NAME"),
		}
		return ach.New(cfg, ach.InMemoryPayoutDirectory{}), nil
	default:
		return sandbox.New(), nil
	}
}

// runReconciliationSweep polls railclient.Client.Confirm for every
// still-Submitted OUTBOUND instruction on a fixed interval until ctx is
// canceled -- see internal/service/reconciliation.go's doc comment for
// why this polling pattern is the correct one for a rail with no push
// mechanism. A sweep error is logged, never fatal -- the next interval
// retries.
func runReconciliationSweep(ctx context.Context, svc *service.Service, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			summary, err := svc.RunReconciliationSweep(ctx)
			if err != nil {
				log.Printf("payment-service: reconciliation sweep error: %v", err)
				continue
			}
			if summary.Checked > 0 {
				log.Printf("payment-service: reconciliation sweep checked=%d confirmed=%d failed=%d pending=%d unmatched=%d",
					summary.Checked, summary.Confirmed, summary.Failed, summary.StillPending, summary.Unmatched)
			}
		}
	}
}

// runInboundSweep drains railclient.Client.ReceiveInbound on a fixed
// interval until ctx is canceled.
func runInboundSweep(ctx context.Context, svc *service.Service, railName string, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			summary, err := svc.ReceiveInboundPayments(ctx, railName)
			if err != nil {
				log.Printf("payment-service: inbound sweep error: %v", err)
				continue
			}
			if summary.Received > 0 || summary.Returned > 0 {
				log.Printf("payment-service: inbound sweep received=%d returned=%d returnedUnmatched=%d returnedNoCompliantPath=%d",
					summary.Received, summary.Returned, summary.ReturnedUnmatched, summary.ReturnedNoCompliantPath)
			}
		}
	}
}
