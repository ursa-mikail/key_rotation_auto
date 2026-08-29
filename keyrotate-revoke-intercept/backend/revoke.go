package main

import (
	"context"
	"database/sql"
	"log"
	"math/rand"
	"time"
)

// revokeResult summarizes what happened to one /api/revoke (or
// interceptor-fired) call.
type revokeResult struct {
	KeysetID   string `json:"keysetId"`
	Mode       string `json:"mode"` // "auto-rotate" | "halt"
	Rotated    bool   `json:"rotated"`
	Terminated bool   `json:"terminated"`
	Skipped    string `json:"skipped,omitempty"`
	FromKeyID  string `json:"fromKeyId,omitempty"`
	ToKeyID    string `json:"toKeyId,omitempty"`
}

// runRevokeInterceptorLoop is the "randomly intercept some [keysets]
// to revoke" background process: on a fixed tick, with a configurable
// probability, it picks ONE uniformly random still-active keyset and
// revokes it -- completely independent of, and able to land at any
// instant relative to, that same keyset's own timer-driven rotation.
// This is exactly what creates the collision case: the revoke path
// (via rotateOneKeyset/runHaltKeyset) and the timer loop's
// rotateOneKeyset call both lock the same rotation_state row, so
// Postgres itself decides who goes first -- nothing here needs its own
// coordination logic.
func runRevokeInterceptorLoop(ctx context.Context, db *sql.DB, sharedDir string, interval time.Duration, probability float64) {
	if probability <= 0 {
		log.Printf("revoke interceptor disabled (REVOKE_INTERCEPT_PROBABILITY <= 0)")
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			var paused bool
			if err := db.QueryRowContext(ctx, `SELECT paused FROM system_state WHERE id = 1`).Scan(&paused); err != nil {
				log.Printf("revoke interceptor: failed to check pause state: %v", err)
				continue
			}
			// The kill switch stops the chaos monkey too -- "stop
			// everything" should mean everything, not just the timer.
			if paused {
				continue
			}
			if rand.Float64() > probability {
				continue
			}

			keysetID, ok, err := pickRandomActiveKeyset(ctx, db)
			if err != nil {
				log.Printf("revoke interceptor: failed to pick a keyset: %v", err)
				continue
			}
			if !ok {
				continue // every keyset is already terminated
			}

			res, err := processRevoke(ctx, db, keysetID)
			if err != nil {
				log.Printf("revoke interceptor: error processing keyset %s: %v", keysetID, err)
				continue
			}
			log.Printf("revoke interceptor: keyset=%s mode=%s rotated=%v terminated=%v %s",
				res.KeysetID, res.Mode, res.Rotated, res.Terminated, res.Skipped)

			if res.Rotated || res.Terminated {
				if err := refreshTerraformVars(ctx, db, sharedDir); err != nil {
					log.Printf("warning: failed to write terraform vars after revoke: %v", err)
				}
			}
		}
	}
}

// pickRandomActiveKeyset draws uniformly from every keyset that still
// has a rotation_state row -- i.e. every keyset that has NOT already
// been terminated. Letting Postgres do the random pick (`ORDER BY
// random() LIMIT 1`) is fine at this scale (dozens of rows).
func pickRandomActiveKeyset(ctx context.Context, db *sql.DB) (string, bool, error) {
	var id string
	err := db.QueryRowContext(ctx, `SELECT keyset_id FROM rotation_state ORDER BY random() LIMIT 1`).Scan(&id)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmtErr("pick random active keyset", err)
	}
	return id, true, nil
}

// processRevoke is the single entry point both the interceptor loop
// and the manual POST /api/revoke handler call. It reads the CURRENT
// revoke mode (system_state.revoke_auto_rotate) at the moment of
// processing -- not at genesis, not at the moment the interceptor
// tick fired -- so flipping the toggle mid-flight always applies to
// whatever revoke is processed next.
func processRevoke(ctx context.Context, db *sql.DB, keysetID string) (*revokeResult, error) {
	var autoRotate bool
	if err := db.QueryRowContext(ctx, `SELECT revoke_auto_rotate FROM system_state WHERE id = 1`).Scan(&autoRotate); err != nil {
		return nil, fmtErr("load revoke_auto_rotate", err)
	}

	if autoRotate {
		// force=true: skip the due-check entirely and rotate right
		// now, regardless of this keyset's own timer. The row lock
		// inside rotateOneKeyset is what actually resolves any
		// collision with a simultaneous timer-driven rotation for the
		// same keyset -- see the big comment on rotateOneKeyset.
		res, err := rotateOneKeyset(ctx, db, keysetID, "revoke", true)
		if err != nil {
			return nil, err
		}
		return &revokeResult{
			KeysetID: keysetID, Mode: "auto-rotate", Rotated: res.Rotated, Skipped: res.Skipped,
			FromKeyID: res.FromKeyID, ToKeyID: res.ToKeyID,
		}, nil
	}

	return runHaltKeyset(ctx, db, keysetID)
}

// runHaltKeyset is the halt-mode revoke path: mark the keyset
// permanently terminated and delete its rotation_state row, so it
// drops out of every future due-check AND every future random revoke
// draw. Like rotateOneKeyset, the FIRST thing this does is lock the
// same rotation_state row with FOR UPDATE -- so if a timer rotation is
// already in flight for this keyset, this call simply waits for it to
// finish, then halts the keyset immediately after that rotation
// committed ("after you rotate, revoke anyway"). If the row is already
// gone (a previous revoke got here first), this is a harmless no-op.
func runHaltKeyset(ctx context.Context, db *sql.DB, keysetID string) (*revokeResult, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmtErr("begin tx", err)
	}
	defer tx.Rollback()

	var lockedOK bool
	err = tx.QueryRowContext(ctx,
		`SELECT true FROM rotation_state WHERE keyset_id = $1 FOR UPDATE`, keysetID,
	).Scan(&lockedOK)
	if err == sql.ErrNoRows {
		return &revokeResult{KeysetID: keysetID, Mode: "halt", Skipped: "keyset already terminated (or unknown)"}, nil
	}
	if err != nil {
		return nil, fmtErr("lock rotation_state", err)
	}

	var primaryKeyID string
	if err := tx.QueryRowContext(ctx,
		`SELECT key_id FROM keys WHERE keyset_id = $1 AND status = 'primary'`, keysetID,
	).Scan(&primaryKeyID); err != nil {
		return nil, fmtErr("load primary key", err)
	}

	// This key stays `status = 'primary'` forever (no replacement is
	// ever created in halt mode), but it WAS revoked -- mark that on
	// the row itself so keyStatusLabel() can flip its tfvars/HCL
	// status to DISABLED, same as the auto-rotate case does for the
	// key it replaces.
	if _, err := tx.ExecContext(ctx, `UPDATE keys SET revoked_at = now() WHERE key_id = $1`, primaryKeyID); err != nil {
		return nil, fmtErr("mark primary key revoked", err)
	}

	rotationID, err := newUUIDv4()
	if err != nil {
		return nil, fmtErr("new rotation id", err)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO rotation_events (rotation_id, keyset_id, trigger, from_key_id, to_key_id, outcome, reason)
		 VALUES ($1, $2, 'revoke', $3, NULL, 'revoked_terminated', $4)`,
		rotationID, keysetID, primaryKeyID,
		"revoked; halted (REVOKE_AUTO_ROTATE=false) -- no further rotation for this keyset",
	); err != nil {
		return nil, fmtErr("insert rotation_event", err)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE keysets SET terminated = true, terminated_at = now() WHERE keyset_id = $1`, keysetID,
	); err != nil {
		return nil, fmtErr("mark keyset terminated", err)
	}

	// Deleting this row is what makes termination "stick" everywhere
	// else in the system for free: the timer loop's due-query, and
	// pickRandomActiveKeyset's random draw, both source directly from
	// rotation_state, so a keyset with no row here is simply invisible
	// to every future rotation-related decision.
	if _, err := tx.ExecContext(ctx, `DELETE FROM rotation_state WHERE keyset_id = $1`, keysetID); err != nil {
		return nil, fmtErr("delete rotation_state", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmtErr("commit", err)
	}

	return &revokeResult{KeysetID: keysetID, Mode: "halt", Terminated: true, FromKeyID: primaryKeyID}, nil
}

func setRevokeAutoRotate(ctx context.Context, db *sql.DB, autoRotate bool) error {
	_, err := db.ExecContext(ctx, `UPDATE system_state SET revoke_auto_rotate = $1 WHERE id = 1`, autoRotate)
	return err
}
