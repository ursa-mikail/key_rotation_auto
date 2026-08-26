package main

import "time"

// KeyRecord mirrors the keys table, minus raw material (never sent to clients).
type KeyRecord struct {
	KeyID       string     `json:"keyId"`
	Generation  int        `json:"generation"`
	ParentKeyID *string    `json:"parentKeyId"`
	CreatedAt   time.Time  `json:"createdAt"`
	Status      string     `json:"status"`
	VerifiedAt  *time.Time `json:"verifiedAt,omitempty"`
}

// GenesisRequest is what the TypeScript frontend posts to create key
// generation 0. The frontend generates the raw material client-side
// (Web Crypto getRandomValues) so the backend never has to be trusted
// with "the only copy" of the very first key.
type GenesisRequest struct {
	KeyID     string `json:"keyId"`
	CreatedAt string `json:"createdAt"`
	Algorithm string `json:"algorithm"`
	Material  string `json:"material"` // base64, 32 bytes for AES-256-GCM
}

// StatusResponse is the polling/SSE payload the UI renders.
type StatusResponse struct {
	PrimaryKeyID    *string     `json:"primaryKeyId"`
	Generation      int         `json:"generation"`
	LastRotatedAt   *time.Time  `json:"lastRotatedAt"`
	IntervalSeconds int         `json:"intervalSeconds"`
	NextRotationIn  float64     `json:"nextRotationInSeconds"`
	Keys            []KeyRecord `json:"keys"`
	LastRotationID  string      `json:"lastRotationId,omitempty"`
	LastTestPassed  *bool       `json:"lastTestPassed,omitempty"`
	LastTestDetail  string      `json:"lastTestDetail,omitempty"`

	// Kill switch state. Read from rotation_state inside the same
	// locked transaction the rotation loop uses, so it's authoritative
	// and race-free across replicas -- see tryRotate in rotation.go.
	Paused       bool       `json:"paused"`
	PausedAt     *time.Time `json:"pausedAt,omitempty"`
	PausedReason string     `json:"pausedReason,omitempty"`

	// Artifacts expose the actual files the two loops read/write, so
	// the UI can show them instead of asking the person to `docker exec`
	// into a container to find them.
	TfVars          ArtifactView `json:"tfVars"`
	TerraformOutput ArtifactView `json:"terraformOutput"`
	TfVarsHash      string       `json:"tfVarsHash,omitempty"`
	LastAppliedHash string       `json:"lastAppliedHash,omitempty"`
	TerraformInSync *bool        `json:"terraformInSync,omitempty"`

	// TfConfig is the actual terraform/main.tf source (the .tf file
	// itself, HCL, not a variable snapshot). Read-only reference so
	// the UI can show *why* the tfvars/output json look the way they
	// do, alongside them -- it's static, mounted read-only into the
	// backend container, and only changes if someone edits the repo.
	TfConfig ArtifactView `json:"tfConfig"`

	// History is the append-only rotation ledger straight from
	// rotation_events -- every attempt ever made, kept forever. This
	// is the real "appended" history; TfVars/TerraformOutput above are
	// deliberately overwritten snapshots of *current* state instead.
	// See README, "Snapshot vs. history" for why both exist on purpose.
	History []RotationEventRecord `json:"history"`

	// GenesisAttempts is the server-side audit trail of every
	// /api/genesis request received, in order, regardless of outcome.
	// This is what makes a rapid re-click of "Generate Genesis Key"
	// traceable: each attempt gets a row here the instant the backend
	// receives it, before any DB write that could actually create a key.
	GenesisAttempts []GenesisAttemptRecord `json:"genesisAttempts"`
}

// RotationEventRecord mirrors one row of the append-only
// rotation_events table -- nothing here is ever updated in place
// except applied/reason on the same row (to finalize a still-pending
// attempt), and no row is ever deleted.
type RotationEventRecord struct {
	RotationID  string    `json:"rotationId"`
	TriggeredAt time.Time `json:"triggeredAt"`
	FromKeyID   *string   `json:"fromKeyId"`
	ToKeyID     *string   `json:"toKeyId"`
	Applied     bool      `json:"applied"`
	Reason      string    `json:"reason"`
}

// GenesisAttemptRecord mirrors one row of the append-only
// genesis_attempts audit table.
type GenesisAttemptRecord struct {
	ID              int64     `json:"id"`
	ReceivedAt      time.Time `json:"receivedAt"`
	AttemptedKeyID  string    `json:"attemptedKeyId,omitempty"`
	ClientCreatedAt string    `json:"clientCreatedAt,omitempty"`
	RemoteAddr      string    `json:"remoteAddr,omitempty"`
	Outcome         string    `json:"outcome"`
	Detail          string    `json:"detail,omitempty"`
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
