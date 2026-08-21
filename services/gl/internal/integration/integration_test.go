//go:build integration

// Package integration exercises the GL Posting Engine against a real
// Postgres — every other test in this codebase runs against in-memory
// fakes (internal/service, internal/api) or, for internal/auth, a real
// httptest server. Nothing before this file has ever actually run
// migrations/0001_init.up.sql, exercised internal/store/postgres's real
// SQL, or proven the two database-level invariants (1: balanced entries,
// via a deferred constraint trigger; 3: immutability, via GRANT/REVOKE
// on the gl_app role) through the ACTUAL application code path — those
// were previously verified only with raw psql commands run by hand (see
// PR_DESCRIPTION.md). This file proves them through
// internal/store/postgres and internal/service themselves, authenticated
// as gl_app exactly as cmd/gl-service/main.go requires in production.
// Gated behind the "integration" build tag and requires a working Docker
// daemon:
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

	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/coa"
	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/domain"
	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/postingrules"
	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/service"
	"github.com/AdemolaAdcar/core_banking_loan/services/gl/internal/store/postgres"
)

func usd(amount int64) domain.Money { return domain.Money{Amount: amount, Currency: "USD"} }

func TestGLPostingEngine_AgainstLivePostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	adminPool, appPool, connStr := startPostgres(ctx, t)
	_ = connStr

	chart := coa.MustLoad()
	st := postgres.New(appPool) // connected AS gl_app -- the real production identity
	svc := service.New(st, chart)

	// --- 1. Post through the real application code path, as gl_app ----
	amt := usd(1500000)
	out, err := svc.PostJournalEntry(ctx, service.PostJournalEntryInput{
		IdempotencyKey: "disb:1", PostingRuleCode: postingrules.PRDISB01, LoanAccountID: "loan-1", Amount: &amt,
	})
	if err != nil {
		t.Fatalf("PostJournalEntry: %v", err)
	}
	if !out.Balanced() {
		t.Fatalf("expected balanced entry")
	}

	reloaded, err := st.GetJournalEntry(ctx, out.ID)
	if err != nil {
		t.Fatalf("GetJournalEntry: %v", err)
	}
	if len(reloaded.Lines) != 2 || reloaded.Lines[0].GLAccount != coa.LoanReceivable {
		t.Fatalf("unexpected reloaded lines: %+v", reloaded.Lines)
	}

	// --- 2. Invariant 1, live: a hand-crafted UNBALANCED insert, as
	// gl_app, through the real deferred constraint trigger -----------
	unbalancedID := "je-unbalanced-manual"
	tx, err := appPool.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning manual tx: %v", err)
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO journal_entries (id, source_event_id, posting_rule_code, posting_rule_version, loan_account_id, posted_at, period_id, is_prior_period_adjustment)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		unbalancedID, "disb:unbalanced", "PR-DISB-01", "1.0.0", "loan-1", time.Now().UTC(), domain.PeriodID(time.Now().UTC()), false)
	if err != nil {
		t.Fatalf("inserting unbalanced entry header: %v", err)
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO journal_entry_lines (journal_entry_id, line_order, gl_account, direction, amount, currency, running_balance_after)
		 VALUES ($1,0,$2,'DEBIT',1500000,'USD',1500000)`, unbalancedID, coa.LoanReceivable)
	if err != nil {
		t.Fatalf("inserting unbalanced debit line: %v", err)
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO journal_entry_lines (journal_entry_id, line_order, gl_account, direction, amount, currency, running_balance_after)
		 VALUES ($1,1,$2,'CREDIT',1499999,'USD',-1499999)`, unbalancedID, coa.CashNostro) // deliberately off by 1
	if err != nil {
		t.Fatalf("inserting unbalanced credit line: %v", err)
	}
	if err := tx.Commit(ctx); err == nil {
		t.Fatalf("expected the database's deferred constraint trigger to reject this unbalanced entry at COMMIT, but it succeeded")
	}

	var count int
	if err := appPool.QueryRow(ctx, "SELECT count(*) FROM journal_entries WHERE id = $1", unbalancedID).Scan(&count); err != nil {
		t.Fatalf("checking rollback: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected the rejected unbalanced entry to be fully rolled back, found %d rows", count)
	}

	// --- 3. Invariant 3, live: gl_app cannot UPDATE or DELETE --------
	_, err = appPool.Exec(ctx, "UPDATE journal_entries SET posting_rule_code = 'HACKED' WHERE id = $1", out.ID)
	if err == nil {
		t.Fatalf("expected UPDATE to be rejected for gl_app")
	}
	_, err = appPool.Exec(ctx, "DELETE FROM journal_entry_lines WHERE journal_entry_id = $1", out.ID)
	if err == nil {
		t.Fatalf("expected DELETE to be rejected for gl_app")
	}

	// --- 4. Reversal, through the real application code path ---------
	target := "disb:1"
	reversal, err := svc.PostJournalEntry(ctx, service.PostJournalEntryInput{
		IdempotencyKey: "disb:1:reversal", PostingRuleCode: postingrules.PRDISB02, LoanAccountID: "loan-1",
		Amount: &amt, ReversalOfSourceEventID: &target,
	})
	if err != nil {
		t.Fatalf("posting reversal: %v", err)
	}
	if reversal.Lines[0].Direction != domain.Credit {
		t.Fatalf("expected the reversal's first line to be a CREDIT (mirroring the original DEBIT), got %s", reversal.Lines[0].Direction)
	}

	// --- 5. Closing the current period blocks BOTH reversal (already
	// proven above via the July/August close-then-reverse pattern isn't
	// exercised here, so prove the ordinary-posting side directly) AND
	// any new ordinary posting from silently landing in it -----------
	currentPeriod := domain.PeriodID(time.Now().UTC())
	if _, err := svc.ClosePeriod(ctx, currentPeriod, "ops.analyst"); err != nil {
		t.Fatalf("ClosePeriod: %v", err)
	}
	otherAmt := usd(500)
	_, err = svc.PostJournalEntry(ctx, service.PostJournalEntryInput{
		IdempotencyKey: "delinq:1", PostingRuleCode: postingrules.PRDELINQ01, LoanAccountID: "loan-2", Amount: &otherAmt,
	})
	if err == nil {
		t.Fatalf("expected an ordinary (non-adjustment) posting to be rejected once its own current period (%s) is closed", currentPeriod)
	}

	// A prior-period adjustment explicitly targeting that same closed
	// period must still succeed.
	adjOut, err := svc.PostJournalEntry(ctx, service.PostJournalEntryInput{
		IdempotencyKey: "delinq:1:adjustment", PostingRuleCode: postingrules.PRDELINQ01, LoanAccountID: "loan-2",
		Amount: &otherAmt, PriorPeriodAdjustmentForPeriodID: &currentPeriod,
	})
	if err != nil {
		t.Fatalf("expected a prior-period adjustment against the closed period to succeed: %v", err)
	}
	if !adjOut.IsPriorPeriodAdjustment {
		t.Fatalf("expected isPriorPeriodAdjustment=true")
	}

	// --- 6. Live trial balance reflects every posting so far ---------
	tb, err := svc.GetTrialBalance(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("GetTrialBalance: %v", err)
	}
	var totalDebits, totalCredits int64
	for _, l := range tb {
		totalDebits += l.DebitTotal
		totalCredits += l.CreditTotal
	}
	if totalDebits != totalCredits {
		t.Fatalf("expected live trial balance to always balance, got debits=%d credits=%d", totalDebits, totalCredits)
	}

	_ = adminPool // kept alive for the duration of the test via t.Cleanup registered in startPostgres
}

func startPostgres(ctx context.Context, t *testing.T) (adminPool, appPool *pgxpool.Pool, connStrTemplate string) {
	t.Helper()

	migrationPath, err := filepath.Abs(filepath.Join("..", "..", "migrations", "0001_init.up.sql"))
	if err != nil {
		t.Fatalf("resolving migration path: %v", err)
	}

	pgContainer, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("gl"),
		tcpostgres.WithUsername("gl"),
		tcpostgres.WithPassword("gl"),
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

	// The migration creates gl_app with LOGIN but no usable password
	// (see migrations/0001_init.up.sql's package doc comment) -- set one
	// here, exactly the deployment step PR_DESCRIPTION.md documents as
	// required before the role is usable.
	if _, err := admin.Exec(ctx, "ALTER ROLE gl_app WITH PASSWORD 'integration-test-password'"); err != nil {
		t.Fatalf("setting gl_app password: %v", err)
	}

	host, err := pgContainer.Host(ctx)
	if err != nil {
		t.Fatalf("getting container host: %v", err)
	}
	port, err := pgContainer.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatalf("getting mapped port: %v", err)
	}
	appConnStr := "postgres://gl_app:integration-test-password@" + host + ":" + port.Port() + "/gl?sslmode=disable"
	app, err := pgxpool.New(ctx, appConnStr)
	if err != nil {
		t.Fatalf("connecting as gl_app: %v", err)
	}
	t.Cleanup(app.Close)
	if err := app.Ping(ctx); err != nil {
		t.Fatalf("pinging gl_app connection: %v", err)
	}

	return admin, app, adminConnStr
}
