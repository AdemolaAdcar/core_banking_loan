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
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	kafkago "github.com/segmentio/kafka-go"

	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/api"
	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/auth"
	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/glclient"
	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/relay"
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

	// LAS_SERVICE_KAFKA_BROKERS is a comma-separated broker list. The
	// relay (internal/relay) reads unpublished rows from the outbox
	// table this service's own writes populate and delivers them here —
	// see internal/outbox's package doc comment for why this is a
	// separate, independently-polling path rather than a synchronous
	// publish inside the request handler.
	kafkaBrokers := os.Getenv("LAS_SERVICE_KAFKA_BROKERS")
	if kafkaBrokers == "" {
		return fmt.Errorf("LAS_SERVICE_KAFKA_BROKERS is required")
	}
	kafkaWriter := &kafkago.Writer{
		Addr:         kafkago.TCP(strings.Split(kafkaBrokers, ",")...),
		Balancer:     &kafkago.LeastBytes{},
		RequiredAcks: kafkago.RequireAll,
		// Topic is deliberately left unset -- every outbox entry sets
		// its own Topic per message (every loan.*/delinquency.*/
		// payment.* event type shares this one Writer), and kafka-go
		// rejects a Writer that has both a fixed Topic and per-message
		// topics.
	}
	defer kafkaWriter.Close()
	publisher := relay.NewPublisher(st, kafkaWriter, 100)

	outboxPollInterval := 2 * time.Second
	if v := os.Getenv("LAS_SERVICE_OUTBOX_POLL_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			outboxPollInterval = d
		}
	}
	go runOutboxRelay(ctx, publisher, outboxPollInterval)

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

// runOutboxRelay polls the outbox on a fixed interval until ctx is
// canceled. A publish error is logged, never fatal -- the outbox pattern
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
				log.Printf("las-service: outbox relay error: %v", err)
				continue
			}
			if n > 0 {
				log.Printf("las-service: outbox relay published %d event(s)", n)
			}
		}
	}
}
