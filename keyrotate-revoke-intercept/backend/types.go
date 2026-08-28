package main

import "time"

// KeysetRecord mirrors the keysets table. Every keyset in this variant
// rotates on its own random interval -- there's no static/non-rotating
// tier -- until (if ever) it's `Terminated` by a halt-mode revoke,
// which is permanent.
type KeysetRecord struct {
	KeysetID     string     `json:"keysetId"`
	Index        int        `json:"index"`
	CreatedAt    time.Time  `json:"createdAt"`
	Terminated   bool       `json:"terminated"`
	TerminatedAt *time.Time `json:"terminatedAt,omitempty"`
}

// KeyRecord mirrors the keys table, minus raw material.
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
// renders -- one row per keyset.
type KeysetSummary struct {
	KeysetID   string `json:"keysetId"`
	Index      int    `json:"index"`
	Terminated bool   `json:"terminated"`
	Generation int    `json:"generation"`

	PrimaryKeyID string `json:"primaryKeyId"`

	// Status is a derived top-level label:
	//   "rotating"   -- still cycling normally
	//   "terminated" -- permanently halted by a halt-mode revoke
	Status string `json:"status"`

	// LastEventOutcome/At mirror this keyset's most recent
	// rotation_events row, regardless of which trigger produced it --
	// this is what lets the UI (and the tfvars/output json, see
	// tfKeysetEntry in rotation.go) show "revoked, then rotated" vs
	// "revoked, then terminated" vs a perfectly ordinary "rotated" on
	// schedule, in real time.
	LastEventOutcome string     `json:"lastEventOutcome,omitempty"`
	LastEventTrigger string     `json:"lastEventTrigger,omitempty"`
	LastEventAt      *time.Time `json:"lastEventAt,omitempty"`

	ExpiresAt      *time.Time `json:"expiresAt,omitempty"`
	NextRotationIn *float64   `json:"nextRotationInSeconds,omitempty"`
	MinSeconds     *int       `json:"minSeconds,omitempty"`
	MaxSeconds     *int       `json:"maxSeconds,omitempty"`
}

// GenesisRequest optionally overrides N for one batch-genesis call.
type GenesisRequest struct {
	KeysetCount *int `json:"keysetCount,omitempty"`
}

// RevokeRequest is the (optional) body /api/revoke accepts. If
// KeysetID is omitted, the server picks one uniformly at random from
// every still-active (non-terminated) keyset -- the same draw the
// background interceptor loop performs on its own.
type RevokeRequest struct {
	KeysetID *string `json:"keysetId,omitempty"`
}

// StatusResponse is the polling/SSE payload the UI renders.
type StatusResponse struct {
	Initialized bool `json:"initialized"`
	KeysetCount int  `json:"keysetCount"` // N

	Keysets []KeysetSummary `json:"keysets"`
	Keys    []KeyRecord     `json:"keys"`

	// Kill switch. Global -- stops the whole rotation timer loop AND
	// the revoke interceptor loop at once (see runStatusBroadcastLoop /
	// tryRotateAll / runRevokeInterceptorLoop).
	Paused       bool       `json:"paused"`
	PausedAt     *time.Time `json:"pausedAt,omitempty"`
	PausedReason string     `json:"pausedReason,omitempty"`

	// RevokeAutoRotate mirrors system_state.revoke_auto_rotate: true =
	// a revoke immediately emergency-rotates and the keyset resumes
	// normal cycling; false = a revoke permanently halts that keyset.
	RevokeAutoRotate bool `json:"revokeAutoRotate"`

	TfVars          ArtifactView `json:"tfVars"`
	TerraformOutput ArtifactView `json:"terraformOutput"`
	TfVarsHash      string       `json:"tfVarsHash,omitempty"`
	TerraformInSync *bool        `json:"terraformInSync,omitempty"`
	TfConfig        ArtifactView `json:"tfConfig"`

	RenderedResource ArtifactView `json:"renderedResource"`

	History         []RotationEventRecord  `json:"history"`
	GenesisAttempts []GenesisAttemptRecord `json:"genesisAttempts"`
}

// RotationEventRecord mirrors one row of the append-only
// rotation_events table.
type RotationEventRecord struct {
	RotationID  string    `json:"rotationId"`
	KeysetID    string    `json:"keysetId"`
	Trigger     string    `json:"trigger"` // "timer" | "revoke"
	TriggeredAt time.Time `json:"triggeredAt"`
	FromKeyID   *string   `json:"fromKeyId"`
	ToKeyID     *string   `json:"toKeyId"`
	Outcome     string    `json:"outcome"` // rotated | revoked_rotated | revoked_terminated | skipped | failed | in_progress
	Reason      string    `json:"reason"`
}

// GenesisAttemptRecord mirrors one row of genesis_attempts.
type GenesisAttemptRecord struct {
	ID         int64     `json:"id"`
	ReceivedAt time.Time `json:"receivedAt"`
	RemoteAddr string    `json:"remoteAddr,omitempty"`
	Outcome    string    `json:"outcome"`
	Detail     string    `json:"detail,omitempty"`
}

// ArtifactView is a raw file the frontend can render directly.
type ArtifactView struct {
	Path         string     `json:"path"`
	Exists       bool       `json:"exists"`
	Content      string     `json:"content,omitempty"`
	LastModified *time.Time `json:"lastModified,omitempty"`
}
