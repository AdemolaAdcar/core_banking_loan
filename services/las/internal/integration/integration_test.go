//go:build integration

// Package integration exercises the Loan Account Subledger against a
// real Postgres — every other test in this codebase runs against the
// in-memory fakeStore (internal/service) or in-memory glclient.Fake.
// Nothing before this file has ever actually run migrations/
// 0001_init.up.sql, exercised internal/store/postgres's real SQL
// (including its ON CONFLICT upsert clauses), or proven that las_app's
// GRANTs actually restrict what this service's own database connection
// can do. GLPostingAPI itself is NOT live here — this service's own
// scope is the state machine, persistence, and balance projection, not
// GL's posting logic, so glclient.Fake stands in for GL exactly as it
// does in every internal/service unit test; see PR_DESCRIPTION.md for
// why a live GL is out of scope for this pass. Gated behind the
// "integration" build tag and requires a working Docker daemon:
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

	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/glclient"
	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/service"
	"github.com/AdemolaAdcar/core_banking_loan/services/las/internal/store/postgres"
)

func usd(amount int64) domain.Money { return domain.Money{Amount: amount, Currency: "USD"} }

func TestLoanAccountLifecycle_AgainstLivePostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	adminPool, appPool := startPostgres(ctx, t)

	gl := glclient.NewFake()
	st := postgres.New(appPool) // connected AS las_app -- the real production identity
	svc := service.New(st, gl)

	// --- 1. Book, through the real SQL path -----------------------------
	terms := domain.TermSet{PrincipalAmount: usd(100000), AnnualInterestRateBps: 1200, TermMonths: 24, DayCountConvention: "ACTUAL_365"}
	account, err := svc.BookLoanAccount(ctx, service.BookLoanAccountInput{
		ApprovalReferenceID: "approval-int-1", PartyID: "party-1", BookedBy: "officer-1", Terms: terms,
	})
	if err != nil {
		t.Fatalf("BookLoanAccount: %v", err)
	}
	reloaded, err := st.GetLoanAccount(ctx, account.LoanAccountID)
	if err != nil {
		t.Fatalf("GetLoanAccount after book: %v", err)
	}
	if reloaded.Status != domain.StatusApproved || reloaded.CurrentTermVersion.PrincipalAmount.Amount != 100000 {
		t.Fatalf("unexpected reloaded account: %+v", reloaded)
	}

	// Idempotent replay through the real database's unique constraint on
	// approval_reference_id (FindLoanAccountByApprovalReference), not just
	// the in-memory fake's map lookup.
	replay, err := svc.BookLoanAccount(ctx, service.BookLoanAccountInput{
		ApprovalReferenceID: "approval-int-1", PartyID: "party-1", BookedBy: "officer-1", Terms: terms,
	})
	if err != nil {
		t.Fatalf("BookLoanAccount (replay): %v", err)
	}
	if replay.LoanAccountID != account.LoanAccountID {
		t.Fatalf("expected the replay to return the original account")
	}

	// --- 2. Disburse, through the real SQL path --------------------------
	if _, err := svc.CreateDisbursement(ctx, account.LoanAccountID, "disb-int-1", "officer-1"); err != nil {
		t.Fatalf("CreateDisbursement: %v", err)
	}
	disbursement, err := svc.ConfirmDisbursementFunding(ctx, "disb-int-1", "instr-int-1")
	if err != nil {
		t.Fatalf("ConfirmDisbursementFunding: %v", err)
	}
	if disbursement.JournalEntryID == nil {
		t.Fatalf("expected a journalEntryId to be persisted")
	}
	reloadedAfterDisb, err := st.GetLoanAccount(ctx, account.LoanAccountID)
	if err != nil {
		t.Fatalf("GetLoanAccount after disbursement: %v", err)
	}
	if reloadedAfterDisb.Status != domain.StatusDisbursed {
		t.Fatalf("expected Disbursed in the real database, got %s", reloadedAfterDisb.Status)
	}

	// --- 3. Ordinary repayment, proving the balance_projections cache is
	// actually persisted and re-read from Postgres (not just held in
	// process memory) ------------------------------------------------------
	gl.Statements[account.LoanAccountID] = []domain.StatementLine{
		{GLAccount: domain.GLAccountLoanReceivable, Direction: domain.Debit, Amount: usd(100000), PostedAt: time.Unix(0, 0)},
	}
	ref := account.LoanAccountID
	repayResult, err := svc.ReceiveRepaymentNotification(ctx, service.ReceiveRepaymentNotificationInput{
		IdempotencyKey: "pay-int-1", LoanAccountRef: &ref, Amount: usd(400), Rail: "ACH", ReceivedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("ReceiveRepaymentNotification: %v", err)
	}
	if repayResult.Kind != service.KindRepayment || repayResult.Repayment.Status != domain.RepaymentPosted {
		t.Fatalf("unexpected repayment result: %+v", repayResult)
	}

	// glclient.Fake.Statements is a static test double, not a real
	// accumulating ledger -- it doesn't append the repayment's own
	// credit line automatically. Update it now to simulate GL's real
	// statement catching up (exactly what a real GL would show on the
	// NEXT read), then force a second refresh -- this proves the real
	// Postgres UPSERT's UPDATE-on-conflict path, not just its initial
	// INSERT, actually persists a changed projection.
	gl.Statements[account.LoanAccountID] = []domain.StatementLine{
		{GLAccount: domain.GLAccountLoanReceivable, Direction: domain.Debit, Amount: usd(100000), PostedAt: time.Unix(0, 0)},
		{GLAccount: domain.GLAccountLoanReceivable, Direction: domain.Credit, Amount: usd(400), PostedAt: time.Unix(0, 0)},
	}
	if _, err := svc.GetBalanceProjection(ctx, account.LoanAccountID); err != nil {
		t.Fatalf("GetBalanceProjection (forced refresh): %v", err)
	}

	persistedProjection, found, err := st.GetBalanceProjection(ctx, account.LoanAccountID)
	if err != nil {
		t.Fatalf("GetBalanceProjection: %v", err)
	}
	if !found {
		t.Fatalf("expected a persisted balance_projections row after RefreshBalanceProjection")
	}
	if persistedProjection.OutstandingPrincipal.Amount != 99600 {
		t.Fatalf("expected persisted outstanding principal 99600 (100000-400), got %d", persistedProjection.OutstandingPrincipal.Amount)
	}

	// --- 4. Modification: real term_versions insert, second row --------
	mod, err := svc.ApplyModification(ctx, service.ApplyModificationInput{
		LoanAccountID: account.LoanAccountID, ModificationID: "mod-int-1", EffectiveDate: time.Now(), ConfirmedBy: "ops-1",
		Capitalization: &domain.Capitalization{InterestAmount: usd(50), FeeAmount: usd(0)},
	})
	if err != nil {
		t.Fatalf("ApplyModification: %v", err)
	}
	if mod.NewTermVersion != 2 {
		t.Fatalf("expected new term version 2, got %d", mod.NewTermVersion)
	}
	versions, err := st.ListTermVersions(ctx, account.LoanAccountID)
	if err != nil {
		t.Fatalf("ListTermVersions: %v", err)
	}
	if len(versions) != 2 {
		t.Fatalf("expected 2 persisted term versions, got %d", len(versions))
	}

	// --- 5. Charge-off, then recovery, through the real SQL path --------
	gl.Statements[account.LoanAccountID] = []domain.StatementLine{
		{GLAccount: domain.GLAccountLoanReceivable, Direction: domain.Debit, Amount: usd(50000), PostedAt: time.Unix(0, 0)},
	}
	chargeoff, err := svc.RecordChargeOff(ctx, account.LoanAccountID, "chargeoff-int-1", "ops-1")
	if err != nil {
		t.Fatalf("RecordChargeOff: %v", err)
	}
	if chargeoff.Status != domain.ChargeOffDone {
		t.Fatalf("expected ChargedOff, got %s", chargeoff.Status)
	}
	reloadedAfterChargeOff, err := st.GetLoanAccount(ctx, account.LoanAccountID)
	if err != nil {
		t.Fatalf("GetLoanAccount after charge-off: %v", err)
	}
	if reloadedAfterChargeOff.Status != domain.StatusChargedOff {
		t.Fatalf("expected ChargedOff in the real database, got %s", reloadedAfterChargeOff.Status)
	}

	recoveryResult, err := svc.ReceiveRepaymentNotification(ctx, service.ReceiveRepaymentNotificationInput{
		IdempotencyKey: "recovery-int-1", LoanAccountRef: &ref, Amount: usd(1000), Rail: "ACH", ReceivedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("ReceiveRepaymentNotification (recovery): %v", err)
	}
	if recoveryResult.Kind != service.KindRecovery {
		t.Fatalf("expected a recovery against a ChargedOff account, got Kind=%s", recoveryResult.Kind)
	}
	persistedRecovery, err := st.GetRecovery(ctx, "recovery-int-1")
	if err != nil {
		t.Fatalf("GetRecovery: %v", err)
	}
	if persistedRecovery.Amount.Amount != 1000 {
		t.Fatalf("expected persisted recovery amount 1000, got %d", persistedRecovery.Amount.Amount)
	}

	// --- 6. Every domain event actually landed in the real outbox table -
	unpublished, err := st.ListUnpublished(ctx, 100)
	if err != nil {
		t.Fatalf("ListUnpublished: %v", err)
	}
	expectedTopics := map[string]bool{
		"loan.account.booked": false, "loan.account.disbursed": false, "loan.repayment.posted": false,
		"loan.terms.modified": false, "loan.account.chargedoff": false,
	}
	for _, e := range unpublished {
		if _, ok := expectedTopics[e.Topic]; ok {
			expectedTopics[e.Topic] = true
		}
	}
	for topic, seen := range expectedTopics {
		if !seen {
			t.Fatalf("expected an unpublished outbox entry for topic %q, got topics: %+v", topic, unpublished)
		}
	}

	// --- 7. las_app's GRANTs actually restrict what this connection can
	// do -- the migration documents "no DELETE grant on any table", never
	// previously verified against a real Postgres. This is NOT a ledger-
	// immutability invariant the way GL's is (see migrations/
	// 0001_init.up.sql's package doc comment) -- ordinary rows here ARE
	// mutable via UPDATE, just never via DELETE. --------------------------
	if _, err := appPool.Exec(ctx, "DELETE FROM loan_accounts WHERE id = $1", account.LoanAccountID); err == nil {
		t.Fatalf("expected DELETE to be rejected for las_app (no DELETE grant in the migration)")
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
		tcpostgres.WithDatabase("las"),
		tcpostgres.WithUsername("las"),
		tcpostgres.WithPassword("las"),
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

	// The migration creates las_app with LOGIN but no usable password
	// (see migrations/0001_init.up.sql's package doc comment) -- set one
	// here, exactly the deployment step PR_DESCRIPTION.md documents as
	// required before the role is usable.
	if _, err := admin.Exec(ctx, "ALTER ROLE las_app WITH PASSWORD 'integration-test-password'"); err != nil {
		t.Fatalf("setting las_app password: %v", err)
	}

	host, err := pgContainer.Host(ctx)
	if err != nil {
		t.Fatalf("getting container host: %v", err)
	}
	port, err := pgContainer.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatalf("getting mapped port: %v", err)
	}
	appConnStr := "postgres://las_app:integration-test-password@" + host + ":" + port.Port() + "/las?sslmode=disable"
	app, err := pgxpool.New(ctx, appConnStr)
	if err != nil {
		t.Fatalf("connecting as las_app: %v", err)
	}
	t.Cleanup(app.Close)
	if err := app.Ping(ctx); err != nil {
		t.Fatalf("pinging las_app connection: %v", err)
	}

	return admin, app
}
