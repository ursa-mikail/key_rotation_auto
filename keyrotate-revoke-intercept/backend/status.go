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

func readArtifact(path string) ArtifactView {
	view := ArtifactView{Path: path}
	info, err := os.Stat(path)
	if err != nil {
		return view
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

// primaryExpirations parses a tfvars/output-shaped json blob and
// returns keysetId -> that keyset's current primary key's expiration,
// for the tfvars/output sync check.
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

func listKeys(ctx context.Context, db *sql.DB) ([]KeyRecord, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT key_id, keyset_id, generation, parent_key_id, created_at, expires_at, status, verified_at, revoked_at
		 FROM keys ORDER BY keyset_id ASC, generation ASC`)
	if err != nil {
		return nil, fmtErr("query keys", err)
	}
	defer rows.Close()

	out := []KeyRecord{}
	for rows.Next() {
		var k KeyRecord
		var revokedAt sql.NullTime
		if err := rows.Scan(&k.KeyID, &k.KeysetID, &k.Generation, &k.ParentKeyID, &k.CreatedAt, &k.ExpiresAt, &k.Status, &k.VerifiedAt, &revokedAt); err != nil {
			return nil, fmtErr("scan key row", err)
		}
		if revokedAt.Valid {
			k.RevokedAt = &revokedAt.Time
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func listKeysets(ctx context.Context, db *sql.DB) ([]KeysetRecord, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT keyset_id, idx, created_at, terminated, terminated_at FROM keysets ORDER BY idx ASC`)
	if err != nil {
		return nil, fmtErr("query keysets", err)
	}
	defer rows.Close()

	out := []KeysetRecord{}
	for rows.Next() {
		var k KeysetRecord
		var terminatedAt sql.NullTime
		if err := rows.Scan(&k.KeysetID, &k.Index, &k.CreatedAt, &k.Terminated, &terminatedAt); err != nil {
			return nil, fmtErr("scan keyset row", err)
		}
		if terminatedAt.Valid {
			k.TerminatedAt = &terminatedAt.Time
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// lastEvent bundles the outcome/trigger/timestamp of a keyset's most
// recent rotation_events row.
type lastEvent struct {
	outcome, trigger string
	at               time.Time
}

// listLastEvents returns, for every keyset that has at least one
// rotation_events row, only its single most recent one -- via a
// LATERAL join rather than fetching full history and reducing in Go,
// since only N rows (one per keyset) are actually needed here.
func listLastEvents(ctx context.Context, db *sql.DB) (map[string]lastEvent, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT ks.keyset_id, re.outcome, re.trigger, re.triggered_at
		FROM keysets ks
		JOIN LATERAL (
			SELECT outcome, trigger, triggered_at FROM rotation_events
			WHERE keyset_id = ks.keyset_id AND outcome != 'in_progress'
			ORDER BY triggered_at DESC LIMIT 1
		) re ON true
	`)
	if err != nil {
		return nil, fmtErr("query last events", err)
	}
	defer rows.Close()

	out := make(map[string]lastEvent)
	for rows.Next() {
		var keysetID string
		var e lastEvent
		if err := rows.Scan(&keysetID, &e.outcome, &e.trigger, &e.at); err != nil {
			return nil, fmtErr("scan last event row", err)
		}
		out[keysetID] = e
	}
	return out, rows.Err()
}

// listKeysetsWithKeys bundles every keyset with its full key list and
// its most recent rotation_events outcome -- the shape
// writeTerraformVars and renderKeysetResourceHCL consume, so the
// tfvars/output json and the live HCL both show the "revoked and
// rotated / revoked and terminated" status in real time.
func listKeysetsWithKeys(ctx context.Context, db *sql.DB) ([]keysetWithKeys, error) {
	keysets, err := listKeysets(ctx, db)
	if err != nil {
		return nil, err
	}
	keys, err := listKeys(ctx, db)
	if err != nil {
		return nil, err
	}
	lastEvents, err := listLastEvents(ctx, db)
	if err != nil {
		return nil, err
	}
	byKeyset := make(map[string][]KeyRecord, len(keysets))
	for _, k := range keys {
		byKeyset[k.KeysetID] = append(byKeyset[k.KeysetID], k)
	}
	out := make([]keysetWithKeys, 0, len(keysets))
	for _, ks := range keysets {
		entry := keysetWithKeys{KeysetRecord: ks, Keys: byKeyset[ks.KeysetID]}
		if e, ok := lastEvents[ks.KeysetID]; ok {
			entry.LastEventOutcome = e.outcome
			entry.LastEventTrigger = e.trigger
			t := e.at
			entry.LastEventAt = &t
		}
		out = append(out, entry)
	}
	return out, nil
}

// buildKeysetSummaries joins keysets + current primary key +
// rotation_state (for still-active keysets) + most recent
// rotation_events row into the per-row shape the dashboard renders.
func buildKeysetSummaries(ctx context.Context, db *sql.DB) ([]KeysetSummary, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT
			ks.keyset_id, ks.idx, ks.terminated,
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

	summaries := []KeysetSummary{}
	now := time.Now()
	for rows.Next() {
		var s KeysetSummary
		var expiresAt sql.NullTime
		var minSeconds, maxSeconds sql.NullInt64
		if err := rows.Scan(
			&s.KeysetID, &s.Index, &s.Terminated,
			&s.PrimaryKeyID, &s.Generation,
			&expiresAt, &minSeconds, &maxSeconds,
		); err != nil {
			return nil, fmtErr("scan keyset summary row", err)
		}

		if s.Terminated {
			s.Status = "terminated"
		} else {
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

		summaries = append(summaries, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	lastEvents, err := listLastEvents(ctx, db)
	if err != nil {
		return nil, err
	}
	for i := range summaries {
		if e, ok := lastEvents[summaries[i].KeysetID]; ok {
			summaries[i].LastEventOutcome = e.outcome
			summaries[i].LastEventTrigger = e.trigger
			t := e.at
			summaries[i].LastEventAt = &t
		}
	}

	return summaries, nil
}

func loadStatus(ctx context.Context, db *sql.DB, sharedDir, tfConfigPath string) (*StatusResponse, error) {
	resp := &StatusResponse{}

	var paused bool
	var pausedAt sql.NullTime
	var pausedReason sql.NullString
	err := db.QueryRowContext(ctx,
		`SELECT initialized, keyset_count, paused, paused_at, paused_reason, revoke_auto_rotate
		 FROM system_state WHERE id = 1`,
	).Scan(&resp.Initialized, &resp.KeysetCount, &paused, &pausedAt, &pausedReason, &resp.RevokeAutoRotate)
	if err != nil {
		return nil, fmtErr("query system_state", err)
	}
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

	if tfvarsExp, ok := primaryExpirations(resp.TfVars.Content); ok {
		if outputExp, ok := primaryExpirations(resp.TerraformOutput.Content); ok {
			inSync := mapsEqual(tfvarsExp, outputExp)
			resp.TerraformInSync = &inSync
		}
	}

	return resp, nil
}
