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

// rotationLockKey is the advisory-lock key for the TIMER loop only
// (tryRotateAll). It ensures only one backend replica evaluates "which
// keysets are due" per tick. The revoke path (revoke.go) does NOT use
// this lock -- it doesn't need to, because both paths ultimately
// serialize on the SAME per-keyset `rotation_state ... FOR UPDATE` row
// lock inside rotateOneKeyset/runHaltKeyset, which is what actually
// prevents a timer rotation and a revoke from stepping on each other
// for the same keyset.
const rotationLockKey = 918_273_645

const (
	terraformKeyType = "AES-256-GCM"
	terraformKeyBits = 256
)

type rotationResult struct {
	KeysetID      string
	Rotated       bool
	Skipped       string
	RotationID    string
	FromKeyID     string
	ToKeyID       string
	TestPassed    bool
	TestDetail    string
	NextExpiresAt time.Time
}

// runStatusBroadcastLoop only reads and broadcasts state for the UI's
// live countdowns -- it never decides to rotate or revoke.
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

// tryRotateAll is the timer-driven path: an external caller (Terraform's
// local-exec provisioner) POSTs in whenever it believes at least one
// keyset is due. This function re-derives, from Postgres's own clock,
// exactly which keysets are ACTUALLY due, and rotates every one of
// them, each in its own transaction. Terminated keysets can never
// appear here -- they have no rotation_state row at all (see
// runHaltKeyset in revoke.go).
func tryRotateAll(ctx context.Context, db *sql.DB, sharedDir string) ([]rotationResult, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmtErr("acquire conn", err)
	}
	defer conn.Close()

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
	anyChanged := false
	for _, keysetID := range dueIDs {
		res, err := rotateOneKeyset(ctx, db, keysetID, "timer", false)
		if err != nil {
			log.Printf("rotation error for keyset %s: %v", keysetID, err)
			results = append(results, rotationResult{KeysetID: keysetID, Skipped: fmt.Sprintf("internal error: %v", err)})
			continue
		}
		results = append(results, *res)
		if res.Rotated {
			anyChanged = true
		}
	}

	if anyChanged {
		if err := refreshTerraformVars(ctx, db, sharedDir); err != nil {
			log.Printf("warning: failed to write terraform vars: %v", err)
		}
	}

	return results, nil
}

// refreshTerraformVars re-reads the full current DB state and writes
// it out fresh. Shared by the timer path (tryRotateAll), the revoke
// path (revoke.go), and genesis -- every state-changing event in this
// service ends with a call to this, which is what keeps the tfvars
// file "real time."
func refreshTerraformVars(ctx context.Context, db *sql.DB, sharedDir string) error {
	keysets, err := listKeysetsWithKeys(ctx, db)
	if err != nil {
		return err
	}
	return writeTerraformVars(sharedDir, keysets)
}

// rotateOneKeyset performs one keyset's verify-then-promote rotation.
// It's shared by BOTH triggers:
//   - trigger="timer", force=false : normal, on-schedule rotation. The
//     due-check (now >= expires_at) is enforced.
//   - trigger="revoke", force=true : emergency rotation because a
//     revoke landed while REVOKE_AUTO_ROTATE is on. The due-check is
//     skipped entirely -- it rotates right now regardless of its timer.
//
// Either way, the FIRST thing this function does is lock this keyset's
// rotation_state row with FOR UPDATE. That single lock is what makes a
// timer-rotation and a revoke racing for the same keyset safe: whichever
// call gets the row first runs to completion (commit or rollback)
// before the second one even reads the keyset's state, so the second
// call always acts on the true, post-first-call reality -- never on a
// stale read. If the row is gone entirely (sql.ErrNoRows), this keyset
// was already terminated by a prior halt-mode revoke -- there's nothing
// left to rotate.
func rotateOneKeyset(ctx context.Context, db *sql.DB, keysetID, trigger string, force bool) (*rotationResult, error) {
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
		return &rotationResult{KeysetID: keysetID, Skipped: "keyset already terminated (or unknown)"}, nil
	}
	if err != nil {
		return nil, fmtErr("load rotation_state", err)
	}

	if !force && now.Before(expiresAt) {
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

	// Timer rotations use a deterministic id (same keyset + same
	// primary key + same expiry instant always hash the same), so a
	// duplicate call before that window changes is a harmless
	// primary-key conflict, not a second rotation. Revoke-triggered
	// rotations are one-shot by construction (guarded entirely by the
	// row lock above, not by a retry-safety-net caller), so a fresh
	// UUID is simplest there.
	var rotationID string
	if trigger == "timer" {
		rotationID = computeRotationID(keysetID, primaryKeyID, expiresAt)
	} else {
		id, err := newUUIDv4()
		if err != nil {
			return nil, fmtErr("new rotation id", err)
		}
		rotationID = id
	}

	res, err := tx.ExecContext(ctx,
		`INSERT INTO rotation_events (rotation_id, keyset_id, trigger, from_key_id, outcome, reason)
		 VALUES ($1, $2, $3, $4, 'in_progress', 'in-progress')
		 ON CONFLICT (rotation_id) DO NOTHING`,
		rotationID, keysetID, trigger, primaryKeyID,
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
				`UPDATE rotation_events SET outcome = 'failed', reason = $2 WHERE rotation_id = $1`,
				rotationID, detail,
			)
			conn.Close()
		}
		return &rotationResult{
			KeysetID: keysetID, Rotated: false, RotationID: rotationID, FromKeyID: primaryKeyID,
			TestPassed: false, TestDetail: detail,
		}, nil
	}

	// A revoke-triggered rotation means THIS specific old key was the
	// one that got revoked -- mark it here, on the key row itself, so
	// that fact survives independent of whatever the keyset does
	// afterward (keeps rotating normally in auto-rotate mode). A
	// timer-triggered rotation retires the key for a completely
	// ordinary reason -- its scheduled interval simply elapsed -- so
	// revoked_at stays NULL for that case.
	if trigger == "revoke" {
		if _, err := tx.ExecContext(ctx,
			`UPDATE keys SET status = 'retired', revoked_at = $2 WHERE key_id = $1`,
			primaryKeyID, now,
		); err != nil {
			return nil, fmtErr("retire and mark revoked old primary", err)
		}
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE keys SET status = 'retired' WHERE key_id = $1`, primaryKeyID); err != nil {
			return nil, fmtErr("retire old primary", err)
		}
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

	outcome := "rotated"
	reason := "ok"
	if trigger == "revoke" {
		outcome = "revoked_rotated"
		reason = "revoked; emergency-rotated (REVOKE_AUTO_ROTATE=true) and resumed normal cycling"
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE rotation_events SET outcome = $2, to_key_id = $3, reason = $4 WHERE rotation_id = $1`,
		rotationID, outcome, newKeyID, reason,
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

type tfKeyEntry struct {
	Label      string `json:"label"`
	Expiration string `json:"expiration"`
	Length     int    `json:"length"`
	Status     string `json:"status"`
	Primary    bool   `json:"primary"`
}

// tfKeysetEntry mirrors one element of var.keysets. `Terminated` and
// `LastOutcome`/`LastTrigger`/`LastEventAt` are what let a reader of
// the raw tfvars/output json (or the live-rendered HCL) see "revoked,
// then rotated" vs "revoked, then terminated" vs an ordinary scheduled
// rotation WITHOUT cross-referencing rotation_events separately --
// they're denormalized onto every snapshot, in real time.
type tfKeysetEntry struct {
	ID          string       `json:"id"`
	Type        string       `json:"type"`
	Terminated  bool         `json:"terminated"`
	LastOutcome string       `json:"lastOutcome,omitempty"`
	LastTrigger string       `json:"lastTrigger,omitempty"`
	LastEventAt string       `json:"lastEventAt,omitempty"`
	Keys        []tfKeyEntry `json:"keys"`
}

type tfVarsFile struct {
	Keysets []tfKeysetEntry `json:"keysets"`
}

// keysetWithKeys bundles a KeysetRecord + its full key list + its most
// recent rotation_events row (see listKeysetsWithKeys in status.go) --
// everything writeTerraformVars and renderKeysetResourceHCL need to
// show the "revoke and rotate / revoke and terminate" status live.
type keysetWithKeys struct {
	KeysetRecord
	Keys             []KeyRecord
	LastEventOutcome string
	LastEventTrigger string
	LastEventAt      *time.Time
}

// keyStatusLabel is the KMS-style enabled/disabled label for one key
// entry in the tfvars/output json and the live HCL. Driven purely by
// whether THIS key was ever revoked (keys.revoked_at, set in
// rotateOneKeyset for the auto-rotate case and in runHaltKeyset for
// the halt case) -- NOT by whether its keyset is currently terminated.
// That distinction matters: in auto-rotate mode a keyset can have a
// revoked (DISABLED) retired key sitting right next to a perfectly
// healthy, un-revoked (ENABLED) current primary key, and the keyset
// itself is never terminated at all.
func keyStatusLabel(revokedAt *time.Time) string {
	if revokedAt != nil {
		return "DISABLED"
	}
	return "ENABLED"
}

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
			ID: ks.KeysetID, Type: terraformKeyType, Terminated: ks.Terminated,
			LastOutcome: ks.LastEventOutcome, LastTrigger: ks.LastEventTrigger,
			Keys: make([]tfKeyEntry, 0, len(ks.Keys)),
		}
		if ks.LastEventAt != nil {
			entry.LastEventAt = ks.LastEventAt.UTC().Format(time.RFC3339Nano)
		}
		for _, k := range ks.Keys {
			entry.Keys = append(entry.Keys, tfKeyEntry{
				Label:      k.CreatedAt.UTC().Format(time.RFC3339Nano),
				Expiration: k.ExpiresAt.UTC().Format(time.RFC3339Nano),
				Length:     terraformKeyBits,
				Status:     keyStatusLabel(k.RevokedAt),
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
