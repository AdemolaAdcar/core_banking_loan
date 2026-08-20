# General Ledger & Posting Engine

## What changed

Implements the GL Posting Engine end to end — spec extension, Chart of
Accounts manifest, posting-rule logic, database-level invariant
enforcement, and HTTP transport. This is the one component in the
entire system permitted to write a `JournalEntry`; every one of this
role's seven non-negotiable invariants (Section 7.3) is treated as a
hard constraint, enforced at multiple independent layers, not asserted
once and trusted.

## Posting rules implemented, and their manifest version

**All 11 rules** from `docs/design-notes.md` Appendix B ("Consolidated
Posting-Rule Catalog"), exactly as approved in the currently-shipped
spec — no rule invented, no account mapping changed:

| Rule | Debit / Credit |
|---|---|
| PR-DISB-01 | Dr LoanReceivable / Cr CashNostro |
| PR-DISB-02 | reversal of PR-DISB-01 |
| PR-ACCR-01 | Dr InterestReceivable / Cr InterestIncome |
| PR-REPAY-01 | Dr CashNostro / Cr FeeReceivable + InterestReceivable + LoanReceivable |
| PR-REPAY-02 | reversal of PR-REPAY-01 |
| PR-DELINQ-01 | Dr FeeReceivable / Cr FeeIncome |
| PR-DELINQ-02 | reversal of PR-DELINQ-01 |
| PR-PAYOFF-01 | Dr CashNostro / Cr FeeReceivable + InterestReceivable + LoanReceivable |
| PR-CHGOFF-01 | Dr AllowanceForLoanLosses / Cr FeeReceivable + InterestReceivable + LoanReceivable |
| PR-CHGOFF-02 | Dr CashNostro / Cr RecoveryIncome (NOT a reversal) |
| PR-MOD-01 | Dr LoanReceivable / Cr InterestReceivable + FeeReceivable |

**Posting-rule manifest version: `1.0.0`** (`postingrules.ManifestVersion`,
`services/gl/internal/postingrules/rules.go`) — stamped onto every
posted entry's `postingRuleVersion` field. Bump this deliberately
whenever a rule's account mapping or logic changes.

## Two known, pre-existing defects — implemented exactly as spec'd, flagged prominently, not silently accepted

Per your explicit direction: implement per the currently-approved spec
and flag loudly, since neither violates one of the 7 numbered invariants
directly and fixing either is a spec-design decision outside this
codegen role's authority.

1. **`AllowanceForLoanLosses` (1900) is never funded.** It's correctly
   modeled as a contra-asset in the new Chart of Accounts manifest
   (natural CREDIT balance), but no rule in the 11-rule catalog ever
   credits it — PR-CHGOFF-01 is the only rule that touches this account,
   and it only debits it. Every write-off drives the account further
   into an unfunded position. See `PRCHGOFF01Lines`'s doc comment in
   `internal/postingrules/rules.go` and `specs/coa/README.md`.
2. **`PR-REPAY-01`/`PR-PAYOFF-01` derive the CashNostro debit as the SUM
   of the allocation credits.** `PostJournalEntryRequest`'s `Allocation`
   shape has no independent "amount actually received" field, so an
   over- or under-payment relative to what was actually collected is
   structurally invisible to the ledger — the function has no other
   number to compare the sum against. See `PRREPAY01Lines`'s doc
   comment.

Both were flagged in an earlier phase (the Chart of Accounts & Posting
Rules Agent), remain unresolved in the currently-approved v0.4.0 spec,
and are re-flagged here rather than fixed unilaterally or silently
implemented as if correct.

## What's explicitly out of scope, flagged rather than silently worked around

- **GL cannot validate `loanAccountId` against a live account service.**
  There is no running Loan Account Subledger service in this repo yet
  (same situation `services/crm` flagged for its own `loanAccountId`
  dependency). `postJournalEntry` accepts any syntactically plausible
  `loanAccountId` without existence verification against AccountAPI.
- **A concrete Kafka-backed `outbox.Publisher`** — not built here,
  consistent with how `services/party` and `services/crm` handled the
  identical situation on their first pass. The transactional outbox
  write (`gl.entry.posted`, `gl.period.closed`) is complete and tested;
  nothing yet delivers those rows to a broker.
- **KMS integration, migration execution against a real deployment,
  integration tests against live infrastructure, CI** — all
  deliberately out of scope for this codegen pass, matching the exact
  boundaries `services/party`/`services/crm` drew before their own
  follow-up phases addressed each one individually. The one exception:
  the two database-level invariants below WERE verified live (see
  below) — that verification was too central to this role's own
  "database is the last line of defense" requirement to defer.
- **The Postgres running-balance snapshot (`runningBalanceAfter`) is a
  best-effort, non-authoritative annotation.** It's computed from a
  live query at write time and stored for read convenience (matching
  what `journal-entry.schema.json` documents), but under concurrent
  writes to the *same* `(loanAccountId, glAccount)` pair without an
  explicit row lock, two concurrent lines could theoretically compute
  their snapshot from the same pre-write balance. This does **not**
  violate any of the 7 invariants — `getTrialBalance`/
  `getStatementOfAccount`/`getGlAccountBalance` never read this stored
  snapshot as authoritative; they always re-derive their own live
  `SUM()` from scratch (invariant 6). Documented here as a known,
  minor, non-invariant-violating simplification rather than adding a
  `SELECT ... FOR UPDATE` row-locking scheme to an already-large change.

## Verified live against real Postgres, not just written

The two database-level invariants — this role's own "last line of
defense" requirement — were run against an actual disposable Postgres
container before anything was built on top of them:

- **Invariant 1** (a `DEFERRABLE INITIALLY DEFERRED` constraint trigger
  on `journal_entry_lines`, fired per-row but evaluated at COMMIT once
  every line of an atomic posting has landed): confirmed a balanced
  2-line entry commits; confirmed an unbalanced entry is rejected
  **at COMMIT** with the whole transaction rolled back (`SELECT count(*)
  FROM journal_entries WHERE id = 'je-2'` → `0` after the rejected
  attempt); confirmed a multi-currency entry is rejected; confirmed
  fewer than 2 lines is rejected.
- **Invariant 3**: confirmed the `gl_app` role — the only role this
  service's own database connection may ever use (see
  `cmd/gl-service/main.go`'s package doc comment) — gets
  `permission denied for table journal_entries` on `UPDATE`, and the
  same on `DELETE` against `journal_entry_lines`.

## Spec version implemented

- `specs/openapi/gl-posting-engine.yaml` **v0.4.0** (breaking from
  v0.3.0) — added `postingRuleVersion`/`periodId`/
  `isPriorPeriodAdjustment`/`adjustmentForPeriodId` to `JournalEntry`,
  `priorPeriodAdjustmentForPeriodId` to `PostJournalEntryRequest`, and
  three new resources: `GET /trial-balance`,
  `GET /loan-accounts/{loanAccountId}/statement`,
  `GET/POST /periods/{periodId}`.
- `specs/schemas/journal-entry.schema.json` (extended, same increment).
- `specs/asyncapi/gl-posting-engine-events.yaml` **v0.4.0** — added
  `gl.period.closed`.
- `specs/coa/chart-of-accounts.json` **v1.0.0** (new) — the first
  committed Chart of Accounts this repo has ever had; previously only 8
  bare account-name strings existed as an example string in a schema
  description.

## Unit tests written, organized by invariant and required edge case

**77 tests total** across `internal/coa`, `internal/domain`,
`internal/postingrules`, `internal/auth`, `internal/service`, and
`internal/api` — `go build ./...`, `go vet ./...`, `gofmt -l .` (clean),
`go test ./...`, and `go test -race ./...` all pass.

### Invariant 1 — every entry's debits sum exactly to its credits, per currency
- `internal/domain`: `TestValidateBalanced_TwoLine_Balanced`,
  `TestValidateBalanced_Unbalanced_Rejected`,
  `TestValidateBalanced_ZeroOrNegativeAmount_Rejected`,
  `TestNewJournalEntry_Balanced_Succeeds`,
  `TestNewJournalEntry_Unbalanced_Refused` — `NewJournalEntry` is the
  ONLY constructor in the codebase; it cannot produce an unbalanced
  value.
- `internal/postingrules`: every rule's test (`TestPRDISB01Lines`,
  `TestPRACCR01Lines`, `TestPRDELINQ01Lines`, `TestPRCHGOFF02Lines`,
  `TestPRREPAY01Lines_*`, `TestPRPAYOFF01Lines`, `TestPRCHGOFF01Lines`,
  `TestPRMOD01Lines_*`) asserts `domain.ValidateBalanced` on its output.
- `internal/service`: `TestPostJournalEntry_PRDISB01_AlwaysBalanced`.
- **Database, verified live** (see above) — the actual "last line of
  defense," a `DEFERRABLE INITIALLY DEFERRED` constraint trigger, not
  application code alone.

### Invariant 2 — posting is atomic
- `TestPostJournalEntry_OutboxFailure_NothingPersisted` — a simulated
  failure partway through the transaction (after the entry write,
  before the outbox insert) leaves **zero** entries persisted; the
  entire `WithinTx` call rolls back together.

### Invariant 3 — posted entries are immutable
- **Database, verified live** (see above) — `gl_app` has no UPDATE/DELETE
  grant on `journal_entries`/`journal_entry_lines`, confirmed against a
  real Postgres instance.
- `store.Tx`/`store.Store` have no `UpdateJournalEntry`/
  `DeleteJournalEntry` method at all (structural, not a runtime-testable
  fact, but see `store.go`'s package doc comment).
- `TestReversal_ProducesNewEntry_OriginalUnchanged` — the actual
  correction path (a reversal) never touches the original entry's
  stored lines.

### Invariant 4 — Idempotency-Key required; same key+payload replays; same key+different payload conflicts
- `internal/api`: `TestPostJournalEntry_MissingIdempotencyKey_400`,
  `TestIdempotency_ReplaySameKeySamePayload_NoSecondEntry`,
  `TestIdempotency_SameKeyDifferentPayload_409` — the middleware-level
  replay cache (ported from `services/party`/`services/crm`).
- `internal/service`:
  `TestPostJournalEntry_DuplicateSourceEventID_ReturnsWinnerNotError` —
  the SECOND, independent backstop: a genuine database-level race
  (`journal_entries.source_event_id UNIQUE` constraint) resolves to the
  original entry, not an error and not a duplicate posting.

### Invariant 5 — every entry stores its sourceEventId and posting-rule version
- `TestPostJournalEntry_StoresSourceEventIDAndPostingRuleVersion`.

### Invariant 6 — trial balance / statement always computed live
- `TestGetTrialBalance_ReflectsEveryNewlyPostedEntry_NoSeparateUpdateStep`
  — calls the exact same `GetTrialBalance` method twice, before and
  after a new posting, with no intervening "refresh" call of any kind;
  the second call reflects the new entry because it's architecturally
  impossible for it not to (there is no other code path that could have
  updated a cached total, because none exists).
- `TestGetAccountBalance_UnknownAccount_NotFound`.

### Invariant 7 — period close locks prior-period entries from reversal; corrections after close are new current-period entries tagged as prior-period adjustments
- `TestPostJournalEntry_ReversalOfClosedPeriodEntry_Rejected`.
- `TestPostJournalEntry_PriorPeriodAdjustment_TaggedCorrectly` — the
  correction entry belongs to the CURRENT open period, tagged
  `isPriorPeriodAdjustment=true` / `adjustmentForPeriodId` pointing at
  the closed period.
- `TestPostJournalEntry_PriorPeriodAdjustment_TargetPeriodNotClosed_Rejected`.
- `TestClosePeriod_ChronologicalOrder_Enforced` — cannot close August
  while July (which has posted entries) is still Open; closing July
  first, then August, succeeds.
- `TestClosePeriod_Idempotent`.
- `internal/api`: `TestClosePeriod_ChronologicalOrder_409`,
  `TestClosePeriod_Idempotent`,
  `TestPostJournalEntry_ReversalOfClosedPeriod_422`.

### Required edge case — entries with more than two lines
- `internal/postingrules`:
  `TestPRREPAY01Lines_FullAllocation_ProducesFourLines`,
  `TestPRCHGOFF01Lines` (3 lines).
- `internal/service`: `TestPostJournalEntry_PRREPAY01_MoreThanTwoLines`.

### Required edge case — multi-currency rejection (until explicitly in scope)
- `internal/domain`: `TestValidateBalanced_MultiCurrency_Rejected`.
- `internal/service`: `TestPostJournalEntry_MultiCurrencyAllocation_Rejected`.
- **Database, verified live** (see above).

### Required edge case — concurrent posting to the same account
- `TestPostJournalEntry_ConcurrentPostingToSameAccount` — 20 real
  goroutines, each posting a distinct entry to the same
  `(loanAccountId, glAccount)` pair against a mutex-guarded fake store,
  run under `go test -race`: zero data races, all 20 succeed
  independently, and the live-queried aggregate balance afterward
  correctly reflects all 20 postings. Genuine database-level concurrent-
  transaction behavior (no incorrect rejection, no deadlock) was not
  re-verified live in this pass — see "what's explicitly out of scope."

### Required edge case — reversal of a reversal
- `internal/domain`:
  `TestMirrorForReversal_ReversalOfAReversal_RestoresOriginalDirections`
  — pure double-negation proof at the lowest level.
- `internal/service`: `TestPostJournalEntry_ReversalOfAReversal` — posts
  PR-DISB-01, reverses it with PR-DISB-02, then reverses THAT reversal
  with PR-DISB-02 again; confirms the second reversal's lines exactly
  restore the original entry's direction/account/amount, and that it
  references the FIRST reversal directly (one hop only — the audit
  chain is never collapsed).
- Also: `TestPostJournalEntry_ReversalAmountMismatch_Rejected` (a
  caller-mistake guard: the caller-supplied amount must match what's
  actually being reversed, even though the mirrored lines are already
  correct regardless) and
  `TestPostJournalEntry_ReversalTargetNotFound_Rejected`.

### Other request-shape validation
- `TestPostJournalEntry_UnknownRuleCode_Rejected`,
  `TestPostJournalEntry_WrongInputShape_Rejected`,
  `TestPostJournalEntry_MissingRequiredMetadata_Rejected`.

### Supporting packages
- `internal/coa` (4 tests): all 8 accounts load with correct
  type/normal-balance/contra-asset flags;
  `TestManifestMatchesCanonicalSource` guards against
  `services/gl/internal/coa/chart-of-accounts.json` (the embedded copy
  `go:embed` requires, since embed cannot reach outside this module)
  drifting from `specs/coa/chart-of-accounts.json` (the canonical
  source) — fails loudly, byte-for-byte, if they ever diverge.
- `internal/auth` (8 tests, ported from `services/party`): JWKS/RS256
  validation against real RSA keys and a real JWKS endpoint, including
  the RS256/HS256 algorithm-confusion attack rejection.
- `internal/api`: auth enforcement (`TestAuth_MissingAuthorizationHeader_401`,
  `TestAuth_InsufficientScope_403`), plus every read endpoint
  (`TestGetJournalEntry_NotFound_404`, `TestFindBySourceEvent_RoundTrips`,
  `TestGetAccountBalance_UnknownAccount_404`,
  `TestGetTrialBalance_ReflectsPostedEntries`).
