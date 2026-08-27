package main

import (
	"context"
	crand "crypto/rand"
	"database/sql"
	"fmt"
	"log"
	"math/rand"
	"time"
)

// genesisBatchResult summarizes one batch-genesis call for the API
// response and the audit log.
type genesisBatchResult struct {
	Status        string `json:"status"` // created | already-initialized
	KeysetCount   int    `json:"keysetCount"`
	RotatingCount int    `json:"rotatingCount"`
	RevokedCount  int    `json:"revokedCount"`
	StaticCount   int    `json:"staticCount"`
}

// runGenesisBatch is the entire "N=50 keysets, M=20 rotating, L=random(0..M)
// revoked" event. It is a one-time, idempotent, all-or-nothing batch:
// if system_state.initialized is already true, it's a no-op that just
// reports what already exists. Otherwise it creates every keyset, its
// genesis key, and (for rotating keysets) its rotation_state row, all
// inside one transaction.
func runGenesisBatch(ctx context.Context, db *sql.DB, sharedDir string, n, m, minSeconds, maxSeconds int) (*genesisBatchResult, error) {
	if n < 1 {
		return nil, fmt.Errorf("keysetCount must be >= 1")
	}
	if m < 0 || m > n {
		return nil, fmt.Errorf("rotatingCount must be between 0 and keysetCount")
	}

	var already bool
	var existingN, existingM, existingL int
	err := db.QueryRowContext(ctx,
		`SELECT initialized, keyset_count, rotating_count, revoked_count FROM system_state WHERE id = 1`,
	).Scan(&already, &existingN, &existingM, &existingL)
	if err != nil {
		return nil, fmtErr("read system_state", err)
	}
	if already {
		return &genesisBatchResult{
			Status: "already-initialized", KeysetCount: existingN, RotatingCount: existingM,
			RevokedCount: existingL, StaticCount: existingN - existingM,
		}, nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmtErr("begin tx", err)
	}
	defer tx.Rollback()

	// Randomly select which M of the N keysets rotate: shuffle a
	// 0..N-1 index list and take the first M as the rotating set.
	order := rand.Perm(n)
	rotatingSet := make(map[int]bool, m)
	for _, idx := range order[:m] {
		rotatingSet[idx] = true
	}

	// L = random(0, M) inclusive -- how many of the M rotating
	// keysets are ALSO flagged revoked (need immediate/emergency
	// renewal rather than waiting for their normal random expiry).
	// rand.Intn(m+1) is safe even when m == 0 (yields exactly 0).
	l := rand.Intn(m + 1)
	revokedSet := make(map[int]bool, l)
	for _, idx := range order[:l] {
		revokedSet[idx] = true
	}

	now := time.Now().UTC()

	for i := 0; i < n; i++ {
		keysetID := fmt.Sprintf("unit_%02d_keyset", i+1)
		rotating := rotatingSet[i]
		revoked := rotating && revokedSet[i]

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO keysets (keyset_id, idx, rotating, revoked, created_at) VALUES ($1, $2, $3, $4, $5)`,
			keysetID, i+1, rotating, revoked, now,
		); err != nil {
			return nil, fmtErr(fmt.Sprintf("insert keyset %s", keysetID), err)
		}

		keyID, err := newUUIDv4()
		if err != nil {
			return nil, fmtErr("new uuid", err)
		}
		material := make([]byte, 32) // AES-256
		if _, err := crand.Read(material); err != nil {
			return nil, fmtErr("generate key material", err)
		}

		var expiresAt time.Time
		switch {
		case revoked:
			// Already due: set a couple of seconds in the past so
			// there's no ambiguity against clock precision between Go
			// and Postgres -- this keyset's very first rotation fires
			// on the loop's next pass, modeling "had to be renewed
			// immediately."
			expiresAt = now.Add(-2 * time.Second)
		case rotating:
			expiresAt = now.Add(randomJitter(minSeconds, maxSeconds))
		default:
			// Static: not scheduled to rotate at all. Far-future
			// expiration so it is, in effect, never due -- and no
			// rotation_state row is created for it either (belt and
			// suspenders: even a manual due-check can't fire on it).
			expiresAt = now.AddDate(100, 0, 0)
		}

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO keys (key_id, keyset_id, generation, parent_key_id, created_at, expires_at, material, status)
			 VALUES ($1, $2, 0, NULL, $3, $4, $5, 'primary')`,
			keyID, keysetID, now, expiresAt, material,
		); err != nil {
			return nil, fmtErr(fmt.Sprintf("insert genesis key for %s", keysetID), err)
		}

		ciphertext, nonce, err := sealTestVector(material)
		if err != nil {
			return nil, fmtErr("seal test vector", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO test_vectors (key_id, plaintext, ciphertext, nonce) VALUES ($1, $2, $3, $4)`,
			keyID, testPlaintext, ciphertext, nonce,
		); err != nil {
			return nil, fmtErr("insert test vector", err)
		}

		if rotating {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO rotation_state (keyset_id, last_rotated_at, expires_at, min_seconds, max_seconds)
				 VALUES ($1, $2, $3, $4, $5)`,
				keysetID, now, expiresAt, minSeconds, maxSeconds,
			); err != nil {
				return nil, fmtErr(fmt.Sprintf("insert rotation_state for %s", keysetID), err)
			}
		}
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE system_state
		 SET initialized = true, keyset_count = $1, rotating_count = $2, revoked_count = $3
		 WHERE id = 1`,
		n, m, l,
	); err != nil {
		return nil, fmtErr("update system_state", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmtErr("commit", err)
	}

	// Same infra side-effect tryRotateAll performs after a rotation
	// commits, done here too so the tf-config/tfvars tabs show the
	// full N-keyset set immediately after genesis instead of sitting
	// on "not written yet."
	if keysets, err := listKeysetsWithKeys(ctx, db); err != nil {
		log.Printf("warning: failed to list keysets for terraform vars after genesis: %v", err)
	} else if err := writeTerraformVars(sharedDir, keysets); err != nil {
		log.Printf("warning: failed to write terraform vars after genesis: %v", err)
	}

	return &genesisBatchResult{
		Status: "created", KeysetCount: n, RotatingCount: m, RevokedCount: l, StaticCount: n - m,
	}, nil
}
