package main

import (
	"context"
	"database/sql"
)

// setPaused flips the global kill switch. Stops the timer loop AND the
// revoke interceptor loop -- both check this same flag on their own
// next tick.
func setPaused(ctx context.Context, db *sql.DB, paused bool, reason string) error {
	if paused {
		_, err := db.ExecContext(ctx,
			`UPDATE system_state SET paused = true, paused_at = now(), paused_reason = $1 WHERE id = 1`,
			reason,
		)
		return err
	}
	_, err := db.ExecContext(ctx,
		`UPDATE system_state SET paused = false, paused_at = NULL, paused_reason = NULL WHERE id = 1`,
	)
	return err
}

func logGenesisAttempt(ctx context.Context, db *sql.DB, remoteAddr, outcome, detail string) {
	db.ExecContext(ctx,
		`INSERT INTO genesis_attempts (remote_addr, outcome, detail) VALUES ($1, $2, $3)`,
		nullIfEmpty(remoteAddr), outcome, nullIfEmpty(detail),
	)
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// listHistory returns the append-only rotation ledger, most recent
// first, across every keyset and both triggers (timer + revoke).
// Excludes rows still sitting at the transient 'in_progress' outcome
// (a crash mid-transaction would otherwise leave a permanently
// confusing row; in practice these are only ever visible for the
// instant between the two writes inside rotateOneKeyset's own
// transaction, so a caller polling /api/status essentially never sees
// one anyway).
func listHistory(ctx context.Context, db *sql.DB, limit int) ([]RotationEventRecord, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT rotation_id, keyset_id, trigger, triggered_at, from_key_id, to_key_id, outcome, reason
		 FROM rotation_events WHERE outcome != 'in_progress' ORDER BY triggered_at DESC LIMIT $1`,
		limit,
	)
	if err != nil {
		return nil, fmtErr("query rotation_events", err)
	}
	defer rows.Close()

	out := []RotationEventRecord{}
	for rows.Next() {
		var e RotationEventRecord
		var fromKeyID, toKeyID sql.NullString
		if err := rows.Scan(&e.RotationID, &e.KeysetID, &e.Trigger, &e.TriggeredAt, &fromKeyID, &toKeyID, &e.Outcome, &e.Reason); err != nil {
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

func listGenesisAttempts(ctx context.Context, db *sql.DB, limit int) ([]GenesisAttemptRecord, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, received_at, COALESCE(remote_addr, ''), outcome, COALESCE(detail, '')
		 FROM genesis_attempts ORDER BY received_at DESC LIMIT $1`,
		limit,
	)
	if err != nil {
		return nil, fmtErr("query genesis_attempts", err)
	}
	defer rows.Close()

	out := []GenesisAttemptRecord{}
	for rows.Next() {
		var a GenesisAttemptRecord
		if err := rows.Scan(&a.ID, &a.ReceivedAt, &a.RemoteAddr, &a.Outcome, &a.Detail); err != nil {
			return nil, fmtErr("scan genesis_attempts row", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
