package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"time"
)

func withCORS(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// handleGenesis is the entire "N keysets, all rotating" batch event.
// Idempotent: once system_state.initialized is true, every subsequent
// call just reports what already exists.
func handleGenesis(db *sql.DB, sharedDir string, defaultN, minSeconds, maxSeconds int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ctx := r.Context()

		n := defaultN
		var req GenesisRequest
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&req)
		}
		if req.KeysetCount != nil {
			n = *req.KeysetCount
		}

		result, err := runGenesisBatch(ctx, db, sharedDir, n, minSeconds, maxSeconds)
		if err != nil {
			logGenesisAttempt(ctx, db, r.RemoteAddr, "rejected", err.Error())
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}

		if result.Status == "already-initialized" {
			logGenesisAttempt(ctx, db, r.RemoteAddr, "already-initialized", "batch genesis already ran for this stack")
		} else {
			logGenesisAttempt(ctx, db, r.RemoteAddr, "created", "batch genesis created keysets")
		}

		status := http.StatusCreated
		if result.Status == "already-initialized" {
			status = http.StatusOK
		}
		writeJSON(w, status, result)
	}
}

// handleRotateIfDue is the timer-triggered path Terraform's
// local-exec provisioner calls.
func handleRotateIfDue(db *sql.DB, sharedDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		results, err := tryRotateAll(r.Context(), db, sharedDir)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}

		out := make([]map[string]any, 0, len(results))
		rotatedCount := 0
		for _, res := range results {
			entry := map[string]any{
				"keysetId":   res.KeysetID,
				"rotated":    res.Rotated,
				"reason":     res.Skipped,
				"rotationId": res.RotationID,
			}
			if res.Rotated {
				rotatedCount++
				entry["fromKeyId"] = res.FromKeyID
				entry["toKeyId"] = res.ToKeyID
				entry["testPassed"] = res.TestPassed
				entry["testDetail"] = res.TestDetail
				entry["nextExpiresAt"] = res.NextExpiresAt.UTC().Format(time.RFC3339)
			}
			out = append(out, entry)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"rotatedCount": rotatedCount,
			"results":      out,
		})
	}
}

// handleRevoke is the manual trigger for the SAME path the background
// interceptor loop calls on its own: POST an (optional) keysetId, or
// omit it to have the server pick a random still-active keyset itself
// -- useful for demoing the collision case on demand ("call for
// revoke" right as a keyset's normal timer is about to fire).
func handleRevoke(db *sql.DB, sharedDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ctx := r.Context()

		var req RevokeRequest
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&req)
		}

		keysetID := ""
		if req.KeysetID != nil {
			keysetID = *req.KeysetID
		} else {
			id, ok, err := pickRandomActiveKeyset(ctx, db)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			if !ok {
				writeJSON(w, http.StatusOK, map[string]string{"skipped": "every keyset is already terminated"})
				return
			}
			keysetID = id
		}

		res, err := processRevoke(ctx, db, keysetID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if res.Rotated || res.Terminated {
			if err := refreshTerraformVars(ctx, db, sharedDir); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
		}
		writeJSON(w, http.StatusOK, res)
	}
}

// handleRevokeMode is the "give an option if we want to turn on
// revoke and auto-rotate / just halt after rotation" toggle: POST
// {"autoRotate": true|false}.
func handleRevokeMode(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			AutoRotate bool `json:"autoRotate"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body: expected {\"autoRotate\": true|false}"})
			return
		}
		if err := setRevokeAutoRotate(r.Context(), db, req.AutoRotate); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"autoRotate": req.AutoRotate})
	}
}

func handleStatus(db *sql.DB, sharedDir, tfConfigPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status, err := loadStatus(r.Context(), db, sharedDir, tfConfigPath)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, status)
	}
}

// handleRotationControl is the kill switch: POST /api/rotation/pause
// and POST /api/rotation/resume both route here. Stops (or resumes)
// both the timer loop and the revoke interceptor loop at once.
func handleRotationControl(db *sql.DB, paused bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Reason string `json:"reason"`
		}
		json.NewDecoder(r.Body).Decode(&req)

		reason := req.Reason
		if paused && reason == "" {
			reason = "paused via API"
		}
		if err := setPaused(r.Context(), db, paused, reason); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"paused": paused})
	}
}

func handleKeys(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		keys, err := listKeys(r.Context(), db)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, keys)
	}
}

func handleKeysets(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		keysets, err := buildKeysetSummaries(r.Context(), db)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, keysets)
	}
}

func handleHealthz(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := db.PingContext(ctx); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "db unreachable"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}
