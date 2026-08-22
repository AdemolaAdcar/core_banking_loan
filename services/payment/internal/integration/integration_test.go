//go:build integration

// Package integration exercises Payment Execution against a real
// Postgres — every other test in this codebase runs against the
// in-memory fakeStore (internal/service), sandbox.Sandbox, or
// accountclient.Fake. Nothing before this file has ever actually run
// migrations/0001_init.up.sql, exercised internal/store/postgres's real
// SQL (including its ON CONFLICT upsert on payment_instructions), or
// proven that payment_app's GRANTs actually restrict what this
// service's own database connection can do. AccountAPI (LAS) is NOT
// live in this test — accountclient.Fake stands in for it, exactly as
// it does in every internal/service unit test, since no live
// Payment<->LAS cross-service integration exists in this repo yet; see
// PR_DESCRIPTION.md. Gated behind the "integration" build tag and
// requires a working Docker daemon:
//
//	go test -tags=integration ./internal/integration/... -v
package integration

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/accountclient"
	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/railclient"
	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/rails/sandbox"
	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/service"
	"github.com/AdemolaAdcar/core_banking_loan/services/payment/internal/store/postgres"
)

func usd(amount int64) domain.Money { return domain.Money{Amount: amount, Currency: "USD"} }

func TestPaymentExecutionLifecycle_AgainstLivePostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	adminPool, appPool := startPostgres(ctx, t)

	rail := sandbox.New()
	account := accountclient.NewFake()
	st := postgres.New(appPool) // connected AS payment_app -- the real production identity
	svc := service.New(st, rail, account)

	// --- 1. Initiate a disbursement, through the real SQL path --------
	out, err := svc.InitiateDisbursement(ctx, service.InitiateDisbursementInput{
		IdempotencyKey: "instr-1", LoanAccountID: "loan-1", PartyID: "party-1", JournalEntryID: "je-1", Amount: usd(150000),
	})
	if err != nil {
		t.Fatalf("InitiateDisbursement: %v", err)
	}
	reloaded, err := st.GetPaymentInstruction(ctx, out.InstructionID)
	if err != nil {
		t.Fatalf("GetPaymentInstruction after initiate: %v", err)
	}
	if reloaded.Status != domain.StatusSubmitted || reloaded.RailReference == nil {
		t.Fatalf("unexpected reloaded instruction: %+v", reloaded)
	}

	// Idempotent replay through the real database's primary-key lookup,
	// not just the in-memory fake's map lookup.
	replay, err := svc.InitiateDisbursement(ctx, service.InitiateDisbursementInput{
		IdempotencyKey: "instr-1", LoanAccountID: "loan-1", PartyID: "party-1", JournalEntryID: "je-1", Amount: usd(150000),
	})
	if err != nil {
		t.Fatalf("InitiateDisbursement (replay): %v", err)
	}
	if replay.InstructionID != out.InstructionID {
		t.Fatalf("expected the replay to return the original instruction")
	}

	// --- 2. Reconciliation sweep confirms it, through the real SQL
	// UPDATE (ON CONFLICT) path -----------------------------------------
	summary, err := svc.RunReconciliationSweep(ctx)
	if err != nil {
		t.Fatalf("RunReconciliationSweep: %v", err)
	}
	if summary.Confirmed != 1 {
		t.Fatalf("expected 1 confirmed, got %+v", summary)
	}
	reloaded, err = st.GetPaymentInstruction(ctx, out.InstructionID)
	if err != nil {
		t.Fatalf("GetPaymentInstruction after sweep: %v", err)
	}
	if reloaded.Status != domain.StatusExecuted {
		t.Fatalf("expected Executed in the real database, got %s", reloaded.Status)
	}

	unpublished, err := st.ListUnpublished(ctx, 100)
	if err != nil {
		t.Fatalf("ListUnpublished: %v", err)
	}
	var sawConfirmed bool
	for _, e := range unpublished {
		if e.Topic == "payment.disbursement.confirmed" {
			sawConfirmed = true
		}
	}
	if !sawConfirmed {
		t.Fatalf("expected a payment.disbursement.confirmed outbox row, got topics: %+v", unpublished)
	}

	// --- 3. Inbound receipt, then a rail-reported return, through the
	// real SQL path (including the persisted inbound cursor) -----------
	t0 := time.Now().UTC()
	rail.QueueInbound(railclient.InboundEvent{Kind: railclient.InboundReceived, RailReference: "in-1", Amount: usd(500), Rail: "sandbox", OccurredAt: t0})
	inboundSummary, err := svc.ReceiveInboundPayments(ctx, "sandbox")
	if err != nil {
		t.Fatalf("ReceiveInboundPayments: %v", err)
	}
	if inboundSummary.Received != 1 {
		t.Fatalf("expected 1 received, got %+v", inboundSummary)
	}
	cursor, found, err := st.GetInboundCursor(ctx, "sandbox")
	if err != nil {
		t.Fatalf("GetInboundCursor: %v", err)
	}
	if !found || !cursor.Equal(t0) {
		t.Fatalf("expected the persisted cursor to equal %v, got found=%v cursor=%v", t0, found, cursor)
	}

	t1 := t0.Add(time.Hour)
	rail.QueueInbound(railclient.InboundEvent{Kind: railclient.InboundReturned, RailReference: "in-1-ret", OriginalRailReference: "in-1", Amount: usd(500), Rail: "sandbox", OccurredAt: t1})
	inboundSummary, err = svc.ReceiveInboundPayments(ctx, "sandbox")
	if err != nil {
		t.Fatalf("ReceiveInboundPayments (return): %v", err)
	}
	if inboundSummary.Returned != 1 {
		t.Fatalf("expected 1 returned, got %+v", inboundSummary)
	}
	if len(account.ReverseCalls) != 1 || account.ReverseCalls[0].RepaymentID != "in-1" {
		t.Fatalf("expected exactly one ReverseRepayment call for in-1, got %+v", account.ReverseCalls)
	}
	reloaded, err = st.GetPaymentInstruction(ctx, "in-1")
	if err != nil {
		t.Fatalf("GetPaymentInstruction (in-1) after return: %v", err)
	}
	if reloaded.Status != domain.StatusReturned {
		t.Fatalf("expected Returned in the real database, got %s", reloaded.Status)
	}

	// --- 4. Unmatched confirmation: real INSERT into
	// reconciliation_exceptions, never posted speculatively ------------
	if err := svc.ProcessConfirmation(ctx, "no-such-rail-reference", railclient.Outcome{Status: railclient.OutcomeExecuted}); err != nil {
		t.Fatalf("ProcessConfirmation (unmatched): %v", err)
	}
	var exceptionCount int
	if err := appPool.QueryRow(ctx, "SELECT count(*) FROM reconciliation_exceptions WHERE kind = 'UNMATCHED_CONFIRMATION'").Scan(&exceptionCount); err != nil {
		t.Fatalf("querying reconciliation_exceptions: %v", err)
	}
	if exceptionCount != 1 {
		t.Fatalf("expected exactly 1 UNMATCHED_CONFIRMATION row, got %d", exceptionCount)
	}

	// --- 5. payment_app's GRANTs, verified live for the first time:
	// confirms DELETE is rejected -- not a ledger-immutability invariant
	// the way GL's is (see migrations/0001_init.up.sql's doc comment),
	// just confirms no DELETE grant exists on any table --------------
	if _, err := appPool.Exec(ctx, "DELETE FROM payment_instructions WHERE id = $1", out.InstructionID); err == nil {
		t.Fatalf("expected DELETE to be rejected for payment_app (no DELETE grant in the migration)")
	}

	_ = adminPool // kept alive for the duration of the test via t.Cleanup registered in startPostgres
}

func startPostgres(ctx context.Context, t *testing.T) (adminPool, appPool *pgxpool.Pool) {
	t.Helper()

	migrationPath, err := filepath.Abs(filepath.Join("..", "..", "migrations", "0001_init.up.sql"))
	if err != nil {
		t.Fatalf("resolving migration path: %v", err)
	}

	pgContainer, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("payment"),
		tcpostgres.WithUsername("payment"),
		tcpostgres.WithPassword("payment"),
		tcpostgres.WithInitScripts(migrationPath),
		testcontainers.WithAdditionalWaitStrategy(
			wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("starting postgres container: %v", err)
	}
	t.Cleanup(func() { _ = pgContainer.Terminate(context.Background()) })

	adminConnStr, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("getting admin connection string: %v", err)
	}
	admin, err := pgxpool.New(ctx, adminConnStr)
	if err != nil {
		t.Fatalf("connecting as admin: %v", err)
	}
	t.Cleanup(admin.Close)
	if err := admin.Ping(ctx); err != nil {
		t.Fatalf("pinging admin connection: %v", err)
	}

	// The migration creates payment_app with LOGIN but no usable
	// password (see migrations/0001_init.up.sql's package doc comment)
	// -- set one here, exactly the deployment step PR_DESCRIPTION.md
	// documents as required before the role is usable.
	if _, err := admin.Exec(ctx, "ALTER ROLE payment_app WITH PASSWORD 'integration-test-password'"); err != nil {
		t.Fatalf("setting payment_app password: %v", err)
	}

	host, err := pgContainer.Host(ctx)
	if err != nil {
		t.Fatalf("getting container host: %v", err)
	}
	port, err := pgContainer.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatalf("getting mapped port: %v", err)
	}
	appConnStr := "postgres://payment_app:integration-test-password@" + host + ":" + port.Port() + "/payment?sslmode=disable"
	app, err := pgxpool.New(ctx, appConnStr)
	if err != nil {
		t.Fatalf("connecting as payment_app: %v", err)
	}
	t.Cleanup(app.Close)
	if err := app.Ping(ctx); err != nil {
		t.Fatalf("pinging payment_app connection: %v", err)
	}

	return admin, app
}
