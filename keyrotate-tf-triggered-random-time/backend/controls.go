package main

import (
	"context"
	"database/sql"
)

// setPaused flips the kill switch. It's a plain UPDATE, not wrapped in
// the rotation loop's own transaction -- the two only need to agree
// via the row's current value, which tryRotate reads with FOR UPDATE
// on its own next tick, not via any in-process signal.
func setPaused(ctx context.Context, db *sql.DB, paused bool, reason string) error {
	if paused {
		_, err := db.ExecContext(ctx,
			`UPDATE rotation_state SET paused = true, paused_at = now(), paused_reason = $1 WHERE id = 1`,
			reason,
		)
		return err
	}
	_, err := db.ExecContext(ctx,
		`UPDATE rotation_state SET paused = false, paused_at = NULL, paused_reason = NULL WHERE id = 1`,
	)
	return err
}

// logGenesisAttempt records exactly one row per /api/genesis request
// the backend receives, before or immediately after the actual
// create-key logic runs. This is called on EVERY branch of
// handleGenesis -- success, "already-initialized" no-op, DB conflict,
// and validation errors -- so the audit trail is a true record of
// "what the server actually saw," not just of successful genesis
// events. That's what closes the re-click gap: even if two clicks
// both reach the backend before either response comes back, both are
// durably logged with a server-assigned timestamp, independent of
// whatever the button's disabled/enabled state was in the browser.
func logGenesisAttempt(ctx context.Context, db *sql.DB, keyID, clientCreatedAt, remoteAddr, outcome, detail string) {
	// Best-effort: a failure to write the audit row should never block
	// or fail the actual genesis response. Logged by the caller if needed.
	db.ExecContext(ctx,
		`INSERT INTO genesis_attempts (attempted_key_id, client_created_at, remote_addr, outcome, detail)
		 VALUES ($1, $2, $3, $4, $5)`,
		nullIfEmpty(keyID), nullIfEmpty(clientCreatedAt), nullIfEmpty(remoteAddr), outcome, nullIfEmpty(detail),
	)
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// listHistory returns the append-only rotation ledger, most recent
// first. Nothing in rotation_events is ever deleted, and rows are
// only ever updated once (a pending attempt finalized to
// applied=true/false) -- so this is a true history, unlike the
// tfvars/output files which are overwritten snapshots of only the
// current state.
func listHistory(ctx context.Context, db *sql.DB, limit int) ([]RotationEventRecord, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT rotation_id, triggered_at, from_key_id, to_key_id, applied, reason
		 FROM rotation_events ORDER BY triggered_at DESC LIMIT $1`,
		limit,
	)
	if err != nil {
		return nil, fmtErr("query rotation_events", err)
	}
	defer rows.Close()

	// Initialized as an empty slice, not left as a nil one: Go's
	// encoding/json marshals a nil slice as the JSON literal `null`,
	// not `[]`. A frontend that (reasonably) assumes any array-typed
	// API field is always an array and calls .map() on it directly
	// will throw on `null` -- and if that throw happens inside a
	// polling loop's try/catch (as it did here), it gets miscounted as
	// a connection failure rather than what it actually is: a shape
	// mismatch between this response and what the client expected.
	out := []RotationEventRecord{}
	for rows.Next() {
		var e RotationEventRecord
		var fromKeyID, toKeyID sql.NullString
		if err := rows.Scan(&e.RotationID, &e.TriggeredAt, &fromKeyID, &toKeyID, &e.Applied, &e.Reason); err != nil {
			return nil, fmtErr("scan rotation_event row", err)
		}
		if fromKeyID.Valid {
			e.FromKeyID = &fromKeyID.String
		}
		if toKeyID.Valid {
			e.ToKeyID = &toKeyID.String
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// listGenesisAttempts returns the genesis audit trail, most recent
// first.
func listGenesisAttempts(ctx context.Context, db *sql.DB, limit int) ([]GenesisAttemptRecord, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, received_at, COALESCE(attempted_key_id, ''), COALESCE(client_created_at, ''),
		        COALESCE(remote_addr, ''), outcome, COALESCE(detail, '')
		 FROM genesis_attempts ORDER BY received_at DESC LIMIT $1`,
		limit,
	)
	if err != nil {
		return nil, fmtErr("query genesis_attempts", err)
	}
	defer rows.Close()

	// Same reasoning as listHistory above: empty slice, not nil, so
	// this always serializes as `[]` even when there are zero rows.
	out := []GenesisAttemptRecord{}
	for rows.Next() {
		var a GenesisAttemptRecord
		if err := rows.Scan(&a.ID, &a.ReceivedAt, &a.AttemptedKeyID, &a.ClientCreatedAt, &a.RemoteAddr, &a.Outcome, &a.Detail); err != nil {
			return nil, fmtErr("scan genesis_attempts row", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
