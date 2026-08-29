-- Key rotation service schema — "all rotate + revoke interceptor" variant.
--
-- Every one of the N keysets rotates on its own random interval (there is
-- no static/non-rotating tier in this variant). Independently, a
-- background "revoke interceptor" loop randomly picks a still-active
-- keyset and fires an out-of-band revoke against it -- modeling "this
-- key got compromised right now, not on its normal schedule."
--
-- A revoke can land at any instant, including the same instant the
-- timer-driven rotation loop is mid-rotation for that exact keyset. Both
-- paths lock the SAME per-keyset row (rotation_state) with
-- `SELECT ... FOR UPDATE` before acting, so Postgres itself serializes
-- them: whichever transaction gets there first commits fully, and the
-- second one re-reads the now-current reality and acts on THAT --
-- nothing is ever silently dropped because of a collision.

CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE IF NOT EXISTS keysets (
    keyset_id     TEXT PRIMARY KEY,
    idx           INT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Set true the moment a halt-mode revoke lands on this keyset.
    -- Permanent: once terminated, a keyset never rotates again, by
    -- design -- there is no "un-terminate."
    terminated    BOOLEAN NOT NULL DEFAULT false,
    terminated_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS keys (
    key_id        UUID PRIMARY KEY,
    keyset_id     TEXT NOT NULL REFERENCES keysets(keyset_id),
    generation    INT NOT NULL,
    parent_key_id UUID REFERENCES keys(key_id),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    material      BYTEA NOT NULL,
    status        TEXT NOT NULL CHECK (status IN ('pending', 'verified', 'primary', 'retired')),
    verified_at   TIMESTAMPTZ,
    -- Set the instant THIS SPECIFIC key was revoked -- independent of
    -- what happened to its keyset afterward. In auto-rotate mode, the
    -- OLD primary key gets this set right before it's retired (its
    -- replacement does NOT get it set -- the replacement wasn't
    -- revoked, it's the clean new key). In halt mode, the CURRENT
    -- primary key gets this set and then stays primary forever (no
    -- replacement is ever created). This is what actually answers
    -- "was this key revoked," independent of `keysets.terminated`
    -- (which only tells you whether the KEYSET stopped rotating, not
    -- which key, if any, was the one that got revoked).
    revoked_at    TIMESTAMPTZ
);

CREATE UNIQUE INDEX IF NOT EXISTS one_primary_key_per_keyset ON keys (keyset_id) WHERE status = 'primary';
CREATE INDEX IF NOT EXISTS keys_keyset_idx ON keys (keyset_id, generation);

-- Append-only ledger of EVERY rotation-related event, across both
-- triggers. `trigger` says what caused this row; `outcome` says what
-- actually happened. The four outcomes that matter for the UI/tfvars
-- "status" fields are:
--   rotated             -- a normal, on-schedule (timer) rotation
--   revoked_rotated     -- a revoke landed and REVOKE_AUTO_ROTATE=true,
--                          so it was immediately, emergency-rotated and
--                          resumed its normal random-interval cycling
--   revoked_terminated  -- a revoke landed and REVOKE_AUTO_ROTATE=false,
--                          so the keyset was halted instead -- no new
--                          key, no further rotation, ever
--   failed              -- the post-rotation liveness test failed;
--                          nothing was promoted, the old primary key is
--                          untouched
-- (there's also a transient `in_progress` outcome a row is briefly
-- inserted with, before being finalized to one of the above in the same
-- transaction -- so a crash mid-rotation never leaves a permanently
-- ambiguous row).
CREATE TABLE IF NOT EXISTS rotation_events (
    rotation_id  TEXT PRIMARY KEY,
    keyset_id    TEXT NOT NULL REFERENCES keysets(keyset_id),
    trigger      TEXT NOT NULL CHECK (trigger IN ('timer', 'revoke')),
    triggered_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    from_key_id  UUID,
    to_key_id    UUID,
    outcome      TEXT NOT NULL DEFAULT 'in_progress'
        CHECK (outcome IN ('in_progress', 'rotated', 'revoked_rotated', 'revoked_terminated', 'skipped', 'failed')),
    reason       TEXT
);
CREATE INDEX IF NOT EXISTS rotation_events_keyset_idx ON rotation_events (keyset_id, triggered_at DESC);

CREATE TABLE IF NOT EXISTS test_vectors (
    key_id     UUID PRIMARY KEY REFERENCES keys(key_id) ON DELETE CASCADE,
    plaintext  BYTEA NOT NULL,
    ciphertext BYTEA NOT NULL,
    nonce      BYTEA NOT NULL
);

-- The per-keyset rotation clock AND the per-keyset lock point both the
-- timer loop and the revoke path serialize on. Row PRESENCE means
-- "still active, still cycling"; a keyset's row is DELETED the instant
-- it's halted by a revoke (see runHaltKeyset in revoke.go) -- so a
-- terminated keyset simply, permanently disappears from every future
-- due-check and every future random revoke draw, with no separate
-- "is this one terminated" filter needed at the SQL level (though the
-- keysets.terminated flag above is still kept as the permanent,
-- queryable historical record).
CREATE TABLE IF NOT EXISTS rotation_state (
    keyset_id       TEXT PRIMARY KEY REFERENCES keysets(keyset_id),
    last_rotated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    min_seconds     INT NOT NULL DEFAULT 3,
    max_seconds     INT NOT NULL DEFAULT 20,
    CHECK (min_seconds >= 1 AND max_seconds >= min_seconds)
);

-- Single global row: kill switch, batch-genesis summary, and the
-- revoke-mode toggle ("what happens when a revoke lands").
CREATE TABLE IF NOT EXISTS system_state (
    id                 INT PRIMARY KEY DEFAULT 1,
    initialized        BOOLEAN NOT NULL DEFAULT false,
    keyset_count       INT NOT NULL DEFAULT 0, -- N
    paused             BOOLEAN NOT NULL DEFAULT false,
    paused_at          TIMESTAMPTZ,
    paused_reason      TEXT,
    -- true  = REVOKE_AUTO_ROTATE mode: a revoke immediately, emergency-
    --         rotates the keyset, which then resumes normal cycling.
    -- false = HALT mode: a revoke permanently stops that keyset --
    --         no new key, no further rotation, ever, for that keyset.
    -- Global, and live-toggleable at any time (see /api/revoke-mode);
    -- read fresh, inside the transaction, at the moment each revoke is
    -- actually processed -- so which mode applies is always whatever
    -- was configured at THAT instant, not at genesis time.
    revoke_auto_rotate BOOLEAN NOT NULL DEFAULT true,
    CHECK (id = 1)
);
INSERT INTO system_state (id) VALUES (1) ON CONFLICT (id) DO NOTHING;

CREATE TABLE IF NOT EXISTS genesis_attempts (
    id          BIGSERIAL PRIMARY KEY,
    received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    remote_addr TEXT,
    outcome     TEXT NOT NULL, -- created | already-initialized | rejected
    detail      TEXT
);
