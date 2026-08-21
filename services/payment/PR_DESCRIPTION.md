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

## What's explicitly out of scope, flagged rather than silently worked around

Same discipline every other service in this repo has followed:

- **No live Kafka outbox publisher** (`internal/relay`) — the
  transactional outbox write (`payment.disbursement.confirmed`,
  `payment.disbursement.failed`, `payment.inbound.received`) is
  complete and unit-tested; nothing yet delivers those rows to a
  broker. Same first-pass boundary party/CRM/GL/LAS each drew before
  their own follow-up phase.
- **Migration runner verification, integration test, CI** — not run in
  this pass; `Makefile` targets exist (`migrate-up`/`down`/`version`,
  `test-integration`) but `internal/integration/` doesn't exist yet.
- **`PAYMENT_SERVICE_ACCOUNT_BASE_URL` (AccountAPI/LAS) is real HTTP**,
  but nothing in this repo runs a live cross-service call between the
  two services yet — every service test uses `accountclient.Fake`.

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
