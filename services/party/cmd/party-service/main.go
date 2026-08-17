// Command party-service starts the Party/CIF HTTP service: chi router,
// Postgres-backed store, AES-256-GCM field-level PII encryption, wired
// together here and nowhere else in this codebase.
package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AdemolaAdcar/core_banking_loan/services/party/internal/api"
	"github.com/AdemolaAdcar/core_banking_loan/services/party/internal/auth"
	"github.com/AdemolaAdcar/core_banking_loan/services/party/internal/pii"
	"github.com/AdemolaAdcar/core_banking_loan/services/party/internal/service"
	"github.com/AdemolaAdcar/core_banking_loan/services/party/internal/store/postgres"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("party-service: %v", err)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	databaseURL := os.Getenv("PARTY_SERVICE_DATABASE_URL")
	if databaseURL == "" {
		return fmt.Errorf("PARTY_SERVICE_DATABASE_URL is required")
	}
	addr := os.Getenv("PARTY_SERVICE_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	// PARTY_SERVICE_ENCRYPTION_KEY is expected to be the already-resolved
	// 32-byte AES-256 key, base64-encoded — resolved from a KMS envelope
	// decrypt at deploy time upstream of this process. This process never
	// talks to a KMS itself; that is a deliberate boundary, not an
	// oversight (see internal/pii's package doc comment).
	keyB64 := os.Getenv("PARTY_SERVICE_ENCRYPTION_KEY")
	if keyB64 == "" {
		return fmt.Errorf("PARTY_SERVICE_ENCRYPTION_KEY is required (base64-encoded 32-byte AES-256 key)")
	}
	key, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil {
		return fmt.Errorf("decoding PARTY_SERVICE_ENCRYPTION_KEY: %w", err)
	}
	encryptor, err := pii.NewAESGCMEncryptor(key)
	if err != nil {
		return fmt.Errorf("initializing encryptor: %w", err)
	}

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("connecting to database: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("pinging database: %w", err)
	}

	// PARTY_SERVICE_JWKS_URL points at the internal OAuth2 authorization
	// server's key set (see party-cif.yaml's serviceAuth.tokenUrl for the
	// same server's token endpoint). Issuer/audience are optional but
	// strongly recommended in production — leaving either empty skips
	// that specific check.
	jwksURL := os.Getenv("PARTY_SERVICE_JWKS_URL")
	if jwksURL == "" {
		return fmt.Errorf("PARTY_SERVICE_JWKS_URL is required")
	}
	validator := auth.NewJWKSValidator(jwksURL, os.Getenv("PARTY_SERVICE_TOKEN_ISSUER"), os.Getenv("PARTY_SERVICE_TOKEN_AUDIENCE"))

	st := postgres.New(pool, encryptor)
	svc := service.New(st)
	srv := api.NewServer(svc, st, validator)

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("party-service: listening on %s", addr)
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
