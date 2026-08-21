// Command las-service starts the Loan Account Subledger HTTP service:
// chi router, Postgres-backed store, and an HTTP-backed GLPostingAPI
// typed client, wired together here and nowhere else in this codebase.
//
// The database connection this process uses MUST authenticate as the
// las_app role the migration creates (see migrations/0001_init.up.sql),
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

	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/api"
	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/auth"
	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/glclient"
	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/service"
	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/store/postgres"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("las-service: %v", err)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	databaseURL := os.Getenv("LAS_SERVICE_DATABASE_URL")
	if databaseURL == "" {
		return fmt.Errorf("LAS_SERVICE_DATABASE_URL is required (must authenticate as the las_app role -- see package doc comment)")
	}
	addr := os.Getenv("LAS_SERVICE_ADDR")
	if addr == "" {
		addr = ":8083"
	}
	glBaseURL := os.Getenv("LAS_SERVICE_GL_BASE_URL")
	if glBaseURL == "" {
		return fmt.Errorf("LAS_SERVICE_GL_BASE_URL is required (GLPostingAPI's base URL -- this service's typed client is the only way it talks to GL)")
	}

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("connecting to database: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("pinging database: %w", err)
	}

	jwksURL := os.Getenv("LAS_SERVICE_JWKS_URL")
	if jwksURL == "" {
		return fmt.Errorf("LAS_SERVICE_JWKS_URL is required")
	}
	validator := auth.NewJWKSValidator(jwksURL, os.Getenv("LAS_SERVICE_TOKEN_ISSUER"), os.Getenv("LAS_SERVICE_TOKEN_AUDIENCE"))

	var tokens glclient.TokenSource
	if tokenURL := os.Getenv("LAS_SERVICE_GL_TOKEN_URL"); tokenURL != "" {
		tokens = glclient.NewClientCredentialsTokenSource(tokenURL, os.Getenv("LAS_SERVICE_GL_CLIENT_ID"), os.Getenv("LAS_SERVICE_GL_CLIENT_SECRET"), os.Getenv("LAS_SERVICE_GL_SCOPE"))
	}
	glc := glclient.NewHTTPClient(glBaseURL, tokens)

	st := postgres.New(pool)
	svc := service.New(st, glc)
	srv := api.NewServer(svc, st, validator)

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("las-service: listening on %s", addr)
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
