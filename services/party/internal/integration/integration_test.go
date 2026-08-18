//go:build integration

// Package integration exercises the Party/CIF service against real
// Postgres and Kafka — every other test in this codebase runs against
// in-memory fakes (internal/service, internal/api) or, for
// internal/auth/internal/relay, against fakes/a real-but-local httptest
// server. Nothing before this file has ever actually run
// migrations/0001_init.up.sql, exercised internal/store/postgres's real
// SQL and real AES-GCM encrypt/decrypt round-trip, or proven a message
// published via internal/relay is actually consumable from a real
// broker. This file is gated behind the "integration" build tag and
// requires a working Docker daemon (testcontainers-go starts and tears
// down disposable Postgres and Kafka containers itself — no manually
// managed infrastructure, no shared/reused containers, no fixed ports):
//
//	go test -tags=integration ./internal/integration/... -v
package integration

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	kafkago "github.com/segmentio/kafka-go"
	"github.com/testcontainers/testcontainers-go"
	tckafka "github.com/testcontainers/testcontainers-go/modules/kafka"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/AdemolaAdcar/core_banking_loan/services/party/internal/events"
	"github.com/AdemolaAdcar/core_banking_loan/services/party/internal/pii"
	"github.com/AdemolaAdcar/core_banking_loan/services/party/internal/relay"
	"github.com/AdemolaAdcar/core_banking_loan/services/party/internal/service"
	"github.com/AdemolaAdcar/core_banking_loan/services/party/internal/store/postgres"
)

func TestPartyLifecycle_AgainstLivePostgresAndKafka(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	pool, encryptor := startPostgres(ctx, t)
	brokers := startKafka(ctx, t)
	// Topics are provisioned explicitly, matching how production topics
	// are expected to be managed (out of band, not auto-created on
	// first publish -- see the writer built below).
	createTopics(ctx, t, brokers, events.TopicPartyCreated, events.TopicPartyUpdated, events.TopicPartyTombstoned)

	st := postgres.New(pool, encryptor)
	svc := service.New(st)

	// --- 1. Create, through the real encryption + real SQL path -------
	dob := time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC)
	created, err := svc.FindOrCreateParty(ctx, service.FindOrCreateInput{
		IdempotencyKey: "int-test-create",
		FirstName:      "Integration",
		LastName:       "Test",
		DateOfBirth:    dob,
		SSN:            "123-45-6789",
		Email:          "integration@example.com",
		Phone:          "5125551234",
	})
	if err != nil {
		t.Fatalf("FindOrCreateParty: %v", err)
	}
	if !created.Created {
		t.Fatalf("expected a new party to be created, got matched=%v", created.Decision.Matched)
	}

	// Read it back through the real store -- proves the AES-GCM
	// encrypt-on-write / decrypt-on-read round-trip actually works
	// against real Postgres, not just in-memory structs.
	reloaded, err := st.GetParty(ctx, created.Party.ID)
	if err != nil {
		t.Fatalf("GetParty after create: %v", err)
	}
	if reloaded.FirstName != "Integration" || reloaded.LastName != "Test" {
		t.Fatalf("expected decrypted name to round-trip through Postgres, got %q %q", reloaded.FirstName, reloaded.LastName)
	}
	if reloaded.SSNLast4() != "6789" {
		t.Fatalf("expected ssnLast4 6789, got %q", reloaded.SSNLast4())
	}

	// --- 2. Dedup against a real indexed SQL query, not the fake ------
	dup, err := svc.FindOrCreateParty(ctx, service.FindOrCreateInput{
		IdempotencyKey: "int-test-dup",
		FirstName:      "Integration",
		LastName:       "Test",
		DateOfBirth:    dob,
		SSN:            "123-45-6789", // same SSN -> must hit ssn_hash exact match
	})
	if err != nil {
		t.Fatalf("FindOrCreateParty (duplicate): %v", err)
	}
	if dup.Created {
		t.Fatalf("expected the SSN-exact dedup rule to match against real Postgres, got a second party created")
	}
	if dup.Party.ID != created.Party.ID {
		t.Fatalf("expected dedup match to resolve to the original party %s, got %s", created.Party.ID, dup.Party.ID)
	}

	// --- 3. Update -------------------------------------------------
	newEmail := "updated@example.com"
	updated, err := svc.UpdateParty(ctx, service.UpdatePartyInput{PartyID: created.Party.ID, Email: &newEmail})
	if err != nil {
		t.Fatalf("UpdateParty: %v", err)
	}
	if updated.Email != newEmail {
		t.Fatalf("expected updated email %q, got %q", newEmail, updated.Email)
	}

	// --- 4. Tombstone ------------------------------------------------
	tombstoned, err := svc.TombstoneParty(ctx, service.TombstonePartyInput{
		PartyID: created.Party.ID, Reason: "integration test cleanup", Actor: "integration-test",
	})
	if err != nil {
		t.Fatalf("TombstoneParty: %v", err)
	}
	if !tombstoned.Tombstoned {
		t.Fatalf("expected party to be tombstoned")
	}

	// --- 5. Relay: real outbox rows -> real Kafka broker --------------
	writer := &kafkago.Writer{Addr: kafkago.TCP(brokers...), Balancer: &kafkago.LeastBytes{}}
	defer writer.Close()
	publisher := relay.NewPublisher(st, writer, 100)

	published, err := publisher.PublishUnpublished(ctx)
	if err != nil {
		t.Fatalf("PublishUnpublished: %v", err)
	}
	// party.created (step 1) + party.updated (step 3) + party.tombstoned
	// (step 4) -- the SSN-exact dedup match in step 2 wrote NO outbox
	// entry, since it matched an existing party rather than creating one.
	if published != 3 {
		t.Fatalf("expected 3 outbox entries published (created/updated/tombstoned), got %d", published)
	}

	// A second call must find nothing left -- proves MarkPublished
	// actually persisted against real Postgres, not just in memory.
	again, err := publisher.PublishUnpublished(ctx)
	if err != nil {
		t.Fatalf("PublishUnpublished (second call): %v", err)
	}
	if again != 0 {
		t.Fatalf("expected no unpublished entries left after the first publish, got %d", again)
	}

	// --- 6. Consume from the real broker to prove delivery happened ---
	reader := kafkago.NewReader(kafkago.ReaderConfig{
		Brokers:     brokers,
		Topic:       events.TopicPartyCreated,
		GroupID:     "integration-test-consumer",
		StartOffset: kafkago.FirstOffset,
	})
	defer reader.Close()

	readCtx, readCancel := context.WithTimeout(ctx, 30*time.Second)
	defer readCancel()
	msg, err := reader.ReadMessage(readCtx)
	if err != nil {
		t.Fatalf("expected to consume a real party.created message from kafka: %v", err)
	}

	var payload events.PartyCreatedPayload
	if err := json.Unmarshal(msg.Value, &payload); err != nil {
		t.Fatalf("decoding consumed party.created payload: %v", err)
	}
	if payload.PartyID != created.Party.ID {
		t.Fatalf("expected consumed message for party %s, got %s", created.Party.ID, payload.PartyID)
	}
	if payload.Status != "Active" || payload.KYCStatus != "Unverified" {
		t.Fatalf("unexpected consumed payload: %+v", payload)
	}
}

func startPostgres(ctx context.Context, t *testing.T) (*pgxpool.Pool, *pii.AESGCMEncryptor) {
	t.Helper()

	migrationPath, err := filepath.Abs(filepath.Join("..", "..", "migrations", "0001_init.up.sql"))
	if err != nil {
		t.Fatalf("resolving migration path: %v", err)
	}

	pgContainer, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("party"),
		tcpostgres.WithUsername("party"),
		tcpostgres.WithPassword("party"),
		tcpostgres.WithInitScripts(migrationPath),
		testcontainers.WithAdditionalWaitStrategy(
			wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("starting postgres container: %v", err)
	}
	t.Cleanup(func() { _ = pgContainer.Terminate(context.Background()) })

	connStr, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("getting postgres connection string: %v", err)
	}
	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		t.Fatalf("connecting to postgres: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("pinging postgres: %v", err)
	}

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("generating test encryption key: %v", err)
	}
	encryptor, err := pii.NewAESGCMEncryptor(key)
	if err != nil {
		t.Fatalf("building encryptor: %v", err)
	}
	return pool, encryptor
}

func createTopics(ctx context.Context, t *testing.T, brokers []string, topics ...string) {
	t.Helper()
	conn, err := kafkago.DialContext(ctx, "tcp", brokers[0])
	if err != nil {
		t.Fatalf("dialing kafka to create topics: %v", err)
	}
	defer conn.Close()

	controller, err := conn.Controller()
	if err != nil {
		t.Fatalf("finding kafka controller: %v", err)
	}
	controllerConn, err := kafkago.DialContext(ctx, "tcp", net.JoinHostPort(controller.Host, strconv.Itoa(controller.Port)))
	if err != nil {
		t.Fatalf("dialing kafka controller: %v", err)
	}
	defer controllerConn.Close()

	configs := make([]kafkago.TopicConfig, len(topics))
	for i, topic := range topics {
		configs[i] = kafkago.TopicConfig{Topic: topic, NumPartitions: 1, ReplicationFactor: 1}
	}
	if err := controllerConn.CreateTopics(configs...); err != nil {
		t.Fatalf("creating topics %v: %v", topics, err)
	}
}

func startKafka(ctx context.Context, t *testing.T) []string {
	t.Helper()

	kafkaContainer, err := tckafka.Run(ctx, "confluentinc/confluent-local:7.5.0", tckafka.WithClusterID("integration-test"))
	if err != nil {
		t.Fatalf("starting kafka container: %v", err)
	}
	t.Cleanup(func() { _ = kafkaContainer.Terminate(context.Background()) })

	brokers, err := kafkaContainer.Brokers(ctx)
	if err != nil {
		t.Fatalf("getting kafka brokers: %v", err)
	}
	return brokers
}
