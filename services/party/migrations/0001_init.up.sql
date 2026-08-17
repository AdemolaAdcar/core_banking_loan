-- Party/CIF service schema.
--
-- Every x-pii:true field from specs/schemas/party.schema.json is stored
-- ONLY as ciphertext (the *_enc columns, produced by internal/pii.Encryptor)
-- plus, where the dedup engine needs an indexed exact-match lookup, a
-- deterministic SHA-256 hash column (*_hash). Hash columns are NOT a
-- confidentiality control by themselves -- they exist purely so the
-- dedup query can use a btree index instead of decrypting every row in
-- the table. Confidentiality comes entirely from the *_enc columns using
-- authenticated (AES-256-GCM) encryption; a hash column never stores
-- anything an attacker could not already compute if they knew the
-- underlying value, which is a deliberately accepted, narrow exception
-- to "no PII at rest outside the encryption boundary" -- a hash is not
-- material an attacker can invert.
--
-- Records are never deleted (7-year audit retention). Tombstoning sets
-- tombstoned = true and never removes the row.

CREATE TABLE parties (
    id                  TEXT PRIMARY KEY,
    status              TEXT NOT NULL,
    kyc_status          TEXT NOT NULL,

    first_name_enc      TEXT NOT NULL,
    last_name_enc       TEXT NOT NULL,
    date_of_birth_enc   TEXT NOT NULL,
    dob_hash            TEXT NOT NULL,

    ssn_enc             TEXT NOT NULL,
    ssn_hash            TEXT NOT NULL,
    ssn_last4           TEXT NOT NULL,

    email_enc           TEXT NOT NULL DEFAULT '',
    email_hash          TEXT NOT NULL DEFAULT '',
    phone_enc           TEXT NOT NULL DEFAULT '',
    phone_hash          TEXT NOT NULL DEFAULT '',

    tombstoned          BOOLEAN NOT NULL DEFAULT FALSE,
    tombstone_reason    TEXT,
    tombstoned_by       TEXT,
    tombstoned_at       TIMESTAMPTZ,

    created_at          TIMESTAMPTZ NOT NULL,
    updated_at          TIMESTAMPTZ NOT NULL
);

-- Indexed columns backing the dedup engine's candidate lookup
-- (store.DedupCandidateFilter). Each is an exact-match filter feeding an
-- OR'd query -- deliberately not a full-table scan compared in
-- application code.
CREATE INDEX idx_parties_ssn_hash ON parties (ssn_hash) WHERE ssn_hash <> '';
CREATE INDEX idx_parties_email_hash ON parties (email_hash) WHERE email_hash <> '';
CREATE INDEX idx_parties_phone_hash ON parties (phone_hash) WHERE phone_hash <> '';
CREATE INDEX idx_parties_dob_hash ON parties (dob_hash);

CREATE TABLE identity_documents (
    id                      TEXT PRIMARY KEY,
    party_id                TEXT NOT NULL REFERENCES parties (id),
    document_type           TEXT NOT NULL,
    version                 INTEGER NOT NULL,
    supersedes_document_id  TEXT REFERENCES identity_documents (id),

    document_number_enc     TEXT NOT NULL,
    document_number_last4   TEXT NOT NULL,
    issuing_authority       TEXT NOT NULL DEFAULT '',
    expires_at              TIMESTAMPTZ,

    created_at              TIMESTAMPTZ NOT NULL,

    -- A document version is append-only per (party, type) -- never
    -- re-issued in place.
    UNIQUE (party_id, document_type, version)
);

CREATE INDEX idx_identity_documents_party_id ON identity_documents (party_id);

-- Every candidate the dedup engine considered on a findOrCreateParty
-- call, matched or not -- an auditor must be able to see the full set
-- that was evaluated, not only the eventual decision.
CREATE TABLE dedup_audit (
    id                    BIGSERIAL PRIMARY KEY,
    applicant_request_id  TEXT NOT NULL,
    matched_party_id      TEXT NOT NULL REFERENCES parties (id),
    rule_id               TEXT NOT NULL,
    confidence            NUMERIC(4, 3) NOT NULL,
    matched_fields        JSONB NOT NULL,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_dedup_audit_applicant_request_id ON dedup_audit (applicant_request_id);
CREATE INDEX idx_dedup_audit_matched_party_id ON dedup_audit (matched_party_id);

-- Transactional outbox: every domain event insert commits in the same
-- transaction as the business write it describes. published_at is set
-- by the relay (internal/outbox.Publisher), never by request-path code.
CREATE TABLE outbox (
    id            TEXT PRIMARY KEY,
    topic         TEXT NOT NULL,
    payload_json  JSONB NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL,
    published_at  TIMESTAMPTZ
);

CREATE INDEX idx_outbox_unpublished ON outbox (created_at) WHERE published_at IS NULL;

-- Idempotency-Key replay cache for state-changing endpoints
-- (findOrCreateParty, updateParty, tombstoneParty, addIdentityDocument).
CREATE TABLE idempotency_keys (
    idempotency_key  TEXT PRIMARY KEY,
    request_hash     TEXT NOT NULL,
    response_json    JSONB NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
