# Party/CIF service: find-or-create, dedup, identity documents, tombstoning

## What changed

Implements the Party/CIF service end to end — spec extension, business logic,
persistence, and HTTP transport. This is a new service (`services/party`);
nothing here modifies any other module.

**Spec changes (prerequisite for this implementation, since the prior spec was
read-only):**
- `specs/schemas/party.schema.json`: extended `Party` with PII fields
  (firstName, lastName, dateOfBirth, email, phone, ssnLast4 — all `x-pii:
  true`) and tombstone fields; added `IdentityDocument` and
  `DedupMatchExplanation` defs.
- `specs/openapi/party-cif.yaml`: **v0.3.0 → v0.4.0, breaking.** Added
  `POST /parties:find-or-create`, `PATCH /parties/{partyId}`,
  `POST /parties/{partyId}:tombstone`, `GET/POST /parties/{partyId}/documents`,
  `GET /parties/{partyId}/documents/{documentId}`. Added `party:write` scope.
- `specs/asyncapi/party-events.yaml` (new file): `party.created`,
  `party.updated`, `party.tombstoned` — payloads carry zero PII by
  construction (IDs, status enums, changed-field *names*, never values).

**Implementation (`services/party/`), Go 1.26, chi + pgx/v5 + Postgres:**

- `internal/domain` — dedup engine (`dedup.go`): seven named, deterministic
  rules (SSN exact → name+DOB exact → email/phone+name corroborated →
  bounded Levenshtein near-match+DOB → email/phone only), fixed confidence
  scores, no ML/probabilistic component. `Decide()` auto-matches at ≥0.80
  confidence and conservatively refuses to auto-match when the top two
  results are both above threshold but reference different parties.
- `internal/pii` — `AESGCMEncryptor`: AES-256-GCM field-level encryption,
  fails closed on tampered ciphertext, fresh nonce per call.
- `internal/events` / `internal/outbox` — typed event payloads matching
  `party-events.yaml` exactly; transactional outbox pattern (event insert
  and business write commit together or not at all).
- `internal/store` — persistence contract (`Store`/`Tx` interfaces);
  `internal/store/postgres` is the only package that imports pgx or
  decrypts/encrypts PII. Dedup candidate lookup is one indexed query over
  hash columns (`ssn_hash`/`email_hash`/`phone_hash`/`dob_hash`), never a
  full-table scan; hash columns are index keys, not a confidentiality
  control — the `*_enc` columns (AES-256-GCM) are.
- `internal/service` — orchestration: `FindOrCreateParty`,
  `UpdateParty` (refuses writes to a tombstoned party, no-ops when nothing
  actually changed), `TombstoneParty` (idempotent — retrying a tombstone
  is a no-op success, not an error), `AddIdentityDocument`
  (version = prior max + 1, `supersedesDocumentId` set, prior row never
  edited). Every dedup candidate considered — matched or not — is recorded
  to the audit log before the call returns.
- `internal/api` — chi handlers for every `party-cif.yaml` operation, plus
  Idempotency-Key middleware (cache-after-success against the store; see
  `internal/api/idempotency.go` doc comment for the "at-least-once, safe on
  retry" tradeoff this implies).
- `cmd/party-service` — wires Postgres pool, AES key (base64 via
  `PARTY_SERVICE_ENCRYPTION_KEY`, expected pre-resolved from a KMS envelope
  decrypt upstream of this process — this process never talks to a KMS
  itself), and the HTTP server together.
- `migrations/0001_init.up.sql` / `.down.sql` — `parties`,
  `identity_documents`, `dedup_audit`, `outbox`, `idempotency_keys`.

## Ground rules honored

- **No PII in logs or audit trail.** `dedup_audit.matched_fields` stores
  field *names* only, matching `DedupMatchExplanation.matchedFields`.
  `party.updated` events carry `changedFields` (names), never values.
- **No deletes.** Tombstoning sets `tombstoned = TRUE` and never removes a
  row (`internal/service.TombstoneParty`, `migrations/0001_init.up.sql`).
- **Encryption boundary.** Every `x-pii: true` field is encrypted
  (`internal/pii.AESGCMEncryptor`) before it reaches `internal/store/postgres`
  and is decrypted only there; `internal/service` and everything above it
  works with plaintext in memory for the duration of one request only.
- **Transactional outbox.** Every `CreateParty`/`UpdateParty`/
  `TombstoneParty` write and its corresponding domain event insert happen
  inside the same `store.WithinTx` call — see `internal/service/service.go`.
- **Deterministic, explainable dedup.** Every match is traceable to one
  named rule (`domain.RuleSSNExact`, etc.) and a fixed confidence score,
  never an opaque or probabilistic score.

## What's explicitly out of scope

- A concrete Kafka-backed `outbox.Publisher` (the relay that reads
  unpublished outbox rows and actually delivers them) — the interface and
  an in-memory test fake exist; a production implementation is a
  deployment-time concern, not this change.
- KMS integration for the encryption key — `cmd/party-service/main.go`
  takes an already-resolved 32-byte key from an environment variable by
  design; resolving it from a KMS is upstream of this process.
- Running migrations automatically at startup (no migration-runner
  dependency was added; `migrations/*.sql` are plain SQL files intended for
  whatever migration tool this repo standardizes on).

## Spec version implemented

- `specs/openapi/party-cif.yaml` v0.4.0
- `specs/schemas/party.schema.json` (Party/IdentityDocument/DedupMatchExplanation defs, current)
- `specs/asyncapi/party-events.yaml` (new)

## Unit tests written

**`internal/domain/dedup_test.go` (19 tests)** — the dedup engine, the
highest-risk piece of this change:
- `TestEvaluateCandidate_SSNExactMatch_HighestConfidence`,
  `TestEvaluateCandidate_NameDOBExact` — highest-confidence rules fire
  correctly and take priority.
- `TestEvaluateCandidate_NearMatchName_WithMatchingDOB`,
  `TestEvaluateCandidate_NearMatchName_TooDifferent_NoMatch`,
  `TestNamesNearMatch_BoundedByLength` — near-match names (a data-entry
  variant) match when DOB corroborates and length-bounded edit distance is
  small; names that are simply different do not.
- `TestEvaluateCandidate_EmailReusedAcrossHousehold_NotAutoMatched`,
  `TestEvaluateCandidate_PhoneReusedAcrossHousehold_NotAutoMatched` — a
  shared email/phone across two different people (a household) alone never
  clears the auto-match threshold.
- `TestEvaluateCandidate_EmailPlusNameCorroborated_HigherConfidenceThanEmailAlone`
  — corroborating signals produce a materially higher score than one alone.
- `TestEvaluateAll_SortsByDescendingConfidence` — candidate ranking.
- `TestDecide_NoResults_NotMatched`, `TestDecide_BelowThreshold_NotMatched`,
  `TestDecide_AboveThreshold_Matched` — the auto-match threshold boundary.
- `TestDecide_AmbiguousTopTwoDifferentParties_ConservativelyNotMatched`,
  `TestDecide_TopTwoSameParty_StillMatched` — ambiguous collisions between
  two distinct existing parties are refused, not silently resolved.
- `TestEvaluateCandidate_MatchesTombstonedParty_EngineDoesNotFilterTombstoned`,
  `TestEvaluateAll_ReapplicationAfterTombstone_StillSurfacesAsTopCandidate`
  — re-application after tombstone still surfaces the tombstoned party as
  the top candidate (the engine itself is tombstone-agnostic; the service
  layer decides what to do with that).
- `TestNormalizeName`, `TestNormalizeEmail`, `TestNormalizePhone` —
  normalization functions used identically by both applicant and candidate
  sides of every comparison.

**`internal/pii/encryption_test.go` (6 tests)**:
- `TestAESGCMEncryptor_RoundTrip` — encrypt/decrypt round-trips correctly.
- `TestAESGCMEncryptor_EmptyStringPassesThrough` — an empty (unset optional
  PII field) value stays empty rather than becoming a spurious ciphertext.
- `TestAESGCMEncryptor_TamperedCiphertextFailsClosed` — tampering is always
  detected (authenticated encryption), never silently decrypted into
  garbage.
- `TestAESGCMEncryptor_TwoEncryptionsOfSameValueDifferByNonce` — no nonce
  reuse, so identical plaintexts don't produce identical ciphertexts.
- `TestNewAESGCMEncryptor_RejectsWrongKeyLength` — a non-32-byte key is
  rejected outright, never silently truncated/padded.
- `TestAESGCMEncryptor_WrongKeyCannotDecrypt` — decryption with the wrong
  key fails closed.

**`internal/service/service_test.go` (12 tests)**, against an in-memory fake
`store.Store` (`internal/service/fake_store_test.go`):
- `TestFindOrCreateParty_NoCandidates_CreatesNewPartyAndPublishesOutbox` —
  the create path writes exactly one `party.created` outbox entry.
- `TestFindOrCreateParty_SSNExactMatch_ReturnsExistingParty_NoWrite` — a
  matched applicant returns the existing party and writes nothing new.
- `TestFindOrCreateParty_RecordsEveryCandidateConsidered_NotOnlyTheWinner`
  — every candidate the engine considered is recorded to the audit log,
  not only the eventual winner.
- `TestFindOrCreateParty_AmbiguousTopTwo_CreatesNewParty_StillAudited` —
  an ambiguous collision still creates a new party (conservative) while
  still auditing both candidates it considered.
- `TestUpdateParty_TombstonedParty_Refused` — updating a tombstoned party
  is refused (`ErrPartyTombstoned`), no outbox entry written.
- `TestUpdateParty_NoActualChange_IsNoOp_NoWriteNoEvent` — supplying the
  same value that's already on file writes nothing and publishes nothing.
- `TestUpdateParty_EmailChanged_PublishesChangedFieldNamesOnly` — a real
  change writes and publishes exactly one `party.updated` event.
- `TestTombstoneParty_FirstCall_WritesAndPublishes` — the first tombstone
  call writes and publishes.
- `TestTombstoneParty_AlreadyTombstoned_IsIdempotent_NoDuplicateEvent` — a
  retried tombstone call on an already-tombstoned party succeeds without
  writing a second event.
- `TestAddIdentityDocument_FirstDocumentOfType_IsVersion1_NoSupersedes`,
  `TestAddIdentityDocument_SecondDocumentOfSameType_IncrementsVersionAndSupersedes`,
  `TestAddIdentityDocument_DifferentTypesVersionIndependently` — document
  versioning: version increments and supersedes-linking per (party, type),
  independent across different document types, and the prior version is
  never removed from the fake store.

**`internal/api/handlers_test.go` (8 tests)**, against an in-memory fake
`store.Store` (`internal/api/fake_store_test.go`), using `net/http/httptest`:
- `TestFindOrCreateParty_MissingIdempotencyKey_400` — the required header
  is actually enforced.
- `TestFindOrCreateParty_CreatesParty_201` — the create path returns 201
  with the expected response shape.
- `TestIdempotency_ReplaySameKeySamePayload_ReturnsCachedResponse_NoSecondPartyCreated`
  — a retried request with the same key and payload replays the identical
  cached response and does not create a second party.
- `TestIdempotency_SameKeyDifferentPayload_409` — reusing a key with a
  materially different payload is refused, per spec.
- `TestGetParty_NotFound_404` — `store.ErrNotFound` maps to HTTP 404.
- `TestUpdateParty_Tombstoned_409` — `service.ErrPartyTombstoned` maps to
  HTTP 409.
- `TestTombstoneParty_Idempotent_SecondCallStillReturns200` — the HTTP
  layer surfaces the service's idempotent-tombstone behavior correctly.
- `TestAddIdentityDocument_SecondVersionSupersedesFirst` — versioning is
  correct end-to-end through the HTTP layer.

**Verification:** `go build ./...`, `go vet ./...`, and `gofmt -l .` (clean)
all pass; `go test ./...` — 45/45 tests passing across
`internal/domain`, `internal/pii`, `internal/service`, and `internal/api`.
`internal/store/postgres` and `cmd/party-service` have no unit tests — they
require a live Postgres instance to exercise meaningfully; that is
integration-test territory, not unit-test territory, and is out of scope
for this change.
