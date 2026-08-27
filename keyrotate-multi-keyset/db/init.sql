-- Key rotation service schema — multi-keyset variant
-- Applied automatically by the postgres image on first container start
-- (mounted into /docker-entrypoint-initdb.d/).
--
-- This variant manages N independent keysets at once (default N=50),
-- not a single keyset. A random subset of M of them (default M=20)
-- are "rotating" -- they get a random expiry and actually cycle keys
-- over time, exactly like the single-keyset base version did. The
-- other N-M sit "static" -- genesis'd once, never scheduled to
-- rotate. Of the M rotating keysets, a random L (0..M) are additionally
-- flagged "revoked" at genesis time: their very first key is created
-- already-expired, so it gets renewed on the first apply cycle instead
-- of waiting for a normal random interval -- modeling "this key was
-- compromised/revoked and had to be replaced immediately."

CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- One row per keyset (unit_01 .. unit_50 by default). This is the new
-- top-level entity in this variant -- everything else (keys, rotation
-- state, rotation events) is now scoped to a keyset_id.
CREATE TABLE IF NOT EXISTS keysets (
    keyset_id  TEXT PRIMARY KEY,
    idx        INT NOT NULL,
    -- Was this keyset one of the M randomly selected for active
    -- rotation? Static (non-rotating) keysets still have a primary
    -- key, they just never get a rotation_state row and never rotate.
    rotating   BOOLEAN NOT NULL DEFAULT false,
    -- Was this keyset additionally flagged for immediate/emergency
    -- renewal at genesis? Permanent historical flag -- it stays true
    -- forever even after the emergency renewal happens (generation
    -- advances past 0), so the UI can still show "this one needed a
    -- forced renewal at birth" as provenance.
    revoked    BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS keys (
    key_id        UUID PRIMARY KEY,
    keyset_id     TEXT NOT NULL REFERENCES keysets(keyset_id),
    generation    INT NOT NULL,
    parent_key_id UUID REFERENCES keys(key_id),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- The instant THIS key is scheduled to be superseded. For a
    -- revoked-at-genesis key this is set slightly in the PAST, on
    -- purpose, so it's due the moment the rotation loop first looks
    -- at it. For a static (non-rotating) keyset's key this is set far
    -- in the future so it is, in effect, never due.
    expires_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    material      BYTEA NOT NULL,
    status        TEXT NOT NULL CHECK (status IN ('pending', 'verified', 'primary', 'retired')),
    verified_at   TIMESTAMPTZ
);

-- Postgres cannot express "at most one primary row per keyset" as a
-- plain CHECK, so a partial unique index (now keyed by keyset_id, not
-- global) physically prevents two primary keys existing at once
-- WITHIN the same keyset, even under concurrent writers. Different
-- keysets are completely independent of each other.
CREATE UNIQUE INDEX IF NOT EXISTS one_primary_key_per_keyset ON keys (keyset_id) WHERE status = 'primary';

CREATE INDEX IF NOT EXISTS keys_keyset_idx ON keys (keyset_id, generation);

-- Idempotency ledger, now scoped by keyset_id. rotation_id is
-- deterministic (derived from keyset_id + primary key + its expiry
-- instant), so a duplicate rotation attempt for the same
-- keyset/window is a primary-key conflict, not a second rotation.
CREATE TABLE IF NOT EXISTS rotation_events (
    rotation_id  TEXT PRIMARY KEY,
    keyset_id    TEXT NOT NULL REFERENCES keysets(keyset_id),
    triggered_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    from_key_id  UUID,
    to_key_id    UUID,
    applied      BOOLEAN NOT NULL DEFAULT false,
    reason       TEXT
);
CREATE INDEX IF NOT EXISTS rotation_events_keyset_idx ON rotation_events (keyset_id, triggered_at DESC);

-- One fixed-plaintext test vector per key, encrypted under that key.
-- The liveness check decrypts every key belonging to the SAME keyset
-- (never across keysets) on every rotation tick for that keyset, to
-- prove that keyset's whole lineage still works.
CREATE TABLE IF NOT EXISTS test_vectors (
    key_id     UUID PRIMARY KEY REFERENCES keys(key_id) ON DELETE CASCADE,
    plaintext  BYTEA NOT NULL,
    ciphertext BYTEA NOT NULL,
    nonce      BYTEA NOT NULL
);

-- Rotation clock, now ONE ROW PER ROTATING KEYSET instead of a single
-- global row. Static (non-rotating) keysets deliberately have no row
-- here at all -- "not scheduled to rotate" is represented by absence,
-- not by some sentinel value. now()-based comparisons happen against
-- the DB's clock, not any individual backend replica's clock.
CREATE TABLE IF NOT EXISTS rotation_state (
    keyset_id       TEXT PRIMARY KEY REFERENCES keysets(keyset_id),
    last_rotated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    min_seconds     INT NOT NULL DEFAULT 3,
    max_seconds     INT NOT NULL DEFAULT 20,
    CHECK (min_seconds >= 1 AND max_seconds >= min_seconds)
);

-- Single global row: the kill switch and the batch-genesis summary.
-- The kill switch is intentionally global (one switch stops rotation
-- for every rotating keyset at once) rather than per-keyset -- same
-- "stop everything" semantics the base version had, just now applied
-- across N keysets instead of one.
CREATE TABLE IF NOT EXISTS system_state (
    id             INT PRIMARY KEY DEFAULT 1,
    initialized    BOOLEAN NOT NULL DEFAULT false,
    keyset_count   INT NOT NULL DEFAULT 0, -- N
    rotating_count INT NOT NULL DEFAULT 0, -- M
    revoked_count  INT NOT NULL DEFAULT 0, -- L
    paused         BOOLEAN NOT NULL DEFAULT false,
    paused_at      TIMESTAMPTZ,
    paused_reason  TEXT,
    CHECK (id = 1)
);
INSERT INTO system_state (id) VALUES (1) ON CONFLICT (id) DO NOTHING;

-- Server-side, append-only audit trail of every /api/genesis request
-- the backend ever received (there is normally exactly one real
-- "created" row, ever, plus one "already-initialized" row per extra
-- click after that).
CREATE TABLE IF NOT EXISTS genesis_attempts (
    id          BIGSERIAL PRIMARY KEY,
    received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    remote_addr TEXT,
    outcome     TEXT NOT NULL, -- created | already-initialized | rejected
    detail      TEXT
);
