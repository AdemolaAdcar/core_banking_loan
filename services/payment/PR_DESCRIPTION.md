# Payment Execution

## What changed

Implements Payment Execution end to end: a single `railclient.Client`
abstraction every payment rail is built behind, a deterministic
no-real-rail-dependency sandbox, a concrete ACH adapter built against
standard NACHA batch-file conventions, reconciliation logic matching
rail confirmations back to `PaymentInstruction` records, and the
compensating-reversal paths this role's ground rules require for both
directions (outbound disbursement failures/returns, inbound
payment returns). Domain code (`internal/service`) never imports a rail
SDK directly — see `internal/railclient`'s doc comment.

## PaymentRailClient: four methods, resolved from a terse ground rule

The ground rules name four methods verbatim — "initiate, confirm,
receiveInbound, returnPayment" — with no further specification, and no
rail-specific technical documentation exists anywhere in this repo to
disambiguate them further (only the design note's "cut over per payment
rail, ACH first" line, which is why ACH is the concrete rail built
here). Rather than guess silently, this pass resolved each into a
distinct, non-overlapping responsibility and documents the reasoning
directly in `internal/railclient/client.go`'s doc comment, flagged here
for Architect Agent review the same way a prior codegen phase flagged an
ambiguous ground-rule conflict instead of silently picking one reading:

| Method | Resolved meaning |
|---|---|
| `Initiate` | Send money OUT (a disbursement). |
| `Confirm` | Check the current outcome of a previously-`Initiate`d submission — the one method both a polling loop AND a push-style (webhook) adapter funnel into. |
| `ReceiveInbound` | Pull money that ARRIVED, batch-style — also surfaces newly-discovered RETURNS of previously-received inbound payments via a `Kind` discriminator, since a file-based rail processes both through the same batch cadence. |
| `ReturnPayment` | Originate a return of a specific, previously-received inbound payment — an action THIS system originates (Ops/compliance-initiated), distinct from `ReceiveInbound` surfacing a return the RAIL originated. |

## Idempotency: two independent layers, per the ground rule

"A retried `initiateDisbursement` call with the same key must never
result in two outbound payments, even if the first attempt's response
was lost" is enforced at two independent layers, same discipline every
other service in this repo applies to its own idempotency-critical
paths:

1. **`internal/service`'s own lookup-before-initiate check** —
   `InitiateDisbursement` looks up the `PaymentInstruction` by
   `IdempotencyKey` first and returns the existing record on a hit,
   never calling the rail again.
2. **Every rail adapter's own dedup ledger** — both `internal/rails/sandbox`
   and `internal/rails/ach` independently refuse to double-submit for a
   reused `InstructionID` (same-payload replay returns the original
   `Submission`; different-payload reuse returns `ErrDuplicateInstruction`).
   This is what makes it SAFE to retry `rail.Initiate` after a crash
   between a successful rail submission and this service's own
   `SavePaymentInstruction` call — the rail itself, not just this
   service's database, is the second backstop.

`ReturnPayment` carries the same two-layer guarantee for the
inbound-return direction (`ReturnPaymentInput.IdempotencyKey`).

## Reconciliation: matching confirmations to instructions, unmatched = exception

`internal/service/reconciliation.go` implements both integration
patterns a rail might use:

- **`RunReconciliationSweep`** — polls `Confirm` for every OUTBOUND
  instruction still `Submitted` (the correct pattern for a pure-polling
  rail like ACH).
- **`ProcessConfirmation`** — the push/webhook equivalent, reusing the
  exact same underlying `applyOutcome` logic (a real webhook-capable
  rail would call this directly per delivered notification instead of
  ever being polled).

**This role's literal ground rule** — "An unmatched confirmation is
logged as an exception, never posted speculatively" — is enforced at
two points, both proven by tests:

- A rail reference that matches no known `PaymentInstruction` at all
  (`ProcessConfirmation`'s `store.GetPaymentInstructionByRailReference`
  miss, or the sweep's own `Confirm` returning `ErrNotFound` for a
  reference this service itself recorded) → a `domain.ReconciliationException`
  (`UNMATCHED_CONFIRMATION`), zero events published, zero
  `PaymentInstruction` fabricated.
- A confirmation reporting a **different, contradicting terminal
  status** than what's already recorded (e.g. already `Executed`, now
  reported `Failed` — structurally impossible per
  `domain.validTransitions`, since `Executed` only permits `Returned`)
  is treated as a data-integrity anomaly, filed as the same exception
  kind, and the original record is left untouched — never silently
  overwritten.
- The exact same outcome reported twice (a re-delivered, identical
  confirmation) is a safe no-op: no duplicate event, no exception.

## Compensating reversal: never a manual correction, never a silent write-off

This role's other central ground rule — "A failed, returned, or
reversed payment triggers a compensating reversal journal entry through
the normal posting path" — is satisfied differently per direction,
respecting existing module ownership (`docs/design-notes.md`'s CB-DISB
section: LAS/AccountAPI owns the disbursement state machine and its own
PR-DISB-02 reversal; this service does not call GLPostingAPI directly):

- **Outbound (disbursement) failed/returned**: this service's job stops
  at reliably publishing `payment.disbursement.failed` via the
  transactional outbox — LAS's own already-built `ReverseDisbursement`
  (PR-DISB-02) is the actual compensating posting. **Known cross-service
  gap, not this pass's to close**: LAS does not yet have an event
  consumer wired to this topic (its own PR_DESCRIPTION.md already
  flags "PaymentAPI is not called... nothing here yet consumes a real
  Payment Execution event"). Flagged for the Architect Agent: this
  event's consumer side needs to be built before the compensating
  reversal actually happens end to end in a real deployment.
- **Inbound payment returned (rail-originated, `ReceiveInbound`'s
  `InboundReturned` events)**: this service calls AccountAPI's
  `reverseRepayment` (`accountclient.ReverseRepayment`) synchronously —
  LAS's existing PR-REPAY-02 reversal path.
- **Inbound payment returned (Ops-originated, `ReturnInboundPayment`)**:
  same `reverseRepayment` call, deliberately sequenced BEFORE the rail
  submission (`internal/service/returns.go`'s doc comment) — the ledger
  correction is durable and correct even if the physical rail
  origination needs an independent retry.
- **The one case this service REFUSES to complete automatically** —
  this role's own "refuse and explain the compliant alternative"
  clause, made concrete: a returned inbound payment whose original
  `PaymentInstruction` has `Purpose=PAYOFF` has **no existing AccountAPI
  reversal endpoint to call** — `services/las`'s own design note
  documents this explicitly ("There is no reversal endpoint for a
  Payoff in the shipped contract... open question carried forward
  unresolved"). Rather than silently no-op (a silent write-off) or call
  `reverseRepayment` against a resource it was never built for, this
  service refuses: it still records the return on its own
  `PaymentInstruction` (accurate local bookkeeping) and files a
  `NO_COMPLIANT_REVERSAL_PATH` reconciliation exception so Ops performs
  the GL correction through a reviewed manual process. See
  `service.ErrNoCompliantReversalPath`'s doc comment and
  `TestReceiveInboundPayments_ReturnOfPayoff_RefusedWithException`/
  `TestReturnInboundPayment_Payoff_Refused`.

The refusal clause's OTHER half — "If a requirement would post a
disbursement or repayment without a matched, confirmed rail event,
refuse" — is satisfied structurally, not by a runtime check:
`domain.NewOutboundDisbursement` only ever constructs a
`PaymentInstruction` in `Submitted` status, and `TransitionTo` (the only
function that can move one to `Executed`) is called ONLY from the
reconciliation path above — there is no method anywhere in this service,
and no HTTP handler, that lets a caller mark one `Executed` directly.

## Rail limitations discovered — for the Architect Agent

Per this role's own deliverable list, the concrete limitations this
pass's ACH adapter surfaced, each with where it's enforced in code:

1. **Settlement delay / no real-time confirmation.** ACH has no
   push-style acknowledgment at all. `Initiate` only appends to the
   currently open batch; nothing is even transmitted until an
   operations/scheduler decision calls `CutBatch`, and even a cut
   batch's entries stay `Pending` under `Confirm` until a SEPARATE,
   LATER `ApplySettlementFile` call supplies an outcome — in reality,
   one to two banking days after transmission. See
   `TestInitiate_ThenConfirm_StaysPendingUntilCutAndSettled`.
2. **Batch-only confirmation, on both directions.** Inbound credits are
   surfaced the same way, via `IngestIncomingBatch`/`IngestReturnReport`
   feeding `ReceiveInbound`'s cursor-based pull — there is no real-time
   inbound push either.
3. **No rail-side idempotency guarantee assumed.** This adapter builds
   its own in-memory dedup ledger (`Initiate`/`ReturnPayment`, keyed by
   instruction ID) rather than trusting a real ACH operator/ODFI to
   deduplicate — a real integration would need to confirm what
   idempotency guarantee, if any, its specific ODFI/processor actually
   offers at the file-submission layer; this adapter does not assume
   one exists.
4. **The ACH return window is real and asymmetric.** A return can only
   be originated by the RECEIVING bank (the RDFI) within a short window
   after settlement (2 banking days for most reason codes) — after
   that, the only remaining path is an entirely new, uninsured outbound
   credit requiring the original sender's independently-obtained
   banking details, which this service does not have and does not
   fabricate. `internal/rails/ach.Config.ReturnWindow` enforces this
   (`ErrReturnWindowExpired`); default 48h is a wall-clock
   simplification of NACHA's actual "2 BANKING days" (excludes
   weekends/holidays) — flagged, not silently treated as equivalent.
5. **Routing-number check-digit validation is not performed.** A
   malformed routing number from `PayoutAccountResolver` is trusted
   as-is rather than rejected before file generation (`nacha.go`'s doc
   comment).
6. **No file transmission, and no incoming-file parsing.** This
   adapter generates a correctly-shaped outbound NACHA file
   (`CutBatch`) but never sends it anywhere — an SFTP/bank-API delivery
   mechanism is entirely outside this repo. Symmetrically, it accepts
   already-structured Go data for settlement/return/incoming-credit
   ingestion rather than parsing a raw incoming NACHA file — both real,
   separate scopes this pass does not cover.
7. **No production `PayoutAccountResolver`.** `internal/rails/ach.InMemoryPayoutDirectory`
   is a fixed test/local map — a real deployment needs a PartyAPI (or
   vault-backed) lookup wired in, or every `initiateDisbursement` call
   against the ACH rail fails `RAIL_REJECTED`. `cmd/payment-service`
   currently wires an EMPTY directory when `PAYMENT_SERVICE_RAIL=ach`
   is selected — flagged loudly in that file's own doc comment, not
   silently non-functional.
8. **`payment-instruction.schema.json`'s `purpose` enum has no
   `RECOVERY` value.** AccountAPI's `receiveRepaymentNotification` can
   return `Kind=Recovery` (a charged-off account's recovery payment),
   but this service's own `PaymentInstruction.Purpose` has nowhere
   valid to record that — it's left as `REPAYMENT`
   (`internal/service/inbound.go`'s comment). A genuine shared-schema
   gap, not something this pass invents a value for.
9. **CutBatch/ApplySettlementFile/IngestIncomingBatch/IngestReturnReport
   are NOT wired into any automatic scheduler.** These are rail-specific
   operations outside the generic `railclient.Client` interface's
   scope (a real deployment's batch-processing job would call them
   directly); `cmd/payment-service` only drives the two generic,
   interface-level sweeps (`RunReconciliationSweep`,
   `ReceiveInboundPayments`).

## Follow-up: migration runner, integration test, and CI

Closes the same gap already closed for `services/party`, `services/crm`,
`services/gl`, and `services/las`: `golang-migrate` CLI (pinned
`v4.19.1`) verified against a disposable Postgres — `up` (from empty, 5
application tables plus `golang-migrate`'s own `schema_migrations`) /
`version` / `down 1` (clean removal back to just `schema_migrations`) /
`up` again (clean reapply) — all confirmed locally.

**Integration test** (`internal/integration/integration_test.go`, build
tag `integration`, new) exercises the full write path against a real
Postgres for the first time — nothing before this had ever actually run
`migrations/0001_init.up.sql` or exercised `internal/store/postgres`'s
real SQL, including its `payment_instructions` `ON CONFLICT` upsert.
AccountAPI (LAS) is **not** live in this test — `accountclient.Fake`
stands in for it, exactly as it does in every `internal/service` unit
test, since no live Payment↔LAS cross-service integration exists in
this repo yet:

- Initiates a disbursement through the real SQL path, confirms the
  idempotent replay resolves through the database's own primary-key
  lookup, not just an in-memory map.
- Runs a reconciliation sweep against the real database, confirming the
  `Submitted -> Executed` transition actually persists via the UPDATE
  branch of the `ON CONFLICT` upsert, and that a
  `payment.disbursement.confirmed` row lands in the real `outbox` table.
- Receives an inbound payment, confirms the `inbound_cursors` table
  persists the cursor correctly; then processes a rail-reported return
  for that same payment, confirming the compensating `ReverseRepayment`
  call fires and the instruction's status persists as `Returned`.
- Files an unmatched confirmation, confirms the row lands in the real
  `reconciliation_exceptions` table with `kind = 'UNMATCHED_CONFIRMATION'`.
- **`payment_app`'s GRANTs, verified live for the first time**: confirms
  a `DELETE` against `payment_instructions` is rejected — the migration
  documents "no DELETE grant on any table," previously asserted only by
  reading the SQL, never exercised against a real Postgres. Not a
  ledger-immutability invariant the way GL's is (this service's tables
  are ordinary mutable entities with UPDATE granted) — it only proves
  rows can't be silently deleted.

**CI**: `.github/workflows/payment-ci.yml` (new), scoped to
`services/payment/**` and the Payment Execution specs. Same three-job
shape as `party-ci.yml`/`crm-ci.yml`/`gl-ci.yml`/`las-ci.yml`, with
`build-test` also running `go test -race ./...` — reconciliation/inbound
sweeps and concurrent disbursement submissions must never double-post
or race on the same `PaymentInstruction`.

**Verified for real, not just written**: ran the integration test twice
against a live Colima Docker daemon on this machine — both passed
(~1.1–1.4s each), no leftover containers.

## Follow-up: Kafka outbox publisher

`internal/relay` (new, ported near-verbatim from `services/las`/
`services/crm`) — `Publisher.PublishUnpublished` reads a batch of
unpublished outbox rows via `outbox.Reader`, writes each to its own
topic on a shared `*kafkago.Writer` (every `payment.*` event type
shares one `Writer`; `Topic` is deliberately left unset on it since
kafka-go rejects a Writer with both a fixed `Topic` and per-message
topics), and only marks a row published after the Kafka write succeeds
— nothing is marked published if the write fails, so a crash or broker
outage between the two just means the same rows are safely retried on
the next poll (at-least-once, matching every other topic in this
system). 5 unit tests against fake reader/writer doubles, ported from
the identical pattern already proven in `services/las`/`services/crm`.

Wired into `cmd/payment-service/main.go`: `PAYMENT_SERVICE_KAFKA_BROKERS`
(comma-separated, required) builds the `*kafkago.Writer`;
`runOutboxRelay` polls on `PAYMENT_SERVICE_OUTBOX_POLL_INTERVAL`
(default 2s) in a background goroutine for the process lifetime,
logging but never treating a publish error as fatal.

## Still not started

- **A live Payment↔LAS cross-service integration** — every test in this
  repo, including the integration test above, still uses
  `accountclient.Fake`.

## Unit tests: 49 tests, all passing under `go test -race`

- `internal/domain` (8 tests): the `PaymentInstruction` status-transition
  table, exhaustively enumerated (4 valid pairs, 8 invalid pairs
  guarded by a count assertion), constructors, `WithRailReference`.
- `internal/rails/sandbox` (8 tests): idempotent replay and
  same-key-different-payload rejection for `Initiate`/`ReturnPayment`,
  armed-outcome control, unknown-reference `ErrNotFound`,
  `ReceiveInbound`'s since-filter/sort, error injection.
- `internal/rails/ach` (12 tests): unknown-party `RAIL_REJECTED`, the
  full Pending-until-cut-and-settled lifecycle, returned-entry
  failure-reason mapping, unmatched-settlement-trace reporting, a
  well-formed 94-column/block-padded NACHA file assertion (record
  types 1/5/6 checked directly), incoming-batch/return-report ingestion
  and matching, and the return-window limitation both ways
  (`ErrReturnWindowExpired` outside the window, success + idempotent
  replay inside it).
- `internal/service` (21 tests): disbursement idempotency (including
  "nothing persisted on rail rejection, retry still works"),
  reconciliation (confirms/still-pending/fails, unmatched exception,
  conflicting-terminal-status anomaly, same-outcome-twice no-op),
  inbound receipt (new/idempotent-skip/AccountAPI-failure-doesn't-
  advance-cursor, Payoff-kind purpose mapping), inbound return
  (matched compensating reversal, unmatched exception, the Payoff
  refusal case), and Ops-initiated `ReturnInboundPayment` (success
  ordering, Payoff refusal, unknown-instruction, rail-failure-leaves-
  ledger-already-corrected).
