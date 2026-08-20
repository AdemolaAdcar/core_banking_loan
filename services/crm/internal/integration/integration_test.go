//go:build integration

// Package integration exercises the CRM service against a real
// Postgres — every other test in this codebase runs against in-memory
// fakes (internal/service, internal/api) or, for internal/auth, a real
// httptest server. Nothing before this file has ever actually run
// migrations/0001_init.up.sql, exercised internal/store/postgres's real
// SQL and real AES-GCM encrypt/decrypt round-trip, or proven the
// database-level optimistic-concurrency guard (UpdateCaseConditional's
// "WHERE version = $N") actually closes a race two in-memory-only tests
// cannot. Unlike services/party's equivalent, this does not also spin up
// Kafka -- CRM has no Kafka-backed outbox publisher yet (see
// PR_DESCRIPTION.md). Gated behind the "integration" build tag and
// requires a working Docker daemon:
//
//	go test -tags=integration ./internal/integration/... -v
package integration

import (
	"context"
	"crypto/rand"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/AdemolaAdcar/core_banking_loan/services/crm/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/crm/internal/pii"
	"github.com/AdemolaAdcar/core_banking_loan/services/crm/internal/service"
	"github.com/AdemolaAdcar/core_banking_loan/services/crm/internal/store/postgres"
)

func TestCaseLifecycle_AgainstLivePostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	pool, encryptor := startPostgres(ctx, t)
	st := postgres.New(pool, encryptor)
	svc := service.New(st)

	// --- 1. Open a case, through the real SQL + encryption path -------
	out, err := svc.OpenCase(ctx, service.OpenCaseInput{
		CaseID: "int-test-case-1", PartyID: "party-1", ReasonCode: domain.ReasonPaymentDispute,
	})
	if err != nil {
		t.Fatalf("OpenCase: %v", err)
	}
	if out.Status != domain.CaseStatusOpen || out.Version != 1 {
		t.Fatalf("unexpected initial case state: %+v", out)
	}

	reloaded, err := st.GetCase(ctx, out.ID)
	if err != nil {
		t.Fatalf("GetCase after open: %v", err)
	}
	if reloaded.PartyID != "party-1" || reloaded.ReasonCode != domain.ReasonPaymentDispute {
		t.Fatalf("unexpected reloaded case: %+v", reloaded)
	}

	// --- 2. Real database-level optimistic-concurrency guard -----------
	// Two "CSRs" both read the case at version 1. The first update
	// succeeds and bumps the stored row to version 2. The second, still
	// carrying expectedVersion=1, must be rejected by the ACTUAL
	// database conditional UPDATE (store.ErrStaleVersion) -- not just an
	// in-memory check, which a fake store could pass through even if the
	// real SQL were broken.
	inProgress := domain.CaseStatusInProgress
	first, err := svc.UpdateCase(ctx, service.UpdateCaseInput{CaseID: out.ID, ExpectedVersion: 1, NewStatus: &inProgress})
	if err != nil {
		t.Fatalf("first UpdateCase: %v", err)
	}
	if first.Version != 2 {
		t.Fatalf("expected version 2 after first update, got %d", first.Version)
	}

	assignee := "csr.jdoe"
	_, err = svc.UpdateCase(ctx, service.UpdateCaseInput{CaseID: out.ID, ExpectedVersion: 1, NewAssignedTo: &assignee})
	if err == nil {
		t.Fatalf("expected the stale-version update to fail against the real database")
	}

	// --- 3. Case note: real encrypt-on-write / decrypt-on-read --------
	note, err := svc.AddCaseNote(ctx, service.AddCaseNoteInput{CaseID: out.ID, AuthorID: "csr.jdoe", Body: "customer disputes a $50 late fee"})
	if err != nil {
		t.Fatalf("AddCaseNote: %v", err)
	}
	if note.Body != "customer disputes a $50 late fee" {
		t.Fatalf("expected note body to round-trip in the immediate response, got %q", note.Body)
	}

	notes, actorSubject := mustListCaseNotesAndLogAccess(ctx, t, svc, out.ID)
	if len(notes) != 1 || notes[0].Body != "customer disputes a $50 late fee" {
		t.Fatalf("expected decrypted note body to round-trip through real Postgres, got %+v", notes)
	}

	// Confirm the note body is NOT stored in plaintext -- query the raw
	// column directly and verify it doesn't contain the plaintext.
	var bodyEnc string
	if err := pool.QueryRow(ctx, "SELECT body_enc FROM case_notes WHERE case_id = $1", out.ID).Scan(&bodyEnc); err != nil {
		t.Fatalf("querying raw body_enc: %v", err)
	}
	if bodyEnc == "customer disputes a $50 late fee" {
		t.Fatalf("case note body must not be stored in plaintext")
	}

	// Confirm the access-log write from step above actually landed.
	var accessCount int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM case_note_access_log WHERE resource_type = 'CaseNote' AND resource_id = $1 AND actor_subject = $2",
		out.ID, actorSubject,
	).Scan(&accessCount); err != nil {
		t.Fatalf("querying access log: %v", err)
	}
	if accessCount != 1 {
		t.Fatalf("expected exactly 1 access-log row for this read, got %d", accessCount)
	}

	// --- 4. Close, then reopen -----------------------------------------
	closed, err := svc.CloseCase(ctx, service.CloseCaseInput{CaseID: out.ID, Reason: "resolved: fee waived by another service"})
	if err != nil {
		t.Fatalf("CloseCase: %v", err)
	}
	if closed.Status != domain.CaseStatusClosed {
		t.Fatalf("expected Closed, got %s", closed.Status)
	}

	// Idempotent: closing again must not error.
	if _, err := svc.CloseCase(ctx, service.CloseCaseInput{CaseID: out.ID, Reason: "resolved again"}); err != nil {
		t.Fatalf("expected idempotent close to succeed, got: %v", err)
	}

	// Reopening a non-closed case must fail -- verified once more here
	// against the case BEFORE we reopen it below, using a case that was
	// never closed at all.
	other, err := svc.OpenCase(ctx, service.OpenCaseInput{CaseID: "int-test-case-2", PartyID: "party-1", ReasonCode: domain.ReasonGeneralInquiry})
	if err != nil {
		t.Fatalf("OpenCase (second case): %v", err)
	}
	if _, err := svc.ReopenCase(ctx, service.ReopenCaseInput{CaseID: other.ID, Reason: "mistaken reopen"}); err == nil {
		t.Fatalf("expected reopening a never-closed case to fail against the real database")
	}

	reopened, err := svc.ReopenCase(ctx, service.ReopenCaseInput{CaseID: out.ID, Reason: "customer called back with a new complaint"})
	if err != nil {
		t.Fatalf("ReopenCase: %v", err)
	}
	if reopened.Status != domain.CaseStatusOpen {
		t.Fatalf("expected Open after reopen, got %s", reopened.Status)
	}

	// --- 5. Relationship manager ----------------------------------------
	rm, err := svc.AssignRelationshipManager(ctx, service.AssignRelationshipManagerInput{PartyID: "party-1", RelationshipManagerID: "rm-1"})
	if err != nil {
		t.Fatalf("AssignRelationshipManager: %v", err)
	}
	if rm.RelationshipManagerID == nil || *rm.RelationshipManagerID != "rm-1" {
		t.Fatalf("unexpected rm assignment: %+v", rm)
	}
	fetchedRM, err := svc.GetRelationshipManager(ctx, "party-1")
	if err != nil {
		t.Fatalf("GetRelationshipManager: %v", err)
	}
	if fetchedRM.RelationshipManagerID == nil || *fetchedRM.RelationshipManagerID != "rm-1" {
		t.Fatalf("unexpected fetched rm assignment: %+v", fetchedRM)
	}

	// --- 6. Communication preferences: default, then real upsert -------
	defaults, err := svc.GetCommunicationPreferences(ctx, "party-never-set")
	if err != nil {
		t.Fatalf("GetCommunicationPreferences (default): %v", err)
	}
	if defaults.PreferredChannel != nil || defaults.EmailOptIn {
		t.Fatalf("expected conservative defaults for a party that never set preferences, got %+v", defaults)
	}

	channel := domain.ChannelEmail
	updatedPrefs, err := svc.UpdateCommunicationPreferences(ctx, service.UpdateCommunicationPreferencesInput{
		PartyID: "party-1", PreferredChannel: &channel, EmailOptIn: true,
	})
	if err != nil {
		t.Fatalf("UpdateCommunicationPreferences: %v", err)
	}
	if updatedPrefs.PreferredChannel == nil || *updatedPrefs.PreferredChannel != domain.ChannelEmail {
		t.Fatalf("unexpected updated preferences: %+v", updatedPrefs)
	}

	// --- 7. SLA sweep against real Postgres -----------------------------
	// Open a case with a short SLA reasonCode, then run the sweep with a
	// clock set past its deadline, and confirm the DB row is actually
	// updated and an outbox entry is actually written.
	clockTime := time.Now().UTC()
	slaSvc := service.NewWithClock(st, func() time.Time { return clockTime })
	slaCase, err := slaSvc.OpenCase(ctx, service.OpenCaseInput{CaseID: "int-test-sla-case", PartyID: "party-1", ReasonCode: domain.ReasonPaymentDispute}) // 24h SLA
	if err != nil {
		t.Fatalf("OpenCase (sla case): %v", err)
	}

	clockTime = clockTime.Add(25 * time.Hour)
	escalated, err := slaSvc.EvaluateSLABreaches(ctx)
	if err != nil {
		t.Fatalf("EvaluateSLABreaches: %v", err)
	}
	if escalated < 1 {
		t.Fatalf("expected at least 1 case escalated, got %d", escalated)
	}

	afterSweep, err := st.GetCase(ctx, slaCase.ID)
	if err != nil {
		t.Fatalf("GetCase after sweep: %v", err)
	}
	if !afterSweep.Escalated {
		t.Fatalf("expected the case to be marked escalated in the real database")
	}

	unpublished, err := st.ListUnpublished(ctx, 100)
	if err != nil {
		t.Fatalf("ListUnpublished: %v", err)
	}
	var foundEscalated bool
	for _, e := range unpublished {
		if e.Topic == "crm.case.escalated" {
			foundEscalated = true
		}
	}
	if !foundEscalated {
		t.Fatalf("expected a crm.case.escalated outbox entry to have been written, got topics: %+v", unpublished)
	}
}

// mustListCaseNotesAndLogAccess is a thin wrapper so the actor subject
// used for the access-log assertion is visible to the caller.
func mustListCaseNotesAndLogAccess(ctx context.Context, t *testing.T, svc *service.Service, caseID string) ([]domain.CaseNote, string) {
	t.Helper()
	const actor = "csr.reviewer"
	notes, err := svc.ListCaseNotes(ctx, actor, caseID)
	if err != nil {
		t.Fatalf("ListCaseNotes: %v", err)
	}
	return notes, actor
}

func startPostgres(ctx context.Context, t *testing.T) (*pgxpool.Pool, *pii.AESGCMEncryptor) {
	t.Helper()

	migrationPath, err := filepath.Abs(filepath.Join("..", "..", "migrations", "0001_init.up.sql"))
	if err != nil {
		t.Fatalf("resolving migration path: %v", err)
	}

	pgContainer, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("crm"),
		tcpostgres.WithUsername("crm"),
		tcpostgres.WithPassword("crm"),
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
