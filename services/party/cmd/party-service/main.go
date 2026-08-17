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
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	kafkago "github.com/segmentio/kafka-go"

	"github.com/AdemolaAdcar/core_banking_loan/services/party/internal/api"
	"github.com/AdemolaAdcar/core_banking_loan/services/party/internal/auth"
	"github.com/AdemolaAdcar/core_banking_loan/services/party/internal/pii"
	"github.com/AdemolaAdcar/core_banking_loan/services/party/internal/relay"
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

	// PARTY_SERVICE_KAFKA_BROKERS is a comma-separated broker list. The
	// relay (internal/relay) reads unpublished rows from the outbox
	// table this service's own writes populate and delivers them here —
	// see internal/outbox's package doc comment for why this is a
	// separate, independently-polling path rather than a synchronous
	// publish inside the request handler.
	kafkaBrokers := os.Getenv("PARTY_SERVICE_KAFKA_BROKERS")
	if kafkaBrokers == "" {
		return fmt.Errorf("PARTY_SERVICE_KAFKA_BROKERS is required")
	}
	kafkaWriter := &kafkago.Writer{
		Addr:         kafkago.TCP(strings.Split(kafkaBrokers, ",")...),
		Balancer:     &kafkago.LeastBytes{},
		RequiredAcks: kafkago.RequireAll,
		// Topic is deliberately left unset -- every outbox entry sets
		// its own Topic per message (party.created vs. party.updated vs.
		// party.tombstoned all share this one Writer); kafka-go rejects
		// a Writer that has both a fixed Topic and per-message topics.
	}
	defer kafkaWriter.Close()
	publisher := relay.NewPublisher(st, kafkaWriter, 100)

	pollInterval := 2 * time.Second
	if v := os.Getenv("PARTY_SERVICE_OUTBOX_POLL_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			pollInterval = d
		}
	}
	go runOutboxRelay(ctx, publisher, pollInterval)

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

// runOutboxRelay polls the outbox on a fixed interval until ctx is
// canceled. A publish error is logged, never fatal — the outbox pattern
// exists precisely so a transient Kafka outage doesn't affect the
// request path; the next poll retries whatever wasn't published,
// including anything skipped by this failed attempt.
func runOutboxRelay(ctx context.Context, publisher *relay.Publisher, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := publisher.PublishUnpublished(ctx)
			if err != nil {
				log.Printf("party-service: outbox relay error: %v", err)
				continue
			}
			if n > 0 {
				log.Printf("party-service: outbox relay published %d event(s)", n)
			}
		}
	}
}
