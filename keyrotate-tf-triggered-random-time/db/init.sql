-- Key rotation service schema
-- Applied automatically by the postgres image on first container start
-- (mounted into /docker-entrypoint-initdb.d/).

CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE IF NOT EXISTS keys (
    key_id        UUID PRIMARY KEY,
    generation    INT NOT NULL,
    parent_key_id UUID REFERENCES keys(key_id),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- The instant THIS key is scheduled to be superseded. Set once,
    -- at the same moment the key is created, to whatever random
    -- expiry was rolled for that rotation cycle. This is what lets
    -- Terraform's keys list carry a per-entry `expiration` field
    -- (matching a real keyset resource shape) instead of the whole
    -- file needing one bolted-on top-level expires_at value.
    expires_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    material      BYTEA NOT NULL,
    status        TEXT NOT NULL CHECK (status IN ('pending', 'verified', 'primary', 'retired')),
    verified_at   TIMESTAMPTZ
);
ALTER TABLE keys ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ NOT NULL DEFAULT now();

-- Postgres cannot express "at most one row" as a normal CHECK constraint,
-- so a partial unique index is used to physically prevent two primary
-- keys from ever existing at once, even under concurrent writers.
CREATE UNIQUE INDEX IF NOT EXISTS one_primary_key ON keys ((true)) WHERE status = 'primary';

-- Idempotency ledger. rotation_id is deterministic (derived from the
-- current primary key + the rotation time-window), so a duplicate
-- rotation attempt for the same window is a primary-key conflict,
-- not a second rotation.
CREATE TABLE IF NOT EXISTS rotation_events (
    rotation_id  TEXT PRIMARY KEY,
    triggered_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    from_key_id  UUID,
    to_key_id    UUID,
    applied      BOOLEAN NOT NULL DEFAULT false,
    reason       TEXT
);

-- One fixed-plaintext test vector per key, encrypted under that key.
-- The liveness check decrypts every non-retired key's own vector on
-- every rotation tick to prove the whole active key set still works.
CREATE TABLE IF NOT EXISTS test_vectors (
    key_id     UUID PRIMARY KEY REFERENCES keys(key_id) ON DELETE CASCADE,
    plaintext  BYTEA NOT NULL,
    ciphertext BYTEA NOT NULL,
    nonce      BYTEA NOT NULL
);

-- Single-row rotation clock. now()-based comparisons happen against
-- the DB's clock, not any individual backend replica's clock.
--
-- `paused` is the kill switch: it lives here (not in a Go global var)
-- for the same reason `last_rotated_at` does -- it must be read inside
-- the SAME locked, FOR-UPDATE transaction that decides whether to
-- rotate, so the switch is race-free across replicas and survives a
-- backend restart without needing to be re-applied.
-- Single-row rotation clock. In this variant, rotation is
-- externally triggered (by Terraform's local-exec, via
-- POST /api/rotate-if-due) rather than by Go's own ticker -- so
-- `expires_at` is the one fact any external trigger MUST read fresh
-- before deciding to call in, and Go still re-validates it inside the
-- locked transaction regardless of what the caller believed.
CREATE TABLE IF NOT EXISTS rotation_state (
    id               INT PRIMARY KEY DEFAULT 1,
    last_rotated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    min_seconds      INT NOT NULL DEFAULT 3,
    max_seconds      INT NOT NULL DEFAULT 20,
    paused           BOOLEAN NOT NULL DEFAULT false,
    paused_at        TIMESTAMPTZ,
    paused_reason    TEXT,
    CHECK (id = 1),
    CHECK (min_seconds >= 1 AND max_seconds >= min_seconds)
);
INSERT INTO rotation_state (id) VALUES (1) ON CONFLICT (id) DO NOTHING;

-- Idempotent migration path for anyone upgrading an existing stack
-- in place (pgdata volume already initialized, so the CREATE TABLE
-- above was a no-op and never saw the new columns). Safe to re-run.
ALTER TABLE rotation_state ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ NOT NULL DEFAULT now();
ALTER TABLE rotation_state ADD COLUMN IF NOT EXISTS min_seconds INT NOT NULL DEFAULT 3;
ALTER TABLE rotation_state ADD COLUMN IF NOT EXISTS max_seconds INT NOT NULL DEFAULT 20;
ALTER TABLE rotation_state ADD COLUMN IF NOT EXISTS paused BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE rotation_state ADD COLUMN IF NOT EXISTS paused_at TIMESTAMPTZ;
ALTER TABLE rotation_state ADD COLUMN IF NOT EXISTS paused_reason TEXT;

-- Server-side, append-only audit trail of every /api/genesis request
-- the backend ever received -- successful, rejected, or errored. This
-- is what makes a re-click "traced": the trace is a durable DB row
-- written before the handler returns, not something inferred from the
-- frontend's button state (which resets after every request and
-- proves nothing about what actually happened server-side).
CREATE TABLE IF NOT EXISTS genesis_attempts (
    id               BIGSERIAL PRIMARY KEY,
    received_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    attempted_key_id TEXT,
    client_created_at TEXT,
    remote_addr      TEXT,
    outcome          TEXT NOT NULL, -- created | already-initialized | rejected | invalid
    detail           TEXT
);
