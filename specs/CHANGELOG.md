# Spec Changelog

## v0.3.0 retrofit — 2026-08-16

Retrofit of every existing internal-module spec to the API/Event Spec
Agent's conventions, plus the new CRMAPI contract. All changes below are
**BREAKING** for any consumer generated against the prior (v0.2.0 / v0.1.0)
shapes.

### specs/schemas/ (new)

Shared JSON Schema objects, Draft 2020-12, referenced via `$ref` from every
OpenAPI/AsyncAPI file rather than duplicated inline:

| File | Defines |
|---|---|
| `money.schema.json` | `Money` — `{amount: integer (minor units), currency: ISO 4217}` |
| `error.schema.json` | `Error` — the shared error body for every 4xx/5xx response |
| `party.schema.json` | `Party` |
| `journal-entry.schema.json` | `JournalEntryLine`, `JournalEntry` (now carries `runningBalanceAfter` per line) |
| `loan-account.schema.json` | `TermSet`, `TermVersion`, `LoanAccount` |
| `payment-instruction.schema.json` | `PaymentInstruction` |
| `service-case.schema.json` | `Interaction`, `ServiceCase`, `Customer360` |

`Allocation` (fee/interest/principal split, used by `PR-REPAY-*`/`PR-PAYOFF-01`)
was deliberately **not** added to this shared set — it wasn't in the role's
named list, so it remains duplicated locally in `gl-posting-engine.yaml` and
`loan-account-subledger.yaml`, each now built from shared `Money` fields.

### Breaking changes, per file

**`openapi/party-cif.yaml`** (PartyAdapter) — `0.2.0 → 0.3.0`
- `Party` moved to the shared schema, no local schema changes needed (read-only, no Money, no idempotency).

**`openapi/gl-posting-engine.yaml`** (GLPostingAdapter) — `0.2.0 → 0.3.0`
- `sourceEventId` removed from the request body; replaced by a required `Idempotency-Key` header. GL persists the header value as the response's `sourceEventId`.
- `amount`/`currency` bare fields replaced by nested `Money` objects (`amount`, and each `Allocation` sub-field).
- Response `JournalEntry` is now the shared schema and gains `runningBalanceAfter` (Money) on every line — callers no longer need a separate read to confirm posting effect.
- Registered `PR-PAYOFF-01` (new, CB-PAYOFF).
- **Documentation fix, not a behavior change**: the pre-retrofit design note and Design Doc described `PR-REPAY-01`/the original `PR-PAYOFF-01` draft as crediting `FeeIncome`/`InterestIncome` on collection. That would double-recognize income already booked at accrual/assessment time (`PR-ACCR-01`, `PR-DELINQ-01`). The actual schema never encoded GL account names (always GL-internal), so no schema changed — only the documented income-recognition principle, now stated explicitly on the shared `JournalEntry` schema.
- Full 400/401/403/404/409/422/500 coverage added to every operation.

**`openapi/payment-execution.yaml`** (PaymentAdapter) — `0.2.0 → 0.3.0`
- `disbursementId` removed from the request body; replaced by a required `Idempotency-Key` header.
- Response is now the shared `PaymentInstruction` object.
- Path renamed `/payments:disburse` → `/payment-instructions:disburse`; operationId renamed `disbursePayment` → `initiateDisbursement` to match the role's canonical operation name.
- Full error coverage added.

**`openapi/loan-account-subledger.yaml`** (AccountAdapter) — `0.2.0 → 0.3.0`
- `TermSet`/`TermVersion`/`LoanAccount` moved to the shared schema; `annualInterestRate` (decimal) renamed `annualInterestRateBps` (integer basis points) as part of that move.
- Every mutating operation's client-supplied ID (`approvalReferenceId`, `disbursementId`, `feeId`-derived waiver key, `repaymentId`-derived reversal key) moved from the request body to a required `Idempotency-Key` header. **Exception, explicitly documented**: `runDailyAccrual` and `runDailyDelinquencyAssessment` take no Idempotency-Key — both are idempotent by construction on their own `(asOf, partitionIndex)` body parameters, so an additional header would be redundant.
- **New operation**: `receiveRepaymentNotification` (`POST /repayments:notify`). CB-REPAY originally modeled repayment creation as purely event-driven (PaymentAdapter publishes an event, AccountAdapter subscribes). Retrofitted to add this as the authoritative synchronous, Idempotency-Key-bearing trigger, matching the role's explicit naming of it as one of the three canonical balance-affecting operations. The corresponding event (`payment.inbound.received`) still publishes, now for observability/other consumers only, not as the processing trigger.
- **New paths (CB-PAYOFF, folded into this retrofit)**: `GET /loan-accounts/{id}/payoff-quote`, `GET /payoffs/{payoffId}`. Payoff settlement itself reuses `receiveRepaymentNotification` via an optional `payoffQuoteId` field rather than a separate settlement endpoint — a payoff payment is an inbound payment, just one tagged with a quote reference. No reversal endpoint exists yet for a returned/NSF payoff payment — open question carried from the Design Doc, not yet a specified flow.
- Full error coverage added throughout.

**`openapi/crm.yaml`** (CRMAdapter) — new, `0.1.0`
- No prior version. Drafted fresh per the role's instruction, flagged here for the loan domain's architecture team to add to Core Banking Adapter Layer Section 16.1.
- Three operations: `logInteraction`, `openCase`, `getCustomer360`. Zero Money fields anywhere in the file or in `service-case.schema.json` — verified by direct grep, not just asserted in prose.

**`asyncapi/*.yaml`** — all four files, `0.2.0/0.1.0 → 0.3.0`
- Every topic renamed to `<context>.<entity>.<eventPastTense>`, version suffix (`.v1`) dropped (version now lives only in `info.version`):

  | Old topic | New topic |
  |---|---|
  | `gl.journal-entry.posted.v1` | `gl.entry.posted` |
  | `loan.account.booked.v1` | `loan.account.booked` |
  | `loan.booking.rejected.v1` | `loan.booking.rejected` |
  | `loan.account.disbursed.v1` | `loan.account.disbursed` |
  | `payment.unmatched.v1` | `payment.match.failed` |
  | `loan.repayment.posted.v1` | `loan.repayment.posted` |
  | `delinquency.status.changed.v1` | `delinquency.status.changed` |
  | `loan.account.nonaccrual.v1` | `loan.nonaccrual.flagged` |
  | `payment.received.v1` | `payment.inbound.received` |
  | `payment.executed.v1` | `payment.disbursement.confirmed` |
  | `payment.failed.v1` | `payment.disbursement.failed` |
  | `accrual.exception.raised.v1` | `accrual.exception.raised` |
  | `repayment.exception.raised.v1` | `repayment.exception.raised` |
  | `delinquency.exception.raised.v1` | `delinquency.exception.raised` |
  | *(none — new)* | `loan.account.closed` (CB-PAYOFF) |
  | *(none — new)* | `payoff.exception.raised` (CB-PAYOFF) |

- `gl.entry.posted`'s payload is now the shared `JournalEntry` schema (gains `runningBalanceAfter`).
- `payment.disbursement.confirmed`/`payment.disbursement.failed` payloads are now the shared `PaymentInstruction` object.
- Every Money-bearing event field moved to the shared `Money` object.
- Consumer references renamed from generic module names (LAS, GL, PAY, BEOD, CRM) to adapter names (AccountAdapter, GLPostingAdapter, PaymentAdapter, BatchEodAdapter, CRMAdapter) for consistency with the adapter-facing framing.

### Migration notes

- Any consumer built against v0.2.0 request/response shapes must update: (1) every Money-bearing field from a bare number to `{amount, currency}`; (2) every idempotency key from a body field to the `Idempotency-Key` header (except the two batch-trigger operations, which are unchanged); (3) every event subscription's topic string, per the table above.
- No data migration is implied — this is a contract-shape retrofit only; nothing here changes what's persisted, only how it's described and transmitted.
- `# TODO: confirm with BA/Architect` markers: none required this pass — every shape needed was either already specified by a prior epic's REQ-IDs or resolvable from the API/Event Spec Agent's own stated conventions. The one genuine open item (payoff-payment reversal) is documented inline in `loan-account-subledger.yaml` rather than marked TODO, since it's a scope question for a future epic, not missing information within CB-PAYOFF's current scope.
