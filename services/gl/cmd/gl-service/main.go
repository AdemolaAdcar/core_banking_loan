// Command gl-service starts the General Ledger & Posting Engine HTTP
// service: chi router, Postgres-backed store, and the embedded Chart of
// Accounts, wired together here and nowhere else in this codebase.
//
// The database connection this process uses MUST authenticate as the
// gl_app role the migration creates (see migrations/0001_init.up.sql),
// never as a superuser/owner role -- gl_app is the role invariant 3's
// GRANT/REVOKE restrictions actually apply to. Connecting as any other
// role would silently defeat the database-level immutability
// enforcement this service depends on as its last line of defense.
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

	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/api"
	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/auth"
	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/coa"
	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/service"
	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/store/postgres"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("gl-service: %v", err)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	databaseURL := os.Getenv("GL_SERVICE_DATABASE_URL")
	if databaseURL == "" {
		return fmt.Errorf("GL_SERVICE_DATABASE_URL is required (must authenticate as the gl_app role -- see package doc comment)")
	}
	addr := os.Getenv("GL_SERVICE_ADDR")
	if addr == "" {
		addr = ":8082"
	}

	chart, err := coa.Load()
	if err != nil {
		return fmt.Errorf("loading chart of accounts: %w", err)
	}

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("connecting to database: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("pinging database: %w", err)
	}

	jwksURL := os.Getenv("GL_SERVICE_JWKS_URL")
	if jwksURL == "" {
		return fmt.Errorf("GL_SERVICE_JWKS_URL is required")
	}
	validator := auth.NewJWKSValidator(jwksURL, os.Getenv("GL_SERVICE_TOKEN_ISSUER"), os.Getenv("GL_SERVICE_TOKEN_AUDIENCE"))

	st := postgres.New(pool)
	svc := service.New(st, chart)
	srv := api.NewServer(svc, st, validator)

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("gl-service: listening on %s", addr)
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
