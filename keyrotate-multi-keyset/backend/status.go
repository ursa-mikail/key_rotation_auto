package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
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

// primaryExpirations parses a tfvars/output-shaped json blob (both
// share the `{"keysets":[{"id","keys":[{"expiration","primary"}...]}...]}`
// shape -- see rotation.go's tfKeysetEntry / terraform/main.tf's
// var.keysets) and returns a map of keysetId -> that keyset's current
// primary key's expiration. Used to compare the tfvars file against
// the terraform output file across every keyset at once.
func primaryExpirations(content string) (map[string]string, bool) {
	if content == "" {
		return nil, false
	}
	var v struct {
		Keysets []struct {
			ID   string `json:"id"`
			Keys []struct {
				Expiration string `json:"expiration"`
				Primary    bool   `json:"primary"`
			} `json:"keys"`
		} `json:"keysets"`
	}
	if err := json.Unmarshal([]byte(content), &v); err != nil {
		return nil, false
	}
	out := make(map[string]string, len(v.Keysets))
	for _, ks := range v.Keysets {
		for _, k := range ks.Keys {
			if k.Primary {
				out[ks.ID] = k.Expiration
				break
			}
		}
	}
	return out, true
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// listKeys returns every key across every keyset, ordered so each
// keyset's own generations stay contiguous and increasing.
func listKeys(ctx context.Context, db *sql.DB) ([]KeyRecord, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT key_id, keyset_id, generation, parent_key_id, created_at, expires_at, status, verified_at
		 FROM keys ORDER BY keyset_id ASC, generation ASC`)
	if err != nil {
		return nil, fmtErr("query keys", err)
	}
	defer rows.Close()

	out := []KeyRecord{}
	for rows.Next() {
		var k KeyRecord
		if err := rows.Scan(&k.KeyID, &k.KeysetID, &k.Generation, &k.ParentKeyID, &k.CreatedAt, &k.ExpiresAt, &k.Status, &k.VerifiedAt); err != nil {
			return nil, fmtErr("scan key row", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// listKeysets returns every keysets row, ordered by idx.
func listKeysets(ctx context.Context, db *sql.DB) ([]KeysetRecord, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT keyset_id, idx, rotating, revoked, created_at FROM keysets ORDER BY idx ASC`)
	if err != nil {
		return nil, fmtErr("query keysets", err)
	}
	defer rows.Close()

	out := []KeysetRecord{}
	for rows.Next() {
		var k KeysetRecord
		if err := rows.Scan(&k.KeysetID, &k.Index, &k.Rotating, &k.Revoked, &k.CreatedAt); err != nil {
			return nil, fmtErr("scan keyset row", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// listKeysetsWithKeys bundles every keyset with its full key list --
// the shape writeTerraformVars and renderKeysetResourceHCL consume.
func listKeysetsWithKeys(ctx context.Context, db *sql.DB) ([]keysetWithKeys, error) {
	keysets, err := listKeysets(ctx, db)
	if err != nil {
		return nil, err
	}
	keys, err := listKeys(ctx, db)
	if err != nil {
		return nil, err
	}
	byKeyset := make(map[string][]KeyRecord, len(keysets))
	for _, k := range keys {
		byKeyset[k.KeysetID] = append(byKeyset[k.KeysetID], k)
	}
	out := make([]keysetWithKeys, 0, len(keysets))
	for _, ks := range keysets {
		out = append(out, keysetWithKeys{KeysetRecord: ks, Keys: byKeyset[ks.KeysetID]})
	}
	return out, nil
}

// buildKeysetSummaries joins keysets + their current primary key +
// (for rotating keysets) rotation_state into the per-row shape the
// dashboard's overview table renders.
func buildKeysetSummaries(ctx context.Context, db *sql.DB) ([]KeysetSummary, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT
			ks.keyset_id, ks.idx, ks.rotating, ks.revoked,
			k.key_id, k.generation,
			rs.expires_at, rs.min_seconds, rs.max_seconds
		FROM keysets ks
		JOIN keys k ON k.keyset_id = ks.keyset_id AND k.status = 'primary'
		LEFT JOIN rotation_state rs ON rs.keyset_id = ks.keyset_id
		ORDER BY ks.idx ASC
	`)
	if err != nil {
		return nil, fmtErr("query keyset summaries", err)
	}
	defer rows.Close()

	out := []KeysetSummary{}
	now := time.Now()
	for rows.Next() {
		var s KeysetSummary
		var expiresAt sql.NullTime
		var minSeconds, maxSeconds sql.NullInt64
		if err := rows.Scan(
			&s.KeysetID, &s.Index, &s.Rotating, &s.Revoked,
			&s.PrimaryKeyID, &s.Generation,
			&expiresAt, &minSeconds, &maxSeconds,
		); err != nil {
			return nil, fmtErr("scan keyset summary row", err)
		}

		switch {
		case !s.Rotating:
			s.Status = "static"
		case s.Revoked && s.Generation == 0:
			s.Status = "pending-renewal"
		case s.Revoked:
			s.Status = "renewed"
		default:
			s.Status = "rotating"
		}

		if expiresAt.Valid {
			t := expiresAt.Time
			s.ExpiresAt = &t
			nextIn := t.Sub(now).Seconds()
			if nextIn < 0 {
				nextIn = 0
			}
			s.NextRotationIn = &nextIn
		}
		if minSeconds.Valid {
			m := int(minSeconds.Int64)
			s.MinSeconds = &m
		}
		if maxSeconds.Valid {
			m := int(maxSeconds.Int64)
			s.MaxSeconds = &m
		}

		out = append(out, s)
	}
	return out, rows.Err()
}

func loadStatus(ctx context.Context, db *sql.DB, sharedDir, tfConfigPath string) (*StatusResponse, error) {
	resp := &StatusResponse{}

	var paused bool
	var pausedAt sql.NullTime
	var pausedReason sql.NullString
	err := db.QueryRowContext(ctx,
		`SELECT initialized, keyset_count, rotating_count, revoked_count, paused, paused_at, paused_reason
		 FROM system_state WHERE id = 1`,
	).Scan(&resp.Initialized, &resp.KeysetCount, &resp.RotatingCount, &resp.RevokedCount, &paused, &pausedAt, &pausedReason)
	if err != nil {
		return nil, fmtErr("query system_state", err)
	}
	resp.StaticCount = resp.KeysetCount - resp.RotatingCount
	resp.Paused = paused
	resp.PausedReason = pausedReason.String
	if pausedAt.Valid {
		resp.PausedAt = &pausedAt.Time
	}

	summaries, err := buildKeysetSummaries(ctx, db)
	if err != nil {
		return nil, err
	}
	resp.Keysets = summaries

	keys, err := listKeys(ctx, db)
	if err != nil {
		return nil, err
	}
	resp.Keys = keys

	history, err := listHistory(ctx, db, 200)
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

	// Live-rendered HCL for every keyset at once, regenerated fresh
	// on every single status call from whatever listKeysetsWithKeys
	// currently returns.
	keysetsWithKeys, err := listKeysetsWithKeys(ctx, db)
	if err != nil {
		return nil, err
	}
	resp.RenderedResource = ArtifactView{
		Path:    "(live-rendered) resource \"ursa_keyset\" blocks, one per keyset",
		Exists:  len(keysetsWithKeys) > 0,
		Content: renderKeysetResourceHCL(keysetsWithKeys),
	}

	tfvarsPath := filepath.Join(sharedDir, "rotation.auto.tfvars.json")
	outputPath := filepath.Join(sharedDir, "terraform-output", "current-key-reference.json")

	resp.TfVars = readArtifact(tfvarsPath)
	resp.TerraformOutput = readArtifact(outputPath)

	if resp.TfVars.Exists {
		resp.TfVarsHash = sha256Hex([]byte(resp.TfVars.Content))
	}

	// Sync check across every keyset at once: compares the full
	// keysetId -> primary-expiration map between the tfvars file and
	// the terraform output file. Because of the one-cycle lag (see
	// README), these will *normally* disagree for most of any given
	// cycle -- that's expected, not a bug.
	if tfvarsExp, ok := primaryExpirations(resp.TfVars.Content); ok {
		if outputExp, ok := primaryExpirations(resp.TerraformOutput.Content); ok {
			inSync := mapsEqual(tfvarsExp, outputExp)
			resp.TerraformInSync = &inSync
		}
	}

	return resp, nil
}
