package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// readArtifact reads a file for display in the UI. Missing files are
// not an error -- e.g. the terraform output file doesn't exist until
// the first successful `terraform apply`, and the UI should just show
// "not written yet" rather than an error state.
func readArtifact(path string) ArtifactView {
	view := ArtifactView{Path: path}
	info, err := os.Stat(path)
	if err != nil {
		return view // Exists stays false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return view
	}
	view.Exists = true
	view.Content = string(data)
	modTime := info.ModTime()
	view.LastModified = &modTime
	return view
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func listKeys(ctx context.Context, db *sql.DB) ([]KeyRecord, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT key_id, generation, parent_key_id, created_at, status, verified_at
		 FROM keys ORDER BY generation ASC`)
	if err != nil {
		return nil, fmtErr("query keys", err)
	}
	defer rows.Close()

	var out []KeyRecord
	for rows.Next() {
		var k KeyRecord
		if err := rows.Scan(&k.KeyID, &k.Generation, &k.ParentKeyID, &k.CreatedAt, &k.Status, &k.VerifiedAt); err != nil {
			return nil, fmtErr("scan key row", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func loadStatus(ctx context.Context, db *sql.DB, sharedDir, tfConfigPath string) (*StatusResponse, error) {
	keys, err := listKeys(ctx, db)
	if err != nil {
		return nil, err
	}

	var lastRotatedAt time.Time
	var intervalSeconds int
	var paused bool
	var pausedAt sql.NullTime
	var pausedReason sql.NullString
	err = db.QueryRowContext(ctx,
		`SELECT last_rotated_at, interval_seconds, paused, paused_at, paused_reason FROM rotation_state WHERE id = 1`,
	).Scan(&lastRotatedAt, &intervalSeconds, &paused, &pausedAt, &pausedReason)
	if err != nil {
		return nil, fmtErr("query rotation_state", err)
	}

	resp := &StatusResponse{
		Keys:            keys,
		LastRotatedAt:   &lastRotatedAt,
		IntervalSeconds: intervalSeconds,
		Paused:          paused,
		PausedReason:    pausedReason.String,
	}
	if pausedAt.Valid {
		resp.PausedAt = &pausedAt.Time
	}

	history, err := listHistory(ctx, db, 100)
	if err != nil {
		return nil, err
	}
	resp.History = history

	attempts, err := listGenesisAttempts(ctx, db, 50)
	if err != nil {
		return nil, err
	}
	resp.GenesisAttempts = attempts

	resp.TfConfig = readArtifact(tfConfigPath)

	nextIn := time.Duration(intervalSeconds)*time.Second - time.Since(lastRotatedAt)
	if nextIn < 0 {
		nextIn = 0
	}
	resp.NextRotationIn = nextIn.Seconds()

	for _, k := range keys {
		if k.Status == "primary" {
			id := k.KeyID
			resp.PrimaryKeyID = &id
			resp.Generation = k.Generation
		}
	}

	var rotationID, reason string
	var applied bool
	err = db.QueryRowContext(ctx,
		`SELECT rotation_id, applied, reason FROM rotation_events ORDER BY triggered_at DESC LIMIT 1`,
	).Scan(&rotationID, &applied, &reason)
	if err == nil {
		resp.LastRotationID = rotationID
		resp.LastTestPassed = &applied
		resp.LastTestDetail = reason
	} else if err != sql.ErrNoRows {
		return nil, fmtErr("query last rotation_event", err)
	}

	// Surface the actual files the two loops read/write. Both live on
	// the same shared volume: Go writes tfvars.json, the
	// terraform-runner sidecar reads it, applies, and writes both the
	// output file and its own applied-hash marker back into the same
	// directory -- so this one read here sees everything either side
	// has produced, with no cross-container RPC needed.
	tfvarsPath := filepath.Join(sharedDir, "rotation.auto.tfvars.json")
	outputPath := filepath.Join(sharedDir, "terraform-output", "current-key-reference.json")
	hashPath := filepath.Join(sharedDir, ".last-applied-hash")

	resp.TfVars = readArtifact(tfvarsPath)
	resp.TerraformOutput = readArtifact(outputPath)

	if resp.TfVars.Exists {
		resp.TfVarsHash = sha256Hex([]byte(resp.TfVars.Content))
	}
	if hashBytes, err := os.ReadFile(hashPath); err == nil {
		resp.LastAppliedHash = strings.TrimSpace(string(hashBytes))
	}
	if resp.TfVarsHash != "" && resp.LastAppliedHash != "" {
		inSync := resp.TfVarsHash == resp.LastAppliedHash
		resp.TerraformInSync = &inSync
	}

	return resp, nil
}
