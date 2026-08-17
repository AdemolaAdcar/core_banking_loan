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

**`internal/api/handlers_test.go` (12 tests)**, against an in-memory fake
`store.Store` (`internal/api/fake_store_test.go`) and a fake `auth.Validator`
(`internal/api/fake_auth_test.go`), using `net/http/httptest`:
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
- `TestAuth_MissingAuthorizationHeader_401` — every route requires a
  bearer token; there is no unauthenticated route.
- `TestAuth_InvalidToken_401` — a token the validator rejects is refused,
  not passed through.
- `TestAuth_ReadScopeCannotFindOrCreate_403`,
  `TestAuth_WriteScopeCannotGetParty_403` — `party:read` and `party:write`
  are enforced per-operation, exactly as `party-cif.yaml`'s
  `serviceAuth` scheme declares — a read-scoped token cannot call a
  write operation and vice versa.

**`internal/auth/jwks_test.go` (8 tests)** — the JWKS/RS256 token
validator itself, using real RSA keypairs and a real `httptest.Server`
JWKS endpoint (not mocked at the JWT-library level):
- `TestJWKSValidator_ValidToken_ReturnsClaims` — a correctly signed,
  unexpired token validates and its scopes are extracted.
- `TestJWKSValidator_ExpiredToken_Rejected` — an expired token is refused.
- `TestJWKSValidator_WrongSigningKey_Rejected` — a token signed with a
  key different from the one the JWKS endpoint advertises under that
  `kid` fails signature verification.
- `TestJWKSValidator_HS256AlgorithmConfusion_Rejected` — the classic
  RS256/HS256 algorithm-confusion attack (HMAC-signing a forged token
  using the public RSA key's bytes as the HMAC secret) is rejected
  outright, not validated against the RSA key.
- `TestJWKSValidator_WrongIssuer_Rejected` — issuer mismatch is caught
  when an issuer is configured.
- `TestJWKSValidator_UnknownKeyID_TriggersRefreshThenFails` — an unknown
  `kid` triggers exactly one JWKS re-fetch (the mechanism that lets key
  rotation work without a background refresh goroutine) and still fails
  cleanly if the key genuinely doesn't exist.
- `TestClaims_HasScope`, `TestTokenClaims_ScopesFallsBackToScpArray` —
  scope-checking and the `scope`/`scp` claim dual-support.

**Verification:** `go build ./...`, `go vet ./...`, and `gofmt -l .` (clean)
all pass; `go test ./...` — 57/57 tests passing across
`internal/domain`, `internal/pii`, `internal/service`, `internal/api`, and
`internal/auth`. `internal/store/postgres` and `cmd/party-service` have no
unit tests — they require a live Postgres instance (and, for `main.go`, a
live JWKS endpoint) to exercise meaningfully; that is integration-test
territory, not unit-test territory, and is out of scope for this change.

## Follow-up: auth middleware (added after initial review)

The first pass of this service had no authentication or authorization
enforcement at all, despite `party-cif.yaml` declaring a global
`serviceAuth` OAuth2 security scheme with `party:read`/`party:write`
scopes — every endpoint was reachable by anyone who could route to it.
This is now fixed:

- `internal/auth` — `Validator` interface (mirrors the `pii.Encryptor`
  pattern: a narrow interface with a real implementation and a
  swappable test double); `JWKSValidator` validates RS256-signed access
  tokens against a JSON Web Key Set fetched from the internal
  authorization server, entirely locally per request (no callback to
  the auth server on the request path). Explicitly rejects non-RS256
  tokens before touching the key cache — defense in depth against
  algorithm-confusion attacks — and optionally checks issuer/audience.
- `internal/api/auth_middleware.go` — `withScope` wraps every route in
  `Routes()` with the exact scope that operation declares in the spec;
  missing/invalid token → 401, valid token missing the required scope →
  403.
- `cmd/party-service/main.go` — wires a `JWKSValidator` from
  `PARTY_SERVICE_JWKS_URL` (required), `PARTY_SERVICE_TOKEN_ISSUER` and
  `PARTY_SERVICE_TOKEN_AUDIENCE` (both optional but recommended in
  production).

## Follow-up: Kafka outbox publisher (added after the auth follow-up)

`internal/outbox.Publisher` previously had no concrete implementation —
domain events were written to the `outbox` table transactionally with
every business write, but nothing ever delivered them anywhere. That's
now wired up:

- `internal/outbox.Reader` (new interface) — the minimal read/mark
  contract a relay needs from the outbox table (`ListUnpublished`,
  `MarkPublished`). Declared in `internal/outbox`, not `internal/store`,
  specifically to avoid an import cycle (`internal/store` already imports
  `internal/outbox` for `Inserter`).
- `internal/store/postgres` — `Store.ListUnpublished` /
  `Store.MarkPublished` satisfy `outbox.Reader` structurally; both run
  outside any request-path transaction, since the relay is an
  independently-polling process, not part of the write path.
- `internal/relay` (new package, `segmentio/kafka-go` — pure Go, no
  cgo/librdkafka dependency) — `Publisher.PublishUnpublished` lists a
  batch of unpublished rows, writes each to its own Kafka topic (every
  outbox entry carries its own `Topic`; a single `kafka-go` `Writer` with
  no fixed topic handles all three event types), and only marks a row
  published after the Kafka write actually succeeds. A write failure
  marks nothing, so the same rows are retried on the next poll — an
  intentional at-least-once contract, consistent with every consumer of
  these topics elsewhere in the platform already being expected to be
  idempotent (see `internal/outbox`'s package doc comment).
- `cmd/party-service/main.go` — wires a real `*kafkago.Writer` from
  `PARTY_SERVICE_KAFKA_BROKERS` (required, comma-separated) and runs the
  relay on a ticker (`PARTY_SERVICE_OUTBOX_POLL_INTERVAL`, default 2s) in
  a background goroutine that shuts down with the rest of the process. A
  publish error is logged, never fatal — a transient Kafka outage must
  not take down the HTTP service; the outbox pattern exists specifically
  so business writes stay durable and correct regardless of the broker's
  momentary availability.

5 new tests in `internal/relay/kafka_publisher_test.go`, against a fake
`outbox.Reader` and a fake Kafka `Writer`:
- `TestPublishUnpublished_NoEntries_NoOp` — an empty outbox writes
  nothing to Kafka.
- `TestPublishUnpublished_WritesEachEntryToItsOwnTopic_ThenMarksPublished`
  — each entry is written to its own topic, and only marked published
  after the write.
- `TestPublishUnpublished_WriteFails_NothingMarkedPublished` — a Kafka
  write failure marks nothing published, so the same entries retry.
- `TestPublishUnpublished_MarkPublishedFails_ErrorSurfaced` — a failure
  marking published (after a successful Kafka write) surfaces as an
  error, distinctly from a publish failure.
- `TestPublishUnpublished_RespectsBatchSize` — a poll never fetches more
  than its configured batch size.

**Verification:** `go build ./...`, `go vet ./...`, `gofmt -l .` (clean),
and `go test ./...` all pass — 62/62 tests total.
