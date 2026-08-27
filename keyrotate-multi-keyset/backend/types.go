package main

import "time"

// KeysetRecord mirrors the keysets table -- the new top-level entity
// in this variant. There are N of these (default 50), a random M of
// which (default 20) are `Rotating`, and a random L (0..M) of THOSE
// are additionally `Revoked` (needed an immediate/emergency renewal
// at genesis instead of waiting for their normal random expiry).
type KeysetRecord struct {
	KeysetID  string    `json:"keysetId"`
	Index     int       `json:"index"`
	Rotating  bool      `json:"rotating"`
	Revoked   bool      `json:"revoked"`
	CreatedAt time.Time `json:"createdAt"`
}

// KeyRecord mirrors the keys table, minus raw material (never sent to
// clients). Now scoped to a KeysetID -- every key belongs to exactly
// one keyset, and generations/parent chains never cross keyset
// boundaries.
type KeyRecord struct {
	KeyID       string     `json:"keyId"`
	KeysetID    string     `json:"keysetId"`
	Generation  int        `json:"generation"`
	ParentKeyID *string    `json:"parentKeyId"`
	CreatedAt   time.Time  `json:"createdAt"`
	ExpiresAt   time.Time  `json:"expiresAt"`
	Status      string     `json:"status"`
	VerifiedAt  *time.Time `json:"verifiedAt,omitempty"`
}

// KeysetSummary is the per-keyset view the dashboard's overview table
// renders -- one row per keyset (N rows total), combining keysets +
// its current primary key + (if rotating) rotation_state.
type KeysetSummary struct {
	KeysetID   string `json:"keysetId"`
	Index      int    `json:"index"`
	Rotating   bool   `json:"rotating"`
	Revoked    bool   `json:"revoked"`
	Generation int    `json:"generation"`

	PrimaryKeyID string `json:"primaryKeyId"`

	// Status is a derived label for the UI:
	//   "static"           -- not selected for rotation at genesis
	//   "pending-renewal"  -- revoked at genesis, emergency rotation hasn't fired yet
	//   "renewed"          -- was revoked at genesis, emergency rotation has since happened
	//   "rotating"         -- a normal rotating keyset, cycling on its random interval
	Status string `json:"status"`

	ExpiresAt      *time.Time `json:"expiresAt,omitempty"`
	NextRotationIn *float64   `json:"nextRotationInSeconds,omitempty"`
	MinSeconds     *int       `json:"minSeconds,omitempty"`
	MaxSeconds     *int       `json:"maxSeconds,omitempty"`
}

// GenesisRequest is the (optional) body /api/genesis accepts to
// override the N/M defaults for this one batch-genesis call. All
// fields are optional; the server falls back to its own
// KEYSET_COUNT/ROTATING_COUNT env defaults when omitted.
type GenesisRequest struct {
	KeysetCount   *int `json:"keysetCount,omitempty"`   // N
	RotatingCount *int `json:"rotatingCount,omitempty"`  // M
}

// StatusResponse is the polling/SSE payload the UI renders.
type StatusResponse struct {
	Initialized   bool `json:"initialized"`
	KeysetCount   int  `json:"keysetCount"`   // N
	RotatingCount int  `json:"rotatingCount"`  // M
	RevokedCount  int  `json:"revokedCount"`   // L
	StaticCount   int  `json:"staticCount"`    // N - M

	Keysets []KeysetSummary `json:"keysets"`
	Keys    []KeyRecord     `json:"keys"`

	// Kill switch state. Global -- stops every rotating keyset at
	// once. Read from system_state inside the same code path
	// tryRotateAll uses, so it's authoritative and race-free.
	Paused       bool       `json:"paused"`
	PausedAt     *time.Time `json:"pausedAt,omitempty"`
	PausedReason string     `json:"pausedReason,omitempty"`

	// Artifacts expose the actual files the two loops read/write, so
	// the UI can show them instead of asking the person to `docker exec`
	// into a container to find them.
	TfVars          ArtifactView `json:"tfVars"`
	TerraformOutput ArtifactView `json:"terraformOutput"`
	TfVarsHash      string       `json:"tfVarsHash,omitempty"`
	TerraformInSync *bool        `json:"terraformInSync,omitempty"`

	// TfConfig is the actual terraform/main.tf source (the .tf file
	// itself, HCL, not a variable snapshot). Read-only reference so
	// the UI can show *why* the tfvars/output json look the way they
	// do, alongside them -- it's static, mounted read-only into the
	// backend container, and only changes if someone edits the repo.
	TfConfig ArtifactView `json:"tfConfig"`

	// RenderedResource is the live-populated HCL for every keyset at
	// once, generated fresh from the current DB state on every call
	// (see renderKeysetResourceHCL in hcl_render.go).
	RenderedResource ArtifactView `json:"renderedResource"`

	// History is the append-only rotation ledger straight from
	// rotation_events -- every attempt ever made, across every
	// keyset, kept forever.
	History []RotationEventRecord `json:"history"`

	// GenesisAttempts is the server-side audit trail of every
	// /api/genesis request received, in order, regardless of outcome.
	GenesisAttempts []GenesisAttemptRecord `json:"genesisAttempts"`
}

// RotationEventRecord mirrors one row of the append-only
// rotation_events table -- nothing here is ever updated in place
// except applied/reason on the same row (to finalize a still-pending
// attempt), and no row is ever deleted. Now carries KeysetID so the
// history tab can show (and color) which of the N keysets each event
// belongs to.
type RotationEventRecord struct {
	RotationID  string    `json:"rotationId"`
	KeysetID    string    `json:"keysetId"`
	TriggeredAt time.Time `json:"triggeredAt"`
	FromKeyID   *string   `json:"fromKeyId"`
	ToKeyID     *string   `json:"toKeyId"`
	Applied     bool      `json:"applied"`
	Reason      string    `json:"reason"`
}

// GenesisAttemptRecord mirrors one row of the append-only
// genesis_attempts audit table. There is normally exactly one
// "created" row for the life of a stack (batch genesis is a one-time,
// all-N-keysets-at-once event), plus one "already-initialized" row
// per extra click after that.
type GenesisAttemptRecord struct {
	ID         int64     `json:"id"`
	ReceivedAt time.Time `json:"receivedAt"`
	RemoteAddr string    `json:"remoteAddr,omitempty"`
	Outcome    string    `json:"outcome"`
	Detail     string    `json:"detail,omitempty"`
}

// ArtifactView is a raw file the frontend can render directly (the
// tfvars file Go writes, or the output file Terraform writes). Both
// live on the same shared Docker volume so either process can read
// what the other wrote.
type ArtifactView struct {
	Path         string     `json:"path"`
	Exists       bool       `json:"exists"`
	Content      string     `json:"content,omitempty"`
	LastModified *time.Time `json:"lastModified,omitempty"`
}
