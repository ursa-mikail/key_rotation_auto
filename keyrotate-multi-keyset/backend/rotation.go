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
// on any given tick, so exactly one actor evaluates/performs rotation
// for ALL due keysets that tick -- the lock is global, not per-keyset,
// which is what makes "process every due keyset in one pass" safe
// without needing N separate locks.
const rotationLockKey = 918_273_645

// terraformKeyType and terraformKeyBits are shared with
// terraform/main.tf's matching variable defaults -- every keyset uses
// the same fixed algorithm in this demo.
const (
	terraformKeyType = "AES-256-GCM"
	terraformKeyBits = 256 // matches the fixed 32-byte (256-bit) AES-256 material in crypto.go
)

type rotationResult struct {
	KeysetID      string
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
// for the UI's live countdowns -- it does NOT decide whether to
// rotate. That decision only ever happens inside tryRotateAll, called
// from handleRotateIfDue in response to an inbound HTTP call (in the
// reference deployment: Terraform's local-exec provisioner, firing
// whenever ANY of the N keysets has a due primary key).
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

// tryRotateAll is the entire inbound side of "Terraform triggers Go"
// in this variant, generalized across N keysets: an external caller
// POSTs in whenever IT believes AT LEAST ONE keyset is due (based on
// the tfvars/output json it read). That belief is never trusted on
// its own -- this function re-derives, from Postgres's own clock,
// exactly which rotating keysets are ACTUALLY due right now, and
// rotates every one of them in this single call, each in its own
// transaction. Calling it early, late, or twice in a row is always
// safe: due-ness is re-checked per keyset immediately before each one
// rotates.
func tryRotateAll(ctx context.Context, db *sql.DB, sharedDir string) ([]rotationResult, error) {
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
		return []rotationResult{{Skipped: "lock held by another replica"}}, nil
	}
	defer conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", rotationLockKey)

	var paused bool
	if err := conn.QueryRowContext(ctx, `SELECT paused FROM system_state WHERE id = 1`).Scan(&paused); err != nil {
		return nil, fmtErr("load system_state", err)
	}
	if paused {
		return []rotationResult{{Skipped: "auto-rotation paused (kill switch engaged)"}}, nil
	}

	// Find every ROTATING keyset whose expires_at has passed, using
	// the DB's own clock (not the caller's belief). Static keysets
	// have no rotation_state row at all, so they can never appear
	// here regardless of what any tfvars snapshot claims.
	rows, err := conn.QueryContext(ctx,
		`SELECT keyset_id FROM rotation_state WHERE expires_at <= now() ORDER BY keyset_id`,
	)
	if err != nil {
		return nil, fmtErr("query due keysets", err)
	}
	var dueIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmtErr("scan due keyset id", err)
		}
		dueIDs = append(dueIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmtErr("iterate due keysets", err)
	}

	if len(dueIDs) == 0 {
		return []rotationResult{{Skipped: "no keyset due yet"}}, nil
	}

	results := make([]rotationResult, 0, len(dueIDs))
	anyRotated := false
	for _, keysetID := range dueIDs {
		res, err := rotateOneKeyset(ctx, db, keysetID)
		if err != nil {
			log.Printf("rotation error for keyset %s: %v", keysetID, err)
			results = append(results, rotationResult{KeysetID: keysetID, Skipped: fmt.Sprintf("internal error: %v", err)})
			continue
		}
		results = append(results, *res)
		if res.Rotated {
			anyRotated = true
		}
	}

	// Infra side-effect happens strictly after every rotation in this
	// batch has committed (or not), and only once for the whole
	// batch -- reads the full current state fresh from Postgres so
	// the tfvars file always reflects every keyset/key that currently
	// exists, not just the ones touched this tick.
	if anyRotated {
		keysets, err := listKeysetsWithKeys(ctx, db)
		if err != nil {
			log.Printf("warning: failed to list keysets for terraform vars: %v", err)
		} else if err := writeTerraformVars(sharedDir, keysets); err != nil {
			log.Printf("warning: failed to write terraform vars: %v", err)
		}
	}

	return results, nil
}

// rotateOneKeyset performs one keyset's verify-then-promote rotation,
// scoped entirely to that keyset_id -- same shape as the base
// version's single-keyset tryRotate, just parameterized. The due-check
// is re-verified here, inside a FOR UPDATE transaction on that
// keyset's own rotation_state row, even though the caller
// (tryRotateAll) already filtered by the same condition a moment ago
// -- so a manual/duplicate call directly against this keyset is still
// always safe.
func rotateOneKeyset(ctx context.Context, db *sql.DB, keysetID string) (*rotationResult, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmtErr("begin tx", err)
	}
	defer tx.Rollback()

	var now, expiresAt time.Time
	var minSeconds, maxSeconds int
	err = tx.QueryRowContext(ctx,
		`SELECT now(), expires_at, min_seconds, max_seconds
		 FROM rotation_state WHERE keyset_id = $1 FOR UPDATE`,
		keysetID,
	).Scan(&now, &expiresAt, &minSeconds, &maxSeconds)
	if err == sql.ErrNoRows {
		return &rotationResult{KeysetID: keysetID, Skipped: "not a rotating keyset (no rotation_state row)"}, nil
	}
	if err != nil {
		return nil, fmtErr("load rotation_state", err)
	}

	if now.Before(expiresAt) {
		return &rotationResult{KeysetID: keysetID, Skipped: fmt.Sprintf("not due yet (expires at %s)", expiresAt.Format(time.RFC3339Nano))}, nil
	}

	var primaryKeyID string
	var generation int
	var primaryMaterial []byte
	err = tx.QueryRowContext(ctx,
		`SELECT key_id, generation, material FROM keys WHERE keyset_id = $1 AND status = 'primary'`,
		keysetID,
	).Scan(&primaryKeyID, &generation, &primaryMaterial)
	if err == sql.ErrNoRows {
		return &rotationResult{KeysetID: keysetID, Skipped: "no primary key (unexpected post-genesis)"}, nil
	}
	if err != nil {
		return nil, fmtErr("load primary key", err)
	}

	// Deterministic rotation id: same keyset + same primary key + same
	// expiry instant always hashes to the same id. A duplicate call
	// before that expiry changes collides on rotation_events'
	// primary key and is rejected by the DB itself.
	rotationID := computeRotationID(keysetID, primaryKeyID, expiresAt)

	res, err := tx.ExecContext(ctx,
		`INSERT INTO rotation_events (rotation_id, keyset_id, from_key_id, reason) VALUES ($1, $2, $3, 'in-progress')
		 ON CONFLICT (rotation_id) DO NOTHING`,
		rotationID, keysetID, primaryKeyID,
	)
	if err != nil {
		return nil, fmtErr("insert rotation_event", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return &rotationResult{KeysetID: keysetID, Skipped: "this expiry instant was already handled", RotationID: rotationID}, nil
	}

	newMaterial, err := deriveNextKey(primaryMaterial, now)
	if err != nil {
		return nil, fmtErr("derive next key", err)
	}
	newKeyID, err := newUUIDv4()
	if err != nil {
		return nil, fmtErr("new uuid", err)
	}

	nextExpiresAt := now.Add(randomJitter(minSeconds, maxSeconds))

	_, err = tx.ExecContext(ctx,
		`INSERT INTO keys (key_id, keyset_id, generation, parent_key_id, created_at, expires_at, material, status)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, 'pending')`,
		newKeyID, keysetID, generation+1, primaryKeyID, now, nextExpiresAt, newMaterial,
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

	// Live liveness test, scoped to THIS keyset only: every key that
	// has ever existed within this same keyset (never across keysets
	// -- each is an independent lineage) must still be able to
	// decrypt its own test vector before the new key is allowed to
	// become primary. Verify-then-promote, never promote-then-verify.
	rows, err := tx.QueryContext(ctx,
		`SELECT k.key_id, k.material, tv.ciphertext, tv.nonce
		 FROM keys k JOIN test_vectors tv ON tv.key_id = k.key_id
		 WHERE k.keyset_id = $1`,
		keysetID,
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
		conn, connErr := db.Conn(ctx)
		if connErr == nil {
			conn.ExecContext(ctx,
				`INSERT INTO rotation_events (rotation_id, keyset_id, from_key_id, reason, applied)
				 VALUES ($1, $2, $3, $4, false) ON CONFLICT (rotation_id) DO UPDATE SET reason = $4`,
				rotationID, keysetID, primaryKeyID, detail,
			)
			conn.Close()
		}
		return &rotationResult{
			KeysetID: keysetID, Rotated: false, RotationID: rotationID, FromKeyID: primaryKeyID,
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

	if _, err := tx.ExecContext(ctx,
		`UPDATE rotation_state SET last_rotated_at = $1, expires_at = $2 WHERE keyset_id = $3`,
		now, nextExpiresAt, keysetID,
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

	return &rotationResult{
		KeysetID: keysetID, Rotated: true, RotationID: rotationID, FromKeyID: primaryKeyID, ToKeyID: newKeyID,
		TestPassed: true, TestDetail: fmt.Sprintf("all %d keys in this keyset decrypted their test vector", len(all)),
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

func computeRotationID(keysetID, primaryKeyID string, expiresAt time.Time) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%s", keysetID, primaryKeyID, expiresAt.UTC().Format(time.RFC3339Nano))))
	return hex.EncodeToString(sum[:])
}

// tfKeyEntry mirrors one element of a keyset's `keys` list in
// terraform/main.tf's var.keysets -- each key version carries its OWN
// expiration, matching how a real keyset resource works.
type tfKeyEntry struct {
	Label      string `json:"label"`
	Expiration string `json:"expiration"`
	Length     int    `json:"length"`
	Status     string `json:"status"`
	Primary    bool   `json:"primary"`
}

// tfKeysetEntry mirrors one element of var.keysets -- one per keyset,
// carrying its own full key list plus the rotating/revoked flags so
// the HCL/JSON views can show WHY a given keyset behaves the way it
// does (selected for rotation? flagged for emergency renewal at
// genesis?) without having to cross-reference Postgres.
type tfKeysetEntry struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Rotating bool         `json:"rotating"`
	Revoked  bool         `json:"revoked"`
	Keys     []tfKeyEntry `json:"keys"`
}

// tfVarsFile is the whole exchange file: N keysets, each with its own
// full key list. There is deliberately no single top-level "the
// primary key" or "the expiry" field anywhere in this file --
// everything is per-keyset, per-key, matching how N independent real
// keyset resources would actually look.
type tfVarsFile struct {
	Keysets []tfKeysetEntry `json:"keysets"`
}

// keysetWithKeys bundles a KeysetRecord with every key that currently
// exists for it (all generations, oldest first) -- the shape
// writeTerraformVars and renderKeysetResourceHCL both consume.
type keysetWithKeys struct {
	KeysetRecord
	Keys []KeyRecord
}

// writeTerraformVars writes the *metadata only* (never raw key
// material) that Terraform needs to react to rotations across every
// keyset at once. Every keyset that exists gets an entry; within each,
// every key that has ever existed is included (retired keys are never
// deleted). Writes to a temp file and renames over the target, so a
// concurrent reader (the terraform-runner sidecar) never observes a
// half-written file.
func writeTerraformVars(sharedDir string, keysets []keysetWithKeys) error {
	if sharedDir == "" {
		return nil
	}
	if err := os.MkdirAll(sharedDir, 0o755); err != nil {
		return err
	}

	out := tfVarsFile{Keysets: make([]tfKeysetEntry, 0, len(keysets))}
	for _, ks := range keysets {
		entry := tfKeysetEntry{
			ID: ks.KeysetID, Type: terraformKeyType, Rotating: ks.Rotating, Revoked: ks.Revoked,
			Keys: make([]tfKeyEntry, 0, len(ks.Keys)),
		}
		for _, k := range ks.Keys {
			entry.Keys = append(entry.Keys, tfKeyEntry{
				Label:      k.CreatedAt.UTC().Format(time.RFC3339Nano),
				Expiration: k.ExpiresAt.UTC().Format(time.RFC3339Nano),
				Length:     terraformKeyBits,
				Status:     "ENABLED",
				Primary:    k.Status == "primary",
			})
		}
		out.Keysets = append(out.Keysets, entry)
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
