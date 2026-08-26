package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

// primaryKeyExpiration finds the entry with primary = true inside a
// keyset json's `keys` list and returns its `expiration` field. Used
// on both the tfvars file and the terraform output file (they share
// this shape -- see rotation.go's tfKeyEntry and main.tf's var.keys /
// local_file content) to compare "what Go most recently said" against
// "what Terraform last echoed back," now that expiration is per-key
// rather than a single top-level field on the file.
func primaryKeyExpiration(content string) (string, bool) {
	if content == "" {
		return "", false
	}
	var v struct {
		Keys []struct {
			Expiration string `json:"expiration"`
			Primary    bool   `json:"primary"`
		} `json:"keys"`
	}
	if err := json.Unmarshal([]byte(content), &v); err != nil {
		return "", false
	}
	for _, k := range v.Keys {
		if k.Primary && k.Expiration != "" {
			return k.Expiration, true
		}
	}
	return "", false
}

func listKeys(ctx context.Context, db *sql.DB) ([]KeyRecord, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT key_id, generation, parent_key_id, created_at, expires_at, status, verified_at
		 FROM keys ORDER BY generation ASC`)
	if err != nil {
		return nil, fmtErr("query keys", err)
	}
	defer rows.Close()

	// Empty slice, not nil -- same reasoning as listHistory/
	// listGenesisAttempts in controls.go: this must serialize as `[]`,
	// not `null`, for any client that assumes an array-typed field is
	// always an array.
	out := []KeyRecord{}
	for rows.Next() {
		var k KeyRecord
		if err := rows.Scan(&k.KeyID, &k.Generation, &k.ParentKeyID, &k.CreatedAt, &k.ExpiresAt, &k.Status, &k.VerifiedAt); err != nil {
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
	var expiresAt time.Time
	var minSeconds, maxSeconds int
	var paused bool
	var pausedAt sql.NullTime
	var pausedReason sql.NullString
	err = db.QueryRowContext(ctx,
		`SELECT last_rotated_at, expires_at, min_seconds, max_seconds, paused, paused_at, paused_reason
		 FROM rotation_state WHERE id = 1`,
	).Scan(&lastRotatedAt, &expiresAt, &minSeconds, &maxSeconds, &paused, &pausedAt, &pausedReason)
	if err != nil {
		return nil, fmtErr("query rotation_state", err)
	}

	resp := &StatusResponse{
		Keys:          keys,
		LastRotatedAt: &lastRotatedAt,
		ExpiresAt:     &expiresAt,
		MinSeconds:    minSeconds,
		MaxSeconds:    maxSeconds,
		Paused:        paused,
		PausedReason:  pausedReason.String,
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

	// Live-rendered resource block, regenerated fresh from `keys`
	// (already fetched above) on every single status call -- this is
	// what actually answers "what does the real resource look like
	// right now," as opposed to TfConfig (static source) or TfVars/
	// TerraformOutput (raw JSON).
	resp.RenderedResource = ArtifactView{
		Path:    fmt.Sprintf("(live-rendered) resource %q %q", terraformResourceType, terraformResourceName),
		Exists:  true,
		Content: renderKeysetResourceHCL(keys),
	}

	nextIn := time.Until(expiresAt)
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

	// Sync check: compares the PRIMARY key's own `expiration` (per-key
	// field, per the "your comment about matching a real keyset
	// resource" redesign -- see rotation.go's tfKeyEntry) between the
	// two files. Because of the one-cycle lag documented in main.tf,
	// these will *normally* disagree for most of any given cycle --
	// that's expected, not a bug -- and only briefly agree right after
	// a cycle where nothing changed between the apply and the next poll.
	tfvarsPath := filepath.Join(sharedDir, "rotation.auto.tfvars.json")
	outputPath := filepath.Join(sharedDir, "terraform-output", "current-key-reference.json")

	resp.TfVars = readArtifact(tfvarsPath)
	resp.TerraformOutput = readArtifact(outputPath)

	if resp.TfVars.Exists {
		resp.TfVarsHash = sha256Hex([]byte(resp.TfVars.Content))
	}
	if tfvarsExpiry, ok := primaryKeyExpiration(resp.TfVars.Content); ok {
		if outputExpiry, ok := primaryKeyExpiration(resp.TerraformOutput.Content); ok {
			inSync := tfvarsExpiry == outputExpiry
			resp.TerraformInSync = &inSync
		}
	}

	return resp, nil
}
