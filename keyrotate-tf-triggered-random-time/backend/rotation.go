package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"time"
)

// rotationLockKey is the advisory-lock key. If more than one backend
// replica is ever run, only one of them will win pg_try_advisory_lock
// on any given tick, so exactly one actor evaluates/performs rotation.
const rotationLockKey = 918_273_645

// terraformKeysetID and terraformKeyType are constants shared with
// terraform/main.tf's matching variable defaults (var.id, var.type).
// They identify the keyset itself, not any individual key -- they
// never change across rotations, only the contents of var.keys do.
const (
	terraformKeysetID = "unit_1_keyset"
	terraformKeyType  = "AES-256-GCM"
	terraformKeyBits  = 256 // matches the fixed 32-byte (256-bit) AES-256 material in crypto.go
)

type rotationResult struct {
	Rotated       bool
	Skipped       string // reason, if not rotated
	RotationID    string
	FromKeyID     string
	ToKeyID       string
	TestPassed    bool
	TestDetail    string
	NextExpiresAt time.Time
}

// runStatusBroadcastLoop only reads and broadcasts state every second
// for the UI's live countdown -- it does NOT decide whether to rotate.
// In this variant, that decision only ever happens inside tryRotate,
// called from handleRotateIfDue in response to an inbound HTTP call
// (in the reference deployment: Terraform's local-exec provisioner).
// Go itself no longer has an autonomous "is it time yet" loop -- see
// README, "What Go gives up in this variant" for what that trades away.
func runStatusBroadcastLoop(ctx context.Context, db *sql.DB, hub *sseHub, sharedDir, tfConfigPath string) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if status, err := loadStatus(ctx, db, sharedDir, tfConfigPath); err == nil {
				hub.broadcast(status)
			} else {
				log.Printf("status load error: %v", err)
			}
		}
	}
}

// tryRotate is unchanged in spirit from the ticker-driven version --
// same advisory lock, same one-transaction verify-then-promote flow,
// same idempotency guarantee -- but the due-check is now keyed off a
// random per-rotation `expires_at` instead of a fixed interval, and it
// is the caller's job (an HTTP handler, not a ticker) to decide *when*
// to call this. Calling it early, late, or twice in a row is always
// safe: the function itself is the source of truth for whether a
// rotation actually happens.
func tryRotate(ctx context.Context, db *sql.DB, sharedDir string) (*rotationResult, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmtErr("acquire conn", err)
	}
	defer conn.Close()

	// Only one replica proceeds past this point per tick. Everyone
	// else no-ops this tick and tries again next tick.
	var locked bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", rotationLockKey).Scan(&locked); err != nil {
		return nil, fmtErr("advisory lock", err)
	}
	if !locked {
		return &rotationResult{Skipped: "lock held by another replica"}, nil
	}
	defer conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", rotationLockKey)

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmtErr("begin tx", err)
	}
	// Rollback is a no-op after a successful Commit, so this defer is
	// safe as a catch-all for every early-return error path below.
	defer tx.Rollback()

	var now time.Time
	var lastRotatedAt time.Time
	var expiresAt time.Time
	var minSeconds, maxSeconds int
	var paused bool
	err = tx.QueryRowContext(ctx,
		`SELECT now(), last_rotated_at, expires_at, min_seconds, max_seconds, paused
		 FROM rotation_state WHERE id = 1 FOR UPDATE`,
	).Scan(&now, &lastRotatedAt, &expiresAt, &minSeconds, &maxSeconds, &paused)
	if err != nil {
		return nil, fmtErr("load rotation_state", err)
	}

	// Kill switch. Checked inside the same FOR-UPDATE row lock as the
	// due-time check below, so a pause request that lands mid-call
	// can't race a rotation that's already past the due check -- it
	// either landed before this SELECT (rotation skipped this call) or
	// after (rotation already committed, takes effect on the next
	// call). No half-paused state is observable either way.
	if paused {
		return &rotationResult{Skipped: "auto-rotation paused (kill switch engaged)"}, nil
	}

	// The due-check itself is unchanged in shape from the fixed-interval
	// version -- "has enough time passed" -- just measured against a
	// stored expiry instant instead of last_rotated_at + a constant.
	// Whoever calls this endpoint (the shell loop, in this variant) is
	// expected to have read the SAME expires_at off the tfvars/output
	// json first and only call in when it believes now >= expires_at --
	// but that belief is never trusted on its own; this check is what
	// actually decides, using the DB's own clock.
	if now.Before(expiresAt) {
		return &rotationResult{Skipped: fmt.Sprintf("not due yet (expires at %s)", expiresAt.Format(time.RFC3339Nano))}, nil
	}

	var primaryKeyID string
	var generation int
	var primaryMaterial []byte
	err = tx.QueryRowContext(ctx,
		`SELECT key_id, generation, material FROM keys WHERE status = 'primary'`,
	).Scan(&primaryKeyID, &generation, &primaryMaterial)
	if err == sql.ErrNoRows {
		return &rotationResult{Skipped: "no primary key yet (waiting for genesis)"}, nil
	}
	if err != nil {
		return nil, fmtErr("load primary key", err)
	}

	// Deterministic rotation id: same primary key + same expiry instant
	// always hashes to the same id. A second call that arrives before
	// the expiry instant changes (a retried curl, a duplicate
	// local-exec run, two overlapping shell-loop invocations) collides
	// on the rotation_events primary key and is rejected by the DB
	// itself -- idempotency lives in the schema, not in "please don't
	// call this twice" discipline, exactly as before; only the key
	// changed from a time-bucket to an expiry-instant.
	rotationID := computeRotationID(primaryKeyID, expiresAt)

	res, err := tx.ExecContext(ctx,
		`INSERT INTO rotation_events (rotation_id, from_key_id, reason) VALUES ($1, $2, 'in-progress')
		 ON CONFLICT (rotation_id) DO NOTHING`,
		rotationID, primaryKeyID,
	)
	if err != nil {
		return nil, fmtErr("insert rotation_event", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return &rotationResult{Skipped: "this expiry instant was already handled", RotationID: rotationID}, nil
	}

	newMaterial, err := deriveNextKey(primaryMaterial, now)
	if err != nil {
		return nil, fmtErr("derive next key", err)
	}
	newKeyID, err := newUUIDv4()
	if err != nil {
		return nil, fmtErr("new uuid", err)
	}

	// Rolled here, once, and reused for both the new key's own row and
	// (further below) rotation_state -- this is the single source of
	// truth for "when does the key being created right now expire."
	// Computed before the INSERT specifically so it can be stored ON
	// the key row itself, not bolted on separately: this is what lets
	// every key carry its own expiration, matching a real keyset
	// resource shape (see terraform/main.tf's var.keys object type).
	nextExpiresAt := now.Add(randomJitter(minSeconds, maxSeconds))

	_, err = tx.ExecContext(ctx,
		`INSERT INTO keys (key_id, generation, parent_key_id, created_at, expires_at, material, status)
		 VALUES ($1, $2, $3, $4, $5, $6, 'pending')`,
		newKeyID, generation+1, primaryKeyID, now, nextExpiresAt, newMaterial,
	)
	if err != nil {
		return nil, fmtErr("insert new key", err)
	}

	ciphertext, nonce, err := sealTestVector(newMaterial)
	if err != nil {
		return nil, fmtErr("seal test vector", err)
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO test_vectors (key_id, plaintext, ciphertext, nonce) VALUES ($1, $2, $3, $4)`,
		newKeyID, testPlaintext, ciphertext, nonce,
	)
	if err != nil {
		return nil, fmtErr("insert test vector", err)
	}

	// Live liveness test: every key ever created (we never delete
	// material on rotation) must still be able to decrypt its own
	// test vector before the new key is allowed to become primary.
	// Verify-then-promote, never promote-then-verify.
	rows, err := tx.QueryContext(ctx,
		`SELECT k.key_id, k.material, tv.ciphertext, tv.nonce
		 FROM keys k JOIN test_vectors tv ON tv.key_id = k.key_id`,
	)
	if err != nil {
		return nil, fmtErr("query test vectors", err)
	}
	type kv struct {
		keyID                       string
		material, ciphertext, nonce []byte
	}
	var all []kv
	for rows.Next() {
		var v kv
		if err := rows.Scan(&v.keyID, &v.material, &v.ciphertext, &v.nonce); err != nil {
			rows.Close()
			return nil, fmtErr("scan test vector row", err)
		}
		all = append(all, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmtErr("iterate test vectors", err)
	}

	var failedKeyID string
	var testErr error
	for _, v := range all {
		if err := openTestVector(v.material, v.ciphertext, v.nonce); err != nil {
			failedKeyID = v.keyID
			testErr = err
			break
		}
	}

	if testErr != nil {
		tx.Rollback()
		detail := fmt.Sprintf("liveness test failed for key %s: %v", failedKeyID, testErr)
		// Record the failure on its own statement, since the tx above
		// already rolled back (and therefore rolled back the earlier
		// rotation_events insert too).
		conn.ExecContext(ctx,
			`INSERT INTO rotation_events (rotation_id, from_key_id, reason, applied)
			 VALUES ($1, $2, $3, false) ON CONFLICT (rotation_id) DO UPDATE SET reason = $3`,
			rotationID, primaryKeyID, detail,
		)
		return &rotationResult{
			Rotated: false, RotationID: rotationID, FromKeyID: primaryKeyID,
			TestPassed: false, TestDetail: detail,
		}, nil
	}

	if _, err := tx.ExecContext(ctx, `UPDATE keys SET status = 'retired' WHERE key_id = $1`, primaryKeyID); err != nil {
		return nil, fmtErr("retire old primary", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE keys SET status = 'primary', verified_at = $2 WHERE key_id = $1`,
		newKeyID, now,
	); err != nil {
		return nil, fmtErr("promote new key", err)
	}

	// This is the same nextExpiresAt already stored on the new key's
	// own row above -- reusing the value here (not rerolling it) keeps
	// rotation_state.expires_at and keys.expires_at for the new primary
	// in exact agreement, which is what lets Go's own due-check
	// (rotation_state-based) and Terraform's due-check (keys-list-
	// based, via the primary entry's `expiration`) agree on when the
	// next rotation is due.
	if _, err := tx.ExecContext(ctx,
		`UPDATE rotation_state SET last_rotated_at = $1, expires_at = $2 WHERE id = 1`,
		now, nextExpiresAt,
	); err != nil {
		return nil, fmtErr("update rotation_state", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE rotation_events SET applied = true, to_key_id = $2, reason = 'ok' WHERE rotation_id = $1`,
		rotationID, newKeyID,
	); err != nil {
		return nil, fmtErr("finalize rotation_event", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmtErr("commit", err)
	}

	// Infra side-effect happens strictly after commit, so it only ever
	// reflects a rotation that actually and durably happened. Reads
	// the full key list fresh (not the in-memory `all` slice above,
	// which carries raw material for the liveness test and must never
	// reach a file Terraform consumes) so the tfvars file always
	// reflects every key that currently exists, not just the two
	// involved in this rotation.
	keys, err := listKeys(ctx, db)
	if err != nil {
		log.Printf("warning: failed to list keys for terraform vars: %v", err)
	} else if err := writeTerraformVars(sharedDir, keys); err != nil {
		log.Printf("warning: failed to write terraform vars: %v", err)
	}

	return &rotationResult{
		Rotated: true, RotationID: rotationID, FromKeyID: primaryKeyID, ToKeyID: newKeyID,
		TestPassed: true, TestDetail: fmt.Sprintf("all %d active keys decrypted their test vector", len(all)),
		NextExpiresAt: nextExpiresAt,
	}, nil
}

// randomJitter picks a uniformly random duration in [min, max]
// seconds. Go's global math/rand source has been auto-seeded since
// Go 1.20, so no manual seeding is needed here.
func randomJitter(minSeconds, maxSeconds int) time.Duration {
	if maxSeconds <= minSeconds {
		return time.Duration(minSeconds) * time.Second
	}
	span := maxSeconds - minSeconds
	return time.Duration(minSeconds+rand.Intn(span+1)) * time.Second
}

func computeRotationID(primaryKeyID string, expiresAt time.Time) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%s", primaryKeyID, expiresAt.UTC().Format(time.RFC3339Nano))))
	return hex.EncodeToString(sum[:])
}

// tfKeyEntry mirrors one element of terraform/main.tf's var.keys list
// (list(object({label, expiration, length, status, primary}))), and
// matches the shape of a real keyset resource -- each key version
// carries its OWN expiration, the same way a real KMS key version has
// its own creation/rotation schedule rather than the whole keyset
// resource having a single expiry bolted onto it. Field order here
// only matters for readability -- json.Marshal always emits struct
// fields in declaration order, so re-running this against the same
// DB rows always produces byte-identical JSON.
type tfKeyEntry struct {
	Label      string `json:"label"`
	Expiration string `json:"expiration"`
	Length     int    `json:"length"`
	Status     string `json:"status"`
	Primary    bool   `json:"primary"`
}

// tfVarsFile has no top-level expiry field, deliberately -- expiration
// lives per-entry in Keys, not on the file as a whole. Terraform's
// local.is_due (main.tf) derives "is a rotation due" by finding the
// entry with primary = true and reading ITS expiration.
type tfVarsFile struct {
	ID   string       `json:"id"`
	Type string       `json:"type"`
	Keys []tfKeyEntry `json:"keys"`
}

// writeTerraformVars writes the *metadata only* (never raw key
// material) that Terraform needs to react to a rotation: the full
// current keyset, one entry per key that still exists, each carrying
// its own expiration. Every key that has ever existed is included
// (retired keys are never deleted, per the "old keys are retired,
// never deleted" invariant), status is always "ENABLED" for every one
// of them, and exactly one entry has primary = true. It writes to a
// temp file and renames over the target, so a concurrent reader (the
// terraform-runner sidecar) never observes a half-written file, and
// re-running this with the same DB state produces byte-identical
// output.
func writeTerraformVars(sharedDir string, keys []KeyRecord) error {
	if sharedDir == "" {
		return nil
	}
	if err := os.MkdirAll(sharedDir, 0o755); err != nil {
		return err
	}

	out := tfVarsFile{ID: terraformKeysetID, Type: terraformKeyType, Keys: make([]tfKeyEntry, 0, len(keys))}
	for _, k := range keys {
		// listKeys orders by generation ASC, and only 'primary' and
		// 'retired' rows are ever durably committed (a failed
		// liveness test rolls back its whole transaction, including
		// the 'pending' row it inserted) -- so both statuses map to
		// "ENABLED" here; only `primary` distinguishes them.
		out.Keys = append(out.Keys, tfKeyEntry{
			Label:      k.CreatedAt.UTC().Format(time.RFC3339Nano),
			// RFC3339Nano, not RFC3339: Postgres stores expires_at at
			// microsecond precision and tryRotate's due-check
			// (now.Before(expiresAt)) uses that full precision. If this
			// truncated to whole seconds, Terraform's local.is_due could
			// flip true up to ~1s before Go agrees it's due -- an early
			// local-exec call then gets a truthful "not due yet" from Go,
			// which still counts as a successful create (HTTP 200), so
			// the null_resource's triggers never change and nothing ever
			// retries. See README/main.tf's "one honest caveat" comment;
			// this was the actual mechanism behind it, not just the
			// advisory-lock race originally anticipated there.
			Expiration: k.ExpiresAt.UTC().Format(time.RFC3339Nano),
			Length:     terraformKeyBits,
			Status:     "ENABLED",
			Primary:    k.Status == "primary",
		})
	}

	content, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmtErr("marshal terraform vars", err)
	}
	content = append(content, '\n')

	target := filepath.Join(sharedDir, "rotation.auto.tfvars.json")
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, content, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, target)
}
