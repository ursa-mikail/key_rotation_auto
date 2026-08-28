package main

import (
	"context"
	crand "crypto/rand"
	"database/sql"
	"fmt"
	"log"
	"time"
)

// genesisBatchResult summarizes one batch-genesis call.
type genesisBatchResult struct {
	Status      string `json:"status"` // created | already-initialized
	KeysetCount int    `json:"keysetCount"`
}

// runGenesisBatch creates N keysets, each with its own genesis key and
// its own rotation_state row on a random 3-20s (configurable) jitter --
// every keyset rotates in this variant, there is no static tier. It's
// a one-time, idempotent, all-or-nothing batch: once
// system_state.initialized is true, this is a no-op that just reports
// what already exists.
func runGenesisBatch(ctx context.Context, db *sql.DB, sharedDir string, n, minSeconds, maxSeconds int) (*genesisBatchResult, error) {
	if n < 1 {
		return nil, fmt.Errorf("keysetCount must be >= 1")
	}

	var already bool
	var existingN int
	err := db.QueryRowContext(ctx,
		`SELECT initialized, keyset_count FROM system_state WHERE id = 1`,
	).Scan(&already, &existingN)
	if err != nil {
		return nil, fmtErr("read system_state", err)
	}
	if already {
		return &genesisBatchResult{Status: "already-initialized", KeysetCount: existingN}, nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmtErr("begin tx", err)
	}
	defer tx.Rollback()

	now := time.Now().UTC()

	for i := 0; i < n; i++ {
		keysetID := fmt.Sprintf("unit_%02d_keyset", i+1)

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO keysets (keyset_id, idx, created_at) VALUES ($1, $2, $3)`,
			keysetID, i+1, now,
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

		expiresAt := now.Add(randomJitter(minSeconds, maxSeconds))

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

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO rotation_state (keyset_id, last_rotated_at, expires_at, min_seconds, max_seconds)
			 VALUES ($1, $2, $3, $4, $5)`,
			keysetID, now, expiresAt, minSeconds, maxSeconds,
		); err != nil {
			return nil, fmtErr(fmt.Sprintf("insert rotation_state for %s", keysetID), err)
		}
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE system_state SET initialized = true, keyset_count = $1 WHERE id = 1`, n,
	); err != nil {
		return nil, fmtErr("update system_state", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmtErr("commit", err)
	}

	if keysets, err := listKeysetsWithKeys(ctx, db); err != nil {
		log.Printf("warning: failed to list keysets for terraform vars after genesis: %v", err)
	} else if err := writeTerraformVars(sharedDir, keysets); err != nil {
		log.Printf("warning: failed to write terraform vars after genesis: %v", err)
	}

	return &genesisBatchResult{Status: "created", KeysetCount: n}, nil
}
