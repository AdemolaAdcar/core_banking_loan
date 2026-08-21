# Loan Account Subledger

## What changed

Implements the Loan Account Subledger end to end — the LoanAccount state
machine, all seven domain intents (booking, disbursement, repayment/
payoff/recovery, delinquency, charge-off, modification, accrual), a
typed GLPostingAPI client, and a read-optimized balance projection
rebuilt exclusively from GL's posted lines. This service owns no
balance field of any kind on `LoanAccount` itself and constructs no
journal entry lines — every posting decision (which GL account, debit
or credit) belongs entirely to GLPostingAPI; this service only ever
selects a posting-rule code and the request shape (`amount` /
`allocation` / `capitalization`) that code requires.

## State machine

Five statuses (`Approved`, `PendingDisbursement`, `Disbursed`, `Closed`,
`ChargedOff`), one authoritative transition table
(`internal/domain/loan_account.go`'s `validTransitions`) consulted
everywhere a status change happens — no other code path mutates
`Status`:

```
Approved            -> PendingDisbursement   (createDisbursement accepted)
PendingDisbursement -> Disbursed             (PR-DISB-01 confirmed posted)
Disbursed           -> Approved              (reverseDisbursement confirmed)
Disbursed           -> Closed                (payoff settlement, PR-PAYOFF-01 confirmed)
Disbursed           -> ChargedOff            (charge-off confirmed, PR-CHGOFF-01 posted)
```

`Closed` and `ChargedOff` are both terminal — no cure/reinstatement
transition exists out of either, confirmed against the approved spec
before implementation rather than assumed. Delinquency (`dpd`/
`dpdBucket`/`nonAccrualFlag`) is a separate overlay that never gates or
replaces `Status` — an account stays `Disbursed` while past due.

## Operation → posting-rule mapping

One rule code per triggering condition, restated from
`internal/postingrules/rules.go`'s `Triggers` table (the single source
of truth `internal/service`'s tests assert against, so this table and
the code can never silently drift):

| Operation | Rule | Condition |
|---|---|---|
| `createDisbursement` (funding confirmed) | PR-DISB-01 | Approved account, funding confirmed |
| `reverseDisbursement` | PR-DISB-02 | Ops-confirmed return/failure on a pending-reversal disbursement |
| `runDailyAccrual` | PR-ACCR-01 | one posting per eligible Disbursed, non-excluded account per business day |
| `receiveRepaymentNotification` — ordinary repayment | PR-REPAY-01 | matched, not ChargedOff, no valid/unexpired payoff quote (or underpaid against one) |
| `receiveRepaymentNotification` — payoff | PR-PAYOFF-01 | matched, valid unexpired `payoffQuoteId`, amount ≥ quote total |
| `receiveRepaymentNotification` — recovery | PR-CHGOFF-02 | matched account already ChargedOff — checked **first**, ahead of any `payoffQuoteId` |
| `reverseRepayment` | PR-REPAY-02 | confirmed NSF/return or misapplication of a Posted repayment |
| `runDailyDelinquencyAssessment` | PR-DELINQ-01 | account newly crosses out of Current with a supplied late-fee amount |
| `waiveFee` | PR-DELINQ-02 | confirmed authorized waiver of an Assessed fee |
| `recordChargeOff` | PR-CHGOFF-01 | confirmed, approved charge-off decision on a Disbursed account |
| `applyModification` — Branch A | PR-MOD-01 | capitalization present; Branch B (rate/term only) posts **nothing** — a true no-op, not a zero-amount posting |

`bookLoanAccount` makes **zero** GLPostingAPI calls by design — booking
moves no balance, so there is nothing to translate into a posting.

## Balance projection: always rebuilt, never incrementally adjusted

`BalanceProjection` (`internal/domain/projection.go`) is a read-optimized
cache, not a source of truth. `RebuildFromLines` is the **only** function
in the codebase that produces one, and it always recomputes from the
full line history GL hands back — there is no `UPDATE ... SET
outstanding_principal = outstanding_principal - $1` anywhere. This is
what makes calling it redundantly, out of order, or after a missed event
safe: the result is always exactly what GL's ledger says right now,
never a function of what the projection previously held. Only the three
receivable-category GL accounts (`1200`/`1300`/`1400`) contribute;
`CashNostro`, the income accounts, and `AllowanceForLoanLosses` are
present in a full statement but deliberately excluded — they are not
part of what the borrower still owes.

The repayment waterfall (fees → interest → principal,
`internal/service/repayment.go`'s `waterfall`) applies against this live
projection; any amount beyond total outstanding becomes suspense,
deliberately **not** posted as principal/interest/fee credit, since
crediting more than what is actually owed would misstate the
receivable.

## What's explicitly out of scope, flagged rather than silently worked around

Same discipline every prior service in this repo has followed — implement
what's in this role's scope, flag the boundary loudly rather than
inventing a subsystem that belongs to someone else:

- **PartyAPI is not called.** `bookLoanAccount` accepts any non-empty
  `partyId` without validating it against a live party record
  (REQ-CB-BOOK-002's Blocked/Closed/KYC-not-Verified rejection is not
  enforced here). There is no running Party/CIF integration wired into
  this service yet.
- **PaymentAPI is not called.** `ConfirmDisbursementFunding` — the method
  that actually posts PR-DISB-01 and flips `PendingDisbursement ->
  Disbursed` — is fully implemented and tested against a fake GL client,
  but is not yet wired to a live HTTP/Kafka trigger a PaymentAPI
  `payment.disbursement.confirmed` consumer would invoke. Same pattern
  for the repayment notification path: `receiveRepaymentNotification`
  is exposed and fully implemented, but nothing here yet consumes a real
  Payment Execution event to call it.
- **DPD is supplied, not derived.** `runDailyDelinquencyAssessment` takes
  an explicit `[]DelinquencyUpdate{loanAccountId, dpd, ...}` list
  (REQ-CB-DELINQ-001's "derive DPD by comparing cumulative scheduled
  amounts due against cumulative PR-REPAY-01 postings" needs an
  amortization-schedule subsystem this role does not own and this build
  does not have). Everything from a *known* DPD onward — bucket
  transition, PR-DELINQ-01 fee assessment, non-accrual flagging, event
  publishing — is fully implemented and tested; only the schedule-derivation
  step feeding it is deferred. `internal/api`'s handler currently calls
  this with an empty update list until that producer exists.
- **`projectedInterestAmount` in a payoff quote is not forward-projected.**
  `GetPayoffQuote` returns the account's currently-outstanding
  `InterestReceivable` as of the last posted PR-ACCR-01, not a
  day-by-day projection through `goodThrough` — a full day-count
  projection engine is out of this role's scope.
- **`quoteCache` (issued payoff quotes) is in-process, not persisted.**
  Only the quote's *terms* are cached, purely so a later
  `receiveRepaymentNotification` can validate a `payoffQuoteId`
  reference — `GetPayoffQuote`'s actual balance figures are always
  computed live, never served from this cache. A process restart or a
  second instance behind a load balancer won't see a quote issued by
  another instance; this degrades to an expired/not-found lookup, which
  falls back to ordinary repayment processing (never a silent
  misapplication) — a known simplification, not a hidden one.
- **A concrete Kafka-backed `outbox.Publisher`** — not built here,
  consistent with every other service's first pass. The transactional
  outbox write is complete (`loan.account.booked`,
  `loan.account.disbursed`, `loan.repayment.posted`,
  `loan.account.closed`, `loan.account.chargedoff`,
  `delinquency.status.changed`, `loan.nonaccrual.flagged`,
  `payment.match.failed`, `loan.terms.modified`); nothing yet delivers
  those rows to a broker.
- **KMS integration, migration execution against a real deployment,
  integration tests against live infrastructure, CI** — all out of
  scope for this pass. `Makefile`'s `test-integration` target exists
  and points at `internal/integration/...`, but that package has not
  been written yet — same follow-up shape as `services/party`/
  `services/crm`/`services/gl` before their own follow-up phases.

## Idempotency

Every write operation is idempotent at two independent layers, the same
two-backstop pattern used throughout this repo:

1. **HTTP middleware** (`internal/api/idempotency.go`, ported verbatim):
   `Idempotency-Key` required on every write route, replay of the same
   key+payload returns the cached response, same key with a different
   payload hash is a 409.
2. **Domain-level no-op discipline**: `BookLoanAccount` looks up by
   `approvalReferenceId` first and returns the existing account rather
   than erroring; `CreateDisbursement`/`RecordChargeOff`/
   `ApplyModification` look up by their own idempotency key first;
   `ReceiveRepaymentNotification` checks all three possible prior
   outcomes (Repayment/Payoff/Recovery) before doing anything; accrual
   uses a deterministic `accr:{loanAccountId}:{asOf}` key so a retried
   batch run never double-posts; late-fee assessment checks for an
   existing `Fee` row for the same `(loanAccountId, scheduledDueDate)`
   before posting.

## Crash-safety around GL calls: intermediate states are visible, never silently stuck

Every operation that both changes `LoanAccount.Status` *and* posts to GL
persists an intermediate, poll-able state **before** calling GL, so a
crash between the two never leaves a resource silently stuck in what
looks like its prior state:

- `ReverseDisbursement` saves `DisbursementReversalPending` before
  calling `PR-DISB-02`; a GL failure leaves the disbursement visible via
  `GET` in that state, safe to retry.
- `RecordChargeOff` saves `ChargeOffPending` before calling `PR-CHGOFF-01`,
  same pattern.
- `ReverseRepayment` saves `RepaymentReversalPending` before calling
  `PR-REPAY-02`.
- `WaiveFee` saves `FeeWaivePending` before calling `PR-DELINQ-02`.

None of these transition the `LoanAccount` itself until GL confirms.

## Unit tests written so far — and what's explicitly incomplete

**Domain-level tests are complete** (`internal/domain`, 11 top-level
tests, 20 subtests, all passing):

- `TestValidTransitions` / `TestInvalidTransitions` — every one of the 5
  legal transitions, and an exhaustively enumerated 15 illegal pairs (out
  of all 25 non-self (from,to) combinations among the 5 statuses), each
  asserted to return `ErrInvalidTransition` with the correct `From`/`To`.
  A guard (`if tested != 15`) fails loudly if the exhaustive matrix ever
  changes without the test being updated.
- `TestNoCureFromChargedOff` / `TestClosedIsTerminal` — regression tests
  for the specific ground-rule conflict resolved with the requester
  before implementation: neither terminal status has a cure/reinstatement
  transition, unlike a generic Pending/Active/Delinquent template.
- `TestIsTerminal`, `TestNewLoanAccountStartsApproved`.
- `TestRebuildFromLines_Sequence` — a realistic disbursement → accrual →
  late fee → repayment sequence, proving the waterfall math end to end.
- `TestRebuildFromLines_IgnoresNonReceivableAccounts`,
  `TestRebuildFromLines_EmptyIsZero`,
  `TestRebuildFromLines_AlwaysRecomputesFromFullHistory` (proves no
  incremental-delta path exists to drift), `TestRebuildFromLines_MultiCurrencyRejected`.

**Service-level tests are now written** — 48 top-level tests (plus
subtests) in `internal/service`, all passing under both `go test ./...`
and `go test -race ./...`, using `fakestore_test.go`'s in-memory
`store.Store`/`store.Tx` double and `glclient.Fake`:

- **Idempotent replay** for every write operation (`BookLoanAccount`,
  `CreateDisbursement`, `ConfirmDisbursementFunding`, `RecordChargeOff`,
  `ReceiveRepaymentNotification`) — a retried call with the same key
  returns the original result and posts to GL exactly once.
- **State-machine coverage at the service layer**: `Approved ->
  PendingDisbursement -> Disbursed -> {Approved (reversal) | Closed
  (payoff) | ChargedOff}`, plus the invalid-transition rejections
  (`CreateDisbursement` from a non-Approved account, `RecordChargeOff`
  from a non-Disbursed or already-ChargedOff account).
- **Terminal-state rejection** — the flagged gap: `ReceiveRepaymentNotification`
  against a `Closed` account (`ErrTerminalAccount`), and `UpdateDelinquency`
  against both `Closed` and `ChargedOff` accounts, table-driven over both.
- **Branch-order regression tests** — also flagged gaps:
  `TestReceiveRepaymentNotification_ChargedOffAccount_AlwaysRecovery_EvenWithPayoffQuoteID`
  proves branch (0) (recovery) is checked unconditionally, before ANY
  `payoffQuoteId` branching, by handing a ChargedOff account a
  still-valid quote and confirming it's still treated as a recovery
  (asserts PR-CHGOFF-02 fires, PR-PAYOFF-01 never does).
  `TestRunDailyDelinquencyAssessment_SkipsChargedOffAndClosedAccounts`
  proves the batch-level skip happens before `UpdateDelinquency` is ever
  called for a terminal account, not merely caught by its own guard.
- **The misapplied-repayment correction path** — the other flagged gap:
  `TestReverseRepayment_Misapplied_CreatesCorrectionOnNewAccount` reverses
  a repayment posted against the WRONG account and confirms it reverses
  the original (PR-REPAY-02), posts a fresh PR-REPAY-01 against the
  CORRECT account, and links the two via `CorrectedRepaymentID` — two
  PR-REPAY-01 calls total, one PR-REPAY-02.
- **The repayment waterfall** (fee → interest → principal), payoff exact
  match / overpayment-with-suspense / underpayment-degrades-to-ordinary-
  repayment, Branch A (capitalization) vs. Branch B (true no-op, zero GL
  calls) of `ApplyModification`, delinquency bucket transitions and
  non-accrual flag set/clear (including that `NonAccrualSince` is
  retained across a cure), daily accrual (posts, skips zero-principal,
  excludes non-accrual accounts from eligibility, one failure doesn't
  abort the batch), and fee waiver.
- **Crash-safety**: a simulated GL rejection/unavailability during
  `ConfirmDisbursementFunding` leaves both the disbursement and the
  account exactly as they were (no partial state); a simulated GL
  failure during `ReverseDisbursement` leaves the disbursement visible
  as `ReversalPending`, not silently stuck.
- **Concurrent posting, under `-race`** — the remaining flagged gap:
  `TestConcurrentDisbursementFundingConfirmations_DifferentAccounts` runs
  20 real goroutines, each confirming funding for its own distinct loan
  account through the mutex-guarded fake store and fake GL client
  simultaneously — zero data races, all 20 succeed independently, each
  account ends up `Disbursed` with exactly one PR-DISB-01 posting.

**Fixed while writing these tests**: three test files
(`internal/domain/loan_account_test.go`, `internal/domain/projection_test.go`,
`internal/service/fakestore_test.go`) had drifted from `gofmt`'s expected
struct-tag/map-literal alignment — cosmetic only, caught by `gofmt -l .`,
fixed with `gofmt -w`. `go build ./...`, `go vet ./...`, `gofmt -l .`
(clean), and `go test ./...` all pass.

## Follow-up, not yet started

Matching the exact sequence every other service in this repo has gone
through after its first codegen pass:

1. **Migration runner verification** via the actual `golang-migrate` CLI
   (`up`/`version`/`down`/`up` against a disposable Postgres) — the
   schema and `Makefile` targets exist but this has not been run yet.
2. **Integration test** against live Postgres (and likely a fake or real
   GL endpoint, since this service's own correctness depends on
   GLPostingAPI's responses) — `internal/integration/` does not exist
   yet; `make test-integration` currently has nothing to run.
3. **`.github/workflows/las-ci.yml`** — same three-job shape
   (`build-test`/`migrations`/`integration-test`) as
   `party-ci.yml`/`crm-ci.yml`/`gl-ci.yml`.
4. **Kafka outbox publisher** wiring (`internal/relay`, ported from the
   other three services).
