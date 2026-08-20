# CRM service: interaction logging, case lifecycle, RM assignment, communication preferences

## What changed

Implements the CRM service end to end — spec extension, business logic,
persistence, and HTTP transport. This is a new service (`services/crm`);
nothing here modifies any other module, and nothing here calls, imports,
or references GLPostingAPI or a `Money` value anywhere, at any layer —
CRM has no balance-affecting operation, ever.

**Spec changes (prerequisite, since the prior spec covered only a third
of what this role requires):**

- `specs/schemas/service-case.schema.json`: extended `ServiceCase` with
  `version` (optimistic concurrency), `slaDueAt`/`escalated`,
  `closeReason`/`reopenReason`; added `CaseNote`, `CommunicationPreferences`,
  `RelationshipManagerAssignment` defs; added `currentRelationshipManagerId`
  to `Customer360`.
- `specs/openapi/crm.yaml`: **v0.1.0 → v0.2.0, breaking.** Added
  `POST /cases/{caseId}:update`, `POST /cases/{caseId}:close`,
  `POST /cases/{caseId}:reopen`, `GET/POST /cases/{caseId}/notes`,
  `GET/POST /customers/{partyId}/relationship-manager`,
  `GET/PUT /customers/{partyId}/communication-preferences`. Added 6 new
  scopes (`crm:case:write`/`read` already existed; added
  `crm:relationshipmanager:write`/`read`, `crm:commprefs:write`/`read`).
- `specs/asyncapi/crm-events.yaml` (new file): 9 channels —
  `crm.interaction.logged`, `crm.case.opened`, `crm.case.updated`,
  `crm.case.closed`, `crm.case.reopened`, `crm.case.escalated`,
  `crm.caseNote.added`, `crm.relationshipManager.assigned`,
  `crm.communicationPreferences.updated`. No payload anywhere carries
  case-note/narrative content (`CaseNote.body`, close/reopen reasons) —
  PII-adjacent free text never appears in an event payload, the same
  principle `party-events.yaml` applies to PII. No consumer for these
  events exists elsewhere in this system today (CRM is upstream of
  nothing; every other module is upstream of CRM) — published anyway,
  per the ground rule that every state change publishes its event
  unconditionally, documented as forward-compatible rather than omitted.

**Implementation (`services/crm/`), Go 1.26, chi + pgx/v5 + Postgres —
same stack and conventions as `services/party`:**

- `internal/domain` — `ServiceCase`'s state machine
  (`Open→InProgress→Resolved`, never to/from `Closed` except via
  dedicated `Close`/`Reopen`), a fixed per-`reasonCode` SLA-duration
  table, optimistic-concurrency `Update` (returns which fields actually
  changed, or none on a no-op), idempotent `Close`, non-idempotent
  `Reopen` (rejects reopening a case that was never closed — a genuine
  state error, not a retry), `IsPastSLA`. `InferLoanAccountStatus` derives
  a Customer360 status summary from CRM's own logged interactions.
- `internal/pii`, `internal/outbox` — ported near-verbatim from
  `services/party` (both are already service-agnostic).
- `internal/events` — typed payloads matching `crm-events.yaml` exactly.
- `internal/store` / `internal/store/postgres` — persistence contract +
  pgx implementation. `UpdateCaseConditional` enforces optimistic
  concurrency at the database level too (`WHERE id=$1 AND version=$2`),
  not just in memory — the DB check is what actually closes the race
  between two concurrent `UpdateCase` calls; the in-memory check alone
  cannot. `case_note_access_log` records every read of PII-adjacent
  content with actor and timestamp.
- `internal/service` — orchestration for all 11 write/read operations,
  plus `EvaluateSLABreaches`: a periodic sweep (never a side effect of a
  GET) that escalates cases past their SLA deadline and publishes
  `crm.case.escalated`.
- `internal/auth` — ported from `services/party`, scopes swapped to
  CRM's own 8; `internal/api/auth_middleware.go` now also stashes the
  validated caller's subject into request context, since
  `listCaseNotes`/`getCustomer360` need it for the read-access-log actor.
- `internal/api` — chi handlers for every `crm.yaml` operation,
  Idempotency-Key middleware (ported from `services/party`).
- `cmd/crm-service` — wires Postgres, encryption, JWKS auth, HTTP
  server, and a background SLA-sweep ticker
  (`CRM_SERVICE_SLA_SWEEP_INTERVAL`, default 5m).
- `migrations/0001_init.up.sql` / `.down.sql` — `cases`, `interactions`,
  `case_notes`, `case_note_access_log`, `communication_preferences`,
  `relationship_manager_assignments`, `loan_account_links`, `outbox`,
  `idempotency_keys`.

## Ground rules honored

- **CRM never calls the Posting Engine.** No file in this service
  imports a GL/posting type or references `Money`, anywhere.
- **No embedded/duplicated balance data.** `ServiceCase`/`Interaction`
  reference `LoanAccount` only by opaque ID.
Customer360's loan-account summaries carry **status only** — derived
  from CRM's own interaction log, never a balance figure.
- **PII-adjacent content is encrypted at rest, read-logged, export-excluded.**
  `CaseNote.Body`, `Interaction.Notes`, and close/reopen reasons are
  AES-256-GCM-encrypted in Postgres; `listCaseNotes` and
  `getCustomer360` write to `case_note_access_log` with actor and
  timestamp on every read; none of this content ever appears in an
  outbox/event payload (the one path that would put it in front of a
  bulk consumer).
- **Every state change publishes its event, transactionally.** Every
  write path opens exactly one `store.WithinTx` covering both the
  business write and the outbox insert — see `internal/service/service.go`.

## What's explicitly out of scope, flagged rather than silently worked around

- **A real cross-service dependency this codegen pass could not safely
  build.** `getCustomer360`'s loan-account summaries are derived
  entirely from `loan_account_links` — a table CRM populates itself,
  only when `openCase` supplies both `partyId` and `loanAccountId`
  together (the only place CRM ever learns that pairing).
  `LogInteractionRequest` does not carry `partyId` at all, so an account
  CRM has only ever seen through `logInteraction` — never through a case
  opened against it — cannot appear in a customer's 360 view. **Flagged
  back to the Ledger & Solution Architect Agent**: the real fix is
  either adding `partyId` to `LogInteractionRequest` (coordinated with
  whichever adapter publishes these events, since `partyId` is
  presumably already available to the publisher) or a synchronous,
  properly-scoped read from AccountAPI — neither of which exists as
  running code in this repo yet, and inventing a fake HTTP call to a
  service that doesn't exist would have been worse than documenting the
  gap plainly.
- **`Customer360.displayName`** is omitted from this service's response
  DTO — it's `x-pii: true` and sourced from Party/CIF, which this
  service has no read path into in this increment. Same flagged-gap
  reasoning as above, not an oversight.
- **A concrete Kafka-backed `outbox.Publisher`** — not built here,
  consistent with how `services/party` handled the identical situation
  (its outbox pattern shipped first; the Kafka relay was built as an
  explicit follow-up once asked for). The transactional outbox write
  itself is complete and tested; nothing yet delivers those rows to a
  broker.
- **KMS integration, migration execution, integration tests against
  live infrastructure, CI** — all deliberately out of scope for this
  codegen pass, matching the same boundaries `services/party` drew
  before its own follow-up phases addressed each one individually.

## Spec version implemented

- `specs/openapi/crm.yaml` v0.2.0
- `specs/schemas/service-case.schema.json` (current, with the defs listed above)
- `specs/asyncapi/crm-events.yaml` (new)

## Unit tests written

**`internal/domain/case_test.go` (16 tests)** — the case state machine,
the highest-risk piece of this change:
- `TestNewCase_ComputesSLADueAtFromReasonCode` — SLA deadline computed
  correctly at open.
- `TestUpdate_OpenToInProgress_Succeeds`,
  `TestUpdate_NoActualChange_IsNoOp_VersionUnchanged` — real transitions
  write; resubmitting the same values is a no-op (version untouched).
- `TestUpdate_DisallowedTransition_ResolvedBackToOpen_Rejected`,
  `TestUpdate_ToOrFromClosed_AlwaysRejected` — the state machine rejects
  transitions it doesn't permit, including anything to/from `Closed` via
  `Update` (that's `Close`/`Reopen`'s exclusive boundary).
- **Concurrent updates to the same case**:
  `TestUpdate_StaleVersion_ConcurrentUpdateRejected` — two CSRs reading
  the same case and both submitting updates: the second, stale-version
  submission is rejected; re-reading and retrying succeeds.
- **Reopening a closed case**:
  `TestReopen_ClosedCase_Succeeds_ResetsEscalationAndSLA` (status→Open,
  escalated resets, SLA clock resets from now — not the remainder of the
  original window), `TestReopen_NonClosedCase_Rejected_NotIdempotentNoOp`
  (reopening a case that was never closed is a genuine error, not a
  no-op replay), `TestReopen_GivesFreshSLAWindow_EvenIfOriginalWasAlreadyOverdue`.
- `TestClose_FirstCall_ChangesState`, `TestClose_AlreadyClosed_IsIdempotentNoOp`
  — closing is idempotent, unlike reopening.
- **SLA escalation timing**: `TestIsPastSLA_BeforeDeadline_NotPast`,
  `TestIsPastSLA_AfterDeadline_IsPast`,
  `TestIsPastSLA_AlreadyEscalated_NeverReportedAgain`,
  `TestIsPastSLA_ClosedCase_NeverEscalates`,
  `TestIsPastSLA_ResolvedCase_NeverEscalates` — the escalation predicate
  fires exactly once, only for active cases, only after the deadline.

**`internal/domain/loan_account_status_test.go` (1 test)**:
`TestInferLoanAccountStatus` — every `EventType`→`LoanAccountStatus`
mapping used by Customer360.

**`internal/pii/encryption_test.go` (6 tests)** — ported from
`services/party`, identical coverage (round-trip, empty-string
pass-through, tamper detection, nonce uniqueness, wrong-key rejection).

**`internal/auth/jwks_test.go` (8 tests)** — ported from `services/party`,
identical coverage against real RSA keys and a real JWKS endpoint
(including the RS256/HS256 algorithm-confusion attack test), scopes
swapped to CRM's own.

**`internal/service/service_test.go` (17 tests)**, against an in-memory
fake `store.Store`:
- `TestLogInteraction_WritesRowAndOutboxEntry`,
  `TestOpenCase_CreatesCaseAndLinksLoanAccount` — the two write paths
  that don't go through the state machine.
- **Concurrent updates to the same case** (service-level, through the
  full load→transition→conditional-write path):
  `TestUpdateCase_ConcurrentUpdate_SecondCallerGetsStaleVersionError`.
- `TestUpdateCase_NoActualChange_NoWriteNoEvent`.
- **Reopening a closed case** (service-level):
  `TestReopenCase_ClosedCase_SucceedsAndPublishesEvent`,
  `TestReopenCase_NeverClosed_Rejected`.
- `TestCloseCase_AlreadyClosed_IsIdempotentNoOp`.
- **SLA escalation timing** (service-level, via `EvaluateSLABreaches`
  with an overridable clock):
  `TestEvaluateSLABreaches_PastDueCase_EscalatedAndEventPublished`,
  `TestEvaluateSLABreaches_NotYetDueCase_NotEscalated`,
  `TestEvaluateSLABreaches_AlreadyEscalated_NotEscalatedAgain`,
  `TestEvaluateSLABreaches_ClosedCase_NeverEscalated`.
- `TestListCaseNotes_LogsAccessWithActorAndTimestamp` — the read-audit-log
  ground rule, verified end to end.
- `TestAssignRelationshipManager_FirstAssignment_WritesAndPublishes`,
  `TestAssignRelationshipManager_SameRMAgain_IsNoOp`.
- `TestGetCommunicationPreferences_NeverSet_ReturnsConservativeDefault`,
  `TestUpdateCommunicationPreferences_WritesAndPublishes`.
- `TestGetCustomer360_DerivesLoanAccountStatusFromLatestInteraction_AndLogsAccess`
  — status derived from the LATER of two interactions on the same
  account, plus the access-log write.

**`internal/api/handlers_test.go` (18 tests)**, against an in-memory fake
`store.Store` and a fake `auth.Validator`, using `net/http/httptest`:
- `TestLogInteraction_Creates201`.
- `TestOpenCase_MissingIdempotencyKey_400`,
  `TestOpenCase_UsesIdempotencyKeyAsCaseID` — the spec's convention that
  the Idempotency-Key value IS the resulting case's ID.
- `TestIdempotency_ReplaySameKeySamePayload_NoSecondCaseCreated`,
  `TestIdempotency_SameKeyDifferentPayload_409`.
- `TestGetCase_NotFound_404`.
- **Concurrent updates / invalid transitions surfaced as HTTP 409**:
  `TestUpdateCase_ConcurrentUpdate_409`, `TestUpdateCase_InvalidTransition_409`.
- `TestCloseCase_Idempotent_SecondCallStillReturns200`.
- **Reopening a non-closed case surfaced as HTTP 409**:
  `TestReopenCase_NotClosed_409`.
- `TestAddCaseNote_ThenListCaseNotes`.
- `TestAssignThenGetRelationshipManager`.
- `TestGetCommunicationPreferences_NeverSet_ReturnsDefaults`,
  `TestUpdateCommunicationPreferences_RoundTrips`.
- `TestGetCustomer360_ReturnsOpenCasesAndSummaries`.
- `TestAuth_MissingAuthorizationHeader_401`, `TestAuth_InsufficientScope_403`.

**Verification:** `go build ./...`, `go vet ./...`, `gofmt -l .` (clean),
and `go test ./...` all pass — **66/66 tests** across `internal/domain`,
`internal/pii`, `internal/auth`, `internal/service`, and `internal/api`.
`internal/store/postgres` and `cmd/crm-service` have no unit tests — they
require a live Postgres instance (and, for `main.go`, a live JWKS
endpoint) to exercise meaningfully; that is integration-test territory,
out of scope for this change (see `services/party/internal/integration`
for the pattern this service would follow if asked to add the same).

## Follow-up: migration runner, integration test, and CI

The initial pass above deliberately left the migration runner unused
(never actually run against `migrations/*.sql`), no integration test
against live infrastructure, and no CI at all — matching the exact
boundaries `services/party` drew before its own equivalent follow-up.
That gap is now closed, using the same tools and patterns
`services/party` already established:

- **Migration runner**: [golang-migrate](https://github.com/golang-migrate/migrate),
  same as `services/party`. Verified for real: ran `up`, `version`,
  `down`, `up` again against a disposable local Postgres container and
  confirmed all 9 expected tables (`cases`, `interactions`, `case_notes`,
  `case_note_access_log`, `communication_preferences`,
  `relationship_manager_assignments`, `loan_account_links`, `outbox`,
  `idempotency_keys`, plus `golang-migrate`'s own `schema_migrations`)
  appear after `up` and are fully removed after `down`.
- `services/crm/Makefile` — identical target set to `services/party`'s:
  `build`, `vet`, `fmt-check`, `test`, `test-integration`,
  `migrate-up`/`down`/`version`/`force`.
- **Integration test**: `internal/integration/integration_test.go` (build
  tag `integration`) spins up a disposable Postgres via
  `testcontainers-go`, applies the real migration, and runs one
  comprehensive lifecycle against it: open a case → **prove the
  database-level optimistic-concurrency guard** (`UpdateCaseConditional`'s
  `WHERE version = $N`) actually rejects a stale-version concurrent
  update, not just the in-memory check a fake store could pass even if
  the real SQL were broken → add a case note and confirm it round-trips
  through **real** AES-GCM encryption (and query the raw column directly
  to confirm the body is genuinely not stored in plaintext) → confirm
  the read-access-log row actually lands → close (idempotent) → confirm
  reopening a case that was never closed fails against the real database
  → reopen the case that was closed → assign/read a relationship
  manager → default vs. updated communication preferences → run the SLA
  sweep with a fast-forwarded clock and confirm both the case row and a
  `crm.case.escalated` outbox entry are genuinely written.
  - Unlike `services/party`'s equivalent, this does **not** also spin up
    Kafka — CRM has no Kafka-backed outbox publisher yet (see "What's
    explicitly out of scope" above); the outbox insert itself is
    verified, but nothing delivers it to a broker in this service.
  - Required an exported `service.NewWithClock` constructor (not present
    before this follow-up) — the integration test lives in a different
    package than `internal/service`, so it cannot reach the unexported
    `now` field `internal/service`'s own tests set directly. `New`
    (production default) is unaffected.
  - **Verified for real, not just written**: ran twice against a live
    Colima Docker daemon on this machine — both runs passed
    (`TestCaseLifecycle_AgainstLivePostgres`, ~1.1–1.4s each), no
    leftover containers afterward.
- **CI**: `.github/workflows/crm-ci.yml` (new), scoped to
  `services/crm/**` and the CRM specs. Same three-job shape as
  `party-ci.yml`: `build-test`, `migrations` (real Postgres service
  container, verifies `up`→`down`→`up`), `integration-test`. Carries
  forward the `cache-dependency-path` fix `party-ci.yml` needed after
  its own first run (`setup-go` looks for `go.sum` at the repo root by
  default, not the nested module directory) — applied here from the
  start rather than discovered the same way twice.
