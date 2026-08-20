// Command crm-service starts the CRM HTTP service: chi router,
// Postgres-backed store, AES-256-GCM field-level encryption for
// PII-adjacent content, and the SLA-escalation sweep, wired together
// here and nowhere else in this codebase.
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

	"github.com/AdemolaAdcar/core_banking_loan/services/crm/internal/api"
	"github.com/AdemolaAdcar/core_banking_loan/services/crm/internal/auth"
	"github.com/AdemolaAdcar/core_banking_loan/services/crm/internal/pii"
	"github.com/AdemolaAdcar/core_banking_loan/services/crm/internal/service"
	"github.com/AdemolaAdcar/core_banking_loan/services/crm/internal/store/postgres"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("crm-service: %v", err)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	databaseURL := os.Getenv("CRM_SERVICE_DATABASE_URL")
	if databaseURL == "" {
		return fmt.Errorf("CRM_SERVICE_DATABASE_URL is required")
	}
	addr := os.Getenv("CRM_SERVICE_ADDR")
	if addr == "" {
		addr = ":8081"
	}

	// CRM_SERVICE_ENCRYPTION_KEY is expected to be the already-resolved
	// 32-byte AES-256 key, base64-encoded — resolved from a KMS envelope
	// decrypt at deploy time upstream of this process, same boundary
	// services/party's encryption key takes.
	keyB64 := os.Getenv("CRM_SERVICE_ENCRYPTION_KEY")
	if keyB64 == "" {
		return fmt.Errorf("CRM_SERVICE_ENCRYPTION_KEY is required (base64-encoded 32-byte AES-256 key)")
	}
	key, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil {
		return fmt.Errorf("decoding CRM_SERVICE_ENCRYPTION_KEY: %w", err)
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

	jwksURL := os.Getenv("CRM_SERVICE_JWKS_URL")
	if jwksURL == "" {
		return fmt.Errorf("CRM_SERVICE_JWKS_URL is required")
	}
	validator := auth.NewJWKSValidator(jwksURL, os.Getenv("CRM_SERVICE_TOKEN_ISSUER"), os.Getenv("CRM_SERVICE_TOKEN_AUDIENCE"))

	st := postgres.New(pool, encryptor)
	svc := service.New(st)
	srv := api.NewServer(svc, st, validator)

	slaSweepInterval := 5 * time.Minute
	if v := os.Getenv("CRM_SERVICE_SLA_SWEEP_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			slaSweepInterval = d
		}
	}
	go runSLASweep(ctx, svc, slaSweepInterval)

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("crm-service: listening on %s", addr)
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

// runSLASweep polls for SLA-breaching cases on a fixed interval until ctx
// is canceled -- deliberately a background sweep, not a side effect of
// any GET handler, so reads stay side-effect-free (see
// service.EvaluateSLABreaches's doc comment). A sweep error is logged,
// never fatal -- the next interval retries whatever wasn't escalated.
func runSLASweep(ctx context.Context, svc *service.Service, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := svc.EvaluateSLABreaches(ctx)
			if err != nil {
				log.Printf("crm-service: sla sweep error: %v", err)
				continue
			}
			if n > 0 {
				log.Printf("crm-service: sla sweep escalated %d case(s)", n)
			}
		}
	}
}
